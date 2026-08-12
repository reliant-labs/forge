package migrationlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestLintMigrationsDirDetectsUnsafeAddNotNullColumn(t *testing.T) {
	dir := writeMigration(t, "0001_add_name.up.sql", `ALTER TABLE users ADD COLUMN name text NOT NULL;`)

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	assertFinding(t, result, "unsafe-add-not-null-column", SeverityError)
}

func TestLintMigrationsDirAllowsBackfillBeforeSetNotNull(t *testing.T) {
	dir := writeMigration(t, "0001_backfill.up.sql", `
ALTER TABLE users ADD COLUMN name text;
UPDATE users SET name = 'unknown' WHERE name IS NULL;
ALTER TABLE users ALTER COLUMN name SET NOT NULL;
`)

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("expected no findings, got %#v", result.Findings)
	}
}

func TestLintMigrationsDirDetectsSetNotNullWithoutBackfill(t *testing.T) {
	dir := writeMigration(t, "0001_set_not_null.up.sql", `ALTER TABLE users ALTER COLUMN email SET NOT NULL;`)

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	assertFinding(t, result, "set-not-null-without-backfill", SeverityError)
}

func TestLintMigrationsDirDetectsDestructiveOperations(t *testing.T) {
	dir := writeMigration(t, "0001_drop_column.up.sql", `ALTER TABLE users DROP COLUMN legacy_name;`)

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	assertFinding(t, result, "destructive-change", SeverityError)
}

func TestLintMigrationsDirAllowsDestructiveAllowlist(t *testing.T) {
	dir := writeMigration(t, "0001_drop_column.up.sql", `ALTER TABLE users DROP COLUMN legacy_name;`)

	cfg := DefaultConfig()
	cfg.AllowedDestructive = []string{"0001_drop_column.up.sql"}
	result, err := LintMigrationsDir(dir, cfg)
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("expected no findings, got %#v", result.Findings)
	}
}

// TestLintMigrationsDirAllowsProjectRelativeAllowlistPaths covers the way an
// allowlist entry is actually WRITTEN.
//
// forge.yaml's `database.migration_safety.allowed_destructive` sits beside
// `migrations_dir: db/migrations`, so the natural — and the documented —
// entry is the project-relative path, `db/migrations/0001_drop_column.up.sql`.
// The linter walks an ABSOLUTE directory, so the path it matches against is
// absolute, and filepath.Match has no `**`: a project-relative pattern
// matches neither the absolute path nor the bare basename. Every entry
// written that way silently did nothing, and the file it named kept failing
// CI with a message telling the author to allowlist the file they had
// already allowlisted.
//
// The bare-basename form is covered above; this pins the form a real
// forge.yaml uses.
func TestLintMigrationsDirAllowsProjectRelativeAllowlistPaths(t *testing.T) {
	dir := writeProjectMigration(t, "0001_drop_column.up.sql", `ALTER TABLE users DROP COLUMN legacy_name;`)

	for _, pattern := range []string{
		"db/migrations/0001_drop_column.up.sql", // the documented shape
		"db/migrations/*.up.sql",                // a glob over that directory
		"*_drop_column.up.sql",                  // basename glob (already worked)
	} {
		t.Run(pattern, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.AllowedDestructive = []string{pattern}
			result, err := LintMigrationsDir(dir, cfg)
			if err != nil {
				t.Fatalf("LintMigrationsDir() error = %v", err)
			}
			if len(result.Findings) != 0 {
				t.Fatalf("allowlist entry %q did not suppress the destructive finding — "+
					"an entry that matches nothing is indistinguishable from no entry at "+
					"all, and the error tells the author to add the entry they already "+
					"wrote. Got %#v", pattern, result.Findings)
			}
		})
	}
}

