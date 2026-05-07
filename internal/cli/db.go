package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/database"
	"github.com/spf13/cobra"
)

const defaultMigrationsDir = "db/migrations"

// migrationsDefault returns the configured migrations directory from
// forge.yaml, falling back to defaultMigrationsDir.
func migrationsDefault() string {
	cfg, err := loadProjectConfig()
	if err != nil {
		return defaultMigrationsDir
	}
	if cfg.Database.MigrationsDir != "" {
		return cfg.Database.MigrationsDir
	}
	return defaultMigrationsDir
}

// resolveDSN returns the explicit --dsn flag value, falling back to the
// DATABASE_URL environment variable. The flag wins so users can override the
// env var ad-hoc. Returns an error if neither is set so the caller can surface
// a helpful message.
func resolveDSN(flagDSN string) (string, error) {
	if flagDSN != "" {
		return flagDSN, nil
	}
	if env := os.Getenv("DATABASE_URL"); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("database connection string required: pass --dsn or set DATABASE_URL")
}

func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Database and migration commands",
		Long: `Manage database migrations and contract-sync workflows.

Forge uses a migration-first database model:
- Checked-in SQL migrations in db/migrations/ are the source of truth
- golang-migrate is the canonical migration runner
- Proto DB entities are generated from, or validated against, the migrated schema

The db command includes migration lifecycle commands today plus placeholder
command surfaces for schema introspection, proto sync, and ORM codegen so teams
can standardize on the intended workflow shape before those implementations land.`,
	}

	cmd.AddCommand(newDBMigrationCommand())
	cmd.AddCommand(newDBMigrateCommand())
	cmd.AddCommand(newDBIntrospectCommand())
	cmd.AddCommand(newDBProtoCommand())
	cmd.AddCommand(newDBCodegenCommand())
	cmd.AddCommand(newDBSquashCommand())

	return cmd
}

// newDBSquashCommand creates the `forge db squash` subcommand. Squash spins
// up an ephemeral Postgres in docker, applies every migration in --from-dir
// against it via `migrate up`, dumps the resulting schema + INSERT-shaped
// seed data via `pg_dump`, strips psql meta commands, and writes a fresh
// `<baseline>.up.sql` / `<baseline>.down.sql` pair. The use case is the
// "I have N golang-migrate files and want one canonical baseline" workflow
// — common when pulling a long-lived schema into a new project, or when
// collapsing a grown migration history into a checkpoint.
//
// The command is intentionally self-contained: it brings up its own
// container so the developer doesn't need a local Postgres; it tears the
// container down on success or failure; and it never touches existing
// migration files in --from-dir. The output filenames are derived from
// --to (default `00001_baseline`) and land alongside (or in --out-dir).
func newDBSquashCommand() *cobra.Command {
	var (
		fromDir  string
		baseline string
		outDir   string
		image    string
		dbName   string
		dbUser   string
		dbPass   string
	)

	cmd := &cobra.Command{
		Use:   "squash",
		Short: "Collapse N migrations into one canonical baseline (.up.sql + .down.sql)",
		Long: `Squash applies every migration in --from-dir against an ephemeral Postgres
container, dumps the resulting schema + seed data with pg_dump, and writes
a single baseline migration pair.

Output:
  <out-dir>/<baseline>.up.sql    (CREATE statements + INSERTs for non-schema_migrations rows)
  <out-dir>/<baseline>.down.sql  (DROP SCHEMA public CASCADE; CREATE SCHEMA public)

This is the canonical "N migrations → one baseline" workflow used when
pulling a long-lived schema into a new project, or when collapsing
historical migrations into a checkpoint. Run from the project root.

Requires:
  - docker (for the ephemeral postgres)
  - migrate (golang-migrate CLI) on PATH
  - pg_dump on PATH (matching the postgres image major version)

Examples:
  forge db squash
  forge db squash --from-dir db/migrations --to 00001_baseline
  forge db squash --to 20260506_baseline --out-dir db/baselines/`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBSquash(squashOptions{
				FromDir:  fromDir,
				Baseline: baseline,
				OutDir:   outDir,
				Image:    image,
				DBName:   dbName,
				DBUser:   dbUser,
				DBPass:   dbPass,
			})
		},
	}

	cmd.Flags().StringVar(&fromDir, "from-dir", migrationsDefault(), "Source directory holding the migrations to squash")
	cmd.Flags().StringVar(&baseline, "to", "00001_baseline", "Baseline filename stem (writes <stem>.up.sql + <stem>.down.sql)")
	cmd.Flags().StringVar(&outDir, "out-dir", "", "Output directory for the baseline files (default: same as --from-dir)")
	cmd.Flags().StringVar(&image, "image", "postgres:16-alpine", "Postgres docker image used for the ephemeral container")
	cmd.Flags().StringVar(&dbName, "db-name", "forge_squash", "Database name created inside the ephemeral container")
	cmd.Flags().StringVar(&dbUser, "db-user", "postgres", "Database user (default container superuser)")
	cmd.Flags().StringVar(&dbPass, "db-pass", "forge_squash", "Database password for the ephemeral container")

	return cmd
}

