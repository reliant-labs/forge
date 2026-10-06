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
	// The resolver is what makes a transaction carried in ctx (RunTx) reach
	// every query built off this handle — see ambientResolver in tx.go.
	bdb := bun.NewDB(db, pgdialect.New(), bun.WithConnResolver(ambientResolver{db: db}))
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

// Bun returns the client's bun.IDB. Every query run through it with a ctx
// carrying a RunTx transaction on this database runs inside that
// transaction: query builders via the client's connection resolver, and
// ExecContext/QueryContext/QueryRowContext via the ambientDB wrapper.
func (c *Client) Bun() bun.IDB { return ambientDB{c.bun} }

// BunDB returns the concrete *bun.DB (for advanced callers that need
// connection-pool control or BeginTx with bun's transaction type). Query
// builders off it still join a ctx-carried transaction; its own
// ExecContext/QueryContext/QueryRowContext do NOT — use Bun() for those.
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
// underlying *sql.DB — or to the RunTx transaction ctx carries for it — NOT
// through bun's query formatter: callers write native postgres SQL with
// $1/$2 placeholders, and bun's `?`-rewriting must not touch it. (Generated
// code uses db.Bun()'s typed builders, which handle their own placeholders.)
func (c *Client) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return c.conn(ctx).ExecContext(ctx, query, args...)
}

// Query runs a raw SQL query (escape hatch). See Exec for the
// raw-passthrough rationale.
func (c *Client) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return c.conn(ctx).QueryContext(ctx, query, args...)
}

// QueryRow runs a raw SQL query returning at most one row (escape hatch).
func (c *Client) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return c.conn(ctx).QueryRowContext(ctx, query, args...)
}

// Tx wraps a bun transaction as an orm.Context, so the same generated
// CRUD functions run transparently inside a transaction.
type Tx struct {
	tx      bun.Tx
	dialect Dialect
	// state carries the transaction's pool identity and after-commit
	// callbacks. It is shared with every ctx RunTx derives from this
	// transaction, so Commit runs what AfterCommit registered there.
	state *txState
}

// Bun returns the transaction as a bun.IDB.
func (t *Tx) Bun() bun.IDB { return t.tx }

// Dialect returns the SQL dialect (postgres), so raw-SQL handlers running
// inside a transaction get the same Placeholder()/QuoteIdentifier() seam as
// on *Client. Carried from the parent Client at BeginTx time.
func (t *Tx) Dialect() Dialect { return t.dialect }

// Commit commits the transaction, then runs the callbacks AfterCommit
// registered against it, in order. A failed commit runs none.
func (t *Tx) Commit() error {
	err := t.tx.Commit()
	if t.state == nil {
		return err
	}
	callbacks := t.state.finish(err == nil)
	if err != nil {
		return err
	}
	for _, callback := range callbacks {
		callback(t.state.outer)
	}
	return nil
}

// Rollback rolls back the transaction and discards its after-commit
// callbacks.
func (t *Tx) Rollback() error {
	err := t.tx.Rollback()
	if t.state != nil {
		t.state.finish(false)
	}
	return err
}

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

// RunTransaction on a Tx JOINS the transaction already in progress: it
// runs fn with this same *Tx rather than opening a nested one, which this
// API cannot express (there are no savepoints here). Commit and rollback
// stay the outermost caller's decision — an error from fn propagates and
// the owner of the transaction rolls it back.
//
// Without this, an interactor that wraps its work in RunTransaction could
// not be composed into another interactor that had already opened one.
func (t *Tx) RunTransaction(ctx context.Context, fn func(Context) error) error {
	return fn(t)
}

// BeginTx starts a transaction. It is the low-level primitive: it never
// joins a ctx-carried transaction, never retries, and leaves Commit or
// Rollback to the caller. Prefer RunTx.
func (c *Client) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := c.bun.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	state := &txState{db: c.bun.DB, tx: tx, outer: ctx}
	return &Tx{tx: tx, dialect: c.dialect, state: state}, nil
}

// RunTransaction executes fn within a transaction, committing on success
// and rolling back on error or panic. The transaction Context is passed
// to fn so generated ORM ops transparently use it.
//
// Prefer RunTx for new code. RunTransaction is the handle-passing form and
// keeps its original semantics — the database's default isolation (or
// whatever opts asks for), one attempt, no retry — because its existing
// callers' closures were written for exactly that: re-running them on a
// serialization failure could repeat side effects they were never written to
// repeat. fn also receives only a handle, not a ctx carrying the
// transaction, so code called with the OUTER ctx and the plain client does
// not participate; use the handle, or call handle.RunTx to get such a ctx.
//
// It does join a transaction ctx already carries for this database (from
// RunTx), handing fn a handle on that transaction, so legacy code composes
// inside a RunTx exactly as RunTx composes inside it.
func (c *Client) RunTransaction(ctx context.Context, fn func(ctx Context) error) error {
	return c.RunTransactionWithOptions(ctx, nil, fn)
}

// RunTransactionWithOptions is RunTransaction with custom tx options. opts
// are ignored when it joins a ctx-carried transaction.
func (c *Client) RunTransactionWithOptions(ctx context.Context, opts *sql.TxOptions, fn func(ctx Context) error) error {
	if st := ambientTx(ctx, c.bun.DB); st != nil {
		return fn(&Tx{tx: st.tx, dialect: c.dialect, state: st})
	}
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
