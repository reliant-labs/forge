package migrationlint

import (
	"os"
	"path/filepath"
	"testing"
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

func TestLintMigrationsDirDetectsVolatileDefault(t *testing.T) {
	dir := writeMigration(t, "0001_add_token.up.sql", `ALTER TABLE users ADD COLUMN token uuid NOT NULL DEFAULT gen_random_uuid();`)

	result, err := LintMigrationsDir(dir, DefaultConfig())
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

func assertFinding(t *testing.T, result Result, rule string, severity Severity) {
	t.Helper()
	for _, finding := range result.Findings {
		if finding.Rule == rule && finding.Severity == severity && finding.Line > 0 {
			return
		}
	}
	t.Fatalf("expected finding %s/%s with line number, got %#v", rule, severity, result.Findings)
}
