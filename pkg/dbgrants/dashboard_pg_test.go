package dbgrants

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the dashboard role's two checks against real Postgres. Like
// the convergence tests they boot a server, so they skip under -short. Roles
// are cluster-wide and the server is shared, so each test names its own.

func uniqueRole(prefix string) string {
	return fmt.Sprintf("%s%d_%d", prefix, os.Getpid(), time.Now().UnixNano()%1_000_000_000)
}

func dropRoleAtCleanup(t *testing.T, f *pgFixture, role string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = f.db.ExecContext(context.Background(), `DROP OWNED BY `+role)
		_, _ = f.db.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
}

// EnsureRole creates a missing role NOLOGIN, leaves an existing one exactly as
// it is (its LOGIN belongs to CNPG), and is idempotent.
func TestEnsureRoleCreatesAMissingRoleNologinAndLeavesAnExistingOneAlone(t *testing.T) {
	f := newPGFixture(t, convergeSchemaSQL)
	ctx := context.Background()
	role := uniqueRole("forge_er")
	dropRoleAtCleanup(t, f, role)

	created, err := EnsureRole(ctx, f.db, role)
	if err != nil || !created {
		t.Fatalf("first EnsureRole: created=%v err=%v", created, err)
	}
	var login, inherit bool
	if err := f.db.QueryRowContext(ctx, `SELECT rolcanlogin, rolinherit FROM pg_roles WHERE rolname = $1`, role).Scan(&login, &inherit); err != nil {
		t.Fatal(err)
	}
	if login || inherit {
		t.Errorf("created role: login=%v inherit=%v, want NOLOGIN NOINHERIT", login, inherit)
	}

	// The deploy turns it on; a later run must not turn it back off.
	f.exec(`ALTER ROLE ` + role + ` LOGIN CONNECTION LIMIT 4`)
	if created, err = EnsureRole(ctx, f.db, role); err != nil || created {
		t.Fatalf("second EnsureRole: created=%v err=%v", created, err)
	}
	var limit int
	if err := f.db.QueryRowContext(ctx, `SELECT rolcanlogin, rolconnlimit FROM pg_roles WHERE rolname = $1`, role).Scan(&login, &limit); err != nil {
		t.Fatal(err)
	}
	if !login || limit != 4 {
		t.Errorf("EnsureRole altered an existing role: login=%v connlimit=%d", login, limit)
	}
}

// Migrators of different databases on one server race to create the role:
// every one of them succeeds, and exactly one reports creating it.
func TestEnsureRoleIsSafeUnderConcurrentCreation(t *testing.T) {
	f := newPGFixture(t, convergeSchemaSQL)
	role := uniqueRole("forge_ec")
	dropRoleAtCleanup(t, f, role)

	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		creates int
		errs    []error
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := EnsureRole(context.Background(), f.db, role)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if created {
				creates++
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("concurrent EnsureRole failed: %v", errs)
	}
	if creates != 1 {
		t.Errorf("%d callers report creating the role, want exactly 1", creates)
	}
}

// A connecting role without CREATEROLE (a ManagedDatabase's application role)
// gets ErrRoleNotCreatable, not a generic failure, so the migrator can decide.
func TestEnsureRoleReportsAConnectingRoleThatMayNotCreateRoles(t *testing.T) {
	f := newPGFixture(t, convergeSchemaSQL)
	ctx := context.Background()
	role := uniqueRole("forge_en")
	conn, err := f.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// f.role is NOSUPERUSER NOCREATEROLE.
	if _, err := conn.ExecContext(ctx, `SET ROLE `+f.role); err != nil {
		t.Fatal(err)
	}
	created, err := EnsureRole(ctx, conn, role)
	if created || !errors.Is(err, ErrRoleNotCreatable) {
		t.Fatalf("EnsureRole as a role without CREATEROLE: created=%v err=%v, want ErrRoleNotCreatable", created, err)
	}
}

// What a read-only role may hold is not listed; everything else is, by name.
func TestDashboardPrivilegeViolationsListsOnlyWhatIsNotReading(t *testing.T) {
	f := newPGFixture(t, convergeSchemaSQL+`
CREATE SEQUENCE events_seq;
CREATE VIEW events_v AS SELECT id, model FROM events;
CREATE FUNCTION helper() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
`)
	ctx := context.Background()
	role := f.role
	violations := func() []string {
		t.Helper()
		got, err := DashboardPrivilegeViolations(ctx, f.db, role)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The whole read-only vocabulary: none of it is a violation.
	f.exec(`GRANT USAGE ON SCHEMA public TO ` + role)
	f.exec(`GRANT SELECT (id, model) ON events TO ` + role)
	f.exec(`GRANT SELECT ON other TO ` + role)
	f.exec(`GRANT SELECT ON events_v TO ` + role)
	f.exec(`GRANT SELECT ON SEQUENCE events_seq TO ` + role)
	f.exec(`GRANT CONNECT ON DATABASE "` + currentDB(t, f) + `" TO ` + role)
	if got := violations(); len(got) != 0 {
		t.Fatalf("read privileges reported as violations: %v", got)
	}

	cases := []struct{ grant, revoke, want string }{
		{`GRANT INSERT ON events TO %s`, `REVOKE INSERT ON events FROM %s`, "INSERT on table events"},
		{`GRANT UPDATE (note) ON events TO %s`, `REVOKE UPDATE (note) ON events FROM %s`, "UPDATE on column events.note"},
		{`GRANT SELECT ON other TO %s WITH GRANT OPTION`, `REVOKE GRANT OPTION FOR SELECT ON other FROM %s`, "SELECT on table other WITH GRANT OPTION"},
		{`GRANT USAGE ON SEQUENCE events_seq TO %s`, `REVOKE USAGE ON SEQUENCE events_seq FROM %s`, "USAGE on sequence events_seq"},
		{`GRANT CREATE ON SCHEMA public TO %s`, `REVOKE CREATE ON SCHEMA public FROM %s`, "CREATE on schema public"},
		{`GRANT EXECUTE ON FUNCTION helper() TO %s`, `REVOKE EXECUTE ON FUNCTION helper() FROM %s`, "EXECUTE on function helper()"},
		{`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO %s`, `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON TABLES FROM %s`, " in schema public on new tables"},
		{`GRANT pg_read_all_data TO %s`, `REVOKE pg_read_all_data FROM %s`, "membership in role pg_read_all_data"},
		{`ALTER ROLE %s CREATEDB`, `ALTER ROLE %s NOCREATEDB`, "role attribute CREATEDB"},
	}
	for _, c := range cases {
		f.exec(fmt.Sprintf(c.grant, role))
		got := violations()
		if len(got) != 1 || !strings.Contains(got[0], c.want) {
			t.Errorf("after %q: violations = %v, want one naming %q", fmt.Sprintf(c.grant, role), got, c.want)
		}
		f.exec(fmt.Sprintf(c.revoke, role))
		if got := violations(); len(got) != 0 {
			t.Errorf("after %q: violations = %v, want none", fmt.Sprintf(c.revoke, role), got)
		}
	}

	if got, err := DashboardPrivilegeViolations(ctx, f.db, role+"_missing"); err != nil || len(got) != 0 {
		t.Errorf("a missing role: violations=%v err=%v, want none", got, err)
	}
}

func currentDB(t *testing.T, f *pgFixture) string {
	t.Helper()
	var name string
	if err := f.db.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}
