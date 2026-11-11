package config

import (
	"strings"
	"testing"
)

func loadDB(t *testing.T, yamlBody string) DatabaseConfig {
	t.Helper()
	cfg, err := LoadProject([]byte("name: demo\nmodule_path: example.com/demo\n"+yamlBody), "forge.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg.Database
}

// The directory reads the same through the canonical key, the shorthand, or
// both when they agree.
func TestMigrations_DirKeys(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"neither", "", DefaultMigrationsDir},
		{"shorthand (database.migrations_dir)", "database:\n  migrations_dir: sql/migrations\n", "sql/migrations"},
		{"canonical (database.migrations.dir)", "database:\n  migrations:\n    dir: sql/migrations\n", "sql/migrations"},
		{"both, agreeing", "database:\n  migrations_dir: ./sql/migrations\n  migrations:\n    dir: sql/migrations/\n", "sql/migrations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadDB(t, tc.yaml).MigrationsPath(); got != tc.want {
				t.Errorf("MigrationsPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMigrations_BothKeysDisagreeingIsRefused(t *testing.T) {
	_, err := LoadProject([]byte("name: demo\nmodule_path: example.com/demo\ndatabase:\n  migrations_dir: a\n  migrations:\n    dir: b\n"), "forge.yaml")
	ve := requireValidationError(t, err)
	if !strings.Contains(ve.Error(), "name different directories") {
		t.Errorf("want a refusal naming both keys, got:\n%s", ve.Error())
	}
}

func TestMigrations_ToolDefaultsToForge(t *testing.T) {
	db := loadDB(t, "")
	if db.MigrationTool() != MigrationToolForge || db.UsesMigrationHook() {
		t.Errorf("tool = %q, hook = %v; want forge and no hook", db.MigrationTool(), db.UsesMigrationHook())
	}
	db = loadDB(t, "database:\n  migrations:\n    tool: Goose\n")
	if db.MigrationTool() != MigrationToolGoose || !db.UsesMigrationHook() {
		t.Errorf("tool = %q, hook = %v; want goose (case-folded) and a hook", db.MigrationTool(), db.UsesMigrationHook())
	}
}

func TestMigrations_Validation(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"unknown tool", "database:\n  migrations:\n    tool: liquibase\n", "not a known migration tool"},
		{"custom without apply", "database:\n  migrations:\n    tool: custom\n", "no apply command"},
		{"forge with apply", "database:\n  migrations:\n    apply: [make, migrate]\n", "but tool is forge"},
		{"empty apply argv", "database:\n  migrations:\n    tool: goose\n    apply: [\"\"]\n", "apply has no command"},
		{"dir outside", "database:\n  migrations:\n    dir: ../elsewhere\n", "not a directory inside the project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadProject([]byte("name: demo\nmodule_path: example.com/demo\n"+tc.yaml), "forge.yaml")
			ve := requireValidationError(t, err)
			if !strings.Contains(ve.Error(), tc.want) {
				t.Errorf("want %q, got:\n%s", tc.want, ve.Error())
			}
		})
	}
	for _, ok := range []string{
		"database:\n  migrations:\n    tool: custom\n    apply: [make, migrate]\n",
		"database:\n  migrations:\n    tool: atlas\n    status: [atlas, migrate, status]\n",
		"database:\n  migrations:\n    tool: flyway\n    dir: db/flyway\n",
	} {
		loadDB(t, ok)
	}
}

// The light reader the codegen leaves use agrees with the loader.
func TestMigrations_DatabaseIn(t *testing.T) {
	root := t.TempDir()
	write(t, root, "forge.yaml", "name: demo\nmodule_path: example.com/demo\ndatabase:\n  migrations: {dir: migrations, tool: dbmate}\n")
	if got := MigrationToolIn(root); got != MigrationToolDbmate {
		t.Errorf("MigrationToolIn = %q, want dbmate", got)
	}
	if got := MigrationsDirIn(root); got != "migrations" {
		t.Errorf("MigrationsDirIn = %q, want migrations", got)
	}
	if got := MigrationToolIn(t.TempDir()); got != MigrationToolForge {
		t.Errorf("MigrationToolIn without forge.yaml = %q, want forge", got)
	}
}
