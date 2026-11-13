package dbanalyze

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// Default per-statement limits of a session.
const (
	DefaultStatementTimeout = 60 * time.Second
	DefaultLockTimeout      = 5 * time.Second
)

// SQLSTATE codes a session classifies.
const (
	sqlStateQueryCanceled    = "57014" // statement_timeout
	sqlStateLockNotAvailable = "55P03" // lock_timeout
)

// SessionOptions bound every statement a session runs.
type SessionOptions struct {
	// StatementTimeout is SET LOCAL statement_timeout. Zero means
	// DefaultStatementTimeout; negative means none.
	StatementTimeout time.Duration
	// LockTimeout is SET LOCAL lock_timeout. Zero means
	// DefaultLockTimeout; negative means none.
	LockTimeout time.Duration
}

// Session is ONE connection to the target holding ONE transaction that is
// always rolled back. Everything an analysis does — applying migrations,
// measuring them, explaining queries — runs inside it, so the target is never
// changed: Postgres DDL is transactional, and Close rolls it all back.
//
// A Session is not safe for concurrent use.
type Session struct {
	db   *sql.DB
	conn *sql.Conn
	tx   *sql.Tx
	sp   int
}

// Open connects to dsn, begins the transaction and sets its timeouts.
func Open(ctx context.Context, dsn string, opts SessionOptions) (*Session, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open target: %w", err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to target: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, fmt.Errorf("begin: %w", err)
	}
	s := &Session{db: db, conn: conn, tx: tx}
	settings := []string{
		"SET LOCAL statement_timeout = " + timeoutSetting(opts.StatementTimeout, DefaultStatementTimeout),
		"SET LOCAL lock_timeout = " + timeoutSetting(opts.LockTimeout, DefaultLockTimeout),
		"SET LOCAL application_name = 'forge db analyze'",
	}
	for _, q := range settings {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	return s, nil
}

// timeoutSetting renders a duration as a postgres timeout literal: "0"
// (none) for a negative one, def for zero.
func timeoutSetting(d, def time.Duration) string {
	switch {
	case d < 0:
		return "0"
	case d == 0:
		d = def
	}
	return fmt.Sprintf("'%dms'", d.Milliseconds())
}

// Tx is the session's transaction, for a caller's own reads (EXPLAIN, the
// catalog). A statement that may fail belongs in Exec, which keeps the
// transaction usable after it does; an error in Tx directly aborts it.
func (s *Session) Tx() *sql.Tx { return s.tx }

// Exec runs one statement inside a savepoint: a failure rolls back to it,
// so the transaction stays usable for what follows. The error is the
// statement's own.
func (s *Session) Exec(ctx context.Context, query string) error {
	s.sp++
	name := fmt.Sprintf("forge_analyze_%d", s.sp)
	if _, err := s.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if _, err := s.tx.ExecContext(ctx, query); err != nil {
		// A rollback that fails leaves an aborted transaction: every later
		// call reports it, which is the honest outcome.
		_, _ = s.tx.ExecContext(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT "+name)
		return err
	}
	if _, err := s.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// Close rolls the transaction back and disconnects. It is safe to call
// more than once.
func (s *Session) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	var errs []error
	if err := s.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		errs = append(errs, fmt.Errorf("rollback: %w", err))
	}
	if err := s.conn.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := s.db.Close(); err != nil {
		errs = append(errs, err)
	}
	s.db = nil
	return errors.Join(errs...)
}

// Info reads the server's version and whether the target's statistics can
// size tables. kind and identity are the caller's and are copied in.
//
// Statistics count only on a dsn target: a scratch target's tables are
// empty whatever pg_class says about them.
func (s *Session) Info(ctx context.Context, kind TargetKind, identity string) (TargetInfo, error) {
	info := TargetInfo{Kind: kind, Identity: identity}
	if err := s.tx.QueryRowContext(ctx,
		"SELECT current_setting('server_version'), current_setting('server_version_num')::int").
		Scan(&info.ServerVersion, &info.ServerVersionNum); err != nil {
		return info, fmt.Errorf("read server version: %w", err)
	}
	// "18.1 (Debian 18.1-1.pgdg120+2)" → "18.1".
	info.ServerVersion, _, _ = strings.Cut(info.ServerVersion, " ")
	if kind != TargetDSN {
		return info, nil
	}
	if err := s.tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_stat_user_tables
		WHERE last_analyze IS NOT NULL OR last_autoanalyze IS NOT NULL)`).
		Scan(&info.HasStatistics); err != nil {
		return info, fmt.Errorf("read statistics: %w", err)
	}
	return info, nil
}

// MigrationState is what a target's schema_migrations (golang-migrate's
// version table, which forge's migrator keeps) says.
type MigrationState struct {
	// Applied is false when the table is absent or empty: nothing applied.
	Applied bool
	Version uint64
	Dirty   bool
}

// MigrationState reads the target's schema_migrations without creating it
// (migratekit.Open would: golang-migrate creates its version table on open).
func (s *Session) MigrationState(ctx context.Context) (MigrationState, error) {
	var st MigrationState
	var exists bool
	if err := s.tx.QueryRowContext(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		return st, fmt.Errorf("look for schema_migrations: %w", err)
	}
	if !exists {
		return st, nil
	}
	var version int64
	err := s.tx.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&version, &st.Dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return st, nil
	case err != nil:
		return st, fmt.Errorf("read schema_migrations: %w", err)
	case version < 0:
		// golang-migrate's "no version".
		return MigrationState{}, nil
	}
	st.Applied, st.Version = true, uint64(version)
	return st, nil
}

// sqlState is err's SQLSTATE, or "".
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