// TestLintMigrationsDirAllowlistDoesNotOverMatch is the other half: making
// the allowlist match a project-relative path must not turn it into a
// substring check that silences unrelated files.
func TestLintMigrationsDirAllowlistDoesNotOverMatch(t *testing.T) {
	dir := writeProjectMigration(t, "0001_drop_column.up.sql", `ALTER TABLE users DROP COLUMN legacy_name;`)
	if err := os.WriteFile(filepath.Join(dir, "0002_drop_other.up.sql"),
		[]byte(`ALTER TABLE users DROP COLUMN legacy_name;`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.AllowedDestructive = []string{"db/migrations/0001_drop_column.up.sql"}
	result, err := LintMigrationsDir(dir, cfg)
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("exactly the un-allowlisted migration must still be reported, got %#v", result.Findings)
	}
	if !strings.Contains(result.Findings[0].File, "0002_drop_other") {
		t.Errorf("the surviving finding must be 0002_drop_other, got %s", result.Findings[0].File)
	}
}

// TestLintMigrationsDirHonorsPerFileAllowDestructivePragma pins the per-file
// opt-out for destructive operations: a `-- forge:allow-destructive` (or
// `-- forge-safety: allow-destructive`) comment anywhere in the migration
// silences the destructive-change rule for that file alone, without
// requiring a forge.yaml AllowedDestructive entry. Useful for one-off
// replace-this-table migrations in multi-agent lanes where forge.yaml is
// owned by a different agent. See migrationlint-no-per-file-destructive-
// pragma in FORGE_BACKLOG.
func TestLintMigrationsDirHonorsPerFileAllowDestructivePragma(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{
			name:    "forge_colon_form",
			content: "-- forge:allow-destructive (legacy table rename, intentional)\nDROP TABLE legacy;\nCREATE TABLE legacy (id BIGINT PRIMARY KEY);",
		},
		{
			name:    "forge_safety_form",
			content: "-- forge-safety: allow-destructive — legacy table rename\nDROP TABLE legacy;",
		},
		{
			name:    "uppercase_form",
			content: "--  FORGE:ALLOW-DESTRUCTIVE\nDROP TABLE legacy;",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeMigration(t, "0001_drop.up.sql", tc.content)

			result, err := LintMigrationsDir(dir, DefaultConfig())
			if err != nil {
				t.Fatalf("LintMigrationsDir() error = %v", err)
			}
			for _, f := range result.Findings {
				if f.Rule == "destructive-change" {
					t.Fatalf("destructive-change should be silenced by pragma; got %#v", f)
				}
			}
		})
	}
}

// TestLintMigrationsDirPragmaDoesNotSilenceOtherRules guards against a
// pragma that's too broad — the destructive opt-out must not suppress
// the unsafe-add-not-null-column or volatile-default findings.
func TestLintMigrationsDirPragmaDoesNotSilenceOtherRules(t *testing.T) {
	dir := writeMigration(t, "0001_pragma_plus_unsafe.up.sql",
		"-- forge:allow-destructive\nDROP TABLE legacy;\nALTER TABLE users ADD COLUMN name text NOT NULL;")

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	assertFinding(t, result, "unsafe-add-not-null-column", SeverityError)
	for _, f := range result.Findings {
		if f.Rule == "destructive-change" {
			t.Fatalf("destructive-change should be silenced by pragma; got %#v", f)
		}
	}
}

func TestLintMigrationsDirDetectsVolatileDefault(t *testing.T) {
	dir := writeMigration(t, "0001_add_token.up.sql", `ALTER TABLE users ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();`)

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	assertFinding(t, result, "volatile-default", SeverityWarn)
}

// TestLintMigrationsDirDetectsVolatileDefaultVariants pins the full set
// of non-deterministic DEFAULT expressions the rule must catch on an
// ADD COLUMN. Each of these assigns an unpredictable (or uniformly
// identical) value to every pre-existing row at backfill time, which is
// the correctness trap the rule exists to flag.
func TestLintMigrationsDirDetectsVolatileDefaultVariants(t *testing.T) {
	for _, expr := range []string{
		"now()",
		"NOW ()",
		"current_timestamp",
		"CURRENT_TIMESTAMP",
		"clock_timestamp()",
		"statement_timestamp()",
		"transaction_timestamp()",
		"gen_random_uuid()",
		"uuid_generate_v4()",
		"random()",
		"RANDOM()",
	} {
		t.Run(expr, func(t *testing.T) {
			dir := writeMigration(t, "0001_add_col.up.sql",
				"ALTER TABLE users ADD COLUMN c text DEFAULT "+expr+";")

			result, err := LintMigrationsDir(dir, DefaultConfig())
			if err != nil {
				t.Fatalf("LintMigrationsDir() error = %v", err)
			}
			assertFinding(t, result, "volatile-default", SeverityWarn)
		})
	}
}

