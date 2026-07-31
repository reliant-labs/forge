// Package migratekit is the invariant half of a forge project's `db migrate`
// command tree: opening a migrator over an embedded migration set, closing
// it, and reporting what the schema is doing.
//
// WHAT IS DELIBERATELY NOT HERE: policy. Whether a dirty schema aborts the
// run or is force-cleared, whether "nothing to apply" exits 0 or non-zero,
// whether `down` is allowed to run in production at all — those differ per
// deployment, so they stay in the project's own cmd/<bin>/cmd/db.go where an
// operator can read and change them. This package only reports FACTS
// (Up.Changed, State.Applied, State.Dirty) and never decides what a fact
// means. That split is the whole design: a helper that returned an error for
// a dirty schema would have made "auto-force in staging" unexpressible
// without dropping the library.
//
// WHY THIS IS NOT IN serverkit. serverkit is imported by every forge server,
// and it is deliberately free of any compile-time golang-migrate dependency
// — serverkit.AutoMigrate takes a `func(*sql.DB, *slog.Logger) error` for
// exactly that reason. `db migrate` is a CLI concern, not a serving concern,
// so putting the migrator here keeps golang-migrate out of the link graph of
// every server that never runs a migration.
//
// THE MIGRATION SOURCE IS AN fs.FS, NEVER A DIRECTORY PATH. A production
// image carries the binary and nothing else — the runtime stage copies
// /app/<project> and no db/ tree — so a migrator reading `file://db/migrations`
// can only ever fail there, and a deploy-time migration step that is
// guaranteed to fail is worse than no step at all. Taking an fs.FS means the
// caller hands over its `//go:embed migrations/*.sql` FS and the same command
// works in a checkout and inside the image.
package migratekit

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	// Registers the "pgx5" database driver under the scheme NormalizeDSN
	// rewrites DSNs to. The rewrite and the registration are the same fact
	// stated twice, so they live in the same package: a caller that got the
	// rewrite from here and the registration from somewhere else would
	// produce a DSN with no driver behind it.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Schemes the DSN normalizer understands. Exported so a caller (or a test)
// can assert over the same set the rewrite is driven by, rather than
// restating the string literals.
const (
	// SchemePostgres is the standard postgres:// URL scheme.
	SchemePostgres = "postgres://"
	// SchemePostgresql is the equally standard postgresql:// spelling.
	SchemePostgresql = "postgresql://"
	// SchemePGX5 is what golang-migrate's pgx v5 database driver
	// registers itself as, and therefore the only scheme it will answer
	// to. A DATABASE_URL that a human or a cloud provider wrote uses one
	// of the two above, never this one.
	SchemePGX5 = "pgx5://"
)

// NormalizeDSN rewrites a standard postgres:// or postgresql:// URL to the
// pgx5:// scheme golang-migrate's pgx v5 driver registers under.
//
// It exists because the SAME DSN has to work for two different consumers:
// the application's own pool (database/sql + the pgx stdlib driver, which
// wants postgres://) and the migrator (which resolves its database driver by
// URL scheme). Rather than ask an operator to set a second DATABASE_URL in a
// scheme they have never seen, the one they wrote is translated here.
//
// A DSN in any other scheme is returned unchanged: this is a translation for
// a known pair, not a validator. An unusable DSN produces a loud driver
// error at Open, which is a better report than a silent rewrite.
func NormalizeDSN(dsn string) string {
	for _, prefix := range []string{SchemePostgresql, SchemePostgres} {
		if len(dsn) >= len(prefix) && dsn[:len(prefix)] == prefix {
			return SchemePGX5 + dsn[len(prefix):]
		}
	}
	return dsn
}

// Options configures Open.
type Options struct {
	// FS holds the migration files — typically the project's embedded
	// `forgedb.MigrationsFS`. A nil FS is a hard error: it means the
	// binary embeds no migrations, and a migrate command that exits 0
	// against an unmigrated database is precisely the silence this
	// package exists to remove.
	FS fs.FS
	// Dir is the directory INSIDE FS holding the .sql files — "migrations"
	// for the conventional `//go:embed migrations/*.sql` layout. Empty
	// means the FS root.
	Dir string
	// DSN is the database URL. Passed through NormalizeDSN, so callers
	// hand over the DATABASE_URL they already have.
	DSN string
}

// Migrator is an open migration session. Always Close it.
type Migrator struct {
	m *migrate.Migrate
}

// Open builds a Migrator over the embedded migration set in opts.FS.
//
// Both failure modes are LOUD and distinguishable, because they call for
// different fixes: a nil FS means the binary shipped without migrations
// (regenerate), while an unusable DSN means the database is unreachable or
// misaddressed (fix the environment).
func Open(opts Options) (*Migrator, error) {
	if opts.FS == nil {
		return nil, fmt.Errorf("migratekit: no migration source: this binary embeds no migrations")
	}
	if opts.DSN == "" {
		return nil, fmt.Errorf("migratekit: DSN is required")
	}
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}

	source, err := iofs.New(opts.FS, dir)
	if err != nil {
		return nil, fmt.Errorf("open embedded migrations (%s): %w", dir, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, NormalizeDSN(opts.DSN))
	if err != nil {
		// The source instance owns an open handle even though the
		// migrator never took ownership of it, so close it here or a
		// failed Open leaks it.
		_ = source.Close()
		return nil, fmt.Errorf("open migrator: %w", err)
	}
	return &Migrator{m: m}, nil
}

