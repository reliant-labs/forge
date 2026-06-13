package orm

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/extra/bunotel"

	_ "github.com/lib/pq" // PostgreSQL driver (database/sql registration)
)

// Client is the forge ORM handle. Post-Phase-2 it is a thin wrapper over
// a *bun.DB (uptrace/bun, postgres-pinned): generated CRUD ops reach the
// engine via Bun(); the kept schema-truth machinery (introspect/differ/
// migration) still consults Dialect() and the raw Exec/Query/QueryRow
// seam. forge is postgres-only — the dialect argument is retained on the
// constructors for call-site compatibility and must be "postgres".
type Client struct {
	bun     *bun.DB
	dialect Dialect
}

// NewClient opens a new ORM client. dialectName must be "postgres".
func NewClient(dialectName, dsn string) (*Client, error) {
	if err := requirePostgres(dialectName); err != nil {
		return nil, err
	}
	sqldb, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	return NewClientWithDB(sqldb, dialectName)
}

// NewClientWithDB wraps an existing *sql.DB into an ORM client. This is
// the seam the generated bootstrap/setup and pkg/testkit use: open a
// postgres *sql.DB, hand it here. dialectName must be "postgres".
func NewClientWithDB(db *sql.DB, dialectName string) (*Client, error) {
	if err := requirePostgres(dialectName); err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}
	dialect, err := GetDialect(dialectName)
	if err != nil {
		return nil, err
	}
	bdb := bun.NewDB(db, pgdialect.New())
	// Trace ORM SQL onto the active OTel span (forge's observability
	// convention). The generated layer also opens per-op spans; this adds
	// the SQL statement attributes.
	bdb.AddQueryHook(bunotel.NewQueryHook())
	return &Client{bun: bdb, dialect: dialect}, nil
}

func requirePostgres(dialectName string) error {
	if dialectName != "postgres" {
		return fmt.Errorf("%w: forge is postgres-pinned, got %q", ErrInvalidDialect, dialectName)
	}
	return nil
}

// Bun returns the underlying *bun.DB as a bun.IDB.
func (c *Client) Bun() bun.IDB { return c.bun }

// BunDB returns the concrete *bun.DB (for advanced callers that need
// connection-pool control or BeginTx with bun's transaction type).
func (c *Client) BunDB() *bun.DB { return c.bun }

// DB returns the underlying *sql.DB for advanced usage and for the kept
// schema-truth machinery's database/sql seam.
func (c *Client) DB() *sql.DB { return c.bun.DB }

// Dialect returns the SQL dialect (postgres). Consumed by the kept
// schema-truth machinery (introspect/differ/migration), not by the
// runtime CRUD engine.
func (c *Client) Dialect() Dialect { return c.dialect }

// Close closes the database connection.
func (c *Client) Close() error { return c.bun.Close() }

// Exec runs a raw SQL statement (escape hatch). It goes straight to the
// underlying *sql.DB, NOT through bun's query formatter: callers write
// native postgres SQL with $1/$2 placeholders, and bun's `?`-rewriting
// must not touch it. (Generated code uses db.Bun()'s typed builders,
// which handle their own placeholders.)
func (c *Client) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return c.bun.DB.ExecContext(ctx, query, args...)
}

// Query runs a raw SQL query (escape hatch). See Exec for the
// raw-passthrough rationale.
func (c *Client) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return c.bun.DB.QueryContext(ctx, query, args...)
}

// QueryRow runs a raw SQL query returning at most one row (escape hatch).
func (c *Client) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return c.bun.DB.QueryRowContext(ctx, query, args...)
}

// Tx wraps a bun transaction as an orm.Context, so the same generated
// CRUD functions run transparently inside a transaction.
type Tx struct {
	tx bun.Tx
}

// Bun returns the transaction as a bun.IDB.
func (t *Tx) Bun() bun.IDB { return t.tx }

// Commit commits the transaction.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback rolls back the transaction.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Exec runs a raw SQL statement within the transaction. Like Client.Exec
// it bypasses bun's query formatter (native $1/$2 placeholders) by going
// to the embedded *sql.Tx.
func (t *Tx) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return t.tx.Tx.ExecContext(ctx, query, args...)
}

// Query runs a raw SQL query within the transaction (raw passthrough).
func (t *Tx) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return t.tx.Tx.QueryContext(ctx, query, args...)
}

// QueryRow runs a raw SQL query within the transaction (raw passthrough).
func (t *Tx) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return t.tx.Tx.QueryRowContext(ctx, query, args...)
}

// BeginTx starts a transaction.
func (c *Client) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := c.bun.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// RunTransaction executes fn within a transaction, committing on success
// and rolling back on error or panic. The transaction Context is passed
// to fn so generated ORM ops transparently use it.
func (c *Client) RunTransaction(ctx context.Context, fn func(ctx Context) error) error {
	return c.RunTransactionWithOptions(ctx, nil, fn)
}

// RunTransactionWithOptions is RunTransaction with custom tx options.
func (c *Client) RunTransactionWithOptions(ctx context.Context, opts *sql.TxOptions, fn func(ctx Context) error) error {
	tx, err := c.BeginTx(ctx, opts)
	if err != nil {
		return NewTransactionError("begin", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return NewTransactionError("rollback", fmt.Errorf("rollback failed after error: %v, original error: %w", rbErr, err))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return NewTransactionError("commit", err)
	}
	return nil
}
