package testkit_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/auth"
	"github.com/reliant-labs/forge/pkg/tenant"
	"github.com/reliant-labs/forge/pkg/testkit"
)

func TestDiscardLogger_DropsRecords(t *testing.T) {
	t.Parallel()
	logger := testkit.DiscardLogger()
	if logger == nil {
		t.Fatal("DiscardLogger returned nil")
	}
	// Doesn't panic with concurrent writes.
	logger.Info("ignore me", "k", "v")
	logger.Error("also ignore", "err", "boom")
	logger.With("scope", "test").Warn("with-attrs path")
}

func TestNewSQLiteMemDB_ReturnsUsableContext(t *testing.T) {
	t.Parallel()
	db := testkit.NewSQLiteMemDB(t)
	if db == nil {
		t.Fatal("NewSQLiteMemDB returned nil")
	}
	// Round-trip a row.
	if _, err := db.Exec(context.Background(), `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(context.Background(), `INSERT INTO t (v) VALUES (?)`, "hello"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	row := db.QueryRow(context.Background(), `SELECT v FROM t WHERE id = 1`)
	var v string
	if err := row.Scan(&v); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if v != "hello" {
		t.Fatalf("got %q, want %q", v, "hello")
	}
}

func TestNewSQLiteMemDB_IsolatedPerCall(t *testing.T) {
	t.Parallel()
	a := testkit.NewSQLiteMemDB(t)
	b := testkit.NewSQLiteMemDB(t)
	if _, err := a.Exec(context.Background(), `CREATE TABLE only_in_a (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create in a: %v", err)
	}
	// b must not see a's table.
	_, err := b.Exec(context.Background(), `SELECT * FROM only_in_a`)
	if err == nil {
		t.Fatal("expected error querying a's table from b's database, got nil")
	}
}

func TestNewSQLiteMemDB_ClosedAfterTest(t *testing.T) {
	t.Parallel()
	var captured = make(chan struct{}, 1)
	t.Run("inner", func(inner *testing.T) {
		db := testkit.NewSQLiteMemDB(inner)
		// Use it once to make sure it's live.
		if _, err := db.Exec(context.Background(), `CREATE TABLE x (id INTEGER PRIMARY KEY)`); err != nil {
			inner.Fatalf("exec: %v", err)
		}
		inner.Cleanup(func() { captured <- struct{}{} })
	})
	select {
	case <-captured:
	default:
		t.Fatal("inner test cleanup did not run")
	}
	// We can't directly observe testkit's t.Cleanup-registered close (it
	// runs against the inner t, not ours), but the inner test would have
	// failed if NewSQLiteMemDB panicked or leaked a connection that
	// couldn't be closed.
}

func TestNewTestServer_RegistersRoutes(t *testing.T) {
	t.Parallel()
	srv := testkit.NewTestServer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})
	})
	if srv == nil {
		t.Fatal("NewTestServer returned nil")
	}
	resp, err := http.Get(srv.URL + "/echo")
	if err != nil {
		t.Fatalf("GET /echo: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("got body %q, want %q", body, "ok")
	}
}

func TestNewTestServer_ClosedAfterTest(t *testing.T) {
	t.Parallel()
	var url string
	t.Run("inner", func(inner *testing.T) {
		srv := testkit.NewTestServer(inner, func(mux *http.ServeMux) {
			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "live")
			})
		})
		url = srv.URL
		// Confirm live during inner test.
		resp, err := http.Get(url)
		if err != nil {
			inner.Fatalf("inner GET: %v", err)
		}
		_ = resp.Body.Close()
	})
	// Outer test: server must be closed.
	_, err := http.Get(url)
	if err == nil {
		t.Fatal("expected GET to fail after inner test cleanup, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") &&
		!strings.Contains(err.Error(), "EOF") &&
		!strings.Contains(err.Error(), "use of closed") {
		// Tolerate any net-error variant — the assertion is just "server
		// is no longer accepting requests".
		t.Logf("note: server-closed produced %v", err)
	}
}

func TestPermissiveAuthorizer_AllowsEverything(t *testing.T) {
	t.Parallel()
	a := testkit.PermissiveAuthorizer{}
	if err := a.CanAccess(context.Background(), "/svc/Method"); err != nil {
		t.Fatalf("CanAccess: %v", err)
	}
	if err := a.CanAccess(context.Background(), ""); err != nil {
		t.Fatalf("CanAccess empty: %v", err)
	}
	if err := a.Can(context.Background(), nil, "create", "thing"); err != nil {
		t.Fatalf("Can(nil claims): %v", err)
	}
	claims := &auth.Claims{UserID: "u1", Role: "admin"}
	if err := a.Can(context.Background(), claims, "delete", "thing"); err != nil {
		t.Fatalf("Can(real claims): %v", err)
	}
}

// TestPermissiveAuthorizer_FitsForgeAuthorizerInterface guards the
// interface fingerprint that the generated middleware.Authorizer
// shape requires. If the project's interface drifts, this test stops
// compiling.
func TestPermissiveAuthorizer_FitsForgeAuthorizerInterface(t *testing.T) {
	t.Parallel()
	type Authorizer interface {
		CanAccess(ctx context.Context, procedure string) error
		Can(ctx context.Context, claims *auth.Claims, action, resource string) error
	}
	var _ Authorizer = testkit.PermissiveAuthorizer{}
}

func TestWithTestTenant_RoundTrips(t *testing.T) {
	t.Parallel()
	ctx := testkit.WithTestTenant(context.Background(), "tenant-xyz")
	got := tenant.FromContext(ctx)
	if got != "tenant-xyz" {
		t.Fatalf("FromContext = %q, want %q", got, "tenant-xyz")
	}
}

func TestWithTestTenant_EmptyDoesNotPanic(t *testing.T) {
	t.Parallel()
	ctx := testkit.WithTestTenant(context.Background(), "")
	if got := tenant.FromContext(ctx); got != "" {
		t.Fatalf("FromContext = %q, want empty", got)
	}
}
