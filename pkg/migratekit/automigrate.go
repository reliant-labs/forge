package migratekit

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// AutoMigrate is the BOOT-TIME half of the migration story, the counterpart to
// the Migrator the `db migrate` CLI drives.
//
// WHY IT TAKES AN OPEN *sql.DB WHILE Open TAKES A DSN. The two callers have
// genuinely different handles, and converting between them is the failure
// mode this signature avoids. The server has already opened its pool by the
// time it migrates — serverkit.AutoMigrate hands that exact *sql.DB over — so
// asking for a DSN here would open a SECOND connection to the same database
// during boot, one that bypasses the pool tuning already applied. The CLI, by
// contrast, has no pool and only a URL. So the migrator that runs beside a
// live pool binds to the pool, and the one that runs alone opens its own.
//
// It returns a CLOSURE rather than running immediately because
// serverkit.AutoMigrate takes a `func(*sql.DB, *slog.Logger) error` — that
// indirection is what keeps golang-migrate out of serverkit's link graph, so
// a server that never migrates does not pay for the driver. Binding the FS
// here and the database there is what lets the project's call site be a
// single expression naming its embedded migrations.
//
// The dir argument is the path INSIDE fsys holding the .sql files
// ("migrations" for the conventional `//go:embed migrations/*.sql` layout).
//
// A nil fsys SKIPS, and this is the one place migratekit is quieter than
// Open, which refuses it. The two are answering different questions. `db
// migrate up` is an explicit instruction, so a binary that embeds nothing has
// failed to do what was asked. Boot is not: AutoMigrate runs on every startup
// of a project that may simply not have written its first migration yet, and
// failing there would make a brand-new project unable to start. Both are the
// behaviour the binary's user is entitled to expect from the command they ran.
//
// Every OTHER failure mode is LOUD. An unreadable embedded FS, an unreadable
// schema_migrations table, or a schema left dirty by a half-applied migration
// each abort startup rather than serve traffic against a database in an
// unknown state.
func AutoMigrate(fsys fs.FS, dir string) func(*sql.DB, *slog.Logger) error {
	return func(db *sql.DB, logger *slog.Logger) error {
		return autoMigrate(fsys, dir, db, logger)
	}
}

// autoMigrate is AutoMigrate's body, split out so the closure stays a
// one-liner and the logic is directly testable.
func autoMigrate(fsys fs.FS, dir string, db *sql.DB, logger *slog.Logger) error {
	if fsys == nil {
		// See the AutoMigrate doc comment: absent migrations are a normal
		// state at BOOT (the project has not written its first one), unlike
		// at the explicit `db migrate up` that Open refuses.
		logf(logger, "no migrations embedded, skipping auto-migrate")
		return nil
	}
	if dir == "" {
		dir = "."
	}

	// An embedded-FS read error is a build/packaging bug (corrupt embed,
	// renamed dir) — fail. Only a genuinely empty migration set skips: that
	// is the honest state of a project whose first migration is unwritten.
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("reading embedded migrations (%s): %w", dir, err)
	}
	if !hasSQL(entries) {
		logf(logger, "no migration files found, skipping auto-migrate")
		return nil
	}

	sourceDriver, err := iofs.New(fsys, dir)
	if err != nil {
		return fmt.Errorf("creating migration source (%s): %w", dir, err)
	}

	// postgres.WithInstance binds to the pool the caller already opened —
	// see the signature note above.
	dbDriver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("creating migration db driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", sourceDriver, "postgres", dbDriver)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}

	before, err := stateOf(m)
	if err != nil {
		return err
	}
	logf(logger, "migration status", "current_version", before.Version, "dirty", before.Dirty)

	// A dirty schema means a previous migration failed partway. Running more
	// migrations (or serving traffic) against it compounds the damage — stop
	// and make a human look. The CLI's db.go can express the other policy
	// (force and retry) because Force is exported; boot deliberately cannot.
	if before.Dirty {
		return fmt.Errorf("database schema is dirty at version %d — a previous migration failed partway; "+
			"inspect the database, fix the partial migration, then clear the dirty flag "+
			"(e.g. `migrate force %d`) before restarting", before.Version, before.Version)
	}

	if _, err := foldNoChange(m.Up()); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	after, err := stateOf(m)
	if err != nil {
		return fmt.Errorf("after apply: %w", err)
	}
	if after.Version != before.Version {
		logf(logger, "migrations applied", "from_version", before.Version, "to_version", after.Version)
	} else {
		logf(logger, "no pending migrations")
	}
	return nil
}

// hasSQL reports whether the entry set holds at least one .sql file.
//
// It looks for SQL rather than testing len(entries)==0 because the directory
// can legitimately carry non-SQL company — a .gitkeep holding an
// otherwise-empty dir in git is the common one — and treating that as "there
// are migrations" sends an empty set to iofs.New, which fails.
func hasSQL(entries []fs.DirEntry) bool {
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".sql" {
			return true
		}
	}
	return false
}

// stateOf reads a migrator's state, folding ErrNilVersion into Applied=false.
// Shared with the Migrator.State method so the CLI and boot paths cannot
// disagree about what a fresh database looks like.
func stateOf(m *migrate.Migrate) (State, error) {
	return (&Migrator{m: m}).State()
}

// logf tolerates a nil logger so a caller that has not built one yet (or a
// test) can call AutoMigrate without constructing logging ceremony.
func logf(logger *slog.Logger, msg string, args ...any) {
	if logger != nil {
		logger.Info(msg, args...)
	}
}
