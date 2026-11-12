package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/pgtest"
	"github.com/reliant-labs/forge/pkg/seedplan"
)

// fakeMigrationToolEnv makes this test binary act as a project's migration
// tool: the hook execs it, and TestMain hands control to fakeMigrationTool.
const fakeMigrationToolEnv = "FORGE_CLI_FAKE_MIGRATION_TOOL"

// fakeMigrationTool applies every *.sql in $FORGE_MIGRATIONS_DIR, in name
// order, to $DATABASE_URL, the way a golang-migrate-shaped tool would.
func fakeMigrationTool() int {
	db, err := sql.Open(pgtest.DriverName, os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer func() { _ = db.Close() }()
	files, _ := filepath.Glob(filepath.Join(os.Getenv("FORGE_MIGRATIONS_DIR"), "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if _, err := db.Exec(string(body)); err != nil {
			fmt.Fprintf(os.Stderr, "fake tool: %s: %v\n", filepath.Base(f), err)
			return 1
		}
	}
	return 0
}

// A golang-migrate project with a custom ledger (x-migrations-table), fully
// migrated, has no forge schema_migrations table. Seeding it is not refused,
// apply and reset leave the tool's history untouched, and a database that is
// genuinely behind its migrations is still refused.
func TestSeedOfAHookedProjectWithACustomLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("applies migrations to a real postgres through a subprocess; skipped under -short")
	}
	ctx := context.Background()
	shadowURL, cleanShadow, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("pgtest.NewURL: %v", err)
	}
	t.Cleanup(cleanShadow)
	t.Setenv(pgtest.EnvBaseURL, shadowURL)
	t.Setenv(fakeMigrationToolEnv, "1")
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	migDir := filepath.Join(dir, "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledger := `CREATE TABLE app_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`
	widgets := `CREATE TABLE widgets (id TEXT PRIMARY KEY, label TEXT NOT NULL)`
	for name, body := range map[string]string{"1_ledger.up.sql": ledger, "2_widgets.up.sql": widgets} {
		if err := os.WriteFile(filepath.Join(migDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	forgeYAML := fmt.Sprintf("name: p\nmodule_path: example.com/p\ndatabase:\n  migrations:\n    dir: migrations\n    tool: golang-migrate\n    apply: [%q, \"-database\", \"postgres://h/db?x-migrations-table=app_migrations\", \"up\"]\n", self)
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(forgeYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// The target, migrated by the tool: its ledger holds its history, and
	// there is no schema_migrations.
	targetURL, cleanTarget, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("pgtest.NewURL: %v", err)
	}
	t.Cleanup(cleanTarget)
	db, err := sql.Open(pgtest.DriverName, targetURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{ledger, widgets, `INSERT INTO app_migrations VALUES (1, false), (2, false)`} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	if block, err := seedMigrationsPending(ctx, db, dir, migDir); err != nil || block != nil {
		t.Fatalf("a fully migrated hooked database is refused: %+v, %v", block, err)
	}
	tables, err := shadowdb.Tables(dir, migDir)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := seedplan.BuildLivePlanFromTables(ctx, db, tables, migDir, seedplan.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedplan.Apply(ctx, db, plan); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	if _, err := seedplan.Reset(ctx, db, plan); err != nil {
		t.Fatalf("seed reset: %v", err)
	}
	var history string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(version::text || ':' || dirty::text, ',' ORDER BY version) FROM app_migrations`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != "1:false,2:false" {
		t.Errorf("the tool's history after seed apply and reset = %q, want it untouched (1:false,2:false)", history)
	}
	var widgetRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM widgets`).Scan(&widgetRows); err != nil || widgetRows == 0 {
		t.Errorf("seeding planned no widgets rows (%d, %v): the plan checked nothing", widgetRows, err)
	}

	// A migration the database has not run is still pending.
	if _, err := db.ExecContext(ctx, `ALTER TABLE widgets DROP COLUMN label`); err != nil {
		t.Fatal(err)
	}
	block, err := seedMigrationsPending(ctx, db, dir, migDir)
	if err != nil || block == nil || !strings.Contains(block.Reason, "column public.widgets.label") {
		t.Fatalf("a database behind its migrations: block %+v, err %v; want a refusal naming public.widgets.label", block, err)
	}
}
