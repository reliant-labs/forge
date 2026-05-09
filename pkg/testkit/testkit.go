// Package testkit provides a small set of test helpers that every
// forge-generated bootstrap_testing.go reinvents: a discard logger, an
// in-memory SQLite ORM context, an httptest harness that wraps a
// Connect-mounted service, a permissive Authorizer for non-authz tests,
// and a tenant-context helper for multi-tenant tests.
//
// # Why not absorb the whole bootstrap_testing.go?
//
// The per-service NewTest<Service>(t, opts...) factory is project-specific:
// it knows the service's Deps shape, its Register method, and the proto
// Connect client type. None of that compresses into a library helper —
// every project's test factory looks slightly different. testkit only
// holds the genuinely shared sub-helpers; the wiring shim stays codegen.
//
// # Usage in generated code
//
// Forge's bootstrap_testing.go template calls into testkit from
// defaultTestConfig:
//
//	cfg := &testConfig{
//	    logger: testkit.DiscardLogger(),
//	    cfg:    &config.Config{},
//	    authz:  testkit.PermissiveAuthorizer{},
//	    db:     testkit.NewSQLiteMemDB(t), // only when AnyServiceHasDB
//	}
//
// And NewTest<Svc>Server delegates to testkit.NewTestServer(t, register):
//
//	srv := testkit.NewTestServer(t, func(mux *http.ServeMux) {
//	    svc.Register(mux, connect.WithInterceptors())
//	})
package testkit

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/reliant-labs/forge/pkg/auth"
	"github.com/reliant-labs/forge/pkg/orm"
	"github.com/reliant-labs/forge/pkg/tenant"

	_ "github.com/mattn/go-sqlite3" // SQLite driver

	_ "github.com/reliant-labs/forge/pkg/dialects/sqlite" // register SQLite dialect for tests
)

// DiscardLogger returns a slog.Logger that drops every record. Use it as
// the default logger in unit tests where log output would be noise. The
// logger is safe for concurrent use and never returns errors.
//
// Tests that need to assert on log lines should construct their own
// logger backed by a *bytes.Buffer or testing.TB.Log; this helper exists
// for the common "I do not care what gets logged" case.
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// NewSQLiteMemDB returns a fresh in-memory SQLite ORM client suitable
// for hermetic unit tests. The database is empty — callers that need a
// schema should run migrations or create tables explicitly. The
// underlying connection is closed via t.Cleanup, so the caller does not
// need to defer a close.
//
// Each call yields an isolated database. Two NewSQLiteMemDB calls in the
// same test return ORM contexts backed by independent connections, which
// is the right default for table-driven tests that mutate rows.
//
// The SQLite driver and dialect are registered transitively by
// blank-imports in this package, so projects do not need to import
// either themselves to use testkit.
func NewSQLiteMemDB(t *testing.T) orm.Context {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	client, err := orm.NewClientWithDB(db, "sqlite")
	if err != nil {
		_ = db.Close()
		t.Fatalf("create ORM client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// NewTestServer starts an httptest.Server backed by a fresh
// http.ServeMux and invokes register so callers can mount one or more
// Connect services on the mux. The server is closed via t.Cleanup, so
// the caller does not need to defer srv.Close.
//
// The split — register receives the mux instead of the server — keeps
// testkit independent of any specific service type. A typical call site
// looks like:
//
//	srv := testkit.NewTestServer(t, func(mux *http.ServeMux) {
//	    svc.Register(mux, connect.WithInterceptors(/*...*/))
//	})
//	client := myservicev1connect.NewMyServiceClient(http.DefaultClient, srv.URL)
//
// The client construction stays in the per-service test factory because
// it requires the proto-specific connect package and client constructor.
func NewTestServer(t *testing.T, register func(mux *http.ServeMux)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// PermissiveAuthorizer is an Authorizer implementation for use in unit
// tests. It allows every call. Production authorizers deny by default
// (fail-closed), but tests typically want to exercise business logic
// without authz noise — so the generated NewTest<Service> wires this in
// as the default authz.
//
// Tests that need to exercise real authz rules should pass their own
// Authorizer via WithAuthorizer(...) or supply a full Deps via
// With<Service>Deps(...).
//
// PermissiveAuthorizer satisfies any Authorizer interface with the
// canonical forge shape:
//
//	CanAccess(ctx context.Context, procedure string) error
//	Can(ctx context.Context, claims *auth.Claims, action, resource string) error
//
// In generated projects, the project-local middleware.Authorizer
// interface uses *middleware.Claims, which is itself a type alias for
// *auth.Claims, so PermissiveAuthorizer satisfies it without conversion.
type PermissiveAuthorizer struct{}

// CanAccess always returns nil.
func (PermissiveAuthorizer) CanAccess(context.Context, string) error { return nil }

// Can always returns nil.
func (PermissiveAuthorizer) Can(context.Context, *auth.Claims, string, string) error {
	return nil
}

// WithTestTenant returns a context with the given tenant ID set, using
// the same context key that pkg/tenant's interceptor uses in production.
//
// Use in multi-tenant unit tests to simulate an authenticated tenant
// context without going through the full auth + tenant interceptor
// chain:
//
//	ctx := testkit.WithTestTenant(context.Background(), "tenant-123")
//	resp, err := svc.CreateThing(ctx, ...)
//
// Generated projects re-export this as middleware.WithTestTenant /
// app.WithTestTenant when MultiTenantEnabled is true; calling either
// resolves to this helper.
func WithTestTenant(ctx context.Context, tenantID string) context.Context {
	return tenant.WithTenantID(ctx, tenantID)
}
