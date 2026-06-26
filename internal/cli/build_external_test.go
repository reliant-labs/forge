package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/internal/config"
)

// noBaseImagesCfg is the config the external-build tests pass: no
// docker.base_images declared, so baseImageBuildEnv injects nothing and the
// tests exercise the build_cmd path unchanged.
func noBaseImagesCfg() *config.ProjectConfig { return &config.ProjectConfig{} }

// shellSvc is a test helper building a ServiceEntity whose effective
// build is a ShellBuild (the single shell escape hatch). cwd/env are
// optional.
func shellSvc(name, image, cmd, cwd string, env map[string]string) ServiceEntity {
	return ServiceEntity{
		Name:  name,
		Image: image,
		Build: BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cmd: cmd, Cwd: cwd, Env: env}},
	}
}

// TestKCLHasExternalBuildService_PositiveAndNegative pins the discovery
// helper runBuild uses to decide whether to invoke the external-build
// dispatcher at all. A KCL entity set without any ShellBuild services
// must report false so the dispatcher loop is skipped entirely (no
// state-dir creation, no log noise on projects that don't use the
// escape hatch).
func TestKCLHasExternalBuildService_PositiveAndNegative(t *testing.T) {
	// Positive: one service declares a ShellBuild.
	withCmd := &KCLEntities{
		Services: []ServiceEntity{
			shellSvc("a", "a-img", "docker build .", "", nil),
			{Name: "b"},
		},
	}
	if !kclHasExternalBuildService(withCmd) {
		t.Error("kclHasExternalBuildService: want true when any service has a ShellBuild")
	}

	// Negative: no service declares a ShellBuild (both default to GoBuild).
	noCmd := &KCLEntities{
		Services: []ServiceEntity{{Name: "a"}, {Name: "b"}},
	}
	if kclHasExternalBuildService(noCmd) {
		t.Error("kclHasExternalBuildService: want false when no service has a ShellBuild")
	}

	// Nil-safe: the runBuild path passes entities=nil for projects
	// without a rendered KCL set. Must not panic.
	if kclHasExternalBuildService(nil) {
		t.Error("kclHasExternalBuildService(nil): want false")
	}
}

// TestEffectiveBuildCmd_SingleSource pins the unified resolution: the
// effective ShellBuild's Cmd is the one shell source. A non-shell build
// (GoBuild default, compose/external with no build) returns "".
func TestEffectiveBuildCmd_SingleSource(t *testing.T) {
	cases := []struct {
		name string
		svc  ServiceEntity
		want string
	}{
		{
			name: "ShellBuild cmd is the source",
			svc:  shellSvc("s", "img", "make image", "", nil),
			want: "make image",
		},
		{
			name: "GoBuild default returns empty",
			svc:  ServiceEntity{Name: "s", Deploy: DeployConfigEntity{Type: "host"}},
			want: "",
		},
		{
			name: "external deploy with no build returns empty",
			svc:  ServiceEntity{Name: "s", Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{DeployCmd: "ship.sh"}}},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.svc.EffectiveBuildCmd(); got != c.want {
				t.Errorf("EffectiveBuildCmd: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestEffectiveBuildEnv_FromShell confirms the env-map resolution reads
// the effective ShellBuild's Env (the single source after unification).
func TestEffectiveBuildEnv_FromShell(t *testing.T) {
	svc := shellSvc("s", "img", "make image", "", map[string]string{"FOO": "bar"})
	env := svc.EffectiveBuildEnv()
	if env["FOO"] != "bar" {
		t.Errorf("Shell env: got %q, want bar", env["FOO"])
	}
	// A non-shell build has no shell env.
	if got := (ServiceEntity{Name: "g", Deploy: DeployConfigEntity{Type: "host"}}).EffectiveBuildEnv(); got != nil {
		t.Errorf("non-shell build env: got %v, want nil", got)
	}
}

// TestParseKCLEntities_ShellBuildCwdEnv confirms a ShellBuild's cwd + env
// survive the JSON round-trip into ShellBuild.Cwd/Env and are surfaced by
// the Effective* accessors.
func TestParseKCLEntities_ShellBuildCwdEnv(t *testing.T) {
	js := `{"services":[{"name":"trader","image":"ghcr.io/x","deploy":{"type":"external","deploy_cmd":"ship.sh"},"build":{"type":"shell","cmd":"docker build -t ${IMAGE}:${TAG} ${PROJECT_DIR}","cwd":"../sib","env":{"REGION":"iad"}}}]}`
	entities, err := parseKCLEntities([]byte(js))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	svc := entities.FindService("trader")
	if svc == nil {
		t.Fatal("trader not found")
	}
	if got := svc.EffectiveBuildCmd(); got != "docker build -t ${IMAGE}:${TAG} ${PROJECT_DIR}" {
		t.Errorf("EffectiveBuildCmd: got %q", got)
	}
	if got := svc.EffectiveBuildCwd(); got != "../sib" {
		t.Errorf("EffectiveBuildCwd: got %q, want ../sib", got)
	}
	if got := svc.EffectiveBuildEnv()["REGION"]; got != "iad" {
		t.Errorf("EffectiveBuildEnv[REGION]: got %q, want iad", got)
	}
}

// TestKCLHasExternalBuildService_DetectsShellAlongsideExternalDeploy
// confirms the discovery helper sees a ShellBuild declared on a service
// that ALSO has an External deploy (build + deploy are orthogonal). This
// is the kalshi-trader e2e shape after the build-hatch unification.
func TestKCLHasExternalBuildService_DetectsShellAlongsideExternalDeploy(t *testing.T) {
	ext := &KCLEntities{
		Services: []ServiceEntity{
			{
				Name:   "trader",
				Image:  "ghcr.io/x",
				Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{DeployCmd: "ship.sh"}},
				Build:  BuildConfigEntity{Type: "shell", Shell: &ShellBuild{Cmd: "docker build ."}},
			},
		},
	}
	if !kclHasExternalBuildService(ext) {
		t.Error("want true when a service declares a ShellBuild")
	}
	got := externalBuildServices(ext)
	if len(got) != 1 || got[0].Name != "trader" {
		t.Errorf("externalBuildServices: got %v, want [trader]", got)
	}

	// External deploy with only deploy_cmd (no ShellBuild) must NOT be
	// detected — but note an external deploy synthesizes NO Go default,
	// so EffectiveBuildCmd is "".
	noBuild := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "trader", Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{DeployCmd: "ship.sh"}}},
		},
	}
	if kclHasExternalBuildService(noBuild) {
		t.Error("want false when an external deploy has no ShellBuild")
	}
}

