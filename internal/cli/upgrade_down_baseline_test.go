package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/migrationlint"
)

// downBaselineProject writes a forge.yaml plus the given migration files and
// returns the forge.yaml path.
func downBaselineProject(t *testing.T, yamlBody string, files ...string) string {
	t.Helper()
	root := t.TempDir()
	migDir := filepath.Join(root, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(migDir, f), []byte("SELECT 1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(root, "forge.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// An upgrade over a project with pre-policy down files records the newest one
// as the grandfather line, and the migration-safety lint then passes (one
// warning) instead of failing on history — while a down file added AFTER the
// line is still an error.
func TestRecordDownFilesBaseline_GrandfathersExistingHistoryOnce(t *testing.T) {
	cfgPath := downBaselineProject(t,
		"name: demo\nmodule_path: github.com/example/demo\ndatabase:\n    driver: postgres\n    migrations_dir: db/migrations\n",
		"00001_a.up.sql", "00001_a.down.sql", "00002_b.up.sql", "00002_b.down.sql", "00003_c.up.sql")
	cfg := &config.ProjectConfig{}
	cfg.Database.MigrationsDir = "db/migrations"

	_ = captureStdout(t, func() {
		if err := recordDownFilesBaseline(cfg, cfgPath, false); err != nil {
			t.Fatalf("recordDownFilesBaseline: %v", err)
		}
	})
	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), `down_files_allowed_until: "00002"`) &&
		!strings.Contains(string(raw), "down_files_allowed_until: 00002") {
		t.Fatalf("forge.yaml must record the newest down file's version; got:\n%s", raw)
	}
	loaded, err := config.LoadProject(raw, cfgPath)
	if err != nil {
		t.Fatalf("the written forge.yaml must load (a strict-loader rejection would brick the project): %v", err)
	}
	if got := loaded.Database.MigrationSafety.DownFilesAllowedUntil; got != "00002" {
		t.Fatalf("round-tripped down_files_allowed_until = %q, want 00002", got)
	}

	migDir := filepath.Join(filepath.Dir(cfgPath), "db", "migrations")
	res, err := migrationlint.LintMigrationsDir(migDir, migrationlint.ConfigFromProject(loaded.Database.MigrationSafety))
	if err != nil {
		t.Fatal(err)
	}
	if res.HasErrors() {
		t.Fatalf("grandfathered history must not fail the lint; got %#v", res.Findings)
	}

	// A new down file after the upgrade is still caught.
	if err := os.WriteFile(filepath.Join(migDir, "00003_c.down.sql"), []byte("SELECT 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = migrationlint.LintMigrationsDir(migDir, migrationlint.ConfigFromProject(loaded.Database.MigrationSafety))
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasErrors() {
		t.Fatal("a down file newer than the recorded line must fail the lint")
	}

	// Idempotent: the key is set now, so a second upgrade must not move it
	// (that would grandfather 00003 — the file the rule exists to catch).
	cfg.Database.MigrationSafety.DownFilesAllowedUntil = loaded.Database.MigrationSafety.DownFilesAllowedUntil
	before, _ := os.ReadFile(cfgPath)
	_ = captureStdout(t, func() {
		if err := recordDownFilesBaseline(cfg, cfgPath, false); err != nil {
			t.Fatal(err)
		}
	})
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatalf("an already-set baseline must never move; forge.yaml changed:\n%s", after)
	}
}

// A project with no down files gets no key at all — nothing to grandfather.
func TestRecordDownFilesBaseline_NoDownFilesWritesNothing(t *testing.T) {
	body := "name: demo\ndatabase:\n    driver: postgres\n"
	cfgPath := downBaselineProject(t, body, "00001_a.up.sql")
	_ = captureStdout(t, func() {
		if err := recordDownFilesBaseline(&config.ProjectConfig{}, cfgPath, false); err != nil {
			t.Fatal(err)
		}
	})
	if raw, _ := os.ReadFile(cfgPath); string(raw) != body {
		t.Fatalf("forge.yaml must be untouched; got:\n%s", raw)
	}
}

// --check reports and writes nothing.
func TestRecordDownFilesBaseline_CheckIsReadOnly(t *testing.T) {
	body := "name: demo\n"
	cfgPath := downBaselineProject(t, body, "00007_a.up.sql", "00007_a.down.sql")
	out := captureStdout(t, func() {
		if err := recordDownFilesBaseline(&config.ProjectConfig{}, cfgPath, true); err != nil {
			t.Fatal(err)
		}
	})
	if raw, _ := os.ReadFile(cfgPath); string(raw) != body {
		t.Fatalf("--check must not write forge.yaml; got:\n%s", raw)
	}
	if !strings.Contains(out, `"00007"`) {
		t.Errorf("--check should say what it would record; got %q", out)
	}
}
