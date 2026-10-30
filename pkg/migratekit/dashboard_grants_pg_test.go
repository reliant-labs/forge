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
	"testing/fstest"
	"time"

	"github.com/reliant-labs/forge/pkg/dbgrants"
)

// The dashboard role's grants follow the read-only grants in every run
// (grants.go). The role is opt-in, so its absence is silent; a script that
// fails fails the run.
//
// Like the read-only tests these swap a package variable (dashboardGrantsRole)
// and use a role of their own, so none calls t.Parallel.

// useDashboardRole points the migrator at a dashboard role no other test
// process shares. The role is not created: withDashboardRole does that.
func useDashboardRole(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("forge_dash_t%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	prev := dashboardGrantsRole
	dashboardGrantsRole = name
	t.Cleanup(func() { dashboardGrantsRole = prev })
	return name
}

func withDashboardRole(t *testing.T, dsn, role string) {
	t.Helper()
	admin := openRecordDB(t, dsn)
	if _, err := admin.ExecContext(context.Background(), fmt.Sprintf(`CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE`, role)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.ExecContext(c, `DROP OWNED BY `+role)
		_, _ = admin.ExecContext(c, `DROP ROLE IF EXISTS `+role)
	})
}

func dashboardRelease(migrations map[string]string, readOnly, dashboard string) fstest.MapFS {
	fsys := grantsRelease(migrations, readOnly)
	if dashboard != "" {
		fsys[grantsDir+"/dashboard_gen.sql"] = &fstest.MapFile{Data: []byte(dashboard)}
	}
	return fsys
}

// dashTables: users is marked (id, email readable; api_token secret), notes is not.
func dashTables() []dbgrants.Table {
	users := usersTable(true)
	users.Dashboard = true
	return []dbgrants.Table{users, notesTable}
}

func mustRenderDashboard(t *testing.T, role string, tables []dbgrants.Table) string {
	t.Helper()
	script, err := dbgrants.RenderDashboardFor(role, tables)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func asDashboardRole(t *testing.T, dsn, role string, fn func(ctx context.Context, conn *sql.Conn)) {
	t.Helper()
	prev := testRole
	testRole = role
	defer func() { testRole = prev }()
	asRole(t, dsn, fn)
}

// TestDashboardGrantsConvergeWhenTheRoleExists: after Up the dashboard role
// reads the marked table's allowed columns and nothing else, and forge_ro is
// converged by the same run.
func TestDashboardGrantsConvergeWhenTheRoleExists(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	withRole(t, dsn)
	dash := useDashboardRole(t)
	withDashboardRole(t, dsn, dash)

	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL, "2_notes.up.sql": notesSQL},
		mustRender(t, dashTables()...), mustRenderDashboard(t, dash, dashTables()))
	res := mustUp(t, fsys, dsn)
	if res.Grants == nil || !res.Grants.Applied || !res.Grants.DashboardApplied || res.Grants.DashboardRoleMissing {
		t.Fatalf("Result.Grants = %+v, want both applied", res.Grants)
	}
	seed(t, dsn)

	asDashboardRole(t, dsn, dash, func(ctx context.Context, c *sql.Conn) {
		for _, q := range []string{`SELECT id, email FROM users`, `SELECT count(*) FROM users`} {
			if err := readable(ctx, c, q); err != nil {
				t.Errorf("%s: %v", q, err)
			}
		}
		wantDenied(t, readable(ctx, c, `SELECT api_token FROM users`), "a secret column")
		wantDenied(t, readable(ctx, c, `SELECT * FROM users`), "star over a table with a secret column")
		wantDenied(t, readable(ctx, c, `SELECT id FROM notes`), "an unmarked table")
	})

	// forge_ro still reads the unmarked table: the dashboard role narrows
	// nothing for it.
	asRole(t, dsn, func(ctx context.Context, c *sql.Conn) {
		if err := readable(ctx, c, `SELECT id, body FROM notes`); err != nil {
			t.Errorf("forge_ro lost an unmarked table: %v", err)
		}
	})

	// A re-run with nothing pending converges again.
	if res := mustUp(t, fsys, dsn); res.Changed || res.Grants == nil || !res.Grants.DashboardApplied {
		t.Fatalf("re-run: Changed=%v Grants=%+v", res.Changed, res.Grants)
	}
}

