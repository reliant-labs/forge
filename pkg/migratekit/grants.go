package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"

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
		file string
		into *string
	}{{GrantsFile, &out.readOnly}, {DashboardGrantsFile, &out.dashboard}} {
		p := path.Join(path.Dir(dir), f.file)
		b, err := fs.ReadFile(fsys, p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return grantScripts{}, fmt.Errorf("read embedded grants (%s): %w", p, err)
		}
		*f.into = string(b)
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
				return dbgrants.Apply(ctx, conn, scripts.readOnly)
			}); err != nil {
				return nil, err
			}
			g.Applied = true
		}
	}
	if scripts.dashboard != "" {
		exists, err := dbgrants.RoleExists(ctx, conn, dashboardGrantsRole)
		if err != nil {
			return g, err
		}
		if !exists {
			g.DashboardRoleMissing = true
		} else {
			if err := guardedStatement(ctx, conn, run, "dashboard grants", func() error {
				return dbgrants.ApplyDashboard(ctx, conn, scripts.dashboard)
			}); err != nil {
				return g, err
			}
			g.DashboardApplied = true
		}
	}
	return g, nil
}
