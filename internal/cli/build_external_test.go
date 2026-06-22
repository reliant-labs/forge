package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKCLHasExternalBuildService_PositiveAndNegative pins the discovery
// helper runBuild uses to decide whether to invoke the external-build
// dispatcher at all. A KCL entity set without any build_cmd services
// must report false so the dispatcher loop is skipped entirely (no
// state-dir creation, no log noise on projects that don't use the
// escape hatch).
func TestKCLHasExternalBuildService_PositiveAndNegative(t *testing.T) {
	// Positive: one service declares build_cmd.
	withCmd := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "a", BuildCmd: "docker build ."},
			{Name: "b"},
		},
	}
	if !kclHasExternalBuildService(withCmd) {
		t.Error("kclHasExternalBuildService: want true when any service has build_cmd")
	}

	// Negative: no service declares build_cmd.
	noCmd := &KCLEntities{
		Services: []ServiceEntity{{Name: "a"}, {Name: "b"}},
	}
	if kclHasExternalBuildService(noCmd) {
		t.Error("kclHasExternalBuildService: want false when no service has build_cmd")
	}

	// Nil-safe: the runBuild path passes entities=nil for projects
	// without a rendered KCL set. Must not panic.
	if kclHasExternalBuildService(nil) {
		t.Error("kclHasExternalBuildService(nil): want false")
	}
}

// TestEffectiveBuildCmd_Precedence pins the two-source resolution the
// dispatcher uses: a top-level Service.build_cmd wins; otherwise an
// External target's build_cmd (the build-side mirror of deploy_cmd) is
// used; neither set returns "".
func TestEffectiveBuildCmd_Precedence(t *testing.T) {
	cases := []struct {
		name string
		svc  ServiceEntity
		want string
	}{
		{
			name: "top-level Service.build_cmd wins",
			svc: ServiceEntity{
				BuildCmd: "service-level",
				Deploy:   DeployConfigEntity{Type: "external", External: &ExternalDeploy{BuildCmd: "external-level"}},
			},
			want: "service-level",
		},
		{
			name: "falls back to External.build_cmd",
			svc: ServiceEntity{
				Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{BuildCmd: "external-level"}},
			},
			want: "external-level",
		},
		{
			name: "neither set",
			svc:  ServiceEntity{Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{}}},
			want: "",
		},
		{
			name: "non-external deploy without Service.build_cmd",
			svc:  ServiceEntity{Deploy: DeployConfigEntity{Type: "host"}},
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

// TestEffectiveBuildEnv_Precedence confirms the env-map resolution
// mirrors EffectiveBuildCmd: Service.build_env when the top-level
// build_cmd is set, else the External target's `env` map (so build_cmd
// and deploy_cmd share one config surface).
func TestEffectiveBuildEnv_Precedence(t *testing.T) {
	// External-level build_cmd → External.env feeds the substitution map.
	extSvc := ServiceEntity{
		Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{
			BuildCmd: "docker build .",
			Env:      map[string]string{"REGION": "iad"},
		}},
	}
	if got := extSvc.EffectiveBuildEnv()["REGION"]; got != "iad" {
		t.Errorf("External env: got %q, want iad", got)
	}

	// Top-level build_cmd → Service.build_env wins.
	svcSvc := ServiceEntity{
		BuildCmd: "make image",
		BuildEnv: map[string]string{"FOO": "bar"},
		Deploy:   DeployConfigEntity{Type: "external", External: &ExternalDeploy{Env: map[string]string{"REGION": "iad"}}},
	}
	env := svcSvc.EffectiveBuildEnv()
	if env["FOO"] != "bar" {
		t.Errorf("Service env: got %q, want bar", env["FOO"])
	}
	if _, ok := env["REGION"]; ok {
		t.Error("Service-level path must not leak External.env")
	}
}

// TestKCLHasExternalBuildService_DetectsExternalLevel confirms the
// discovery helper sees a build_cmd declared on the External deploy
// block (not just the top-level Service.build_cmd). This is the exact
// path the kalshi-trader e2e exercises.
func TestKCLHasExternalBuildService_DetectsExternalLevel(t *testing.T) {
	ext := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "trader", Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{
				DeployCmd: "ship.sh",
				BuildCmd:  "docker build .",
			}}},
		},
	}
	if !kclHasExternalBuildService(ext) {
		t.Error("want true when an External target declares build_cmd")
	}
	got := externalBuildServices(ext)
	if len(got) != 1 || got[0].Name != "trader" {
		t.Errorf("externalBuildServices: got %v, want [trader]", got)
	}

	// External with only deploy_cmd (no build_cmd) must NOT be detected.
	noBuild := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "trader", Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{DeployCmd: "ship.sh"}}},
		},
	}
	if kclHasExternalBuildService(noBuild) {
		t.Error("want false when External has deploy_cmd but no build_cmd")
	}
}

