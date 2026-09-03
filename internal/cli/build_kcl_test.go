package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestKCLBuildPlanHelpers covers the small helpers that runBuild uses
// to drive the docker-skip set and the platform override from a parsed
// KCL entity set. The runBuild path itself is exercised end-to-end by
// the cp-forge env smoke (post-agent-A) — these unit tests guard the
// dispatch / accessor invariants.
func TestKCLBuildPlanHelpers(t *testing.T) {
	entities, err := parseKCLEntities([]byte(sampleKCLJSON))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}

	if !kclHasClusterService(entities) {
		t.Error("kclHasClusterService: want true (sample has workspace-proxy)")
	}
	if got := kclFirstClusterPlatform(entities); got != "amd64" {
		t.Errorf("kclFirstClusterPlatform: got %q, want amd64", got)
	}
}

// TestKCLHasClusterService_AllHost confirms the all-host-services
// scenario flips the docker-skip switch — runBuild uses this to decide
// whether the project docker image is needed at all for the env.
func TestKCLHasClusterService_AllHost(t *testing.T) {
	allHost := `{
  "services": [
    {"name": "a", "deploy": {"type": "host", "runner": "go-run"}},
    {"name": "b", "deploy": {"type": "build-only", "build_variants": [{"name": "default"}]}}
  ]
}`
	entities, err := parseKCLEntities([]byte(allHost))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if kclHasClusterService(entities) {
		t.Error("kclHasClusterService: want false when no cluster service")
	}
	if got := kclFirstClusterPlatform(entities); got != "" {
		t.Errorf("kclFirstClusterPlatform on no-cluster: got %q, want empty", got)
	}
}

// TestFilterFrontendsForBuild pins Item 3: host-mode frontends are
// dropped from the prod-build set (their dev server doesn't consume
// the build artifact); cluster-mode frontends are kept; and frontends
// with no KCL deploy block (legacy) fall through to "build" so we
// don't silently change behaviour for projects pre-deploy-discriminator.
func TestFilterFrontendsForBuild(t *testing.T) {
	frontends := []config.FrontendConfig{
		config.FrontendConfig{Name: "web"}.WithDir("frontend"),
		config.FrontendConfig{Name: "admin"}.WithDir("admin"),
		config.FrontendConfig{Name: "legacy"}.WithDir("legacy"),
	}
	entities := &KCLEntities{
		Frontends: []FrontendEntity{
			{Name: "web", Deploy: &FrontendDeployEntity{Type: "host"}},
			{Name: "admin", Deploy: &FrontendDeployEntity{Type: "cluster"}},
			// "legacy" has no Deploy block — falls through to "build".
			{Name: "legacy"},
		},
	}
	got := filterFrontendsForBuild(frontends, entities)
	if len(got) != 2 {
		t.Fatalf("filterFrontendsForBuild: got %d kept, want 2 (admin + legacy)", len(got))
	}
	names := map[string]bool{}
	for _, fe := range got {
		names[fe.Name] = true
	}
	if names["web"] {
		t.Errorf("filterFrontendsForBuild: host-mode 'web' was kept; want skipped")
	}
	if !names["admin"] {
		t.Errorf("filterFrontendsForBuild: cluster-mode 'admin' was dropped; want kept")
	}
	if !names["legacy"] {
		t.Errorf("filterFrontendsForBuild: legacy 'legacy' (no deploy) was dropped; want kept")
	}
}

// TestFrontendDeployMode covers the lookup helper across its three
// branches: matching frontend with deploy → type; matching frontend
// without deploy → ""; missing frontend → "".
func TestFrontendDeployMode(t *testing.T) {
	entities := &KCLEntities{
		Frontends: []FrontendEntity{
			{Name: "web", Deploy: &FrontendDeployEntity{Type: "Host"}},
			{Name: "admin", Deploy: &FrontendDeployEntity{Type: "cluster"}},
			{Name: "legacy"},
		},
	}
	if got := frontendDeployMode(entities, "web"); got != "host" {
		t.Errorf("web: got %q, want host (case-folded)", got)
	}
	if got := frontendDeployMode(entities, "admin"); got != "cluster" {
		t.Errorf("admin: got %q, want cluster", got)
	}
	if got := frontendDeployMode(entities, "legacy"); got != "" {
		t.Errorf("legacy (no deploy): got %q, want empty", got)
	}
	if got := frontendDeployMode(entities, "missing"); got != "" {
		t.Errorf("missing frontend: got %q, want empty", got)
	}
	if got := frontendDeployMode(nil, "web"); got != "" {
		t.Errorf("nil entities: got %q, want empty", got)
	}
}

