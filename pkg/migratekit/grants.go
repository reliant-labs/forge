package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/reliant-labs/forge/pkg/dbgrants"
)

// THE READ-ONLY GRANTS ARE A POST-STEP OF EVERY RUN.
//
// A project's `forge generate` projects the applied schema and the proto
// `forge:secret` markers into db/grants/readonly_gen.sql (pkg/dbgrants), and
// the project embeds it beside its migrations. After a run applies the
// migrations, the same session — still under the run lock — applies that file,
// so the read-only role's privileges are converged by every migration:
// a new table is granted, a new column is granted, and a column marked secret
// stays out.
//
// IT RUNS WHETHER OR NOT ANYTHING WAS PENDING. The role may not have existed
// when the last migration ran (CloudNativePG creates a managed role when the
// Cluster is reconciled, which can be after the first migration Job), and
// the script is idempotent, so a no-op run is the cheapest place to catch up.
//
// IT DOES NOT RUN WHEN THE SCHEMA IS AHEAD OF THIS BINARY. The script is this
// binary's view of the schema: an older release would REVOKE the access to a
// table only the newer release knows, and the newer release's own run
// converges the grants anyway.
//
// THE DASHBOARD ROLE'S GRANTS FOLLOW THEM. db/grants/dashboard_gen.sql
// (dbgrants.DashboardRoleName) is applied right after the read-only grants,
// under the same rules: every run, never when the schema is ahead. The role is
// opt-in (ManagedDatabase.dashboards), so a database without it is the NORMAL
// case and the script is skipped silently (Grants.DashboardRoleMissing, which
// callers do not report as a fault). A project that predates the file embeds
// none and migrates as before.
//
// THE MIGRATOR DOES NOT SEE THE KCL, SO THE ROLE IS THE SWITCH. Dashboards off
// render forge_dash present but NOLOGIN with no password (pkg/deploy), and the
// dashboard script grants nothing to a role that cannot log in and revokes what
// it held (pkg/dbgrants, "Switching dashboards off"). The script runs for such
// a role too, which is what takes the access away; Grants.DashboardRoleDisabled
// says so. Turning dashboards back on makes the role login-capable and the next
// run grants again.
//
// A STALE FILE FAILS CLOSED. Each script re-checks, at apply time, that every
// column it lists is still allowed by the column's catalog comment, and
// withholds one that is not; Grants.Refused / DashboardRefused name them so
// the run logs a warning (pkg/dbgrants, "The apply-time re-check").
//
// A FAILURE IS THE RUN'S FAILURE. The migrations are committed by then, but a
// column that has become secret keeps its old grant until the script succeeds,
// so the run reports it rather than serving on. A rerun finds nothing pending
// and applies the grants again.

// grantsRole is the role the run checks for before applying the grants. It is
// dbgrants.RoleName; a variable only so tests that share a Postgres server (and
// so a role namespace) can each use a role of their own.
var grantsRole = dbgrants.RoleName

// dashboardGrantsRole is grantsRole for the dashboard role.
var dashboardGrantsRole = dbgrants.DashboardRoleName

// GrantsFile is the grants script's path relative to the directory that holds
// the migrations' directory — "grants/readonly_gen.sql" beside
// "migrations/", which is where a project's embed puts it.
const GrantsFile = "grants/readonly_gen.sql"

// DashboardGrantsFile is [GrantsFile] for the dashboard role's script.
const DashboardGrantsFile = "grants/dashboard_gen.sql"

// GrantsRowsDir and DashboardGrantsRowsDir hold each script's per-table rows,
// relative to the same directory as [GrantsFile]: one file per table
// (pkg/dbgrants.Files), spliced into the script in file-name order.
const (
	GrantsRowsDir          = "grants/readonly"
	DashboardGrantsRowsDir = "grants/dashboard"
)

// grantScripts are the scripts one binary embeds, "" for one it does not.
type grantScripts struct{ readOnly, dashboard string }

// Grants reports what a run did with the read-only grants. Nil on a Result
// means the project embeds none.
type Grants struct {
	// Applied is true when the script ran.
	Applied bool
	// RoleMissing is true when the database has no [dbgrants.RoleName], so
	// the script was not run. That is the normal state of a database nobody
	// provisioned the role for, not a fault.
	RoleMissing bool

	// DashboardApplied is true when the dashboard role's script ran.
	DashboardApplied bool
	// DashboardRoleDisabled is true when the role exists but cannot log in,
	// which is how dashboards being off reaches the database. The script ran
	// and revoked everything the role held; it granted nothing.
	DashboardRoleDisabled bool
	// SecurityDefiner lists the SECURITY DEFINER functions a granted role can
	// execute (dbgrants.SecurityDefinerExposure): a way around its column
	// grants. A warning for the caller to log, never a failure.
	SecurityDefiner []string
	// Refused and DashboardRefused are the columns a role's grants file lists
	// that the script withheld, as `schema.table.column`, because the column's
	// current catalog comment no longer allows it: a file older than the
	// schema (dbgrants, "The apply-time re-check"). Not a failure — the
	// withholding is the safe outcome — but a warning for the caller to log,
	// because the file needs `forge generate`.
	Refused, DashboardRefused []string
	// DashboardRoleMissing is true when the binary embeds the dashboard script
	// but the database has no [dbgrants.DashboardRoleName], so it was not run.
	// The role is opt-in: this is the normal state, said by nobody.
	DashboardRoleMissing bool
}

