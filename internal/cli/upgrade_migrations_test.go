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

// TestMigrationSkills_DeclareAGate is the invariant that keeps the worklist
// honest: every shipped migration must declare at least one gate — a version
// range, a detection script, retired, or elective. A migration declaring none
// claims to apply to every project forge ever generated, which is never true;
// a tree of those turns `forge project upgrade` into a catalogue dump that
// users learn to scroll past.
//
// Declared version bounds must also be orderable, or the gate silently
// disappears at comparison time.
func TestMigrationSkills_DeclareAGate(t *testing.T) {
	metas, err := loadMigrationMetas()
	if err != nil {
		t.Fatalf("loadMigrationMetas: %v", err)
	}
	if len(metas) == 0 {
		t.Fatal("no migration skills discovered")
	}
	for _, m := range metas {
		gated := m.Retired || m.Elective ||
			strings.TrimSpace(m.Detection) != "" ||
			strings.TrimSpace(m.AppliesFrom) != "" ||
			strings.TrimSpace(m.AppliesTo) != ""
		if !gated {
			t.Errorf("migration %q declares no gate: add applies-from/applies-to, a detection script, or retired/elective to its frontmatter", m.ID)
		}
		for label, bound := range map[string]string{"applies-from": m.AppliesFrom, "applies-to": m.AppliesTo} {
			if strings.TrimSpace(bound) != "" && semverKey(bound) == "" {
				t.Errorf("migration %q has unorderable %s %q", m.ID, label, bound)
			}
		}
	}
}

