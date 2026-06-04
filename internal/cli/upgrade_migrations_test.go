package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadMigrationMetas_FindsDevTargetSkill verifies the embedded
// templates expose the dev-target-to-kcl-deploy skill (the first
// shipped migration). This pins the discovery convention so future
// migrations can be added by dropping a SKILL.md into the templates
// dir without touching the loader.
func TestLoadMigrationMetas_FindsDevTargetSkill(t *testing.T) {
	metas, err := loadMigrationMetas()
	if err != nil {
		t.Fatalf("loadMigrationMetas: %v", err)
	}
	var found *migrationMeta
	for i := range metas {
		if metas[i].ID == "dev-target-to-kcl-deploy" {
			found = &metas[i]
			break
		}
	}
	if found == nil {
		ids := make([]string, 0, len(metas))
		for _, m := range metas {
			ids = append(ids, m.ID)
		}
		t.Fatalf("dev-target-to-kcl-deploy not found; got IDs: %v", ids)
	}
	if found.AppliesFrom == "" || found.AppliesTo == "" {
		t.Errorf("dev-target-to-kcl-deploy missing version bounds: from=%q to=%q",
			found.AppliesFrom, found.AppliesTo)
	}
	if found.Detection == "" {
		t.Errorf("dev-target-to-kcl-deploy missing detection script")
	}
	if found.SkillPath != "migrations/dev-target-to-kcl-deploy" {
		t.Errorf("unexpected skill path: %q", found.SkillPath)
	}
}

// TestLoadMigrationMetas_FindsVersionDirs verifies the walker picks up
// v*-to-* migration skills (e.g. v0.1-to-v0.2, v0.x-to-contractkit).
// These shipped for several forge versions and are stable anchors for
// pinning the discovery convention.
func TestLoadMigrationMetas_FindsVersionDirs(t *testing.T) {
	metas, err := loadMigrationMetas()
	if err != nil {
		t.Fatalf("loadMigrationMetas: %v", err)
	}

	// Spot-check a couple of IDs that should be present. We don't pin
	// the full set — new migrations land all the time — but these have
	// shipped for several forge versions and are stable anchors.
	wantSome := []string{"v0.1-to-v0.2", "v0.x-to-contractkit"}
	got := make(map[string]string)
	for _, m := range metas {
		got[m.ID] = m.SkillPath
	}
	for _, id := range wantSome {
		skillPath, ok := got[id]
		if !ok {
			ids := make([]string, 0, len(got))
			for k := range got {
				ids = append(ids, k)
			}
			t.Fatalf("migration %q not discovered; got IDs: %v", id, ids)
		}
		// All migrations live under "migrations/" (plural). The
		// SkillPath flows into `forge skill load <path>` so the prefix
		// must reflect the on-disk root.
		want := "migrations/" + id
		if skillPath != want {
			t.Errorf("%s SkillPath = %q, want %q", id, skillPath, want)
		}
	}
}

// TestParseMigrationFrontmatter_ExtractsAllFields exercises the
// migration-specific parser. Quoted values must be unquoted; missing
// fields should leave the meta empty (not error).
func TestParseMigrationFrontmatter_ExtractsAllFields(t *testing.T) {
	body := []byte(`---
name: demo
description: a demo migration
applies-from: v0.5.0
applies-to: "v0.6.0"
detection: 'grep -l foo bar'
---

# body
`)
	m := parseMigrationFrontmatter(body)
	if m.Title != "demo" {
		t.Errorf("Title = %q, want demo", m.Title)
	}
	if m.Description != "a demo migration" {
		t.Errorf("Description = %q", m.Description)
	}
	if m.AppliesFrom != "v0.5.0" {
		t.Errorf("AppliesFrom = %q", m.AppliesFrom)
	}
	if m.AppliesTo != "v0.6.0" {
		t.Errorf("AppliesTo = %q (quotes should be stripped)", m.AppliesTo)
	}
	if m.Detection != "grep -l foo bar" {
		t.Errorf("Detection = %q", m.Detection)
	}
}