// TestParseKCLEntities_ExternalBuildCmd confirms build_cmd on an
// External deploy block survives the JSON round-trip into
// ExternalDeploy.BuildCmd.
func TestParseKCLEntities_ExternalBuildCmd(t *testing.T) {
	js := `{"services":[{"name":"trader","image":"ghcr.io/x","deploy":{"type":"external","deploy_cmd":"ship.sh","build_cmd":"docker build -t ${IMAGE}:${TAG} ${PROJECT_DIR}"}}]}`
	entities, err := parseKCLEntities([]byte(js))
	if err != nil {
		t.Fatalf("parseKCLEntities: %v", err)
	}
	svc := entities.FindService("trader")
	if svc == nil {
		t.Fatal("trader not found")
	}
	if svc.Deploy.External == nil {
		t.Fatal("External deploy nil")
	}
	if got := svc.Deploy.External.BuildCmd; got != "docker build -t ${IMAGE}:${TAG} ${PROJECT_DIR}" {
		t.Errorf("External.BuildCmd: got %q", got)
	}
	if svc.EffectiveBuildCmd() == "" {
		t.Error("EffectiveBuildCmd empty after parse")
	}
}

// TestBuildExternalServices_ExternalLevelWritesDeployState exercises the
// External-level build_cmd path end-to-end and asserts BOTH state files
// land: the per-service audit file AND the deploy-side build-<env>.json
// that `forge deploy` reads (so --tag isn't required afterwards).
func TestBuildExternalServices_ExternalLevelWritesDeployState(t *testing.T) {
	projDir := t.TempDir()
	services := []ServiceEntity{
		{
			Name:  "trader",
			Image: "ghcr.io/x",
			Deploy: DeployConfigEntity{Type: "external", External: &ExternalDeploy{
				DeployCmd: "ship.sh",
				BuildCmd:  "true", // resolved via EffectiveBuildCmd
			}},
		},
	}
	opts := buildOptions{env: "prod", parallel: false}
	results := buildExternalServices(context.Background(), services, opts, "ghcr.io", "forge-test", projDir, "amd64")
	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("results: %+v", results)
	}
	// Deploy-side state file (what forge deploy reads).
	st, err := ReadBuildState(projDir, "prod")
	if err != nil {
		t.Fatalf("ReadBuildState: %v", err)
	}
	if st == nil || st.Tag != "forge-test" {
		t.Errorf("deploy build-state: got %+v, want Tag=forge-test", st)
	}
	// Per-service audit file too.
	auditPath := filepath.Join(projDir, ".forge", "state", "build-prod-trader.json")
	if _, err := os.Stat(auditPath); err != nil {
		t.Errorf("per-service state at %s: %v", auditPath, err)
	}
}

// TestExternalBuildServices_FiltersBuildCmd confirms the dispatcher's
// filter returns only services whose BuildCmd is non-empty. Services
// without a build_cmd flow through the regular Go-build/docker path
// and must NOT appear in the external dispatcher's input set.
func TestExternalBuildServices_FiltersBuildCmd(t *testing.T) {
	entities := &KCLEntities{
		Services: []ServiceEntity{
			{Name: "edge", BuildCmd: "docker build ."},
			{Name: "api"},
			{Name: "daemon", BuildCmd: "go build && docker push"},
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
		t.Errorf("filter dropped a build_cmd service: got %v", names)
	}
	if names["api"] {
		t.Error("filter included a service with no build_cmd")
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
		{
			Name:     "edge",
			Image:    "edge-img",
			BuildCmd: "true",
			// BuildCwd left empty so we don't need to mkdir.
		},
	}
	opts := buildOptions{env: "dev", parallel: false}
	results := buildExternalServices(
		context.Background(),
		services,
		opts,
		"localhost:5051", // registry
		"v1.2.3",         // tag
		projDir,
		"amd64",
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

// TestBuildExternalServices_SkipsWhenCwdMissing pins the local-dev
// pattern: a missing build_cwd is logged as "skipped: …" and surfaces
// as a buildResult with kind=="external-skip" and err=nil. CI runs
// without the optional sibling repo must not fail the build.
//
// Critically: the state file must NOT be written for a skipped build
// (skipped builds produced nothing to deploy, so a state file would
// poison a downstream `forge deploy` into pinning a tag for an image
// that was never pushed).
func TestBuildExternalServices_SkipsWhenCwdMissing(t *testing.T) {
	projDir := t.TempDir()
	services := []ServiceEntity{
		{
			Name:     "edge",
			Image:    "edge-img",
			BuildCmd: "false", // would fail if it ran, proving the skip path short-circuited
			BuildCwd: "missing-sibling",
		},
	}
	opts := buildOptions{env: "dev", parallel: false}
	results := buildExternalServices(
		context.Background(),
		services,
		opts,
		"localhost:5051",
		"v1",
		projDir,
		"amd64",
	)
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	r := results[0]
	if r.err != nil {
		t.Errorf("err: got %v, want nil for skip", r.err)
	}
	if r.kind != "external-skip" {
		t.Errorf("kind: got %q, want external-skip", r.kind)
	}
	// Skipped builds MUST NOT write a state file — that would let a
	// downstream `forge deploy` pin a tag for an image that was never
	// pushed.
	statePath := filepath.Join(projDir, ".forge", "state", "build-dev-edge.json")
	if _, err := os.Stat(statePath); err == nil {
		t.Errorf("state file at %s exists; skipped builds must not write state", statePath)
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
