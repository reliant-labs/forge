package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/pgtest"
	"github.com/reliant-labs/forge/pkg/seedplan"
)

// fakeMigrationToolEnv makes this test binary act as a project's migration
// tool: the hook execs it, and TestMain hands control to fakeMigrationTool.
const fakeMigrationToolEnv = "FORGE_CLI_FAKE_MIGRATION_TOOL"

// fakeMigrationTool applies every <version>_*.up.sql in
// $FORGE_MIGRATIONS_DIR above the version its ledger records, in version
// order, to $DATABASE_URL, the way golang-migrate does: the ledger is the
// table an `x-migrations-table=` argument names (else schema_migrations), one
// row holding the current version, marked dirty while that version runs.
func fakeMigrationTool() int {
	ledger := "schema_migrations"
	for _, a := range os.Args[1:] {
		if _, v, ok := strings.Cut(a, "x-migrations-table="); ok {
			ledger, _, _ = strings.Cut(v, "&")
		}
	}
	db, err := sql.Open(pgtest.DriverName, os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer func() { _ = db.Close() }()
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "fake tool:", err)
		return 1
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + ledger + ` (version bigint PRIMARY KEY, dirty boolean NOT NULL)`); err != nil {
		return fail(err)
	}
	var current int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM ` + ledger).Scan(&current); err != nil {
		return fail(err)
	}
	setVersion := func(v int64, dirty bool) error {
		_, err := db.Exec(`TRUNCATE ` + ledger + `; INSERT INTO ` + ledger + ` VALUES (` + strconv.FormatInt(v, 10) + `, ` + strconv.FormatBool(dirty) + `)`)
		return err
	}
	files, _ := filepath.Glob(filepath.Join(os.Getenv("FORGE_MIGRATIONS_DIR"), "*.up.sql"))
	versionOf := func(f string) int64 {
		n, _ := strconv.ParseInt(strings.SplitN(filepath.Base(f), "_", 2)[0], 10, 64)
		return n
	}
	sort.Slice(files, func(i, j int) bool { return versionOf(files[i]) < versionOf(files[j]) })
	for _, f := range files {
		v := versionOf(f)
		if v <= current {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			return fail(err)
		}
		if err := setVersion(v, true); err != nil {
			return fail(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			return fail(fmt.Errorf("%s: %w", filepath.Base(f), err))
		}
		if err := setVersion(v, false); err != nil {
			return fail(err)
		}
	}
	return 0
}

// hookedSeedProject writes a project whose migrations dir holds files, with
// migrations as the body of forge.yaml's `database.migrations:` block, and
// returns the project and migrations directories. The shadow database the
// tool migrates is a fresh pgtest server's.
func hookedSeedProject(t *testing.T, migrations string, files map[string]string) (dir, migDir string) {
	t.Helper()
	shadowURL, cleanShadow, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("pgtest.NewURL: %v", err)
	}
	t.Cleanup(cleanShadow)
	t.Setenv(pgtest.EnvBaseURL, shadowURL)
	t.Setenv(fakeMigrationToolEnv, "1")

	dir = t.TempDir()
	migDir = filepath.Join(dir, "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(migDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	forgeYAML := "name: p\nmodule_path: example.com/p\ndatabase:\n  migrations:\n    dir: migrations\n" + migrations
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(forgeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, migDir
}

// seedTarget is a fresh database with stmts applied, and its URL.
func seedTarget(t *testing.T, stmts ...string) (*sql.DB, string) {
	t.Helper()
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
	for _, stmt := range stmts {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	return db, targetURL
}

func testExecutable(t *testing.T) string {
	t.Helper()
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// A golang-migrate project with a custom ledger (x-migrations-table), fully
// migrated, has no forge schema_migrations table. Seeding it is not refused,
// apply and reset leave the tool's history untouched, and a database that is
// behind its migrations is still refused: by the tool's ledger when the schema
// matches (a pending data-only migration, a migration that failed after its
// DDL), and by the schema when a table or column is missing.
func TestSeedOfAHookedProjectWithACustomLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("applies migrations to a real postgres through a subprocess; skipped under -short")
	}
	ctx := context.Background()
	widgets := `CREATE TABLE widgets (id TEXT PRIMARY KEY, label TEXT NOT NULL)`
	dir, migDir := hookedSeedProject(t,
		fmt.Sprintf("    tool: golang-migrate\n    apply: [%q, \"-database\", \"postgres://h/db?x-migrations-table=app_migrations\", \"up\"]\n", testExecutable(t)),
		map[string]string{"1_widgets.up.sql": widgets})

	// The target, migrated by the tool: its ledger holds its history, and
	// there is no schema_migrations.
	db, dsn := seedTarget(t, widgets,
		`CREATE TABLE app_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`,
		`INSERT INTO app_migrations VALUES (1, false)`)

	if block, err := seedMigrationsPending(ctx, db, dsn, dir, migDir); err != nil || block != nil {
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
	if history != "1:false" {
		t.Errorf("the tool's history after seed apply and reset = %q, want it untouched (1:false)", history)
	}
	var widgetRows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM widgets`).Scan(&widgetRows); err != nil || widgetRows == 0 {
		t.Errorf("seeding planned no widgets rows (%d, %v): the plan checked nothing", widgetRows, err)
	}

	// A pending data-only migration leaves the schema exactly as it was:
	// the ledger is what says the database is behind.
	if err := os.WriteFile(filepath.Join(migDir, "2_backfill.up.sql"), []byte(`UPDATE widgets SET label = upper(label)`), 0o644); err != nil {
		t.Fatal(err)
	}
	block, err := seedMigrationsPending(ctx, db, dsn, dir, migDir)
	if err != nil || block == nil || !strings.Contains(block.Reason, "app_migrations has not recorded migration 2") {
		t.Fatalf("a database behind a data-only migration: block %+v, err %v; want a refusal naming migration 2", block, err)
	}

	// A migration that failed part-way (golang-migrate's dirty flag) is
	// refused even though its DDL may already be in place.
	if _, err := db.ExecContext(ctx, `UPDATE app_migrations SET version = 2, dirty = true`); err != nil {
		t.Fatal(err)
	}
	block, err = seedMigrationsPending(ctx, db, dsn, dir, migDir)
	if err == nil || block != nil || !strings.Contains(err.Error(), "migration 2 as failed part-way") {
		t.Fatalf("a dirty database: block %+v, err %v; want a refusal naming the failed migration 2", block, err)
	}

	// Caught up again, it is seedable.
	if _, err := db.ExecContext(ctx, `UPDATE app_migrations SET dirty = false`); err != nil {
		t.Fatal(err)
	}
	if block, err := seedMigrationsPending(ctx, db, dsn, dir, migDir); err != nil || block != nil {
		t.Fatalf("a caught-up hooked database is refused: %+v, %v", block, err)
	}

	// A ledger that claims every migration over a schema missing a column is
	// still refused, by the schema.
	if _, err := db.ExecContext(ctx, `ALTER TABLE widgets DROP COLUMN label`); err != nil {
		t.Fatal(err)
	}
	block, err = seedMigrationsPending(ctx, db, dsn, dir, migDir)
	if err != nil || block == nil || !strings.Contains(block.Reason, "column public.widgets.label") {
		t.Fatalf("a database behind its migrations: block %+v, err %v; want a refusal naming public.widgets.label", block, err)
	}
}