// TestParseMigrationFrontmatter_NoFrontmatter returns an empty meta
// rather than panicking on bodies without a leading "---\n".
func TestParseMigrationFrontmatter_NoFrontmatter(t *testing.T) {
	m := parseMigrationFrontmatter([]byte("# just markdown\n"))
	if m.Title != "" || m.Description != "" || m.AppliesFrom != "" {
		t.Errorf("expected empty meta, got %+v", m)
	}
}

// TestVersionInRange covers the half-open [from, to) range semantics
// plus all the empty-bound special cases.
func TestVersionInRange(t *testing.T) {
	tests := []struct {
		name    string
		version string
		from    string
		to      string
		want    bool
	}{
		// Empty project version means "all migrations apply" — the spec
		// case: project with no forge_version pin lists everything.
		{"empty project version", "", "v0.5.0", "v0.6.0", true},
		// "0.0.0" sentinel (EffectiveForgeVersion fallback) treated
		// like empty for the same reason.
		{"0.0.0 sentinel", "0.0.0", "v0.5.0", "v0.6.0", true},
		// Pseudoversion from `go install` against an untagged checkout
		// — real-world projects like cp-forge are pinned to one of
		// these. Must surface every migration, not silently filter
		// them out as "newer than the range".
		{"go install pseudoversion", "v0.0.0-20260530233501-ec0254f463b3+dirty", "v0.5.0", "v0.6.0", true},
		{"in range", "v0.5.0", "v0.5.0", "v0.6.0", true},
		{"in range mid", "v0.5.3", "v0.5.0", "v0.6.0", true},
		{"below range", "v0.4.9", "v0.5.0", "v0.6.0", false},
		{"at upper bound (half-open)", "v0.6.0", "v0.5.0", "v0.6.0", false},
		{"above range", "v0.7.0", "v0.5.0", "v0.6.0", false},
		// Open bounds.
		{"open from", "v0.4.0", "", "v0.6.0", true},
		{"open to", "v9.0.0", "v0.5.0", "", true},
		{"both open", "v0.5.0", "", "", true},
		// Missing patch component normalises to 0.
		{"missing patch", "v0.5", "v0.5.0", "v0.6.0", true},
		// Leading "v" stripped both sides.
		{"unprefixed version", "0.5.0", "v0.5.0", "v0.6.0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := versionInRange(tt.version, tt.from, tt.to)
			if got != tt.want {
				t.Errorf("versionInRange(%q, %q, %q) = %v, want %v",
					tt.version, tt.from, tt.to, got, tt.want)
			}
		})
	}
}

// TestRunDetection_EmptyScriptMatches verifies the "no detection script"
// case is treated as "migration applies".
func TestRunDetection_EmptyScriptMatches(t *testing.T) {
	if !runDetection(t.TempDir(), "") {
		t.Error("expected runDetection to return true for empty script")
	}
}

// TestRunDetection_ScriptExitCode covers the script-runs path. We
// intentionally do not exercise grep over a real forge.yaml here —
// the goal is to prove the script's exit code drives the result.
func TestRunDetection_ScriptExitCode(t *testing.T) {
	dir := t.TempDir()
	if !runDetection(dir, "true") {
		t.Error("script `true` should report match")
	}
	if runDetection(dir, "false") {
		t.Error("script `false` should report no match")
	}
}

// TestRunDetection_ProjectDirSeen verifies the detection script is
// executed with the project root as CWD — important because the
// canonical detection (`grep -l dev_target forge.yaml`) is path-relative.
func TestRunDetection_ProjectDirSeen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if !runDetection(dir, "test -f marker") {
		t.Error("detection script should see CWD = project root")
	}
}

