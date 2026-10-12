package migratekit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
)

// ONE MIGRATION RUN PER DATABASE, WHICHEVER ENTRY POINT STARTED IT.
//
// golang-migrate serializes its own Up with a session advisory lock keyed on
// a hash of (database name, schema, table). The two entry points build its
// driver differently, and golang-migrate names the database differently in
// each: Open goes through the pgx v5 driver's URL constructor, which takes the
// URL PATH ("/app"); AutoMigrate wraps a pool connection, and that constructor
// asks current_database() ("app"). Same database, two lock keys — so
// `db migrate up` (a deploy initContainer, the dev loop's migrate job) never
// excluded AutoMigrate (a server booting with AUTO_MIGRATE) at all. Measured
// with six replicas, half through each entry point, against one database: two
// ran the same migration at once (one saw "Dirty database version"), and one
// lost a race creating schema_migrations itself (duplicate key in
// pg_type_typname_nsp_index). That is every replica of a release starting
// together — exactly the moment a deploy, or a goose cutover, happens.
//
// So migratekit takes its OWN session lock, under one fixed key both entry
// points share (advisory locks are scoped to the current database, so a fixed
// key is already per-database). It is taken before golang-migrate's driver is
// built, because building it creates schema_migrations; and Up and
// AutoMigrate hold it for the whole run, which makes adopt → check → classify
// → record → apply → record one critical section per database rather than a
// row of separately locked steps.
//
// The order is always this lock first, golang-migrate's second, so the two
// cannot deadlock; a binary from before this lock takes only the second.
const runLockKey = 7250513029716893483

// lockRun blocks until conn's session holds the run lock. The caller keeps
// conn for the whole run and must unlockRun it.
func lockRun(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SELECT pg_advisory_lock(%d)", int64(runLockKey))); err != nil {
		return fmt.Errorf("take the migration run lock: %w", err)
	}
	return nil
}

// unlockRun releases the run lock.
//
// A session lock belongs to the server SESSION, not to the *sql.Conn handle,
// and closing the handle hands the session back to the pool still holding
// it — every later run against the database would then wait forever. So a
// connection whose unlock fails is discarded (driver.ErrBadConn) rather than
// returned; closing the session is what releases the lock then.
func unlockRun(ctx context.Context, conn *sql.Conn) {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SELECT pg_advisory_unlock(%d)", int64(runLockKey))); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// withRunLock runs fn holding the run lock on a connection of its own from db.
func withRunLock(ctx context.Context, db *sql.DB, fn func() error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := lockRun(ctx, conn); err != nil {
		return err
	}
	defer unlockRun(ctx, conn)
	return fn()
}
