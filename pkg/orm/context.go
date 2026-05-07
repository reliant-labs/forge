package orm

import (
	"context"
	"database/sql"
)

// Context is the unified interface for database operations.
// It can represent either a direct database connection (Client) or a transaction (Tx).
// This allows functions to work seamlessly with both regular queries and transactional queries.
type Context interface {
	// Exec executes a query without returning any rows
	Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error)

	// Query executes a query that returns rows
	Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)

	// QueryRow executes a query that is expected to return at most one row
	QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row

	// Dialect returns the SQL dialect being used
	Dialect() Dialect
}

// Ensure Client and Tx implement Context interface
var (
	_ Context = (*Client)(nil)
	_ Context = (*Tx)(nil)
)