// TestLoadMigrationMetas_TombstoneIsRetired pins the tombstone's frontmatter.
// v0.1-to-v0.2 migrates toward a DI shape that was itself deleted, so it
// applies to nothing; `retired: true` is what makes that machine-readable
// instead of relying on the description starting with the word TOMBSTONE.
func TestLoadMigrationMetas_TombstoneIsRetired(t *testing.T) {
	metas, err := loadMigrationMetas()
	if err != nil {
		t.Fatalf("loadMigrationMetas: %v", err)
	}
	for _, m := range metas {
		if m.ID == "v0.1-to-v0.2" {
			if !m.Retired {
				t.Error("v0.1-to-v0.2 must declare retired: true — its target shape no longer exists")
			}
			return
		}
	}
	t.Fatal("v0.1-to-v0.2 not discovered")
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
//
// versionInRange is pure ordering — it answers "does this version fall in
// this window" and nothing else. Whether an unorderable or dev-build
// baseline should see a migration is migrationApplies' business
// (TestMigrationApplies_*), not something smuggled in here as a special
// case: conflating the two is what let a dev build be sorted against
// migrations for releases it was never on.
func TestVersionInRange(t *testing.T) {
	tests := []struct {
		name    string
		version string
		from    string
		to      string
		want    bool
	}{
		// A version we cannot order cannot be excluded by a range.
		// (Whether an unorderable baseline should see the migration at
		// all is migrationApplies' call, not this one's.)
		{"empty project version", "", "v0.5.0", "v0.6.0", true},
		{"dev sentinel", "dev", "v0.5.0", "v0.6.0", true},
		// As a pure version, 0.0.0 orders below v0.5.0.
		{"0.0.0 orders below the range", "0.0.0", "v0.5.0", "v0.6.0", false},
		// A dev build's pseudo-version is a pre-release of the next
		// patch. SemVer places it precisely, so it takes part in
		// version gating like any other version.
		{"pseudoversion below range", "v0.0.4-0.20260724212501-dfb85daf8474+dirty", "v0.5.0", "v0.6.0", false},
		{"pseudoversion inside range", "v0.5.4-0.20260724212501-dfb85daf8474+dirty", "v0.5.0", "v0.6.0", true},
		{"pseudoversion is a pre-release of its base", "v0.6.0-0.20260724212501-dfb85daf8474", "v0.5.0", "v0.6.0", true},
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
		// Leading "v" optional on both sides.
		{"unprefixed version", "0.5.0", "v0.5.0", "v0.6.0", true},
		// Released 0.0.x ordering: the hop forge is actually making.
		{"0.0.3 inside a 0.0.x window", "v0.0.3", "v0.0.2", "v0.0.5", true},
		{"0.0.1 below a 0.0.x window", "v0.0.1", "v0.0.2", "v0.0.5", false},
		{"0.0.5 at a 0.0.x upper bound", "v0.0.5", "v0.0.2", "v0.0.5", false},
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
frontends: []
`)
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), cfg, 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	markServiceProject(t, dir)
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

// TestIsPreV01Baseline pins the baseline classification. Forge has never
// released v0.1.0, so every real pin in existence — released 0.0.x tags,
// pseudo-versions from local checkouts, the unpinned sentinel — is below the
// line. Getting this wrong in either direction is how a project gets handed
// the migrations of a release it was never on.
func TestIsPreV01Baseline(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"", true},        // unpinned
		{"0.0.0", true},   // unpinned sentinel
		{"dev", true},     // unorderable sentinel
		{"(devel)", true}, // unorderable sentinel
		{"v0.0.3", true},  // released 0.0.x tag
		{"v0.0.4-0.20260724212501-dfb85daf8474+dirty", true}, // dirty local build
		{"v0.0.0-20260430002332-8f05b089372c", true},         // untagged-base pseudo-version
		{"v0.1.0-0.20260724212501-dfb85daf8474", true},       // pre-release OF v0.1.0 is still below it
		{"v0.1.0", false}, // the first compat tag
		{"v0.1.3", false},
		{"v1.4.0", false},
	}
	for _, tt := range tests {
		if got := isPreV01Baseline(tt.version); got != tt.want {
			t.Errorf("isPreV01Baseline(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

// TestMigrationApplies_RetiredNeverApplies is the regression test for the bug
// this whole path existed to produce: a project was offered
// migrations/v0.1-to-v0.2, a tombstone whose target shape had already been
// deleted. A retired migration applies to nothing, at any baseline, with or
// without a matching detection script.
func TestMigrationApplies_RetiredNeverApplies(t *testing.T) {
	root := t.TempDir()
	retired := migrationMeta{ID: "v0.1-to-v0.2", Retired: true, Detection: "true"}
	for _, baseline := range []string{
		"",
		"0.0.0",
		"dev",
		"v0.0.3",
		"v0.0.4-0.20260724212501-dfb85daf8474+dirty",
		"v0.1.0",
		"v2.0.0",
	} {
		if migrationApplies(retired, baseline, root) {
			t.Errorf("retired migration offered at baseline %q", baseline)
		}
	}
}

// TestMigrationApplies_PreV01BaselineSkipsV01Migrations: a baseline below
// v0.1.0 must never be handed a migration whose range starts at v0.1.0 —
// including when the baseline is a dev build's pseudo-version, which is the
// shape every locally-built forge stamps.
func TestMigrationApplies_PreV01BaselineSkipsV01Migrations(t *testing.T) {
	root := t.TempDir()
	m := migrationMeta{ID: "v0.1-to-v0.2", AppliesFrom: "v0.1.0", AppliesTo: "v0.2.0"}
	for _, baseline := range []string{
		"",       // unpinned: no version evidence, and no detection to fall back on
		"0.0.0",  // unset-field sentinel: same
		"dev",    // unorderable sentinel: same
		"v0.0.3", // released 0.0.x tag: ordered below the window
		"v0.0.4-0.20260724212501-dfb85daf8474+dirty", // dev build: ordered below the window
		"v0.0.0-20260430002332-8f05b089372c",         // untagged-base pseudo-version: same
	} {
		if migrationApplies(m, baseline, root) {
			t.Errorf("baseline %q (pre-v0.1) was offered a v0.1→v0.2 migration", baseline)
		}
	}
	// A project genuinely at v0.1.x still gets it: fixing the pre-v0.1 case
	// must not disarm real version gating.
	if !migrationApplies(m, "v0.1.4", root) {
		t.Error("a v0.1.4 project must still be offered the v0.1→v0.2 migration")
	}
}

// TestMigrationApplies_ReleasedVersionOrdering exercises a real released
// 0.0.x → 0.0.x hop end to end: three migrations with adjacent windows, one
// project version, exactly one match. This is the ordering guarantee the
// upgrade story rests on, at the version range forge actually ships in.
func TestMigrationApplies_ReleasedVersionOrdering(t *testing.T) {
	root := t.TempDir()
	early := migrationMeta{ID: "early", AppliesFrom: "v0.0.1", AppliesTo: "v0.0.3"}
	current := migrationMeta{ID: "current", AppliesFrom: "v0.0.3", AppliesTo: "v0.0.4"}
	later := migrationMeta{ID: "later", AppliesFrom: "v0.0.4", AppliesTo: "v0.1.0"}

	got := applicableMigrations([]migrationMeta{later, early, current}, "v0.0.3", root)
	if len(got) != 1 || got[0].Meta.ID != "current" {
		var ids []string
		for _, r := range got {
			ids = append(ids, r.Meta.ID)
		}
		t.Fatalf("v0.0.3 baseline matched %v, want exactly [current]", ids)
	}

	// Stepping the baseline one release forward moves the window with it.
	got = applicableMigrations([]migrationMeta{later, early, current}, "v0.0.4", root)
	if len(got) != 1 || got[0].Meta.ID != "later" {
		var ids []string
		for _, r := range got {
			ids = append(ids, r.Meta.ID)
		}
		t.Fatalf("v0.0.4 baseline matched %v, want exactly [later]", ids)
	}
}

// TestMigrationApplies_DetectionGatesInsideTheRange: being in range is
// necessary, not sufficient. The project must actually exhibit the old shape.
func TestMigrationApplies_DetectionGatesInsideTheRange(t *testing.T) {
	root := t.TempDir()
	m := migrationMeta{ID: "shape", AppliesFrom: "v0.0.1", AppliesTo: "v0.1.0", Detection: "test -f old-shape-marker"}
	if migrationApplies(m, "v0.0.3", root) {
		t.Error("in-range migration offered to a project that does not exhibit the old shape")
	}
	if err := os.WriteFile(filepath.Join(root, "old-shape-marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !migrationApplies(m, "v0.0.3", root) {
		t.Error("in-range migration withheld from a project that DOES exhibit the old shape")
	}
}

// TestMigrationApplies_DevBaselineUsesDetection is the dev-without-a-tag
// guarantee: a project bridged to a local forge checkout pins a
// pseudo-version, and that must still produce a correct answer — the boundless
// shape migrations are gated on what the project actually contains, with no
// published tag anywhere.
func TestMigrationApplies_DevBaselineUsesDetection(t *testing.T) {
	root := t.TempDir()
	const devPin = "v0.0.4-0.20260724212501-dfb85daf8474+dirty"
	m := migrationMeta{ID: "v0.x-to-typed-di", Detection: "test -f pkg/app/wire_gen.go"}

	if migrationApplies(m, devPin, root) {
		t.Error("dev-build baseline offered a migration whose shape the project does not have")
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "app", "wire_gen.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !migrationApplies(m, devPin, root) {
		t.Error("dev-build baseline denied a migration the project demonstrably needs")
	}
}

// TestMigrationApplies_ElectiveIsNeverPushed: binary=shared is an architecture
// choice, not drift off a shape forge stopped generating. It stays loadable by
// name; it never lands in the automatic worklist.
func TestMigrationApplies_ElectiveIsNeverPushed(t *testing.T) {
	root := t.TempDir()
	m := migrationMeta{ID: "v0.x-to-binary-shared", Elective: true, Detection: "true"}
	if migrationApplies(m, "v0.0.3", root) {
		t.Error("elective migration pushed into the worklist")
	}
}

// TestRunUpgradeApply_RetiredIDRefused: recording a tombstone as "applied"
// would put a lie in .forge/migrations.json.
func TestRunUpgradeApply_RetiredIDRefused(t *testing.T) {
	dir := newTestProject(t)
	withCwd(t, dir, func() {
		var buf bytes.Buffer
		err := runUpgradeApply(&buf, "v0.1-to-v0.2")
		if err == nil {
			t.Fatal("expected an error applying a retired migration")
		}
		if !strings.Contains(err.Error(), "retired") {
			t.Errorf("error should say the migration is retired: %v", err)
		}
	})
}

// TestMigrationApplies_UnknownBaselineNeedsDetection: when the project names
// no version, the version range stops being evidence and a detection script
// becomes mandatory. Without one, the migration cannot show it applies, so it
// stays out of the worklist rather than padding it.
func TestMigrationApplies_UnknownBaselineNeedsDetection(t *testing.T) {
	root := t.TempDir()
	rangeOnly := migrationMeta{ID: "range-only", AppliesFrom: "v0.5.0", AppliesTo: "v0.6.0"}
	detected := migrationMeta{ID: "detected", AppliesFrom: "v0.5.0", AppliesTo: "v0.6.0", Detection: "test -f old-shape-marker"}

	for _, baseline := range []string{"", "0.0.0", "v0.0.0", "dev", "(devel)", "not-a-version"} {
		if migrationApplies(rangeOnly, baseline, root) {
			t.Errorf("baseline %q: range-only migration offered with no evidence", baseline)
		}
		if migrationApplies(detected, baseline, root) {
			t.Errorf("baseline %q: detection-gated migration offered before its shape exists", baseline)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "old-shape-marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, baseline := range []string{"", "0.0.0", "dev"} {
		if !migrationApplies(detected, baseline, root) {
			t.Errorf("baseline %q: detection matched but the migration was withheld", baseline)
		}
		if migrationApplies(rangeOnly, baseline, root) {
			t.Errorf("baseline %q: range-only migration still has no evidence", baseline)
		}
	}
}
