package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The dashboard role exists before any migration runs, and holds read
// privileges only after (dashboard.go). The project grants it with ordinary
// migrations, so these tests write those migrations and then ACT AS the role.
//
// Like the read-only tests these swap a package variable (dashboardRole) and
// use a role of their own, so none calls t.Parallel.

// useDashboardRole points the migrator at a dashboard role no other test
// process shares, and drops it at the end. The role is NOT created: the
// migrator does that.
func useDashboardRole(t *testing.T, dsn string) string {
	t.Helper()
	name := fmt.Sprintf("forge_dash_t%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	prev := dashboardRole
	dashboardRole = name
	admin := openRecordDB(t, dsn)
	t.Cleanup(func() {
		dashboardRole = prev
		c := context.Background()
		_, _ = admin.ExecContext(c, `DROP OWNED BY `+name)
		_, _ = admin.ExecContext(c, `DROP ROLE IF EXISTS `+name)
	})
	return name
}

func asDashboardRole(t *testing.T, dsn, role string, fn func(ctx context.Context, conn *sql.Conn)) {
	t.Helper()
	prev := testRole
	testRole = role
	defer func() { testRole = prev }()
	asRole(t, dsn, fn)
}

// roleDSN is dsn with the user and password swapped for the role's, so a test
// can actually try to log in as it.
func roleDSN(t *testing.T, dsn, role, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

// loginAs reports whether a connection as role succeeds and runs q.
func loginAs(t *testing.T, dsn, role, password, q string) error {
	t.Helper()
	db, err := sql.Open("pgx/v5", sqlDSN(roleDSN(t, dsn, role, password)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(context.Background(), q)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
	}
	return rows.Err()
}

func privilegeRowsOf(t *testing.T, dsn, role string) int {
	t.Helper()
	var n int
	err := openRecordDB(t, dsn).QueryRow(`
SELECT (SELECT count(*) FROM information_schema.table_privileges  WHERE grantee = $1)
     + (SELECT count(*) FROM information_schema.column_privileges WHERE grantee = $1)`, role).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func grantDashboard(role string) string {
	return fmt.Sprintf("GRANT SELECT (id, email) ON users TO %s;", role)
}

// TestDashboardRoleExistsBeforeTheMigrationThatGrantsIt: a fresh database has
// no dashboard role; the run creates it NOLOGIN first, so the project's own
// GRANT migration applies, and the role then reads exactly what it was granted.
func TestDashboardRoleExistsBeforeTheMigrationThatGrantsIt(t *testing.T) {
	dsn := requirePG(t)
	dash := useDashboardRole(t, dsn)
	fsys := release(map[string]string{
		"1_users.up.sql":     usersSQL + notesSQL,
		"2_dashboard.up.sql": grantDashboard(dash),
	})

	res := mustUp(t, fsys, dsn)
	if res.Dashboard == nil || !res.Dashboard.Created || res.Dashboard.Missing || res.Dashboard.CanLogin {
		t.Fatalf("Dashboard = %+v, want created, NOLOGIN", res.Dashboard)
	}
	var login bool
	if err := openRecordDB(t, dsn).QueryRow(`SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, dash).Scan(&login); err != nil {
		t.Fatal(err)
	}
	if login {
		t.Error("the migrator created a role that can log in: that is the deploy's decision")
	}
	seed(t, dsn)
	asDashboardRole(t, dsn, dash, func(ctx context.Context, c *sql.Conn) {
		if err := readable(ctx, c, `SELECT id, email FROM users`); err != nil {
			t.Errorf("the granted columns: %v", err)
		}
		wantDenied(t, readable(ctx, c, `SELECT api_token FROM users`), "a column no migration granted")
		wantDenied(t, readable(ctx, c, `SELECT * FROM users`), "select * over an ungranted column")
	})

	// The next run finds it and leaves it alone.
	if res := mustUp(t, fsys, dsn); res.Dashboard == nil || res.Dashboard.Created {
		t.Errorf("second run: Dashboard = %+v, want found, not created", res.Dashboard)
	}
}

// TestDashboardsOffKeepsTheGrantsButNobodyCanUseThem is the disable path
// against what CNPG does to the role on each side of the switch: on = LOGIN
// with a password; off = NOLOGIN with no password. The migrator never touches
// either, and never revokes: the grants are the project's migrations.
func TestDashboardsOffKeepsTheGrantsButNobodyCanUseThem(t *testing.T) {
	dsn := requirePG(t)
	dash := useDashboardRole(t, dsn)
	admin := openRecordDB(t, dsn)
	exec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	fsys := release(map[string]string{"1_users.up.sql": usersSQL + notesSQL, "2_dashboard.up.sql": grantDashboard(dash)})
	mustUp(t, fsys, dsn)
	seed(t, dsn)

	// ON: CNPG adopts the role the migrator created and gives it a login.
	const pw = "s3cret-on"
	exec(fmt.Sprintf(`ALTER ROLE %s LOGIN PASSWORD '%s'`, dash, pw))
	if res := mustUp(t, fsys, dsn); res.Dashboard == nil || !res.Dashboard.CanLogin {
		t.Fatalf("enabled: Dashboard = %+v, want CanLogin", res.Dashboard)
	}
	if err := loginAs(t, dsn, dash, pw, `SELECT id, email FROM users`); err != nil {
		t.Fatalf("the enabled role cannot read what a migration granted: %v", err)
	}

	// OFF: what CNPG converges the rendered role to.
	exec(fmt.Sprintf(`ALTER ROLE %s NOLOGIN PASSWORD NULL`, dash))
	res := mustUp(t, fsys, dsn)
	if res.Dashboard == nil || res.Dashboard.CanLogin {
		t.Fatalf("disabled: Dashboard = %+v, want not CanLogin", res.Dashboard)
	}
	if err := loginAs(t, dsn, dash, pw, `SELECT 1`); err == nil {
		t.Error("the disabled role logged in with its old password")
	}
	if err := loginAs(t, dsn, dash, "", `SELECT 1`); err == nil {
		t.Error("the disabled role logged in without a password")
	}
	if privilegeRowsOf(t, dsn, dash) == 0 {
		t.Error("the run revoked the project's grants: off must leave them in place, unusable")
	}

	// ON again: the same grants serve, with the new password only.
	const pw2 = "s3cret-back"
	exec(fmt.Sprintf(`ALTER ROLE %s LOGIN PASSWORD '%s'`, dash, pw2))
	mustUp(t, fsys, dsn)
	if err := loginAs(t, dsn, dash, pw2, `SELECT id, email FROM users`); err != nil {
		t.Errorf("the re-enabled role cannot read: %v", err)
	}
	if err := loginAs(t, dsn, dash, pw, `SELECT 1`); err == nil {
		t.Error("the password from before the role was disabled works again")
	}
}

// TestRevokingInALaterMigrationRemovesAccess: removing access is a migration
// too.
func TestRevokingInALaterMigrationRemovesAccess(t *testing.T) {
	dsn := requirePG(t)
	dash := useDashboardRole(t, dsn)
	files := map[string]string{"1_users.up.sql": usersSQL, "2_dashboard.up.sql": grantDashboard(dash)}
	mustUp(t, release(files), dsn)
	files["3_revoke.up.sql"] = fmt.Sprintf("REVOKE SELECT (email) ON users FROM %s;", dash)
	mustUp(t, release(files), dsn)
	asDashboardRole(t, dsn, dash, func(ctx context.Context, c *sql.Conn) {
		if err := readable(ctx, c, `SELECT id FROM users`); err != nil {
			t.Errorf("a column still granted: %v", err)
		}
		wantDenied(t, readable(ctx, c, `SELECT email FROM users`), "a revoked column")
	})
}

// migratorDSN creates a login role that owns nothing and may not create roles
// — a ManagedDatabase's application role — with what it needs to migrate the
// database, and returns a DSN that connects as it.
func migratorDSN(t *testing.T, dsn string) string {
	t.Helper()
	admin := openRecordDB(t, dsn)
	role := fmt.Sprintf("forge_mig_t%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	const pw = "migrator-pw"
	var db string
	if err := admin.QueryRow(`SELECT current_database()`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEROLE NOCREATEDB`, role, pw),
		fmt.Sprintf(`GRANT ALL ON DATABASE "%s" TO %s`, db, role),
		fmt.Sprintf(`GRANT ALL ON SCHEMA public TO %s`, role),
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.ExecContext(c, `DROP OWNED BY `+role)
		_, _ = admin.ExecContext(c, `DROP ROLE IF EXISTS `+role)
	})
	return roleDSN(t, dsn, role, pw)
}

// TestAMigratorThatMayNotCreateTheRole: on a ManagedDatabase the migrating
// role has no CREATEROLE and CNPG may not have created the dashboard role yet.
// A run that grants nothing to it goes on; a run about to apply a migration
// that names it refuses BEFORE applying anything, so the schema is not left
// dirty, and succeeds once the role exists.
func TestAMigratorThatMayNotCreateTheRole(t *testing.T) {
	dsn := requirePG(t)
	dash := useDashboardRole(t, dsn)
	mig := migratorDSN(t, dsn)

	files := map[string]string{"1_users.up.sql": usersSQL}
	res := mustUp(t, release(files), mig)
	if res.Dashboard == nil || !res.Dashboard.Missing || res.Dashboard.Created {
		t.Fatalf("Dashboard = %+v, want missing and not created", res.Dashboard)
	}

	files["2_dashboard.up.sql"] = "-- panels read these\n" + grantDashboard(dash)
	mg, err := Open(Options{FS: release(files), Dir: "migrations", DSN: mig})
	if err != nil {
		t.Fatal(err)
	}
	_, err = mg.Up()
	_ = mg.Close()
	var missing *DashboardRoleMissingError
	if !errors.As(err, &missing) || len(missing.Migrations) != 1 || missing.Migrations[0] != "2_dashboard.up.sql" {
		t.Fatalf("up = %v, want a DashboardRoleMissingError naming 2_dashboard.up.sql", err)
	}
	if !strings.Contains(err.Error(), "Nothing was applied") {
		t.Errorf("the refusal does not say nothing was applied: %v", err)
	}
	var version int
	var dirty bool
	if err := openRecordDB(t, dsn).QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatal(err)
	}
	if version != 1 || dirty {
		t.Fatalf("schema at version %d dirty=%v after the refusal, want 1 and clean", version, dirty)
	}

	// CNPG reconciles the role; the retry applies the grant.
	if _, err := openRecordDB(t, dsn).Exec(`CREATE ROLE ` + dash + ` NOLOGIN`); err != nil {
		t.Fatal(err)
	}
	if res := mustUp(t, release(files), mig); res.After.Version != 2 || res.Dashboard == nil || res.Dashboard.Missing {
		t.Fatalf("retry: version %d, Dashboard = %+v, want 2 and found", res.After.Version, res.Dashboard)
	}
}

// TestADashboardRoleThatMayWriteFailsTheRun: the one check forge keeps. A
// migration granting the dashboard role anything but reads fails the run that
// applied it, and every run after, until a migration revokes it.
func TestADashboardRoleThatMayWriteFailsTheRun(t *testing.T) {
	dsn := requirePG(t)
	dash := useDashboardRole(t, dsn)
	files := map[string]string{
		"1_users.up.sql":     usersSQL,
		"2_dashboard.up.sql": grantDashboard(dash) + fmt.Sprintf("\nGRANT INSERT ON users TO %s;", dash),
	}
	up := func() (Result, error) {
		mg, err := Open(Options{FS: release(files), Dir: "migrations", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mg.Close() }()
		return mg.Up()
	}
	for _, run := range []string{"the run that granted it", "the next run"} {
		_, err := up()
		var priv *DashboardPrivilegeError
		if !errors.As(err, &priv) || len(priv.Violations) != 1 || !strings.Contains(priv.Violations[0], "INSERT on table users") {
			t.Fatalf("%s: up = %v, want a DashboardPrivilegeError naming INSERT on users", run, err)
		}
	}
	files["3_revoke.up.sql"] = fmt.Sprintf("REVOKE INSERT ON users FROM %s;", dash)
	if res, err := up(); err != nil || res.After.Version != 3 {
		t.Fatalf("after the revoking migration: version %d err %v, want 3 and no error", res.After.Version, err)
	}
}

// TestApplyEnsuresTheDashboardRoleToo: the boot path (AutoMigrate -> Apply)
// creates the role and checks it exactly as Up does.
func TestApplyEnsuresTheDashboardRoleToo(t *testing.T) {
	dsn := requirePG(t)
	dash := useDashboardRole(t, dsn)
	db := openRecordDB(t, dsn)
	res, err := Apply(context.Background(), db, release(map[string]string{
		"1_users.up.sql": usersSQL, "2_dashboard.up.sql": grantDashboard(dash),
	}), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if res.Dashboard == nil || !res.Dashboard.Created {
		t.Fatalf("Dashboard = %+v, want created", res.Dashboard)
	}
}

// TestSecurityDefinerFunctionsAreReported: a SECURITY DEFINER function the
// converged role may execute is a way around its column grants. The run still
// succeeds; it says so.
func TestSecurityDefinerFunctionsAreReported(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	withRole(t, dsn)
	fsys := grantsRelease(map[string]string{
		"1_users.up.sql": usersSQL,
		"2_definer.up.sql": `CREATE FUNCTION get_token(uid TEXT) RETURNS TEXT LANGUAGE sql SECURITY DEFINER AS $$ SELECT api_token FROM users WHERE id = uid $$;
CREATE FUNCTION invoker_fn() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;
CREATE FUNCTION locked_definer() RETURNS int LANGUAGE sql SECURITY DEFINER AS $$ SELECT 1 $$;
REVOKE EXECUTE ON FUNCTION locked_definer() FROM PUBLIC;`,
	}, mustRender(t, usersTable(true)))
	res := mustUp(t, fsys, dsn)
	if res.Grants == nil || !res.Grants.Applied {
		t.Fatalf("the run did not apply the grants: %+v", res.Grants)
	}
	if got := strings.Join(res.Grants.SecurityDefiner, ","); got != "get_token(text)" {
		t.Fatalf("SecurityDefiner = %q, want only get_token(text) (not the invoker's function, not the one PUBLIC cannot execute)", got)
	}
	if _, err := openRecordDB(t, dsn).Exec(`INSERT INTO users (id, email, api_token) VALUES ('u1', 'a@example.test', 'tok-SECRET-1')`); err != nil {
		t.Fatal(err)
	}
	// The warning is accurate: the role really does read the secret through it.
	asRole(t, dsn, func(ctx context.Context, c *sql.Conn) {
		wantDenied(t, readable(ctx, c, `SELECT api_token FROM users`), "the column directly")
		if err := readable(ctx, c, `SELECT get_token('u1')`); err != nil {
			t.Errorf("the exposure the warning describes does not exist: %v", err)
		}
	})
}
