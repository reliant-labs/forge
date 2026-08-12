package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/database"
	"github.com/reliant-labs/forge/internal/projectstore"
	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/seedplan"
)

// devModeValue is the runtime MODE value that marks an environment as
// development (config.Mode() keys off the CONFIG_FIELD_ROLE_MODE field). The
// dev env's config.k is scaffolded with it, and `forge run` layers it onto the
// dev host env.
const devModeValue = "development"

// newDBSeedCommand builds `forge db seed` — the runtime materializer for
// deterministic, FK-coherent development seed data.
//
// The planner and applier live in pkg/seedplan, the shipped module, so a
// project's TESTS can seed a foreign-key spine by calling a Go function
// (seedplan.SeedGraph) instead of shelling this binary. That is a deliberate
// widening: seeding is a development capability, and a test is development.
//
// What keeps it out of production is not reachability, then, but the two
// things that actually decide it. The migrate path — the one component that
// runs against non-dev databases — has no seed code path at all
// (TestGeneratedMigrateTemplateHasNoSeedPath pins it), and this command
// hard-refuses any non-dev environment with no override flag. A server binary
// seeds nothing because nothing in it calls a seeder, which is the same
// reason it does not send mail.
func newDBSeedCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Materialize deterministic development seed data at runtime",
		Long: `Materialize deterministic, FK-coherent development seed data directly
into a dev database — no seed files are written into your project.

Seeds are introspected from the APPLIED schema (db/migrations, with real
foreign keys), synthesized deterministically per cell, ordered by a
foreign-key topological sort, and INSERTed idempotently (ON CONFLICT DO
NOTHING). apply/reset refuse any non-dev environment; seeding never runs
against staging or production.

Synthesized values satisfy the schema's constraints BY CONSTRUCTION — CHECK
vocabularies, char_length/varchar caps, numeric ranges — and a single-column
UNIQUE column draws without replacement so it never collides. A constraint
that cannot hold the configured row count (a UNIQUE column backed by a short
CHECK vocabulary) caps that table at plan time with a warning, before any
INSERT. apply is one transaction: it seeds everything or nothing, so a failed
run never leaves a half-populated database and is always safe to retry.

Values you supply in db/seeds/vocab.yaml are validated instead, and an invalid
one is skipped with a warning (that column falls back to built-in synthesis).

An entity that reaches one parent by TWO paths (orders.patient_id, and
orders.prescription_id -> prescriptions.patient_id) carries an invariant the
schema implies but does not state. Seeding it independently produces rows that
contradict the rule, so apply REFUSES and prints the declaration to paste:
COMMENT ON CONSTRAINT ... IS 'forge:ref derived-from=<column>' (or
'authoritative', or 'independent'). Load the db/seeding skill for the table.

Examples:
  forge db seed apply                    # seed the dev database
  forge db seed apply --dsn "$DATABASE_URL"
  forge db seed status                   # per-table seeded-row counts
  forge db seed reset                    # wipe seeded tables and re-seed`,
	}
	cmd.AddCommand(newDBSeedApplyCommand())
	cmd.AddCommand(newDBSeedStatusCommand())
	cmd.AddCommand(newDBSeedResetCommand())
	return cmdutil.StrictGroup(cmd)
}