// TestReadWriteMigrationsState round-trips the state file.
func TestReadWriteMigrationsState(t *testing.T) {
	dir := t.TempDir()
	state := migrationsState{Applied: map[string]string{
		"dev-target-to-kcl-deploy": "2026-06-04T10:00:00Z",
	}}
	if err := writeMigrationsState(dir, state); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readMigrationsState(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Applied["dev-target-to-kcl-deploy"] != "2026-06-04T10:00:00Z" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Absent file = empty applied map, no error.
	got2, err := readMigrationsState(t.TempDir())
	if err != nil {
		t.Fatalf("read empty: %v", err)
	}
	if len(got2.Applied) != 0 {
		t.Errorf("expected empty Applied for absent file, got %+v", got2)
	}
}

// TestRunUpgradeApply_RecordsToStateFile is the integration test for
// the apply subcommand body: it must write a state file containing the
// applied migration id.
func TestRunUpgradeApply_RecordsToStateFile(t *testing.T) {
	dir := newTestProject(t)
	withCwd(t, dir, func() {
		var buf bytes.Buffer
		if err := runUpgradeApply(&buf, "dev-target-to-kcl-deploy"); err != nil {
			t.Fatalf("runUpgradeApply: %v", err)
		}
	})

	data, err := os.ReadFile(filepath.Join(dir, ".forge", "migrations.json"))
	if err != nil {
		t.Fatalf("read migrations.json: %v", err)
	}
	var st migrationsState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := st.Applied["dev-target-to-kcl-deploy"]; !ok {
		t.Errorf("expected dev-target-to-kcl-deploy in applied set, got %+v", st)
	}
}

// TestRunUpgradeApply_UnknownIDErrors makes sure typos surface as a
// UserErr rather than silently appending an unknown migration id.
func TestRunUpgradeApply_UnknownIDErrors(t *testing.T) {
	dir := newTestProject(t)
	withCwd(t, dir, func() {
		var buf bytes.Buffer
		err := runUpgradeApply(&buf, "no-such-migration")
		if err == nil {
			t.Fatal("expected error for unknown migration id")
		}
		if !strings.Contains(err.Error(), "no-such-migration") {
			t.Errorf("error should mention the bad id: %v", err)
		}
	})
}

// TestComputePendingMigrations_NoProjectVersion verifies a project
// without a forge_version pin lists every migration as pending. The
// dev-target-to-kcl-deploy migration's detection script greps for
// "dev_target" in forge.yaml, so we plant that string to make sure
// detection passes too.
func TestComputePendingMigrations_NoProjectVersion(t *testing.T) {
	dir := newTestProject(t)
	// Make detection pass — forge.yaml contains the dev_target string.
	planted := filepath.Join(dir, "forge.yaml")
	body, _ := os.ReadFile(planted)
	body = append(body, []byte("\n# dev_target marker for detection test\n")...)
	if err := os.WriteFile(planted, body, 0o644); err != nil {
		t.Fatalf("plant dev_target: %v", err)
	}

	var pending []pendingMigration
	withCwd(t, dir, func() {
		got, err := computePendingMigrations()
		if err != nil {
			t.Fatalf("computePendingMigrations: %v", err)
		}
		pending = got
	})

	if len(pending) == 0 {
		t.Fatal("expected at least one pending migration on an unversioned project")
	}
	var sawDevTarget bool
	for _, p := range pending {
		if p.Meta.ID == "dev-target-to-kcl-deploy" {
			sawDevTarget = true
			if p.Applied {
				t.Error("dev-target-to-kcl-deploy should NOT be marked applied yet")
			}
		}
	}
	if !sawDevTarget {
		t.Error("dev-target-to-kcl-deploy should be in pending list when detection matches")
	}
}

// TestComputePendingMigrations_DetectionFiltersOut verifies migrations
// whose detection script does NOT match are filtered out. The shipped
// dev-target-to-kcl-deploy migration greps forge.yaml for "dev_target";
// a plain test project without that string should NOT see the
// migration.
func TestComputePendingMigrations_DetectionFiltersOut(t *testing.T) {
	dir := newTestProject(t) // forge.yaml has no dev_target string

	var pending []pendingMigration
	withCwd(t, dir, func() {
		got, err := computePendingMigrations()
		if err != nil {
			t.Fatalf("computePendingMigrations: %v", err)
		}
		pending = got
	})

	for _, p := range pending {
		if p.Meta.ID == "dev-target-to-kcl-deploy" {
			t.Error("dev-target-to-kcl-deploy must be filtered out when detection finds nothing")
		}
	}
}

// TestComputePendingMigrations_VersionRangeFiltersOut covers the
// version-range gate. With a project pinned beyond the migration's
// applies-to bound, the migration should be hidden — even though the
// dev_target detection string is planted in forge.yaml.
func TestComputePendingMigrations_VersionRangeFiltersOut(t *testing.T) {
	dir := newTestProjectWithVersion(t, "v9.0.0")
	// Plant the dev_target marker so detection passes — leaves the
	// version filter as the only remaining gate.
	cfgPath := filepath.Join(dir, "forge.yaml")
	body, _ := os.ReadFile(cfgPath)
	body = append(body, []byte("# dev_target marker\n")...)
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatalf("plant marker: %v", err)
	}

	var pending []pendingMigration
	withCwd(t, dir, func() {
		got, err := computePendingMigrations()
		if err != nil {
			t.Fatalf("computePendingMigrations: %v", err)
		}
		pending = got
	})
	for _, p := range pending {
		if p.Meta.ID == "dev-target-to-kcl-deploy" {
			t.Errorf("dev-target-to-kcl-deploy must be hidden when project version > applies-to; got pending entry %+v", p)
		}
	}
}