// Close releases the migration source and the database connection.
//
// golang-migrate reports the two independently and either can fail; both are
// joined into one error so a caller cannot report one and silently drop the
// other. Safe on a zero Migrator.
func (mg *Migrator) Close() error {
	if mg == nil || mg.m == nil {
		return nil
	}
	srcErr, dbErr := mg.m.Close()
	return errors.Join(srcErr, dbErr)
}

// State is what the schema currently reports.
type State struct {
	// Version is the migration version recorded in the schema. Only
	// meaningful when Applied is true.
	Version uint
	// Dirty means a previous migration failed PARTWAY: the version was
	// recorded but its SQL did not finish. migratekit reports it and
	// stops there — whether that aborts the run or is force-cleared is
	// the caller's policy.
	Dirty bool
	// Applied reports whether any migration has ever been applied. False
	// is a normal, healthy state for a fresh database, which is why it is
	// a field rather than an error: golang-migrate signals it as
	// ErrNilVersion, and a caller that forgot to special-case that
	// sentinel would treat an empty database as broken.
	Applied bool
}

// State reads the current schema state, folding golang-migrate's
// ErrNilVersion sentinel into State.Applied=false. A returned error
// therefore always means the version table is genuinely unreadable.
func (mg *Migrator) State() (State, error) {
	version, dirty, err := mg.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read migration version: %w", err)
	}
	return State{Version: version, Dirty: dirty, Applied: true}, nil
}

// Result describes what a migration call actually did.
type Result struct {
	// Changed reports whether THIS call applied anything.
	//
	// It is taken from the return of the migrate call itself, not from a
	// before/after version comparison, because the advisory lock is taken
	// INSIDE that call: when several replicas race the same release, one
	// applies and the others are released to find nothing to do. Only the
	// call's own return distinguishes "I applied it" from "someone else
	// did"; a version diff would credit every replica.
	Changed bool
	// Before is the state as of entry, After the state on exit. Both are
	// reported so a caller can log a "N -> M" line without a second round
	// trip, and so "no change" still carries the version it is at.
	Before State
	After  State
}

// Up applies every pending migration.
//
// golang-migrate's ErrNoChange is folded into Result.Changed=false rather
// than surfaced as an error: on a concurrent rollout it is the EXPECTED
// outcome for every replica that lost the advisory-lock race, so treating it
// as failure would fail most of a healthy deploy. A caller that genuinely
// wants "fail if nothing was pending" has Changed to test.
func (mg *Migrator) Up() (Result, error) { return mg.apply(mg.m.Up) }

// Steps moves n migrations — positive up, negative down. Same ErrNoChange
// folding as Up.
func (mg *Migrator) Steps(n int) (Result, error) {
	return mg.apply(func() error { return mg.m.Steps(n) })
}

// Force sets the recorded version and CLEARS the dirty flag without running
// any SQL.
//
// It is here so that "auto-force and retry" is expressible as a deployment
// policy in the caller's own db.go — the only alternative being that the
// project imports golang-migrate directly and this package stops being the
// boundary. It does not make forcing safe: the schema is whatever the failed
// migration left behind, and asserting a version does not change that.
func (mg *Migrator) Force(version int) error {
	if err := mg.m.Force(version); err != nil {
		return fmt.Errorf("force migration version %d: %w", version, err)
	}
	return nil
}

// apply is the shared ceremony behind Up and Steps: read state, run, fold
// ErrNoChange, read state again.
func (mg *Migrator) apply(run func() error) (Result, error) {
	before, err := mg.State()
	if err != nil {
		return Result{}, err
	}
	changed, runErr := foldNoChange(run())
	if runErr != nil {
		return Result{}, runErr
	}
	after, err := mg.State()
	if err != nil {
		return Result{}, fmt.Errorf("after apply: %w", err)
	}
	return Result{Changed: changed, Before: before, After: after}, nil
}

// foldNoChange classifies the return of a migrate call into "did it change
// anything" plus a genuine failure.
//
// Split out as a pure function because it is the one piece of apply that
// carries a decision, and it is the piece worth testing without a database:
// the fold must be NARROW. It absorbs exactly migrate.ErrNoChange — the
// expected outcome for every replica that loses an advisory-lock race during
// a concurrent rollout — and passes everything else through, because a
// caller's policy of aborting the rollout depends on seeing a real failure.
// A fold written one character wider (any error means "no change") would turn
// every broken migration into a successful deploy.
func foldNoChange(err error) (changed bool, fatal error) {
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, migrate.ErrNoChange):
		return false, nil
	default:
		return false, err
	}
}