// TestExternalBuildServices_FiltersShellBuilds confirms the dispatcher's
// filter returns only services whose effective build is a ShellBuild.
// Services that default to GoBuild flow through the regular Go-build path
// and must NOT appear in the external dispatcher's input set.
func TestExternalBuildServices_FiltersShellBuilds(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			shellSvc("edge", "edge-img", "docker build .", "", nil),
			{Name: "api"}, // GoBuild default
			shellSvc("daemon", "daemon-img", "go build && docker push", "", nil),
		},
	}
	got := externalBuildServices(entities)
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["edge"] || !names["daemon"] {
		t.Errorf("filter dropped a ShellBuild service: got %v", names)
	}
	if names["api"] {
		t.Error("filter included a service with no ShellBuild")
	}
}

// TestBuildExternalServices_WritesStateAndReturnsResults exercises the
// dispatcher end-to-end against a temp project dir. Asserts:
//
//   - Each declared external service produces one buildResult with
//     kind=="external".
//   - The state file lands at
//     .forge/state/build-<env>-<service>.json after a successful build.
//
// We can't easily inject a fake runner from this package (the runner
// indirection is package-private to buildtarget by design), so we run
// against a real `sh -c true` shell. CI runners always have /bin/sh,
// and `true` is a fast deterministic no-op that exits 0.
func TestBuildExternalServices_WritesStateAndReturnsResults(t *testing.T) {
	projDir := t.TempDir()
	services := []ServiceEntity{
		shellSvc("edge", "edge-img", "true", "", nil), // cwd empty so no mkdir
	}
	opts := buildOptions{env: "dev", parallel: false}
	results := buildExternalServices(
		context.Background(),
		noBaseImagesCfg(),
		services,
		opts,
		"localhost:5051", // registry
		"v1.2.3",         // tag
		projDir,
		"amd64",
		nil, // no rendered entities → env-wide tag applies
	)
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	r := results[0]
	if r.err != nil {
		t.Errorf("err: got %v, want nil", r.err)
	}
	if r.kind != "external" {
		t.Errorf("kind: got %q, want external", r.kind)
	}
	if !strings.Contains(r.name, "edge") {
		t.Errorf("name: got %q, want a name containing 'edge'", r.name)
	}

	// State file should have landed at the canonical path.
	statePath := filepath.Join(projDir, ".forge", "state", "build-dev-edge.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state file at %s: %v", statePath, err)
	}
}