func newDBSeedApplyCommand() *cobra.Command {
	var (
		dsn    string
		env    string
		migDir string
	)
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Materialize seed data into the dev database (dev-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBSeedApply(cmd.Context(), dsn, env, migDir)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	cmd.Flags().StringVar(&env, "env", "dev", "Target environment (must be dev; there is no override)")
	cmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	return cmd
}

func newDBSeedStatusCommand() *cobra.Command {
	var (
		dsn    string
		migDir string
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show per-table seeded-row counts vs the seed model",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBSeedStatus(cmd.Context(), dsn, migDir)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	cmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	return cmd
}

func newDBSeedResetCommand() *cobra.Command {
	var (
		dsn    string
		env    string
		migDir string
	)
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Delete seeded rows (child-first) and re-seed (dev-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBSeedReset(cmd.Context(), dsn, env, migDir)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	cmd.Flags().StringVar(&env, "env", "dev", "Target environment (must be dev; there is no override)")
	cmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	return cmd
}

// seedShadowFor resolves the postgres SERVER that the migrations under migDir
// get applied to so their schema can be introspected. migDir is
// <project>/db/migrations, so the project directory is its grandparent.
//
// This lives in the CLI, not in seedplan, because resolving it means knowing
// how a forge project stores its database coordinates (forge.yaml, .env, the
// per-env KCL config) — see internal/shadowdb. seedplan takes the answer as a
// parameter so it stays a library: it seeds whatever database it is handed
// and has no opinion about where projects keep their config. An empty result
// means "no reachable configured server", and pgtest's embedded postgres is
// used instead.
func seedShadowFor(migDir string) string {
	return shadowdb.Resolve(filepath.Dir(filepath.Dir(migDir)))
}

// seedConfigFromProject maps the project's forge.yaml database.seed block onto
// the seedplan.Config the applier consumes.
func seedConfigFromProject() seedplan.Config {
	store, err := loadProjectStore()
	if err != nil {
		return seedplan.DefaultConfig()
	}
	return seedConfigFromStore(store)
}

// seedConfigFromStore is the pure, testable core of seedConfigFromProject:
// it maps an already-loaded project store onto the seedplan.Config.
func seedConfigFromStore(store *projectstore.Store) seedplan.Config {
	c := seedplan.DefaultConfig()
	s := store.Database().Seed
	if s.Rows > 0 {
		c.Rows = s.Rows
	}
	c.Salt = s.Salt
	if len(s.RowsPerTable) > 0 {
		c.RowsPerTable = s.RowsPerTable
	}
	return c
}

// errNonDevSeedTarget is the structural CLI gate: apply/reset refuse any
// environment forge cannot confirm is dev, and there is deliberately no
// override flag (an override is exactly the conventional hole this gate
// exists to close).
func requireDevSeedTarget(env string) error {
	dev, err := seedTargetIsDev(env)
	if err != nil {
		return err
	}
	if !dev {
		return fmt.Errorf("refusing to seed: environment %q is not dev — seed data is a dev-only feature and there is no override flag.\n"+
			"Seeds never run against staging/production; for a deliberate non-dev demo dataset use psql directly", env)
	}
	return nil
}

// seedTargetIsDev classifies the target environment as dev by reading the
// runtime MODE (the same value config.Mode() reads). It is
// FAIL-CLOSED: any environment it cannot positively confirm as development is
// treated as non-dev.
func seedTargetIsDev(env string) (bool, error) {
	cfgPath, err := findProjectConfigFile()
	if err != nil {
		return false, err
	}
	return seedEnvIsDevIn(filepath.Dir(cfgPath), env), nil
}

// seedEnvIsDevIn is the pure, testable core of the dev classifier. It reads the
// runtime MODE from the per-env KCL config (deploy/kcl/<env>/config.k, where the
// dev env is scaffolded with `environment = "development"`) and returns true
// only for "development"/"dev". FAIL-CLOSED — any environment it cannot
// positively confirm as development (no config source, unset mode,
// production/test/staging) is treated as non-dev.
func seedEnvIsDevIn(projectDir, env string) bool {
	mode, ok := envModeFromKCLConfig(projectDir, env)
	if !ok {
		return false
	}
	return isDevMode(mode)
}

// isDevMode reports whether a runtime MODE string names the development
// environment. Trimmed + case-insensitive; anything else is non-dev.
func isDevMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "development", "dev":
		return true
	}
	return false
}

// kclConfigModeRE extracts the `environment = "value"` MODE assignment from a
// per-env config.k AppConfig instance. KCL uses `=`; the `:` form is accepted
// defensively. Matches the config field NAME the loader/gate assume for the
// MODE role.
var kclConfigModeRE = regexp.MustCompile(`(?m)^\s*environment\s*[:=]\s*"([^"]*)"`)

// envModeFromKCLConfig reads the runtime `environment` MODE value from the
// per-env config source, deploy/kcl/<env>/config.k. Returns (value, true) when
// the file exists and sets `environment`; ("", false) otherwise (absent file,
// or the field unset, inheriting the schema default). Best-effort text read:
// the value is a plain string literal in a forge-scaffolded instance, so a full
// KCL evaluation would be disproportionate for a fail-closed gate.
func envModeFromKCLConfig(projectDir, env string) (string, bool) {
	path := filepath.Join(projectDir, "deploy", "kcl", env, "config.k")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	m := kclConfigModeRE.FindSubmatch(data)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// openSeedDB resolves the DSN and opens a live connection, and (for
// apply/reset) refuses when migrations are pending — seeds apply only against
// a fully-migrated schema.
func openSeedDB(ctx context.Context, dsn, migDir string, checkPending bool) (*sql.DB, error) {
	resolved, err := resolveDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := database.ConnectDB(ctx, resolved)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if checkPending {
		pending, why, perr := seedplan.MigrationsPending(ctx, db, migDir)
		if perr != nil {
			_ = db.Close()
			return nil, perr
		}
		if pending {
			_ = db.Close()
			return nil, fmt.Errorf("refusing to seed: %s. Run `forge db migrate up` first", why)
		}
	}
	return db, nil
}

func runDBSeedApply(ctx context.Context, dsn, env, migDir string) error {
	if err := requireDevSeedTarget(env); err != nil {
		return err
	}
	db, err := openSeedDB(ctx, dsn, migDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	plan, err := seedplan.BuildLivePlan(ctx, db, migDir, seedShadowFor(migDir), seedConfigFromProject())
	if err != nil {
		return err
	}
	printSeedWarnings(plan)
	res, err := seedplan.Apply(ctx, db, plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Seeded %d row(s) across %d table(s):\n", res.Total(), len(res.Tables))
	for _, tr := range res.Tables {
		fmt.Fprintf(os.Stdout, "  %-32s %d\n", tr.Table, tr.Inserted)
	}
	return applyCustomSeedOverlay(ctx, db, migDir)
}

// printSeedWarnings surfaces the plan's warnings: db/seeds/vocab.yaml
// validation problems, and the constraint-satisfaction notes the planner
// records (a row target capped to a UNIQUE column's vocabulary). Warnings
// never fail the seed — an invalid vocab value is skipped and a fully-invalid
// column falls back to built-in synthesis; a MALFORMED vocab file is a hard
// error from BuildLivePlan instead.
func printSeedWarnings(plan *seedplan.Plan) {
	for _, w := range plan.Warnings() {
		fmt.Fprintf(os.Stdout, "warning: %s\n", w)
	}
}

// applyCustomSeedOverlay applies the user-owned db/seeds/custom/*.sql overlay
// (lexicographic order) AFTER the runtime dataset — the sanctioned hook for
// hand-authored / domain-flavored demo data. Missing dir is a no-op.
func applyCustomSeedOverlay(ctx context.Context, db *sql.DB, migDir string) error {
	dir := filepath.Join(filepath.Dir(migDir), "seeds", "custom")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no overlay directory — nothing to apply
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	for _, name := range files {
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			return fmt.Errorf("read custom seed %s: %w", name, rerr)
		}
		if strings.TrimSpace(stripSQLComments(string(raw))) == "" {
			continue // fully-commented example — skip
		}
		if _, eerr := db.ExecContext(ctx, string(raw)); eerr != nil {
			return fmt.Errorf("apply custom seed %s: %w", name, eerr)
		}
		fmt.Fprintf(os.Stdout, "Applied custom overlay %s\n", name)
	}
	return nil
}

// stripSQLComments removes leading -- line comments to detect a
// fully-commented example file (so applying the scaffolded example.sql is a
// clean no-op).
func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "--") {
			continue
		}
		b.WriteString(t)
	}
	return b.String()
}

