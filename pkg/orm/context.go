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

	// RunTransaction executes fn inside a transaction, committing when it
	// returns nil and rolling back on error or panic. The Context handed
	// to fn is the transactional one, so generated ORM ops inside it join
	// the transaction transparently.
	//
	// This lives on the INTERFACE, not just on *Client, because the
	// interface is what forge injects: the CRUD generator writes
	// `DB orm.Context` into a service's Deps, and
	// forgeconv-deps-are-interfaces requires every Deps field to be an
	// interface. With the method only on the concrete type, an
	// app that needed to make two writes atomic could not reach a
	// transaction through the dependency forge itself wired — so every
	// such app re-declared this exact method as a local interface and
	// type-asserted the DB to it. That is forge's own seam, hand-copied
	// per project.
	//
	// On *Tx the call JOINS the transaction already in progress rather
	// than opening a nested one (this API has no savepoints). That is what
	// lets a service be called standalone or from inside a larger
	// transaction without knowing which it is in.
	RunTransaction(ctx context.Context, fn func(Context) error) error

	// Dialect returns the SQL dialect (postgres — forge is postgres-pinned).
	// The raw-SQL escape hatch needs it: a hand-written handler that builds
	// its own SQL string calls db.Dialect().Placeholder(i) for $N parameter
	// markers and db.Dialect().QuoteIdentifier(name) for safe identifiers,
	// instead of hardcoding postgres syntax. Available on both *Client and
	// *Tx so raw SQL composes the same way inside and outside a transaction.
	Dialect() Dialect
}

// Ensure Client and Tx implement Context.
var (
	_ Context = (*Client)(nil)
	_ Context = (*Tx)(nil)
)
