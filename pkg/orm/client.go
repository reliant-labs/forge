package orm

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/lib/pq" // PostgreSQL driver
)

// Client provides the core ORM functionality
type Client struct {
	db      *sql.DB
	dialect Dialect
}

// NewClient creates a new ORM client using the specified dialect.
// dialectName should be one of the registered dialects (e.g., "postgres", "sqlite").
// Use ListDialects() to see all available dialects.
func NewClient(dialectName, dsn string) (*Client, error) {
	db, dialect, err := openWithDialect(dialectName, dsn)
	if err != nil {
		return nil, err
	}

	return &Client{
		db:      db,
		dialect: dialect,
	}, nil
}

// NewClientWithDB creates a new ORM client from an existing database connection.
// This is useful when you need more control over the database connection settings.
func NewClientWithDB(db *sql.DB, dialectName string) (*Client, error) {
	dialect, err := GetDialect(dialectName)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &Client{
		db:      db,
		dialect: dialect,
	}, nil
}

// Close closes the database connection
func (c *Client) Close() error {
	return c.db.Close()
}

// DB returns the underlying *sql.DB for advanced usage
func (c *Client) DB() *sql.DB {
	return c.db
}

// Dialect returns the dialect being used by this client
func (c *Client) Dialect() Dialect {
	return c.dialect
}

// Exec executes a raw SQL query
func (c *Client) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return c.db.ExecContext(ctx, query, args...)
}

// Query executes a raw SQL query and returns rows
func (c *Client) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return c.db.QueryContext(ctx, query, args...)
}

// QueryRow executes a query that returns at most one row
func (c *Client) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return c.db.QueryRowContext(ctx, query, args...)
}

// BeginTx starts a transaction
func (c *Client) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, dialect: c.dialect}, nil
}

// Tx wraps a database transaction
type Tx struct {
	tx      *sql.Tx
	dialect Dialect
}

// Commit commits the transaction
func (t *Tx) Commit() error {
	return t.tx.Commit()
}

// Rollback rolls back the transaction
func (t *Tx) Rollback() error {
	return t.tx.Rollback()
}

// Exec executes a query within the transaction
func (t *Tx) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// Query executes a query within the transaction
func (t *Tx) Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

// QueryRow executes a query within the transaction
func (t *Tx) QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// RunTransaction executes a function within a database transaction.
// If the function returns an error, the transaction is rolled back.
// Otherwise, the transaction is committed.
// The transaction Context is passed to the function, allowing ORM methods
// to transparently use the transaction.
func (c *Client) RunTransaction(ctx context.Context, fn func(ctx Context) error) error {
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return NewTransactionError("begin", err)
	}

	// Ensure we always either commit or rollback
	defer func() {
		if p := recover(); p != nil {
			// If there's a panic, rollback and re-panic
			_ = tx.Rollback()
			panic(p)
		}
	}()

	// Execute the function with the transaction context
	if err := fn(tx); err != nil {
		// If the function returns an error, rollback
		if rbErr := tx.Rollback(); rbErr != nil {
			return NewTransactionError("rollback", fmt.Errorf("rollback failed after error: %v, original error: %w", rbErr, err))
		}
		return err
	}

	// If the function succeeds, commit
	if err := tx.Commit(); err != nil {
		return NewTransactionError("commit", err)
	}

	return nil
}

// RunTransactionWithOptions executes a function within a database transaction with custom options.
// If the function returns an error, the transaction is rolled back.
// Otherwise, the transaction is committed.
func (c *Client) RunTransactionWithOptions(ctx context.Context, opts *sql.TxOptions, fn func(ctx Context) error) error {
	tx, err := c.BeginTx(ctx, opts)
	if err != nil {
		return NewTransactionError("begin", err)
	}

	// Ensure we always either commit or rollback
	defer func() {
		if p := recover(); p != nil {
			// If there's a panic, rollback and re-panic
			_ = tx.Rollback()
			panic(p)
		}
	}()

	// Execute the function with the transaction context
	if err := fn(tx); err != nil {
		// If the function returns an error, rollback
		if rbErr := tx.Rollback(); rbErr != nil {
			return NewTransactionError("rollback", fmt.Errorf("rollback failed after error: %v, original error: %w", rbErr, err))
		}
		return err
	}

	// If the function succeeds, commit
	if err := tx.Commit(); err != nil {
		return NewTransactionError("commit", err)
	}

	return nil
}

// Save inserts or updates a model in the database using an upsert operation.
// This works with any type that implements the Model interface.
//
// Example:
//
//	user := &User{Id: "123", Email: "test@example.com"}
//	err := client.Save(ctx, user)
func (c *Client) Save(ctx context.Context, model Model) error {
	return Save(ctx, c, model)
}

// Delete removes a model from the database by its primary key.
//
// Example:
//
//	user := &User{Id: "123"}
//	err := client.Delete(ctx, user)
func (c *Client) Delete(ctx context.Context, model Model) error {
	return Delete(ctx, c, model)
}

// Dialect returns the SQL dialect being used by this transaction
func (t *Tx) Dialect() Dialect {
	return t.dialect
}