// TestDashboardGrantsSkipSilentlyWithoutTheRole: a database that never opted
// into dashboards migrates, converges forge_ro and reports the dashboard role
// as missing, which is not a fault.
func TestDashboardGrantsSkipSilentlyWithoutTheRole(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	withRole(t, dsn)
	dash := useDashboardRole(t)

	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL},
		mustRender(t, dashTables()...), mustRenderDashboard(t, dash, dashTables()))
	res := mustUp(t, fsys, dsn)
	if res.Grants == nil || !res.Grants.Applied {
		t.Fatalf("forge_ro not converged: %+v", res.Grants)
	}
	if res.Grants.DashboardApplied || !res.Grants.DashboardRoleMissing {
		t.Fatalf("Result.Grants = %+v, want the dashboard role missing and not applied", res.Grants)
	}
}

// TestDashboardGrantsApplyEvenWhenTheReadOnlyRoleIsMissing: the two roles are
// independent. A database with only forge_dash still converges it.
func TestDashboardGrantsApplyEvenWhenTheReadOnlyRoleIsMissing(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	dash := useDashboardRole(t)
	withDashboardRole(t, dsn, dash)

	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL},
		mustRender(t, dashTables()...), mustRenderDashboard(t, dash, dashTables()))
	res := mustUp(t, fsys, dsn)
	if res.Grants == nil || !res.Grants.RoleMissing || !res.Grants.DashboardApplied {
		t.Fatalf("Result.Grants = %+v, want forge_ro missing and the dashboard role applied", res.Grants)
	}
}

// TestDashboardGrantsCatchUpWhenTheRoleAppearsLater: CNPG creates the role when
// the Cluster is reconciled, after the migration Job. The next run, with
// nothing pending, applies the grants.
func TestDashboardGrantsCatchUpWhenTheRoleAppearsLater(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	dash := useDashboardRole(t)
	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL, "2_notes.up.sql": notesSQL},
		mustRender(t, dashTables()...), mustRenderDashboard(t, dash, dashTables()))
	if res := mustUp(t, fsys, dsn); res.Grants == nil || !res.Grants.DashboardRoleMissing {
		t.Fatalf("first run: %+v, want the dashboard role missing", res.Grants)
	}
	withDashboardRole(t, dsn, dash)
	res := mustUp(t, fsys, dsn)
	if res.Changed || res.Grants == nil || !res.Grants.DashboardApplied {
		t.Fatalf("second run: Changed=%v Grants=%+v", res.Changed, res.Grants)
	}
	seed(t, dsn)
	asDashboardRole(t, dsn, dash, func(ctx context.Context, c *sql.Conn) {
		if err := readable(ctx, c, `SELECT id, email FROM users`); err != nil {
			t.Errorf("granted after the role appeared: %v", err)
		}
	})
}