func runDBSeedStatus(ctx context.Context, dsn, migDir string) error {
	db, err := openSeedDB(ctx, dsn, migDir, false)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	plan, err := seedplan.BuildLivePlan(ctx, db, migDir, seedShadowFor(migDir), seedConfigFromProject())
	if err != nil {
		return err
	}
	printSeedWarnings(plan)
	rows, err := seedplan.Status(ctx, db, plan)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "No seedable tables (no entities in the applied schema).")
		return nil
	}
	fmt.Fprintf(os.Stdout, "%-32s %8s %10s\n", "TABLE", "ROWS", "SEED_TARGET")
	for _, r := range rows {
		fmt.Fprintf(os.Stdout, "%-32s %8d %10d\n", r.Table, r.Count, r.Expected)
	}
	return nil
}

func runDBSeedReset(ctx context.Context, dsn, env, migDir string) error {
	if err := requireDevSeedTarget(env); err != nil {
		return err
	}
	db, err := openSeedDB(ctx, dsn, migDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	plan, err := seedplan.BuildLivePlan(ctx, db, migDir, seedShadowFor(migDir), seedConfigFromProject())
	if err != nil {
		return err
	}
	printSeedWarnings(plan)
	res, err := seedplan.Reset(ctx, db, plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Reset and seeded %d row(s) across %d table(s).\n", res.Total(), len(res.Tables))
	return applyCustomSeedOverlay(ctx, db, migDir)
}