type squashOptions struct {
	FromDir  string
	Baseline string
	OutDir   string
	Image    string
	DBName   string
	DBUser   string
	DBPass   string
}

// runDBSquash drives the squash flow end-to-end: prereqs, container
// boot, migrate up, pg_dump, strip, write, teardown. The function is
// careful to tear the container down even on error paths so a failed
// squash doesn't leak a hanging container into `docker ps`.
func runDBSquash(opts squashOptions) error {
	if err := requireMigrate(); err != nil {
		return err
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found on PATH (required for `forge db squash` ephemeral postgres)")
	}
	// pg_dump is invoked inside the ephemeral container via `docker exec`
	// rather than from the host. This removes the postgresql-client
	// install requirement and guarantees the dump tool's major version
	// matches the server's, avoiding cross-version dump-format
	// incompatibilities.

	if opts.FromDir == "" {
		return fmt.Errorf("--from-dir is required")
	}
	absFrom, err := filepath.Abs(opts.FromDir)
	if err != nil {
		return fmt.Errorf("resolve --from-dir: %w", err)
	}
	info, err := os.Stat(absFrom)
	if err != nil {
		return fmt.Errorf("--from-dir %q: %w", opts.FromDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--from-dir %q is not a directory", opts.FromDir)
	}

	outDir := opts.OutDir
	if outDir == "" {
		outDir = absFrom
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create --out-dir %q: %w", outDir, err)
	}

	// Container name is timestamped so a previous orphaned squash doesn't
	// collide with a fresh run, and so concurrent squashes in different
	// projects don't step on each other.
	container := fmt.Sprintf("forge-db-squash-%d", time.Now().UnixNano())

	// Allocate a host port via docker's `-p :5432` so multiple squashes
	// on the same machine don't fight for a single port. We discover the
	// allocated port via `docker port` after start.
	fmt.Fprintf(os.Stdout, "Starting ephemeral postgres (%s)...\n", opts.Image)
	runArgs := []string{
		"run", "-d", "--rm",
		"--name", container,
		"-e", "POSTGRES_PASSWORD=" + opts.DBPass,
		"-e", "POSTGRES_USER=" + opts.DBUser,
		"-e", "POSTGRES_DB=" + opts.DBName,
		"-p", "127.0.0.1::5432",
		opts.Image,
	}
	if out, err := exec.Command("docker", runArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker run: %w\n%s", err, out)
	}

	// Best-effort teardown — runs on every exit path so an error during
	// migrate/pg_dump still cleans up the container.
	defer func() {
		stopCmd := exec.Command("docker", "stop", container)
		stopCmd.Stdout = nil
		stopCmd.Stderr = nil
		_ = stopCmd.Run()
	}()

	host, port, err := dockerPort(container, "5432/tcp")
	if err != nil {
		return fmt.Errorf("discover postgres host port: %w", err)
	}

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		opts.DBUser, opts.DBPass, host, port, opts.DBName)

	if err := waitForPostgres(dsn, 60*time.Second); err != nil {
		return fmt.Errorf("postgres did not become ready: %w", err)
	}

	fmt.Fprintln(os.Stdout, "Applying migrations...")
	upCmd := exec.Command("migrate", "-path", absFrom, "-database", dsn, "up")
	upCmd.Stdout = os.Stdout
	upCmd.Stderr = os.Stderr
	if err := upCmd.Run(); err != nil {
		return fmt.Errorf("migrate up failed: %w", err)
	}

	fmt.Fprintln(os.Stdout, "Dumping schema + seed data via pg_dump...")
	// Run pg_dump inside the container via `docker exec` so we reuse
	// the postgres image's bundled pg_dump and don't require the host
	// to have postgresql-client installed at a matching major version.
	dumpCmd := exec.Command("docker", "exec",
		"-e", "PGPASSWORD="+opts.DBPass,
		container,
		"pg_dump",
		"--no-owner",
		"--no-privileges",
		"--inserts",
		"--exclude-table=public.schema_migrations",
		"-U", opts.DBUser,
		"-d", opts.DBName,
	)
	dumpCmd.Stderr = os.Stderr
	dumpRaw, err := dumpCmd.Output()
	if err != nil {
		return fmt.Errorf("pg_dump failed: %w", err)
	}

	cleaned := stripPSQLMeta(string(dumpRaw))

	stem := opts.Baseline
	if stem == "" {
		stem = "00001_baseline"
	}
	upPath := filepath.Join(outDir, stem+".up.sql")
	downPath := filepath.Join(outDir, stem+".down.sql")

	header := fmt.Sprintf("-- Generated by `forge db squash` from %s\n-- Source: %s\n-- pg_dump --no-owner --no-privileges --inserts --exclude-table=public.schema_migrations\n\n",
		time.Now().UTC().Format(time.RFC3339), absFrom)

	if err := os.WriteFile(upPath, []byte(header+cleaned), 0o644); err != nil {
		return fmt.Errorf("write baseline up: %w", err)
	}
	down := header + "DROP SCHEMA IF EXISTS public CASCADE;\nCREATE SCHEMA public;\n"
	if err := os.WriteFile(downPath, []byte(down), 0o644); err != nil {
		return fmt.Errorf("write baseline down: %w", err)
	}

	fmt.Fprintf(os.Stdout, "\n  Wrote %s\n  Wrote %s\n", upPath, downPath)
	fmt.Fprintln(os.Stdout, "\nNext steps:")
	fmt.Fprintln(os.Stdout, "  1. Review the baseline; ensure no app-state INSERTs leaked into seed data.")
	fmt.Fprintln(os.Stdout, "  2. Move existing migrations out of --from-dir into an archive (or keep & adopt the new ID).")
	fmt.Fprintln(os.Stdout, "  3. `migrate force <id>` against any pre-existing databases so they accept the new baseline.")

	return nil
}