// TestDashboardGrantsFailureFailsTheRun: a dashboard script that errors fails
// the run exactly as a failing read-only script does, and the error names what
// failed. The migrations are already committed by then.
func TestDashboardGrantsFailureFailsTheRun(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	withRole(t, dsn)
	dash := useDashboardRole(t)
	withDashboardRole(t, dsn, dash)

	broken := fmt.Sprintf("DO $$ BEGIN RAISE EXCEPTION 'dashboard grants exploded'; END $$; -- %s", dash)
	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL}, mustRender(t, dashTables()...), broken)

	mg, err := Open(Options{FS: fsys, Dir: "migrations", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mg.Close() }()
	res, err := mg.Up()
	if err == nil {
		t.Fatal("Up succeeded with a failing dashboard script")
	}
	if want := "apply dashboard grants"; !contains(err.Error(), want) || !contains(err.Error(), "dashboard grants exploded") {
		t.Errorf("err = %v, want it to name %q and the cause", err, want)
	}
	if res.Grants == nil || !res.Grants.Applied || res.Grants.DashboardApplied {
		t.Errorf("Result.Grants = %+v, want forge_ro applied (it ran first) and the dashboard role not", res.Grants)
	}
	if !res.Changed {
		t.Error("the migrations, committed before the grants, are not reported")
	}

	// A rerun finds nothing pending and fails again: the failure is not swallowed.
	mg2, err := Open(Options{FS: fsys, Dir: "migrations", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mg2.Close() }()
	if _, err := mg2.Up(); err == nil {
		t.Error("a rerun hid the failing dashboard script")
	}
}

// TestDashboardGrantsAreOptionalInTheEmbed: a project that predates the file
// migrates as before.
func TestDashboardGrantsAreOptionalInTheEmbed(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	withRole(t, dsn)
	res := mustUp(t, grantsRelease(map[string]string{"1_users.up.sql": usersSQL}, mustRender(t, usersTable(true))), dsn)
	if res.Grants == nil || !res.Grants.Applied || res.Grants.DashboardApplied || res.Grants.DashboardRoleMissing {
		t.Fatalf("Result.Grants = %+v, want only forge_ro applied when no dashboard script is embedded", res.Grants)
	}
}

// TestApplyAppliesTheDashboardGrantsToo: the boot path (AutoMigrate -> Apply)
// converges the dashboard grants the same way `db migrate up` does.
func TestApplyAppliesTheDashboardGrantsToo(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	dash := useDashboardRole(t)
	withDashboardRole(t, dsn, dash)
	db := openRecordDB(t, dsn)
	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL}, "", mustRenderDashboard(t, dash, dashTables()))
	res, err := Apply(context.Background(), db, fsys, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if res.Grants == nil || !res.Grants.DashboardApplied {
		t.Fatalf("Apply: Grants = %+v, want the dashboard grants applied", res.Grants)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestDashboardGrantsFailFastBehindABlockerAndTheNextRunAppliesThem: the
// dashboard script is REVOKE/GRANT and takes locks like the read-only one, so
// behind a session holding what it converges it waits for lock_timeout, fails
// the run as the dashboard grants (not the read-only ones, which applied), and
// is applied by the next run.
func TestDashboardGrantsFailFastBehindABlockerAndTheNextRunAppliesThem(t *testing.T) {
	dsn := requirePG(t)
	// forge_ro is not created, so its script is skipped and the blocker below
	// can only be met by the dashboard script.
	useRole(t)
	dash := useDashboardRole(t)
	withDashboardRole(t, dsn, dash)
	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL, "2_notes.up.sql": notesSQL},
		mustRender(t, dashTables()...), mustRenderDashboard(t, dash, dashTables()))
	releaseBlocker := grantsBlocker(t, dsn)

	var err error
	took := timed(func() {
		_, err = upWith(fsys, dsn, RunOptions{Limits: Limits{LockTimeout: 300 * time.Millisecond, LockAttempts: 2, RetryBackoff: 50 * time.Millisecond}})
	})
	var lerr *LockTimeoutError
	if !errors.As(err, &lerr) || !lerr.Grants || lerr.Attempts != 2 {
		t.Fatalf("Up returned %v, want a grants *LockTimeoutError after 2 attempts", err)
	}
	if !strings.Contains(err.Error(), "dashboard grants") {
		t.Errorf("the error does not name the dashboard grants: %v", err)
	}
	if took > 15*time.Second {
		t.Errorf("Up took %s behind a blocker with a 300ms lock timeout", took)
	}

	releaseBlocker()
	res, err := upWith(fsys, dsn, RunOptions{})
	if err != nil || res.Grants == nil || !res.Grants.DashboardApplied {
		t.Fatalf("rerun: %+v, %v, want the dashboard grants applied", res.Grants, err)
	}
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
     + (SELECT count(*) FROM information_schema.column_privileges WHERE grantee = $1)
     + (SELECT count(*) FROM pg_namespace n, aclexplode(n.nspacl) x WHERE x.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1))`, role).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDashboardsOffRevokesEverythingAndTheRoleCannotLogIn is the disable path
// end to end, against what CNPG does to the role on each side of the switch:
// on = LOGIN with a password; off = NOLOGIN with no password (pkg/deploy
// renders it present, never absent). The migrator never sees the KCL: the
// role's own login attribute is the switch.
//
//	enable  -> migrate: the role logs in and reads the marked columns
//	disable -> migrate: it cannot log in, holds no privilege, nothing re-granted
//	enable  -> migrate: it reads again
func TestDashboardsOffRevokesEverythingAndTheRoleCannotLogIn(t *testing.T) {
	dsn := requirePG(t)
	useRole(t)
	dash := useDashboardRole(t)
	withDashboardRole(t, dsn, dash)
	admin := openRecordDB(t, dsn)
	exec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	fsys := dashboardRelease(map[string]string{"1_users.up.sql": usersSQL, "2_notes.up.sql": notesSQL},
		"", mustRenderDashboard(t, dash, dashTables()))

	// ON.
	const pw = "s3cret-on"
	exec(fmt.Sprintf(`ALTER ROLE %s LOGIN PASSWORD '%s'`, dash, pw))
	res := mustUp(t, fsys, dsn)
	if res.Grants == nil || !res.Grants.DashboardApplied || res.Grants.DashboardRoleDisabled {
		t.Fatalf("enabled: Grants = %+v, want applied", res.Grants)
	}
	seed(t, dsn)
	if err := loginAs(t, dsn, dash, pw, `SELECT id, email FROM users`); err != nil {
		t.Fatalf("the enabled role cannot read what it was granted: %v", err)
	}
	if privilegeRowsOf(t, dsn, dash) == 0 {
		t.Fatal("the enabled role holds no privilege: the test would prove nothing")
	}
	// The reason `ensure: absent` cannot be the off switch: CNPG would run
	// exactly this DROP ROLE, and Postgres refuses it.
	if _, err := admin.Exec(`DROP ROLE ` + dash); err == nil || !strings.Contains(err.Error(), "2BP01") {
		t.Fatalf("DROP ROLE of a role that holds grants: %v, want SQLSTATE 2BP01", err)
	}

	// OFF: what CNPG converges the rendered role to.
	exec(fmt.Sprintf(`ALTER ROLE %s NOLOGIN PASSWORD NULL`, dash))
	res = mustUp(t, fsys, dsn)
	if res.Grants == nil || !res.Grants.DashboardRoleDisabled || res.Grants.DashboardApplied {
		t.Fatalf("disabled: Grants = %+v, want the role reported disabled and not applied", res.Grants)
	}
	if err := loginAs(t, dsn, dash, pw, `SELECT 1`); err == nil {
		t.Error("the disabled role logged in with its old password")
	}
	if err := loginAs(t, dsn, dash, "", `SELECT 1`); err == nil {
		t.Error("the disabled role logged in without a password")
	}
	if n := privilegeRowsOf(t, dsn, dash); n != 0 {
		t.Errorf("the disabled role still holds %d privileges", n)
	}
	asDashboardRole(t, dsn, dash, func(ctx context.Context, c *sql.Conn) {
		wantDenied(t, readable(ctx, c, `SELECT id FROM users`), "the disabled role reading a marked table")
		wantDenied(t, readable(ctx, c, `SELECT id FROM notes`), "the disabled role reading an unmarked table")
	})
	// And it stays off: every further migration revokes rather than re-grants.
	mustUp(t, fsys, dsn)
	if n := privilegeRowsOf(t, dsn, dash); n != 0 {
		t.Errorf("a later run re-granted the disabled role: %d privileges", n)
	}

	// ON again: the same role entry flipped back.
	const pw2 = "s3cret-back"
	exec(fmt.Sprintf(`ALTER ROLE %s LOGIN PASSWORD '%s'`, dash, pw2))
	res = mustUp(t, fsys, dsn)
	if res.Grants == nil || !res.Grants.DashboardApplied || res.Grants.DashboardRoleDisabled {
		t.Fatalf("re-enabled: Grants = %+v, want applied", res.Grants)
	}
	if err := loginAs(t, dsn, dash, pw2, `SELECT id, email FROM users`); err != nil {
		t.Errorf("the re-enabled role cannot read: %v", err)
	}
	if err := loginAs(t, dsn, dash, pw2, `SELECT api_token FROM users`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the re-enabled role reads a secret column: %v", err)
	}
	if err := loginAs(t, dsn, dash, pw, `SELECT 1`); err == nil {
		t.Error("the password from before the role was disabled works again")
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
