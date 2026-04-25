package cli

import (
	"fmt"
	"os"
	"os/exec"

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

	return cmd
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

Examples:
  forge db migrate up --dsn=<dsn>
  forge db migrate down --dsn=<dsn>
  forge db migrate status --dsn=<dsn>
  forge db migrate version --dsn=<dsn>
  forge db migrate force 20240102150405 --dsn=<dsn>`,
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
			return runMigrateCommand("up", dsn, migDir)
		},
	}

	upCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string")
	upCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	_ = upCmd.MarkFlagRequired("dsn")

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
			return runMigrateCommand("down", dsn, migDir, "1")
		},
	}

	downCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string")
	downCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	_ = downCmd.MarkFlagRequired("dsn")

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
			return runMigrateStatus(dsn, migDir)
		},
	}

	statusCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string")
	statusCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	_ = statusCmd.MarkFlagRequired("dsn")

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
			return runMigrateVersion(dsn, migDir)
		},
	}

	versionCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string")
	versionCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	_ = versionCmd.MarkFlagRequired("dsn")

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
			return runMigrateCommand("force", dsn, migDir, args[0])
		},
	}

	forceCmd.Flags().StringVar(&dsn, "dsn", "", "Database connection string")
	forceCmd.Flags().StringVar(&migDir, "dir", migrationsDefault(), "Migrations directory")
	_ = forceCmd.MarkFlagRequired("dsn")

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