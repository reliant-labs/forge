package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildtarget"
	"github.com/reliant-labs/forge/internal/cli/audittype"
)

// TestAuditExternalBuilds_NoServicesIsOK pins the "category always
// present" contract: a project with zero services declaring build_cmd
// still gets the external_builds key, with status=ok and a "0
// services" summary. Sub-agents can rely on the key being there
// regardless of feature state.
func TestAuditExternalBuilds_NoServicesIsOK(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deploy", "kcl", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy", "kcl", "dev", "main.k"), []byte("// stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, `{
		"services": [
			{"name":"api","image":"api","deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}
		]
	}`))

	entities, err := RenderKCL(t.Context(), dir, "dev")
	if err != nil {
		t.Fatalf("RenderKCL: %v", err)
	}
	cat := collectExternalBuildEntries(entities, []string{"dev"}, dir)
	if cat.Status != audittype.StatusOK {
		t.Errorf("status = %q, want ok", cat.Status)
	}
	if !strings.Contains(cat.Summary, "0 service") {
		t.Errorf("summary = %q, want '0 service'", cat.Summary)
	}
	// Additive contract: services key must exist even when empty so
	// `jq '.external_builds.details.services | length'` is always 0
	// rather than `null`.
	if _, ok := cat.Details["services"]; !ok {
		t.Error("services key missing from details")
	}
}

// TestAuditExternalBuilds_PresentCwdNoConflictIsOK is the happy path:
// service declares build_cmd, build_cwd resolves to an existing dir,
// no build_env collisions → status=ok.
func TestAuditExternalBuilds_PresentCwdNoConflictIsOK(t *testing.T) {
	dir := t.TempDir()
	// Sibling repo dir that resolves cleanly relative to projectDir.
	if err := os.MkdirAll(filepath.Join(dir, "sibling"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{
		"services": [
			{"name":"gw","image":"my-gw","build":{"type":"shell","cmd":"docker build .","cwd":"sibling"},
			 "deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}
		]
	}`
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	entities, _ := RenderKCL(t.Context(), dir, "dev")
	cat := collectExternalBuildEntries(entities, nil, dir)
	if cat.Status != audittype.StatusOK {
		t.Errorf("status = %q, want ok; details=%v", cat.Status, cat.Details)
	}
	svcs, _ := cat.Details["services"].([]externalBuildEntry)
	if len(svcs) != 1 {
		t.Fatalf("want 1 service entry; got %d", len(svcs))
	}
	if !svcs[0].CwdExists {
		t.Errorf("cwd_exists = false, want true (sibling dir was created)")
	}
	if svcs[0].ResolvedCwd != filepath.Join(dir, "sibling") {
		t.Errorf("resolved_cwd = %q, want %q", svcs[0].ResolvedCwd, filepath.Join(dir, "sibling"))
	}
}

// TestAuditExternalBuilds_MissingCwdWarns covers the dominant
// surfaced finding: build_cwd points at a sibling dir that doesn't
// exist on this machine. Build-side semantics are skip-with-warn, but
// audit calls it out so the user knows why their build skipped.
func TestAuditExternalBuilds_MissingCwdWarns(t *testing.T) {
	dir := t.TempDir()
	// Intentionally do NOT create dir/missing-sibling — we want the
	// stat to fail.
	fixture := `{
		"services": [
			{"name":"gw","image":"gw","build":{"type":"shell","cmd":"docker build .","cwd":"missing-sibling"},
			 "deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}
		]
	}`
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	entities, _ := RenderKCL(t.Context(), dir, "dev")
	cat := collectExternalBuildEntries(entities, nil, dir)
	if cat.Status != audittype.StatusWarn {
		t.Errorf("status = %q, want warn", cat.Status)
	}
	if cnt, _ := cat.Details["missing_cwd_count"].(int); cnt != 1 {
		t.Errorf("missing_cwd_count = %v, want 1", cat.Details["missing_cwd_count"])
	}
	svcs, _ := cat.Details["services"].([]externalBuildEntry)
	if len(svcs) != 1 || svcs[0].CwdExists {
		t.Errorf("expected one entry with CwdExists=false; got %+v", svcs)
	}
}

