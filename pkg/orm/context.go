package orm

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
)

// Context is the unified database handle the generated ORM layer and
// forge/pkg/crud operate against. It can represent either a direct
// connection (*Client) or a transaction (*Tx), so the same generated
// CRUD functions work inside and outside transactions.
//
// Bun engine (epic Phase 2): the query/CRUD engine is uptrace/bun. The
// canonical accessor is Bun(), which returns a bun.IDB — generated ops
// build their SELECT/INSERT/UPDATE/DELETE on Bun's typed query builders
// off this handle. The raw escape hatch (Exec/Query/QueryRow) wraps
// Bun's IConn, preserved here so user-owned handlers can run hand-written
// SQL and so the kept schema-truth machinery (introspect/differ/
// migration) keeps a database/sql seam.
type Context interface {
	// Bun returns the underlying bun.IDB. Generated CRUD functions build
	// their queries on it (db.Bun().NewSelect()/NewInsert()/...). It is
	// also the raw-SQL escape hatch: bun.IDB exposes NewRaw plus the
	// IConn methods below.
	Bun() bun.IDB

	// Exec executes a query without returning any rows. Thin wrapper over
	// bun's ExecContext — the raw-SQL path for user handlers.
	Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error)

	// Query executes a query that returns rows (raw-SQL path).
	Query(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)

	// QueryRow executes a query expected to return at most one row
	// (raw-SQL path).
	QueryRow(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// Ensure Client and Tx implement Context.
var (
	_ Context = (*Client)(nil)
	_ Context = (*Tx)(nil)
)