// TestWritePendingMigrationsHuman_EmptyListSaysUpToDate confirms the
// spec-mandated empty-list message.
func TestWritePendingMigrationsHuman_EmptyListSaysUpToDate(t *testing.T) {
	var buf bytes.Buffer
	if err := writePendingMigrationsHuman(&buf, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(buf.String(), "up to date") {
		t.Errorf("expected 'up to date' message, got: %q", buf.String())
	}
}

// TestWritePendingMigrationsJSON_StableShape pins the JSON keys so
// downstream parsers don't break on a key rename.
func TestWritePendingMigrationsJSON_StableShape(t *testing.T) {
	pending := []pendingMigration{
		{Meta: migrationMeta{ID: "demo", Title: "Demo", AppliesFrom: "v0.5", AppliesTo: "v0.6"}},
	}
	var buf bytes.Buffer
	if err := writePendingMigrationsJSON(&buf, pending); err != nil {
		t.Fatalf("json: %v", err)
	}
	out := buf.String()
	for _, key := range []string{
		`"binary_version"`,
		`"pending"`,
		`"id": "demo"`,
		`"applies_from"`,
		`"applies_to"`,
	} {
		if !strings.Contains(out, key) {
			t.Errorf("JSON missing %s: %s", key, out)
		}
	}
}

// newTestProject creates a minimal forge.yaml + .forge directory in a
// temp dir so the command's project-root walk-up succeeds. Returns the
// project root path.
//
// The forge.yaml mirrors the strict-loader schema: module_path and
// version are required, so a too-small fixture causes LoadStrict to
// fail and the project-version probe falls back to "" (which would
// silently pass version-range filters).
func newTestProject(t *testing.T) string {
	t.Helper()
	return newTestProjectWithVersion(t, "")
}

// newTestProjectWithVersion is like newTestProject but lets the caller
// pin a specific forge_version. The empty string means "no pin".
func newTestProjectWithVersion(t *testing.T, forgeVersion string) string {
	t.Helper()
	dir := t.TempDir()
	pin := ""
	if forgeVersion != "" {
		pin = "forge_version: " + forgeVersion + "\n"
	}
	cfg := []byte(pin + `name: test-project
module_path: github.com/example/test
version: 0.1.0
services: []
frontends: []
`)
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), cfg, 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	return dir
}

// withCwd runs fn with the process CWD switched to dir, restoring it
// on return. The migration command uses findProjectRoot() which walks
// up from cwd, so the test must chdir to exercise the real code path.
//
// Tests that use this helper must not run in parallel — process-wide
// CWD is shared state.
func withCwd(t *testing.T, dir string, fn func()) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	defer func() {
		if err := os.Chdir(prev); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	fn()
}
