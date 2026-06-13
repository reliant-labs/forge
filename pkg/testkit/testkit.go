// Package testkit provides a small set of test helpers that every
// forge-generated bootstrap_testing.go reinvents: a discard logger, a
// real-postgres ORM context (bare, or with the project's embedded
// migrations applied), an httptest harness that wraps a Connect-mounted
// service, a permissive Authorizer for non-authz tests, a claims-bearing
// AuthedContext for handlers that read the current user, and a
// tenant-context helper for multi-tenant tests.
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
//	    db:     testkit.NewPostgresDB(t), // when AnyServiceHasDB
//	}
//
// Projects with embedded migrations also get a migrated variant
// (app.NewMigratedTestDB → NewMigratedPostgresDB) for tests that need the
// real schema.
//
// # Real postgres, not SQLite
//
// forge is postgres-pinned. The DB helpers boot a real ephemeral
// postgres (pkg/pgtest: embedded-postgres by default, or the
// FORGE_TEST_POSTGRES_URL server) and hand each test its own isolated
// database. This is the same engine production runs, so migrations apply
// verbatim and there is no SQLite-portability subset to honor. The first
// call in a process boots the shared server (downloading the pg binary on
// a fresh machine); subsequent calls are cheap per-test databases.
//
// And NewTest<Svc>Server delegates to testkit.NewTestServer(t, register),
// mounting the SAME interceptor chain shape production uses — only the
// authorizer policy differs (permissive by default):
//
//	srv := testkit.NewTestServer(t, func(mux *http.ServeMux) {
//	    svc.Register(mux, connect.WithInterceptors(
//	        middleware.AuthzInterceptor(deps.Authorizer),
//	    ))
//	})
package testkit

