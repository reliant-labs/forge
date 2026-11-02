package dbquery

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/reliant-labs/forge/pkg/pgtest"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// InspectRole tells a reader from every kind of role that can do more: one
// that may write a table, one that owns one, one that can SET ROLE to the
// owner without inheriting it, one that can terminate other sessions, and a
// role attribute.
func TestInspectRole(t *testing.T) {
	testkit.Slow(t, "boots a real postgres (pgtest)")
	dsn, cleanup, err := pgtest.NewURL()
	if err != nil {
		t.Fatalf("pgtest.NewURL: %v", err)
	}
	t.Cleanup(cleanup)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })

	// Roles are cluster-wide on a shared server: name them per run.
	sfx := fmt.Sprintf("_%d", time.Now().UnixNano())
	owner, reader, writer, member, signaler, creator := "own"+sfx, "ro"+sfx, "rw"+sfx, "mem"+sfx, "sig"+sfx, "cr"+sfx
	exec := func(sql string) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, r := range []string{owner, reader, writer, member, signaler} {
		exec(fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'pw'`, r))
	}
	exec(fmt.Sprintf(`CREATE ROLE %s LOGIN CREATEDB PASSWORD 'pw'`, creator))
	t.Cleanup(func() {
		for _, r := range []string{member, signaler, creator, reader, writer, owner} {
			_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP OWNED BY %s`, r))
			_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, r))
		}
	})
	exec(fmt.Sprintf(`GRANT CREATE ON SCHEMA public TO %s`, owner))
	exec(fmt.Sprintf(`SET ROLE %s`, owner))
	exec(`CREATE TABLE ledger (id int PRIMARY KEY, amount int)`)
	exec(`RESET ROLE`)
	exec(fmt.Sprintf(`GRANT SELECT ON ledger TO %s, %s, %s`, reader, writer, signaler))
	exec(fmt.Sprintf(`GRANT UPDATE (amount) ON ledger TO %s`, writer))
	exec(fmt.Sprintf(`ALTER ROLE %s NOINHERIT`, member))
	exec(fmt.Sprintf(`GRANT %s TO %s`, owner, member))
	exec(fmt.Sprintf(`GRANT pg_signal_backend TO %s`, signaler))

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	inspect := func(role string) Role {
		t.Helper()
		as := *u
		as.User = url.UserPassword(role, "pw")
		c, err := pgx.Connect(ctx, as.String())
		if err != nil {
			t.Fatalf("connect as %s: %v", role, err)
		}
		defer func() { _ = c.Close(ctx) }()
		r, err := InspectRole(ctx, c)
		if err != nil {
			t.Fatalf("InspectRole as %s: %v", role, err)
		}
		if r.Name != role {
			t.Errorf("Name = %q, want %q", r.Name, role)
		}
		return r
	}

	if r := inspect(reader); r.Writable() {
		t.Errorf("a SELECT-only role is a reader, got %v", r.Writes)
	}
	for role, want := range map[string]string{
		writer:   "may write 1 relations",
		owner:    "owns 1 relations",
		member:   "owns 1 relations",
		signaler: "member of pg_signal_backend",
		creator:  "CREATEDB",
	} {
		r := inspect(role)
		if !r.Writable() || !strings.Contains(strings.Join(r.Writes, "; "), want) {
			t.Errorf("%s: Writes = %v, want one naming %q", role, r.Writes, want)
		}
	}
}