// dockerPort returns the host:port that docker mapped for the given
// container's internal port spec (e.g. "5432/tcp"). Output of
// `docker port <name> <spec>` is one line of `0.0.0.0:32768` (or
// `[::]:32768`) per binding. We take the first IPv4 binding since that's
// what `127.0.0.1::5432` gives us.
func dockerPort(container, spec string) (string, string, error) {
	out, err := exec.Command("docker", "port", container, spec).Output()
	if err != nil {
		return "", "", fmt.Errorf("docker port: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		// Skip IPv6 bindings since the DSN must be IPv4-friendly for the
		// migrate / pg_dump CLIs running on the host. Any "0.0.0.0:N" or
		// "127.0.0.1:N" line is fine.
		if strings.HasPrefix(line, "[") {
			continue
		}
		if i := strings.LastIndex(line, ":"); i > 0 {
			host := line[:i]
			port := line[i+1:]
			if host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			return host, port, nil
		}
	}
	return "", "", fmt.Errorf("no IPv4 binding for %s in %q", spec, string(out))
}

// waitForPostgres polls until `pg_isready` (or a fallback connect) reports
// ready, or the timeout fires. The container takes ~1-2s to accept
// connections on first boot; we poll every 500ms and bail with a useful
// error rather than letting `migrate up` hit a connection-refused.
func waitForPostgres(dsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := error(nil)
	for time.Now().Before(deadline) {
		db, err := database.ConnectDB(dsn)
		if err != nil {
			last = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err := db.Ping(); err != nil {
			db.Close()
			last = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		db.Close()
		return nil
	}
	if last != nil {
		return fmt.Errorf("timed out after %s: %w", timeout, last)
	}
	return fmt.Errorf("timed out after %s", timeout)
}

// psqlMetaPattern matches lines that begin with a psql meta-command
// (single-letter directives prefixed with `\`). pg_dump emits these for
// SET role / search_path / client_min_messages housekeeping; they're
// noise inside a checked-in migration and would error if a non-psql
// runner tried to apply the file.
var psqlMetaPattern = regexp.MustCompile(`^\\[a-zA-Z]`)

// stripPSQLMeta removes psql meta-command lines from a pg_dump output.
// Comment-only lines (`--`) and SET/SELECT statements are preserved —
// the goal is just to drop the `\connect`, `\restrict`, `\unrestrict`
// directives that pg_dump emits with the `--inserts` flag in modern
// PG versions.
func stripPSQLMeta(in string) string {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(in))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if psqlMetaPattern.MatchString(line) {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// newDBMigrationCommand creates the migration subcommand.
func newDBMigrationCommand() *cobra.Command {
	var (
		migDir    string
		dsn       string
		protoDir  string
		fromProto bool
	)

	migrationCmd := &cobra.Command{
		Use:   "migration",
		Short: "Create new SQL migration files",
		Long: `Create a new SQL migration pair in db/migrations/.

This scaffolds timestamped .up.sql and .down.sql files using golang-migrate's
filename convention. The .up.sql file includes rich schema context so LLMs can
immediately write the migration SQL.

Context includes:
  - Current schema (parsed from existing migrations, or from DB with --dsn)
  - Proto model definitions (scanned from proto/ or --proto-dir)
  - Previous migration content
  - Migration history
  - Schema diff (proto models vs current schema)

Examples:
  forge db migration new add_users_table
  forge db migration new add_preferences --dsn "$DATABASE_URL"
  forge db migration new add_preferences --proto-dir proto/db/v1/
  forge db migration new add_preferences --from-proto
  forge db migration new "backfill account status" --dir db/migrations`,
	}

	newCmd := &cobra.Command{
		Use:   "new [name]",
		Short: "Create a new migration pair with schema context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := &database.MigrationOptions{
				DSN:       dsn,
				ProtoDir:  protoDir,
				FromProto: fromProto,
			}
			return database.CreateMigration(args[0], migDir, opts)
		},
	}
	newCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	newCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string for live schema introspection")
	newCmd.Flags().StringVar(&protoDir, "proto-dir", "", "Directory to scan for proto files (default: proto/)")
	newCmd.Flags().BoolVar(&fromProto, "from-proto", false, "Auto-generate CREATE TABLE SQL from proto message definitions")
	migrationCmd.AddCommand(newCmd)

	return migrationCmd
}

// newDBMigrateCommand creates the migrate subcommand.
func newDBMigrateCommand() *cobra.Command {
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run migration lifecycle commands with golang-migrate",
		Long: `Apply, inspect, and repair migration state using golang-migrate.

Migrations are stored in db/migrations/ by default.
Install golang-migrate from https://github.com/golang-migrate/migrate/tree/master/cmd/migrate.

The connection string can be provided via --dsn or, if the flag is omitted,
the DATABASE_URL environment variable.

Examples:
  forge db migrate up --dsn=<dsn>
  forge db migrate up                   # picks up $DATABASE_URL
  DATABASE_URL=... forge db migrate status
  forge db migrate down --dsn=<dsn>
  forge db migrate version
  forge db migrate force 20240102150405`,
	}

	migrateCmd.AddCommand(newDBMigrateUpCommand())
	migrateCmd.AddCommand(newDBMigrateDownCommand())
	migrateCmd.AddCommand(newDBMigrateStatusCommand())
	migrateCmd.AddCommand(newDBMigrateVersionCommand())
	migrateCmd.AddCommand(newDBMigrateForceCommand())

	return migrateCmd
}

func newDBMigrateUpCommand() *cobra.Command {
	var (
		dsn    string
		migDir string
	)

	upCmd := &cobra.Command{
		Use:   "up",
		Short: "Apply pending migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			return runMigrateCommand("up", resolved, migDir)
		},
	}

	upCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	upCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")

	return upCmd
}