// A frontend the rendered env declares with a cross-repo `source:` pin is
// absent from the CODEGEN inventory by design — forge must never project
// this project's TypeScript into a sibling repository's working tree (see
// config.KCLFrontend.OwnsFrontendCode). It is nonetheless a thing this
// project BUILDS and DEPLOYS: renderBuildEntities materializes its source
// into a local cache specifically so `npm run build` has a directory.
//
// Resolving `--target` against the codegen inventory alone conflated the
// two sets, so control-plane's `forge build prod --target reliant-web`
// printed the frontend in its own plan and then answered "target
// "reliant-web" not found in project config or in env "prod"'s KCL
// services".
func TestResolveNamedBuildTarget_CrossRepoFrontend(t *testing.T) {
	entities, err := parseKCLEntities([]byte(`{
  "frontends": [
    {"name": "reliant-web", "type": "vite",
     "path": "/cache/forge/sources/reliant/web",
     "source": {"repo": "github.com/reliant-labs/reliant", "ref": "v1.7.11"},
     "deploy": {"type": "firebase"}}
  ]
}`))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}

	cfg := &config.ProjectConfig{Name: "control-plane"}
	opts := buildOptions{buildTarget: "reliant-web", env: "prod"}

	// The codegen inventory is empty — exactly control-plane's state.
	frontends, buildBinary, err := resolveNamedBuildTarget(cfg, entities, &opts, nil)
	if err != nil {
		t.Fatalf("resolveNamedBuildTarget: %v\n"+
			"a frontend the env declares and whose source forge already materialized "+
			"must be resolvable as a build target", err)
	}
	if buildBinary {
		t.Error("buildBinary = true; naming a frontend must not also build the project binary")
	}
	if len(frontends) != 1 || frontends[0].Name != "reliant-web" {
		t.Fatalf("frontends = %+v, want exactly [reliant-web]", frontends)
	}
	// The materialized path is what `npm run build` shells into. Falling
	// back to the frontends/<name> convention would run the build in a
	// directory that does not exist.
	if got := frontends[0].DeclaredDir(); got != "/cache/forge/sources/reliant/web" {
		t.Errorf("Path = %q, want the resolved source directory", got)
	}
	if got := frontends[0].Type; got != "vite" {
		t.Errorf("Type = %q, want vite (carried from the KCL declaration)", got)
	}
}

// The codegen inventory still WINS when it has the name: its entry
// carries the project's own declared type and path, and a KCL render is
// per-env while forge.yaml is not.
func TestResolveNamedBuildTarget_InventoryEntryWins(t *testing.T) {
	entities, err := parseKCLEntities([]byte(`{
  "frontends": [{"name": "web", "type": "nextjs", "path": "kcl/path"}]
}`))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	cfg := &config.ProjectConfig{Name: "proj"}
	opts := buildOptions{buildTarget: "web", env: "prod"}
	inventory := []config.FrontendConfig{config.FrontendConfig{Name: "web", Type: "nextjs"}.WithDir("frontends/web")}

	frontends, _, err := resolveNamedBuildTarget(cfg, entities, &opts, inventory)
	if err != nil {
		t.Fatalf("resolveNamedBuildTarget: %v", err)
	}
	if len(frontends) != 1 || frontends[0].DeclaredDir() != "frontends/web" {
		t.Fatalf("frontends = %+v, want the forge.yaml inventory entry", frontends)
	}
}

// An unknown name is still an error, and the message still names both
// places forge looked. Widening the frontend lookup must not turn a typo
// into a silent no-op build.
func TestResolveNamedBuildTarget_UnknownNameStillErrors(t *testing.T) {
	entities, err := parseKCLEntities([]byte(`{"frontends":[{"name":"web","path":"frontends/web"}]}`))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	cfg := &config.ProjectConfig{Name: "proj"}
	opts := buildOptions{buildTarget: "nope", env: "prod"}
	if _, _, err := resolveNamedBuildTarget(cfg, entities, &opts, nil); err == nil {
		t.Fatal("want an error for a target no frontend, binary or service matches")
	}
}

// Naming a frontend must narrow the ENTITY set too, not just pick the
// frontend. Every external-build service reads `entities`, so leaving it
// whole runs each one's build_cmd — the "one command rebuilt my whole
// stack" failure the service-name branch already existed to prevent.
//
// This was unreachable while a frontend resolved only from the codegen
// inventory (no external-build service is ever in it). Making KCL
// frontends resolvable opened it, and `forge build prod --target
// reliant-web` on control-plane hit it: the frontend built in 22s, then
// four unrelated docker pushes ran and failed.
func TestBuildTargetNarrowing_FrontendNameScopesEntities(t *testing.T) {
	entities, err := parseKCLEntities([]byte(`{
  "services": [
    {"name": "api", "deploy": {"type": "cluster", "build_cmd": "docker push everything"}}
  ],
  "frontends": [
    {"name": "reliant-web", "type": "vite", "path": "web",
     "source": {"repo": "github.com/reliant-labs/reliant", "ref": "v1"}},
    {"name": "internal-console", "type": "nextjs", "path": "frontends/internal-console"}
  ]
}`))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}

	got := filterEntitiesByTarget(entities, []string{"reliant-web"})
	if len(got.Services) != 0 {
		t.Errorf("services = %+v, want none — naming a frontend must not run a service's build_cmd", got.Services)
	}
	if len(got.Frontends) != 1 || got.Frontends[0].Name != "reliant-web" {
		t.Errorf("frontends = %+v, want exactly [reliant-web]", got.Frontends)
	}
}

// The narrowing predicate itself: a KCL frontend name must be recognised
// as a narrowable target. Guards the condition in renderBuildEntities
// that decides whether to filter at all.
func TestKCLFrontendAsBuildTarget_RecognisesFrontendNames(t *testing.T) {
	entities, err := parseKCLEntities([]byte(
		`{"frontends":[{"name":"reliant-web","type":"vite","path":"web"}]}`))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	if kclFrontendAsBuildTarget(entities, "reliant-web") == nil {
		t.Error("a frontend the env declares must be recognised as a build target")
	}
	if kclFrontendAsBuildTarget(entities, "nope") != nil {
		t.Error("an unknown name must not be recognised")
	}
	if kclFrontendAsBuildTarget(nil, "reliant-web") != nil {
		t.Error("a nil entity set (no --env) must not resolve anything")
	}
}