// A custom tool has no ledger forge can read, so seeding asks its declared
// `check:` argv, and refuses outright when it declares none.
func TestSeedOfAHookedProjectWithACustomTool(t *testing.T) {
	if testing.Short() {
		t.Skip("applies migrations to a real postgres through a subprocess; skipped under -short")
	}
	ctx := context.Background()
	widgets := `CREATE TABLE widgets (id TEXT PRIMARY KEY, label TEXT NOT NULL)`
	apply := fmt.Sprintf("    tool: custom\n    apply: [%q, \"up\"]\n", testExecutable(t))
	files := map[string]string{"1_widgets.up.sql": widgets}

	dir, migDir := hookedSeedProject(t, apply, files)
	db, dsn := seedTarget(t, widgets)
	_, err := seedMigrationsPending(ctx, db, dsn, dir, migDir)
	if err == nil || !strings.Contains(err.Error(), "database.migrations.check") {
		t.Fatalf("a custom tool without check: err %v; want a refusal asking for database.migrations.check", err)
	}

	dir, migDir = hookedSeedProject(t, apply+"    check: [sh, -c, \"exit 0\"]\n", files)
	if block, err := seedMigrationsPending(ctx, db, dsn, dir, migDir); err != nil || block != nil {
		t.Fatalf("a custom tool whose check passes is refused: %+v, %v", block, err)
	}

	dir, migDir = hookedSeedProject(t, apply+"    check: [sh, -c, \"exit 3\"]\n", files)
	block, err := seedMigrationsPending(ctx, db, dsn, dir, migDir)
	if err != nil || block == nil || !strings.Contains(block.Reason, "not fully migrated") {
		t.Fatalf("a custom tool whose check fails: block %+v, err %v; want a refusal", block, err)
	}
}