func newDBMigrateDownCommand() *cobra.Command {
	var (
		dsn    string
		migDir string
	)

	downCmd := &cobra.Command{
		Use:   "down",
		Short: "Rollback the most recent migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			return runMigrateCommand("down", resolved, migDir, "1")
		},
	}

	downCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	downCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")

	return downCmd
}

func newDBMigrateStatusCommand() *cobra.Command {
	var (
		dsn    string
		migDir string
	)

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show migration status",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			return runMigrateStatus(resolved, migDir)
		},
	}

	statusCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	statusCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")

	return statusCmd
}

func newDBMigrateVersionCommand() *cobra.Command {
	var (
		dsn    string
		migDir string
	)

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Show the current migration version",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			return runMigrateVersion(resolved, migDir)
		},
	}

	versionCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	versionCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")

	return versionCmd
}

func newDBMigrateForceCommand() *cobra.Command {
	var (
		dsn    string
		migDir string
	)

	forceCmd := &cobra.Command{
		Use:   "force [version]",
		Short: "Force the migration version without running SQL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			return runMigrateCommand("force", resolved, migDir, args[0])
		},
	}

	forceCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string (falls back to $DATABASE_URL)")
	forceCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")

	return forceCmd
}

func newDBIntrospectCommand() *cobra.Command {
	var (
		dsn    string
		table  string
		format string
	)

	cmd := &cobra.Command{
		Use:   "introspect",
		Short: "Inspect the migrated database schema",
		Long: `Connect to a PostgreSQL database and display the current schema.

Shows tables, columns, types, constraints, indexes, and foreign keys.

Examples:
  forge db introspect --dsn "postgres://user:pass@localhost/mydb?sslmode=disable"
  forge db introspect --dsn "$DATABASE_URL" --table users
  forge db introspect --dsn "$DATABASE_URL" --format json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBIntrospect(dsn, table, format)
		},
	}

	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string (required)")
	cmd.Flags().StringVar(&table, "table", "", "Filter to a specific table")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text, json")
	_ = cmd.MarkFlagRequired("dsn")

	return cmd
}

func newDBProtoCommand() *cobra.Command {
	protoCmd := &cobra.Command{
		Use:   "proto",
		Short: "Sync or validate proto DB contracts",
		Long: `Sync or validate proto DB entity contracts against the migrated schema.

Examples:
  forge db proto sync-from-db --dsn "$DATABASE_URL"
  forge db proto sync-from-db --dsn "$DATABASE_URL" --table users --out proto/db/v1/
  forge db proto check --dsn "$DATABASE_URL"`,
	}

	// sync-from-db subcommand
	var (
		syncDSN   string
		syncOut   string
		syncTable string
	)
	syncCmd := &cobra.Command{
		Use:   "sync-from-db",
		Short: "Generate or update proto DB entities from the migrated schema",
		Long: `Connect to the database, introspect the schema, and generate proto message
definitions with entity_options and field_options annotations.

One .proto file is generated per table in the output directory.

Examples:
  forge db proto sync-from-db --dsn "postgres://user:pass@localhost/mydb?sslmode=disable"
  forge db proto sync-from-db --dsn "$DATABASE_URL" --table users
  forge db proto sync-from-db --dsn "$DATABASE_URL" --out proto/db/v1/`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBProtoSync(syncDSN, syncOut, syncTable)
		},
	}
	syncCmd.Flags().StringVar(&syncDSN, "dsn", "", "PostgreSQL connection string (required)")
	syncCmd.Flags().StringVar(&syncOut, "out", "proto/db/v1/", "Output directory for proto files")
	syncCmd.Flags().StringVar(&syncTable, "table", "", "Sync a specific table only")
	_ = syncCmd.MarkFlagRequired("dsn")
	protoCmd.AddCommand(syncCmd)

	// check subcommand
	var (
		checkDSN      string
		checkProtoDir string
	)
	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Validate proto DB entities against the migrated schema",
		Long: `Compare the live database schema against proto entity definitions and report
any drift: missing tables, missing columns, type mismatches, or constraint
differences.

Examples:
  forge db proto check --dsn "postgres://user:pass@localhost/mydb?sslmode=disable"
  forge db proto check --dsn "$DATABASE_URL" --proto-dir proto/db/`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBProtoCheck(checkDSN, checkProtoDir)
		},
	}
	checkCmd.Flags().StringVar(&checkDSN, "dsn", "", "PostgreSQL connection string (required)")
	checkCmd.Flags().StringVar(&checkProtoDir, "proto-dir", "proto/db/", "Directory containing proto DB entity files")
	_ = checkCmd.MarkFlagRequired("dsn")
	protoCmd.AddCommand(checkCmd)

	return protoCmd
}

func newDBCodegenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "codegen",
		Short: "Generate ORM code from proto DB entities",
		Long: `Run buf generate with protoc-gen-forge for proto/db/ entity protos.

This is equivalent to the ORM generation step in 'forge generate' but can be
run independently when only DB code needs regeneration.

Examples:
  forge db codegen`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOrmGenerate(".")
		},
	}
}

func runDBIntrospect(dsn, tableFilter, format string) error {
	db, err := database.ConnectDB(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	tables, err := database.IntrospectSchema(db, tableFilter)
	if err != nil {
		return fmt.Errorf("introspecting schema: %w", err)
	}

	if len(tables) == 0 {
		if tableFilter != "" {
			return fmt.Errorf("table %q not found", tableFilter)
		}
		fmt.Println("No tables found in the public schema.")
		return nil
	}

	switch format {
	case "json":
		out, err := database.FormatSchemaJSON(tables)
		if err != nil {
			return err
		}
		fmt.Println(out)
	default:
		fmt.Print(database.FormatSchemaText(tables))
	}

	return nil
}

func runDBProtoSync(dsn, outputDir, tableFilter string) error {
	db, err := database.ConnectDB(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	tables, err := database.IntrospectSchema(db, tableFilter)
	if err != nil {
		return fmt.Errorf("introspecting schema: %w", err)
	}

	if len(tables) == 0 {
		if tableFilter != "" {
			return fmt.Errorf("table %q not found", tableFilter)
		}
		fmt.Println("No tables found in the public schema.")
		return nil
	}

	// Determine Go module path for go_package option.
	goModule, err := codegen.GetModulePath(".")
	if err != nil {
		return fmt.Errorf("reading go.mod for module path: %w", err)
	}

	if err := database.GenerateProtoFiles(tables, outputDir, goModule); err != nil {
		return fmt.Errorf("generating proto files: %w", err)
	}

	for _, t := range tables {
		fmt.Printf("  ✅ %s/%s.proto\n", outputDir, t.Name)
	}
	fmt.Printf("\nGenerated %d proto file(s) in %s\n", len(tables), outputDir)

	return nil
}

func runDBProtoCheck(dsn, protoDir string) error {
	db, err := database.ConnectDB(dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	tables, err := database.IntrospectSchema(db, "")
	if err != nil {
		return fmt.Errorf("introspecting schema: %w", err)
	}

	result, err := database.CompareSchemaToProtos(tables, protoDir)
	if err != nil {
		return fmt.Errorf("comparing schema to protos: %w", err)
	}

	fmt.Print(result.FormatText())

	if !result.IsClean() {
		return fmt.Errorf("schema drift detected: %d difference(s)", len(result.Diffs))
	}

	return nil
}

func requireMigrate() error {
	if _, err := exec.LookPath("migrate"); err != nil {
		return fmt.Errorf("golang-migrate CLI not found. Install it from https://github.com/golang-migrate/migrate/tree/master/cmd/migrate")
	}
	return nil
}

func runMigrateCommand(action, dsn, migDir string, extraArgs ...string) error {
	if err := requireMigrate(); err != nil {
		return err
	}

	args := []string{"-path", migDir, "-database", dsn}
	args = append(args, action)
	args = append(args, extraArgs...)

	cmd := exec.Command("migrate", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("migrate %s failed: %w", action, err)
	}

	return nil
}

func runMigrateVersion(dsn, migDir string) error {
	if err := requireMigrate(); err != nil {
		return err
	}

	cmd := exec.Command("migrate", "-path", migDir, "-database", dsn, "version")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("migrate version failed: %w", err)
	}

	return nil
}

func runMigrateStatus(dsn, migDir string) error {
	if err := requireMigrate(); err != nil {
		return err
	}

	if err := runMigrateVersion(dsn, migDir); err != nil {
		fmt.Println("Migration version check returned an error; this can happen when no migrations have been applied yet.")
		return err
	}

	return nil
}