import (
	"context"
	"database/sql"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/auth"
	"github.com/reliant-labs/forge/pkg/orm"
	"github.com/reliant-labs/forge/pkg/pgtest"
	"github.com/reliant-labs/forge/pkg/tenant"
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

// NewPostgresDB returns a fresh, isolated real-postgres ORM client
// suitable for hermetic unit tests. The database is empty — callers that
// need a schema should run migrations (NewMigratedPostgresDB) or create
// tables explicitly. The connection and the underlying database are
// cleaned up via t.Cleanup, so the caller does not need to defer a close.
//
// Each call yields its OWN database on the process-shared ephemeral
// postgres (pkg/pgtest), so two NewPostgresDB calls in the same test are
// fully isolated — the right default for table-driven tests that mutate
// rows. The first call in a process boots the shared server (downloading
// the postgres binary on a fresh machine, or connecting to
// FORGE_TEST_POSTGRES_URL); subsequent calls only CREATE DATABASE.
func NewPostgresDB(t *testing.T) orm.Context {
	t.Helper()
	db, cleanup, err := pgtest.New()
	if err != nil {
		t.Fatalf("open test postgres: %v", err)
	}
	client, err := orm.NewClientWithDB(db, "postgres")
	if err != nil {
		cleanup()
		t.Fatalf("create ORM client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		cleanup()
	})
	return client
}

// NewMigratedPostgresDB returns a real-postgres ORM client with the
// project's embedded migrations applied, so handler preconditions (tables,
// indexes) hold in unit tests exactly as they do after AutoMigrate in
// production. migrations is typically the project's embedded
// `forgedb.MigrationsFS` (db/embed.go) — the same FS pkg/app/migrate.go
// feeds to golang-migrate. Generated bootstrap testing code exposes this
// as the app.NewMigratedTestDB(t) helper whenever the project has
// migrations.
//
// Files are discovered under a "migrations/" directory inside the FS when
// present (matching the embed layout `//go:embed migrations/*.sql`),
// falling back to the FS root. Only `*.up.sql` files run, ordered by their
// numeric version prefix (the `NNNN_name.up.sql` golang-migrate
// convention), falling back to lexicographic order for non-numeric names.
//
// The SQL executes against real postgres verbatim — the same engine
// production runs — so there is no portability subset to honor. A
// migration that postgres rejects fails loudly here (t.Fatalf names the
// file).
func NewMigratedPostgresDB(t *testing.T, migrations fs.FS) orm.Context {
	t.Helper()
	db, cleanup, err := pgtest.New()
	if err != nil {
		t.Fatalf("open test postgres: %v", err)
	}
	if err := applyUpMigrations(db, migrations); err != nil {
		cleanup()
		t.Fatalf("apply embedded migrations: %v", err)
	}
	client, err := orm.NewClientWithDB(db, "postgres")
	if err != nil {
		cleanup()
		t.Fatalf("create ORM client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		cleanup()
	})
	return client
}

// applyUpMigrations runs every *.up.sql file in migrations against db in
// version order. Split out of NewMigratedPostgresDB for direct testing.
func applyUpMigrations(db *sql.DB, migrations fs.FS) error {
	root := migrations
	if sub, err := fs.Sub(migrations, "migrations"); err == nil {
		if entries, err := fs.ReadDir(sub, "."); err == nil && len(entries) > 0 {
			root = sub
		}
	}
	names, err := fs.Glob(root, "*.up.sql")
	if err != nil {
		return err
	}
	sort.Slice(names, func(i, j int) bool {
		vi, oki := migrationVersion(names[i])
		vj, okj := migrationVersion(names[j])
		if oki && okj && vi != vj {
			return vi < vj
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		sqlBytes, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		if _, err := db.Exec(string(sqlBytes)); err != nil {
			return &migrationError{file: name, err: err}
		}
	}
	return nil
}

// migrationError names the failing migration file so the t.Fatalf in
// NewMigratedPostgresDB points straight at the offending SQL.
type migrationError struct {
	file string
	err  error
}

func (e *migrationError) Error() string { return "migration " + e.file + ": " + e.err.Error() }
func (e *migrationError) Unwrap() error { return e.err }

// migrationVersion parses the numeric version prefix from a
// golang-migrate-style filename ("0002_add_users.up.sql" → 2).
func migrationVersion(name string) (int64, bool) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(name[:idx], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
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

// ClaimsOption mutates the default test claims built by [AuthedContext].
type ClaimsOption func(*auth.Claims)

// WithUserID overrides the test claims' UserID.
func WithUserID(id string) ClaimsOption {
	return func(c *auth.Claims) { c.UserID = id }
}

// WithEmail overrides the test claims' Email.
func WithEmail(email string) ClaimsOption {
	return func(c *auth.Claims) { c.Email = email }
}

// WithOrgID overrides the test claims' OrgID.
func WithOrgID(orgID string) ClaimsOption {
	return func(c *auth.Claims) { c.OrgID = orgID }
}

// WithRoles overrides the test claims' role set. The first role also
// becomes the singular Role field, matching how the auth validator
// populates both.
func WithRoles(roles ...string) ClaimsOption {
	return func(c *auth.Claims) {
		c.Roles = roles
		if len(roles) > 0 {
			c.Role = roles[0]
		} else {
			c.Role = ""
		}
	}
}

// WithClaims replaces the default test claims wholesale. Later options
// still apply on top.
func WithClaims(claims auth.Claims) ClaimsOption {
	return func(c *auth.Claims) { *c = claims }
}

// AuthedContext returns a context carrying authenticated test claims, so
// handlers that read the current user (middleware.GetUser /
// middleware.ClaimsFromContext) see a real principal instead of failing
// CodeUnauthenticated before reaching business logic.
//
// The claims context key is project-local — it lives in the generated
// pkg/middleware package, deliberately unexported so nothing bypasses the
// middleware. withClaims is therefore the project's own setter, the SAME
// function the production auth interceptor uses to install claims:
//
//	ctx := testkit.AuthedContext(t, middleware.ContextWithClaims)
//
// Generated projects re-export this with the setter pre-bound as
// app.AuthedContext(t, opts...) — prefer that form in project tests.
//
// Default claims: UserID "test-user", Email "test-user@example.test",
// Role/Roles "admin" (permissive against generated RBAC tables, mirroring
// the permissive default Authorizer). Override via ClaimsOption values.
func AuthedContext(t *testing.T, withClaims func(context.Context, *auth.Claims) context.Context, opts ...ClaimsOption) context.Context {
	t.Helper()
	claims := &auth.Claims{
		UserID: "test-user",
		Email:  "test-user@example.test",
		Role:   "admin",
		Roles:  []string{"admin"},
	}
	for _, opt := range opts {
		opt(claims)
	}
	return withClaims(context.Background(), claims)
}