// grantsScripts reads the grants scripts that sit beside the migrations'
// directory dir in fsys. A script the project embeds none of is "".
func grantsScripts(fsys fs.FS, dir string) (grantScripts, error) {
	var out grantScripts
	if fsys == nil {
		return out, nil
	}
	for _, f := range []struct {
		file, rows string
		into       *string
	}{{GrantsFile, GrantsRowsDir, &out.readOnly}, {DashboardGrantsFile, DashboardGrantsRowsDir, &out.dashboard}} {
		p := path.Join(path.Dir(dir), f.file)
		b, err := fs.ReadFile(fsys, p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return grantScripts{}, fmt.Errorf("read embedded grants (%s): %w", p, err)
		}
		// fs.Glob returns the names sorted: the order dbgrants.Render uses.
		names, err := fs.Glob(fsys, path.Join(path.Dir(dir), f.rows, "*_gen.sql"))
		if err != nil {
			return grantScripts{}, fmt.Errorf("list embedded grants rows (%s): %w", f.rows, err)
		}
		rows := make([]string, 0, len(names))
		for _, name := range names {
			row, err := fs.ReadFile(fsys, name)
			if err != nil {
				return grantScripts{}, fmt.Errorf("read embedded grants row (%s): %w", name, err)
			}
			rows = append(rows, string(row))
		}
		script, err := dbgrants.Assemble(string(b), rows)
		if err != nil {
			return grantScripts{}, fmt.Errorf("embedded grants (%s): %w", p, err)
		}
		*f.into = script
	}
	return out, nil
}

// applyGrants applies the run's grants scripts on conn, read-only first, or
// reports nil when the project has none. A failed script fails the run, and the
// result still says what ran before it.
//
// REVOKE and GRANT take locks, so each script runs under the run's limits
// (guardedStatement): it waits behind a long reader for lock_timeout, is
// retried, and fails the run with a *LockTimeoutError if the reader outlasts
// the attempts. Each script is its own guarded statement, so a retry reruns
// only the one that timed out; the role lookups take no conflicting lock and
// stay outside. A script added to a run must go through guardedStatement too.
func applyGrants(ctx context.Context, conn *sql.Conn, scripts grantScripts, run RunOptions) (*Grants, error) {
	if scripts == (grantScripts{}) {
		return nil, nil
	}
	g := &Grants{}
	if scripts.readOnly != "" {
		exists, err := dbgrants.RoleExists(ctx, conn, grantsRole)
		if err != nil {
			return nil, err
		}
		if !exists {
			g.RoleMissing = true
		} else {
			if err := guardedStatement(ctx, conn, run, "read-only grants", func() error {
				refused, err := dbgrants.Apply(ctx, conn, scripts.readOnly)
				g.Refused = refused
				return err
			}); err != nil {
				return nil, err
			}
			g.Applied = true
		}
	}
	if scripts.dashboard != "" {
		exists, canLogin, err := dbgrants.RoleLogin(ctx, conn, dashboardGrantsRole)
		if err != nil {
			return g, err
		}
		if !exists {
			g.DashboardRoleMissing = true
		} else {
			if err := guardedStatement(ctx, conn, run, "dashboard grants", func() error {
				refused, err := dbgrants.ApplyDashboard(ctx, conn, scripts.dashboard)
				g.DashboardRefused = refused
				return err
			}); err != nil {
				return g, err
			}
			g.DashboardApplied = canLogin
			g.DashboardRoleDisabled = !canLogin
		}
	}
	// Read after the grants, from the catalog, on the same session: a function
	// created by this very run is seen. Informational, so a failed lookup does
	// not fail a run whose grants succeeded.
	for _, role := range []struct {
		name string
		on   bool
	}{{grantsRole, g.Applied}, {dashboardGrantsRole, g.DashboardApplied}} {
		if !role.on {
			continue
		}
		if fns, err := dbgrants.SecurityDefinerExposure(ctx, conn, role.name); err == nil {
			g.SecurityDefiner = append(g.SecurityDefiner, fns...)
		}
	}
	g.SecurityDefiner = uniqueSorted(g.SecurityDefiner)
	return g, nil
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
