package tdd

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// SetupMockDB returns an in-memory SQLite *sql.DB suitable for hermetic
// unit tests. The DB is closed via t.Cleanup, so the caller does not
// need to close it manually.
//
// The SQLite driver is intentionally NOT imported here — pulling in
// mattn/go-sqlite3 would add a CGO dependency to anything that imports
// pkg/tdd, including projects that don't need it. Instead, callers are
// expected to blank-import the driver in their _test.go file (or in a
// shared test helper), e.g.:
//
//	import _ "github.com/mattn/go-sqlite3"
//
// Forge's bootstrap_testing.go.tmpl already does this for projects with
// a database, so the import is "free" there.
//
// If the driver is not registered, sql.Open returns the usual
// "unknown driver" error, which the helper surfaces via t.Fatalf.
func SetupMockDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v (did you blank-import github.com/mattn/go-sqlite3?)", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// WithTimeout returns a context.Context with the given deadline and a
// cleanup function that cancels it. Suitable for use inside a
// [Case].Setup hook or directly as Case.Ctx.
//
//	tdd.Case[Req, Resp]{
//	    Name: "slow", Req: ...,
//	    Ctx:  func() context.Context { ctx, _ := tdd.WithTimeout(2*time.Second); return ctx }(),
//	},
//
// Cancel is exposed so tests that want eager cleanup can defer it; the
// returned context will already be canceled by Go's runtime once the
// timeout elapses.
func WithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