// TestLintMigrationsDirAllowsDeterministicDefaults is the negative half
// of the rule: a constant DEFAULT is safe on a populated table and must
// not be flagged, or the rule would be noise on ordinary migrations.
func TestLintMigrationsDirAllowsDeterministicDefaults(t *testing.T) {
	for _, expr := range []string{"0", "false", "'unknown'", "'2020-01-01'::timestamptz"} {
		t.Run(expr, func(t *testing.T) {
			dir := writeMigration(t, "0001_add_col.up.sql",
				"ALTER TABLE users ADD COLUMN c text NOT NULL DEFAULT "+expr+";")

			result, err := LintMigrationsDir(dir, DefaultConfig())
			if err != nil {
				t.Fatalf("LintMigrationsDir() error = %v", err)
			}
			for _, f := range result.Findings {
				if f.Rule == "volatile-default" {
					t.Fatalf("deterministic DEFAULT %s must not be flagged; got %#v", expr, f)
				}
			}
		})
	}
}

// TestConfigFromProjectEnablesRulesAtProjectDefaults guards the join
// between the two severity vocabularies: forge.yaml spells the warning
// level "warn" (config.effectiveSeverity normalizes onto that spelling)
// while finding.Severity spells it "warning". If the parser stops
// accepting what the config layer emits, the affected rule silently
// stops firing rather than failing loudly — so assert that a
// default-constructed project config actually leaves every rule ARMED,
// and that an explicit "off" is the only thing that disarms one.
func TestConfigFromProjectEnablesRulesAtProjectDefaults(t *testing.T) {
	cfg := ConfigFromProject(config.MigrationSafetyConfig{})
	if severityFor(cfg.VolatileDefault) != SeverityWarn {
		t.Fatalf("volatile-default disarmed at project defaults: %q parsed to %q",
			cfg.VolatileDefault, severityFor(cfg.VolatileDefault))
	}
	if severityFor(cfg.UnsafeAddColumn) != SeverityError {
		t.Fatalf("unsafe-add-column disarmed at project defaults: %q", cfg.UnsafeAddColumn)
	}
	if severityFor(cfg.DestructiveChange) != SeverityError {
		t.Fatalf("destructive-change disarmed at project defaults: %q", cfg.DestructiveChange)
	}

	// A rule explicitly downgraded to a warning must still fire.
	warned := ConfigFromProject(config.MigrationSafetyConfig{DestructiveChange: "warn"})
	if severityFor(warned.DestructiveChange) != SeverityWarn {
		t.Fatalf("destructive_change: warn disarmed the rule: %q parsed to %q",
			warned.DestructiveChange, severityFor(warned.DestructiveChange))
	}

	// "off" is the one dial that disables.
	disabled := ConfigFromProject(config.MigrationSafetyConfig{VolatileDefault: "off"})
	if severityFor(disabled.VolatileDefault) != "" {
		t.Fatalf("volatile_default: off should disable the rule, got %q", severityFor(disabled.VolatileDefault))
	}
}

// TestLintMigrationsDirVolatileDefaultFiresAtProjectDefaults is the
// end-to-end form of the above: the rule must produce a finding when
// driven by project config, not just by DefaultConfig().
func TestLintMigrationsDirVolatileDefaultFiresAtProjectDefaults(t *testing.T) {
	dir := writeMigration(t, "0001_add_token.up.sql",
		`ALTER TABLE users ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();`)

	result, err := LintMigrationsDir(dir, ConfigFromProject(config.MigrationSafetyConfig{}))
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	assertFinding(t, result, "volatile-default", SeverityWarn)
}

func TestLintMigrationsDirIgnoresDownMigrations(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0001_drop.down.sql"), []byte(`DROP TABLE users;`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("LintMigrationsDir() error = %v", err)
	}
	if len(result.Findings) != 0 {
		t.Fatalf("expected no findings, got %#v", result.Findings)
	}
}

func writeMigration(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeProjectMigration is writeMigration with the real directory LAYOUT:
// <tmp>/db/migrations/<name>, so the absolute path the linter walks actually
// ends in the project-relative tail an allowed_destructive entry names.
// writeMigration's flat temp dir cannot exercise that, which is why the
// project-relative allowlist form went unnoticed.
func writeProjectMigration(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "db", "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertFinding(t *testing.T, result Result, rule string, severity Severity) {
	t.Helper()
	for _, finding := range result.Findings {
		if finding.Rule == rule && finding.Severity == severity && finding.Line > 0 {
			return
		}
	}
	t.Fatalf("expected finding %s/%s with line number, got %#v", rule, severity, result.Findings)
}
