package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestKCLBuildPlanHelpers covers the helpers runBuild uses to decide
// whether the env needs the project image and which arch to build it for,
// over the render-contract goldens.
func TestKCLBuildPlanHelpers(t *testing.T) {
	cluster, _ := loadContract(t, "cluster")
	if !envNeedsProjectImage(cluster) {
		t.Error("envNeedsProjectImage(cluster golden): want true (api is a go-built cluster workload)")
	}
	cluster.ClusterTarget.Platform = "arm64"
	if got := kclFirstClusterPlatform(cluster); got != "arm64" {
		t.Errorf("kclFirstClusterPlatform: got %q, want the declared cluster_target platform arm64", got)
	}
}

// TestEnvNeedsProjectImage_ByRuntimeAndBuild pins the one question
// runBuild asks before building the project image: is some go-built
// workload run FROM an image? Host workloads run their binary directly and
// a workload that only names a third-party image has nothing to build.
func TestEnvNeedsProjectImage_ByRuntimeAndBuild(t *testing.T) {
	noBuild := func(w *WorkloadEntity) { w.Build = BuildConfigEntity{} }
	goBuild := func(w *WorkloadEntity) {
		w.Build = BuildConfigEntity{Type: "go", Go: &GoBuild{Cmd: "./cmd/api", OutputName: "api"}}
	}
	cases := map[string]struct {
		workloads []WorkloadEntity
		want      bool
	}{
		"all host":                        {[]WorkloadEntity{hostWL("a"), hostWL("b")}, false},
		"compose only":                    {[]WorkloadEntity{composeWL("pg", "docker-compose.yml")}, false},
		"cluster, go build":               {[]WorkloadEntity{clusterWL("api", "k3d-dev", "dev")}, true},
		"cluster, prebuilt image":         {[]WorkloadEntity{clusterWL("nats", "k3d-dev", "dev", noBuild)}, false},
		"hosted, prebuilt image":          {[]WorkloadEntity{hostedWL("api")}, false},
		"hosted, declares a go build":     {[]WorkloadEntity{hostedWL("api", goBuild)}, true},
		"host api plus cluster go worker": {[]WorkloadEntity{hostWL("api"), clusterWL("w", "k3d-dev", "dev")}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := envNeedsProjectImage(&KCLEntities{Workloads: tc.workloads}); got != tc.want {
				t.Errorf("envNeedsProjectImage = %v, want %v", got, tc.want)
			}
		})
	}
	if got := kclFirstClusterPlatform(&KCLEntities{Workloads: []WorkloadEntity{hostWL("a")}}); got != "" {
		t.Errorf("kclFirstClusterPlatform on no-cluster: got %q, want empty", got)
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
	entities, err := parseKCLEntities([]byte(`{"output": {
  "workloads": [
    {"name": "api", "kind": "service", "runtime": {"type": "cluster", "cluster": "k3d-dev", "namespace": "dev"},
     "build": {"type": "shell", "cmd": "docker push everything"}, "spec": {"kind": "service"}}
  ],
  "frontends": [
    {"name": "reliant-web", "type": "vite", "path": "web",
     "source": {"repo": "github.com/reliant-labs/reliant", "ref": "v1"}},
    {"name": "internal-console", "type": "nextjs", "path": "frontends/internal-console"}
  ]
}}`))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}

	got := filterEntitiesByTarget(entities, []string{"reliant-web"})
	if len(got.Workloads) != 0 {
		t.Errorf("workloads = %+v, want none — naming a frontend must not run a workload's shell build", got.Workloads)
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

// TestNamedKCLServiceTargetStillBuildsItsBinary pins the fix for a silent
// no-op: `forge build <env> --target <kcl-service>` reported success having
// compiled nothing.
//
// resolveNamedBuildTarget returned buildBinary=false for a KCL service, and
// resolveGoTargets(false, ...) returns nil unconditionally — so no go target
// survived. The command printed a 0s summary with no build lines, which reads
// exactly like a cache hit. In control-plane that shipped a stale admin-server
// image to production against a freshly migrated database; every RPC touching
// the users table 500'd until the real image was built.
//
// The service here mirrors that shape: a go-build service whose cmd is the
// SHARED project binary (control-plane's admin-server runs
// ["./control-plane","public-api"]), which is why "did the target resolve"
// and "did anything get compiled" are different questions.
func TestNamedKCLServiceTargetStillBuildsItsBinary(t *testing.T) {
	entities := &KCLEntities{
		Workloads: []WorkloadEntity{{
			Name:  "admin-server",
			Image: "control-plane",
			Build: BuildConfigEntity{
				Type: "go",
				Go:   &GoBuild{Cmd: "./cmd/control-plane", OutputName: "control-plane"},
			},
		}},
	}
	cfg := &config.ProjectConfig{Name: "control-plane"}
	opts := &buildOptions{buildTarget: "admin-server", env: "prod"}

	frontends, buildBinary, err := resolveNamedBuildTarget(cfg, entities, opts, nil)
	if err != nil {
		t.Fatalf("resolveNamedBuildTarget: %v", err)
	}
	if len(frontends) != 0 {
		t.Errorf("frontends = %d, want 0 — a named service target builds no frontends", len(frontends))
	}
	if !opts.skipFrontends {
		t.Error("skipFrontends = false, want true for a named service target")
	}
	if !buildBinary {
		t.Fatal("buildBinary = false — the named service's binary would not be built, " +
			"so the command succeeds having compiled nothing and a stale image ships")
	}

	// The contract that matters is the one runBuild consumes: resolving the
	// target must yield real work. Asserting the flag alone would pass even if
	// resolveGoTargets later dropped it.
	got := resolveGoTargets(buildBinary, entities, cfg)
	if len(got) == 0 {
		t.Fatal("resolveGoTargets returned no targets for a named go-build service")
	}
	if got[0].cmd != "./cmd/control-plane" {
		t.Errorf("go target cmd = %q, want the service's own ./cmd/control-plane", got[0].cmd)
	}
}