// TestAuditExternalBuilds_BuildEnvConflictWarns exercises the
// reserved-token collision check. A user-declared key matching one of
// IMAGE/TAG/SERVICE/PROJECT_DIR/REGISTRY/TARGETARCH/BUILD_CWD is
// silently shadowed at substitution time — audit warns so the user
// renames the key instead of being surprised at build time.
func TestAuditExternalBuilds_BuildEnvConflictWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sib"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{
		"services": [
			{"name":"gw","image":"gw","build":{"type":"shell","cmd":"docker build .","cwd":"sib","env":{"IMAGE":"oops","CGO_ENABLED":"0","TAG":"v1"}},
			 "deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}
		]
	}`
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	entities, _ := RenderKCL(t.Context(), dir, "dev")
	cat := collectExternalBuildEntries(entities, nil, dir)
	if cat.Status != audittype.StatusWarn {
		t.Errorf("status = %q, want warn", cat.Status)
	}
	if cnt, _ := cat.Details["conflict_count"].(int); cnt != 1 {
		t.Errorf("conflict_count = %v, want 1 (one service with conflicts)", cat.Details["conflict_count"])
	}
	svcs, _ := cat.Details["services"].([]externalBuildEntry)
	if len(svcs) != 1 {
		t.Fatalf("want 1 service; got %d", len(svcs))
	}
	conflicts := svcs[0].ConflictTokens
	// Sorted ascending — IMAGE before TAG.
	if len(conflicts) != 2 || conflicts[0] != "IMAGE" || conflicts[1] != "TAG" {
		t.Errorf("conflict_tokens = %v, want [IMAGE TAG]", conflicts)
	}
}

// TestAuditExternalBuilds_StateReadAggregatesEnvs writes per-env
// state files for two envs and confirms the audit entry aggregates
// both LastBuilds rows in env order.
func TestAuditExternalBuilds_StateReadAggregatesEnvs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Write build-state files for two envs via the buildtarget API
	// so the on-disk shape matches production exactly.
	for _, env := range []string{"dev", "prod"} {
		if err := buildtarget.WriteState(dir, env, buildtarget.State{
			Service:  "gw",
			Image:    "gw",
			Tag:      env + "-tag",
			Registry: "r",
			PushedAt: "2026-01-01T00:00:00Z",
		}); err != nil {
			t.Fatalf("WriteState %s: %v", env, err)
		}
	}
	fixture := `{
		"services": [
			{"name":"gw","image":"gw","build":{"type":"shell","cmd":"docker build .","cwd":"src"},
			 "deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}
		]
	}`
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	entities, _ := RenderKCL(t.Context(), dir, "dev")
	cat := collectExternalBuildEntries(entities, []string{"dev", "prod"}, dir)
	if cat.Status != audittype.StatusOK {
		t.Errorf("status = %q, want ok; details=%v", cat.Status, cat.Details)
	}
	svcs, _ := cat.Details["services"].([]externalBuildEntry)
	if len(svcs) != 1 || len(svcs[0].LastBuilds) != 2 {
		t.Fatalf("want 1 service with 2 last_builds; got %+v", svcs)
	}
	if svcs[0].LastBuilds[0].Env != "dev" || svcs[0].LastBuilds[0].Tag != "dev-tag" {
		t.Errorf("dev state mismatch: %+v", svcs[0].LastBuilds[0])
	}
	if svcs[0].LastBuilds[1].Env != "prod" || svcs[0].LastBuilds[1].Tag != "prod-tag" {
		t.Errorf("prod state mismatch: %+v", svcs[0].LastBuilds[1])
	}
	if cnt, _ := cat.Details["state_count"].(int); cnt != 2 {
		t.Errorf("state_count = %v, want 2", cat.Details["state_count"])
	}
}

// TestAuditExternalBuilds_JSONShape_Golden pins the JSON output shape
// the additive-extension contract sub-agents read against. We
// build a fixture with one service and assert the marshalled output
// has exactly the documented keys. Order-sensitive on the services
// slice (sort by service name); insensitive on map keys (jq doesn't
// care about object key order).
//
// Golden value is asserted via key-presence + value spot-check rather
// than byte-for-byte string equality so future additive fields (new
// audit details, more diagnostics) don't break the test.
func TestAuditExternalBuilds_JSONShape_Golden(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := `{
		"services": [
			{"name":"gw","image":"gw","build":{"type":"shell","cmd":"docker build .","cwd":"src","env":{"CGO_ENABLED":"0"}},
			 "deploy":{"type":"cluster","cluster":"c","namespace":"n","registry":"r"}}
		]
	}`
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t, fixture))
	entities, _ := RenderKCL(t.Context(), dir, "dev")
	cat := collectExternalBuildEntries(entities, nil, dir)

	// Marshal and re-decode through a generic shape so we can assert
	// the JSON contract without coupling to the concrete Go type.
	data, err := json.Marshal(cat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
		Details struct {
			Services []map[string]any `json:"services"`
		} `json:"details"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Status != "ok" {
		t.Errorf("status = %q, want ok", decoded.Status)
	}
	if len(decoded.Details.Services) != 1 {
		t.Fatalf("want 1 service; got %d", len(decoded.Details.Services))
	}
	s := decoded.Details.Services[0]
	for _, k := range []string{"service", "image", "build_cwd", "resolved_cwd", "cwd_exists", "build_env_keys"} {
		if _, ok := s[k]; !ok {
			t.Errorf("service entry missing required key %q; got keys=%v", k, mapKeys(s))
		}
	}
	if s["service"] != "gw" {
		t.Errorf("service = %v, want gw", s["service"])
	}
}