// TestBuildExternalServices_FailsWhenCwdMissing pins the gotcha-B
// contract: a service that IS in the current env (it reached the
// dispatcher) but whose build_cwd is missing must FAIL the build (a
// buildResult with err set), not skip. Skipping reported success while
// producing no image, so a following `forge deploy` referenced an
// unpushed tag → ImagePullBackOff.
//
// Critically: a FAILED build must NOT write a state file either — that
// would still let a downstream `forge deploy` pin a tag for an image
// that was never pushed.
func TestBuildExternalServices_FailsWhenCwdMissing(t *testing.T) {
	projDir := t.TempDir()
	services := []ServiceEntity{
		shellSvc("edge", "edge-img", "false", "missing-sibling", nil), // missing-cwd check short-circuits
	}
	opts := buildOptions{env: "dev", parallel: false}
	results := buildExternalServices(
		context.Background(),
		noBaseImagesCfg(),
		services,
		opts,
		"localhost:5051",
		"v1",
		projDir,
		"amd64",
		nil,
	)
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	r := results[0]
	if r.err == nil {
		t.Fatal("err: got nil, want a failure for a missing build_cwd (skip-masquerading-as-success regression)")
	}
	if !strings.Contains(r.err.Error(), "missing-sibling") {
		t.Errorf("err: got %q, want it to name the missing path", r.err.Error())
	}
	if r.kind != "external" {
		t.Errorf("kind: got %q, want external (a real failure, not external-skip)", r.kind)
	}
	// Failed builds MUST NOT write a state file — that would let a
	// downstream `forge deploy` pin a tag for an image that was never
	// pushed.
	statePath := filepath.Join(projDir, ".forge", "state", "build-dev-edge.json")
	if _, err := os.Stat(statePath); err == nil {
		t.Errorf("state file at %s exists; failed builds must not write state", statePath)
	}
}

// TestBuildExternalServices_RegistryMissOverwritesPriorDigest pins the
// "NO fallback to the previous build-state digest" half of the remote-built
// digest contract. A first build resolves the pushed ref to a digest and
// persists it. A SECOND build of the same env/service then has its registry
// query MISS (remote-built image not yet/never in the registry, or an
// unreachable registry). The miss must overwrite the persisted state with an
// EMPTY digest — never silently retain the prior build's digest, which would
// keep pinning deploy to a stale image (the wrong image or one the registry
// no longer has → ImagePullBackOff, surfacing as a deploy exit 1).
func TestBuildExternalServices_RegistryMissOverwritesPriorDigest(t *testing.T) {
	projDir := t.TempDir()

	const priorDigest = "sha256:" + "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	orig := externalImageDigestResolver
	t.Cleanup(func() { externalImageDigestResolver = orig })

	services := []ServiceEntity{
		shellSvc("reliant-api-server", "reliant", "true", "", nil),
	}
	opts := buildOptions{env: "prod", parallel: false}

	// First build: registry resolves a digest, which gets persisted.
	externalImageDigestResolver = func(_ context.Context, _ string) (string, []string, error) {
		return priorDigest, []string{"linux/amd64"}, nil
	}
	if r := buildExternalServices(context.Background(), noBaseImagesCfg(), services, opts,
		"ghcr.io/reliant-labs", "prod", projDir, "amd64", nil); len(r) != 1 || r[0].err != nil {
		t.Fatalf("first build: %+v", r)
	}
	if dst, err := ReadBuildState(projDir, "prod"); err != nil || dst == nil || dst.Digest != priorDigest {
		t.Fatalf("after first build: want digest %q, got dst=%+v err=%v", priorDigest, dst, err)
	}

	// Second build: registry query MISSES. The persisted digest MUST be
	// overwritten with empty — no fall-through to the prior build-state digest.
	externalImageDigestResolver = func(_ context.Context, ref string) (string, []string, error) {
		return "", nil, fmt.Errorf("not in registry: %s", ref)
	}
	if r := buildExternalServices(context.Background(), noBaseImagesCfg(), services, opts,
		"ghcr.io/reliant-labs", "prod", projDir, "amd64", nil); len(r) != 1 || r[0].err != nil {
		t.Fatalf("second build: %+v", r)
	}

	// Deploy-readable aggregate: digest cleared, so deploy falls back to the tag.
	dst, err := ReadBuildState(projDir, "prod")
	if err != nil || dst == nil {
		t.Fatalf("ReadBuildState after miss: dst=%+v err=%v", dst, err)
	}
	if dst.Digest != "" {
		t.Errorf("deploy-aggregate digest after registry miss: got %q, want empty (no stale fallback)", dst.Digest)
	}
	// Per-service state too.
	st, err := buildtarget.ReadState(projDir, "prod", "reliant-api-server")
	if err != nil || st == nil {
		t.Fatalf("ReadState after miss: st=%+v err=%v", st, err)
	}
	if st.Digest != "" {
		t.Errorf("per-service digest after registry miss: got %q, want empty (no stale fallback)", st.Digest)
	}

	// And deploy resolves to the mutable tag, not the stale digest.
	digests, derr := resolveDeployImageDigests(projDir, "prod", false)
	if derr != nil {
		t.Fatalf("resolveDeployImageDigests: %v", derr)
	}
	if d := digests["reliant"]; d != "" {
		t.Errorf("reliant image digest after registry miss: got %q, want empty (deploy must use the tag)", d)
	}
}

// TestBuildTargetExternalFlag_Accepted confirms the --target flag
// accepts the new "external" literal alongside the legacy "all" and
// per-service values. Wires the user-facing entry point — if the
// flag is renamed or dropped, this test catches it before users hit
// "unknown flag value" from the CLI.
func TestBuildTargetExternalFlag_Accepted(t *testing.T) {
	cmd := newBuildCmd()
	if err := cmd.Flags().Parse([]string{"--target", "external"}); err != nil {
		t.Fatalf("parse --target external: %v", err)
	}
	got, err := cmd.Flags().GetString("target")
	if err != nil {
		t.Fatalf("GetString(target): %v", err)
	}
	if got != "external" {
		t.Errorf("target: got %q, want external", got)
	}
}

// TestResolveExternalBuildTargetArch_Precedence locks in the flag >
// cfg > runtime fallback precedence. External builds always need a
// resolved arch (the user's command expands ${TARGETARCH} into
// `--platform=linux/<arch>` which buildx rejects on empty); so the
// runtime fallback is load-bearing.
func TestResolveExternalBuildTargetArch_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		cfgArch  string
		flagArch string
		// We can't assert against runtime.GOARCH directly because the
		// fallback case's expectation is "anything non-empty"; the
		// test runs on whatever CI arch is current.
		wantEqualFlag    bool // when true, expect == flagArch
		wantEqualCfg     bool // when true, expect == cfgArch
		wantNonEmptyOnly bool // when true, just assert non-empty
	}{
		{name: "flag wins over cfg", cfgArch: "amd64", flagArch: "arm64", wantEqualFlag: true},
		{name: "cfg used when flag empty", cfgArch: "arm64", flagArch: "", wantEqualCfg: true},
		{name: "runtime fallback when both empty", cfgArch: "", flagArch: "", wantNonEmptyOnly: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveExternalBuildTargetArch(c.cfgArch, c.flagArch)
			if c.wantEqualFlag && got != c.flagArch {
				t.Errorf("got %q, want flagArch %q", got, c.flagArch)
			}
			if c.wantEqualCfg && got != c.cfgArch {
				t.Errorf("got %q, want cfgArch %q", got, c.cfgArch)
			}
			if c.wantNonEmptyOnly && got == "" {
				t.Errorf("got empty; runtime fallback should never produce empty")
			}
		})
	}
}