// TestConflictingBuildEnvKeys covers the pure helper. Belt-and-
// braces on the token list — if a new built-in lands in
// buildtarget.Vars but isn't mirrored into externalBuildBuiltinTokens
// here, the corresponding test below catches the drift.
func TestConflictingBuildEnvKeys(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want []string
	}{
		{"empty", nil, nil},
		{"no_conflict", map[string]string{"CGO_ENABLED": "0", "FOO": "bar"}, nil},
		{"single_image", map[string]string{"IMAGE": "x"}, []string{"IMAGE"}},
		{
			"multi_sorted",
			map[string]string{"TAG": "x", "IMAGE": "y", "SERVICE": "z", "OTHER": "w"},
			[]string{"IMAGE", "SERVICE", "TAG"},
		},
		{"build_cwd_token", map[string]string{"BUILD_CWD": "x"}, []string{"BUILD_CWD"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := conflictingBuildEnvKeys(c.in)
			if !equalStringSlices(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestExternalBuildBuiltinTokens_MatchesBuildtargetVars pins the
// drift contract: every key buildtarget.Vars emits must appear in
// externalBuildBuiltinTokens. Without this assertion, a new built-in
// added to buildtarget.Vars would silently stop being detected as a
// conflict on the audit side.
func TestExternalBuildBuiltinTokens_MatchesBuildtargetVars(t *testing.T) {
	// Build a spec with EVERY built-in slot non-empty so Vars emits
	// each key. BuildEnv stays empty so the result is only the
	// built-ins.
	spec := buildtarget.Spec{
		Service:    "s",
		Image:      "i",
		Tag:        "t",
		TargetArch: "a",
		Registry:   "r",
		ProjectDir: "p",
		BuildCwd:   "c",
		BuildCmd:   "x",
	}
	vars := buildtarget.Vars(spec)
	got := make(map[string]struct{}, len(vars))
	for k := range vars {
		got[k] = struct{}{}
	}
	want := make(map[string]struct{}, len(externalBuildBuiltinTokens))
	for _, k := range externalBuildBuiltinTokens {
		want[k] = struct{}{}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("buildtarget.Vars emits %q but externalBuildBuiltinTokens omits it — add it to the audit list", k)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("externalBuildBuiltinTokens lists %q but buildtarget.Vars no longer emits it — drop it from the audit list", k)
		}
	}
}

// TestAuditExternalBuilds_NoCfgIsError pins the contract for a
// non-forge project: nil cfg → status=error, no panic. Mirrors
// auditIngress's no-forge.yaml branch.
func TestAuditExternalBuilds_NoCfgIsError(t *testing.T) {
	cat := auditExternalBuilds(nil, t.TempDir())
	if cat.Status != audittype.StatusError {
		t.Errorf("status = %q, want error", cat.Status)
	}
}

// equalStringSlices is local-only — testify isn't a dep and reflect
// equality flips nil vs empty distinctions in ways we don't want
// here (conflict helper returns nil for "no conflicts").
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