// TestBuildExternalServices_CapturesDigest pins the external-build half of
// deploy-by-digest: when the pushed ref resolves to a content-addressed
// manifest digest, that digest lands in BOTH the per-service State file
// (forge audit/doctor) AND the deploy-readable aggregate build-<env>.json
// (what resolveDeployImageTag reads) — so reliant/workspace-base get pinned
// to `<image>@sha256:...` instead of the mutable env tag. The resolver is
// faked so the test never shells out to docker; it also asserts the resolver
// was queried with the exact ${REGISTRY}/${IMAGE}:${TAG} ref the build_cmd
// pushed.
func TestBuildExternalServices_CapturesDigest(t *testing.T) {
	projDir := t.TempDir()

	const wantDigest = "sha256:" + "abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabc00"
	var gotRef string
	orig := externalImageDigestResolver
	externalImageDigestResolver = func(_ context.Context, ref string) (string, []string, error) {
		gotRef = ref
		return wantDigest, []string{"linux/amd64"}, nil
	}
	t.Cleanup(func() { externalImageDigestResolver = orig })

	services := []ServiceEntity{
		shellSvc("reliant-api-server", "reliant", "true", "", nil),
	}
	opts := buildOptions{env: "staging", parallel: false}
	results := buildExternalServices(
		context.Background(), noBaseImagesCfg(), services, opts,
		"ghcr.io/reliant-labs", "staging", projDir, "amd64", nil,
	)
	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("results: %+v", results)
	}

	// Resolver must have been asked about the exact pushed ref.
	if want := "ghcr.io/reliant-labs/reliant:staging"; gotRef != want {
		t.Errorf("resolver ref: got %q, want %q", gotRef, want)
	}

	// Per-service State carries the digest.
	st, err := buildtarget.ReadState(projDir, "staging", "reliant-api-server")
	if err != nil || st == nil {
		t.Fatalf("ReadState: st=%+v err=%v", st, err)
	}
	if st.Digest != wantDigest {
		t.Errorf("per-service digest: got %q, want %q", st.Digest, wantDigest)
	}
	if len(st.Platforms) != 1 || st.Platforms[0] != "linux/amd64" {
		t.Errorf("per-service platforms: got %v", st.Platforms)
	}

	// Deploy-readable aggregate carries the digest too — this is the file
	// resolveDeployImageTag actually consumes.
	dst, err := ReadBuildState(projDir, "staging")
	if err != nil || dst == nil {
		t.Fatalf("ReadBuildState: dst=%+v err=%v", dst, err)
	}
	if dst.Digest != wantDigest {
		t.Errorf("deploy-aggregate digest: got %q, want %q", dst.Digest, wantDigest)
	}

	// resolveDeployImageTag returns the mutable tag (the env-wide image_tag
	// fallback + the External/Compose ${TAG}); the digest is NOT stamped here
	// anymore.
	ref, plain, _, rerr := resolveDeployImageTag(context.Background(), projDir, "staging", "", false)
	if rerr != nil {
		t.Fatalf("resolveDeployImageTag: %v", rerr)
	}
	if ref != "staging" || plain != "staging" {
		t.Errorf("resolved imageRef=%q plainTag=%q, want both the mutable tag %q", ref, plain, "staging")
	}

	// resolveDeployImageDigests pins the per-IMAGE digest — the reliant image
	// resolves to ITS captured digest (from the per-service external state).
	digests, derr := resolveDeployImageDigests(projDir, "staging", false)
	if derr != nil {
		t.Fatalf("resolveDeployImageDigests: %v", derr)
	}
	if digests["reliant"] != wantDigest {
		t.Errorf("reliant image digest: got %q, want %q", digests["reliant"], wantDigest)
	}
}

// TestBuildExternalServices_NoDigestSafeFallback pins the safety contract:
// when the pushed ref has no resolvable digest (local-only ref — the e2e
// workspace-base/reliant case — or an unreachable registry), the build still
// succeeds and records NO digest, so deploy falls back to the mutable tag
// exactly as before. A digest lookup failure must never break the build or
// the local-registry/e2e path.
func TestBuildExternalServices_NoDigestSafeFallback(t *testing.T) {
	projDir := t.TempDir()

	orig := externalImageDigestResolver
	externalImageDigestResolver = func(_ context.Context, ref string) (string, []string, error) {
		return "", nil, fmt.Errorf("no registry manifest for %s (local-only)", ref)
	}
	t.Cleanup(func() { externalImageDigestResolver = orig })

	services := []ServiceEntity{
		shellSvc("workspace-base", "workspace-base", "true", "", nil),
	}
	opts := buildOptions{env: "e2e", parallel: false}
	results := buildExternalServices(
		context.Background(), noBaseImagesCfg(), services, opts,
		"", "e2e", projDir, "amd64", nil, // empty registry → local ref
	)
	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("build must still succeed on a digest miss: %+v", results)
	}

	st, err := buildtarget.ReadState(projDir, "e2e", "workspace-base")
	if err != nil || st == nil {
		t.Fatalf("ReadState: st=%+v err=%v", st, err)
	}
	if st.Digest != "" {
		t.Errorf("per-service digest: got %q, want empty (safe fallback)", st.Digest)
	}

	dst, err := ReadBuildState(projDir, "e2e")
	if err != nil || dst == nil {
		t.Fatalf("ReadBuildState: dst=%+v err=%v", dst, err)
	}
	if dst.Digest != "" {
		t.Errorf("deploy-aggregate digest: got %q, want empty (safe fallback)", dst.Digest)
	}

	// Deploy resolves to the plain mutable tag, not a digest.
	ref, _, _, rerr := resolveDeployImageTag(context.Background(), projDir, "e2e", "", false)
	if rerr != nil {
		t.Fatalf("resolveDeployImageTag: %v", rerr)
	}
	if ref != "e2e" {
		t.Errorf("resolved imageRef: got %q, want the tag %q (no digest pinned)", ref, "e2e")
	}
}
