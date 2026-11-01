package migratekit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/reliant-labs/forge/pkg/dbgrants"
)

// THE DASHBOARD ROLE EXISTS BEFORE ANY MIGRATION RUNS.
//
// A project grants the dashboard role (dbgrants.DashboardRoleName) with
// ordinary migrations — `GRANT SELECT (id, cost) ON messages TO forge_dash;` —
// so the role must exist wherever those migrations run: a laptop's compose
// Postgres, a test template, CI, and production, with dashboards on or off. So
// every run, before it applies anything, creates the role NOLOGIN when the
// cluster has none (dbgrants.EnsureRole). An existing role is never altered:
// on a ManagedDatabase CNPG declares it, adopts the role this run created, and
// owns its LOGIN, password and connection limit.
//
// THE ONE ROLE THAT MAY NOT CREATE IT IS A MANAGED DATABASE'S OWN. Its
// application role has no CREATEROLE, and CNPG's managed roles can reconcile
// after the first migration Job starts. The run then goes on — a project that
// never grants the role must not be blocked by it — unless a migration it is
// about to apply names the role: that migration would fail part-way and leave
// the schema dirty, so the run refuses before applying anything, with a
// *DashboardRoleMissingError. The Job's retry, after CNPG has created the
// role, succeeds.
//
// THE ROLE HOLDS READ PRIVILEGES ONLY. After the migrations (and the read-only
// grants), the run reads what the role holds from the catalog and fails with a
// *DashboardPrivilegeError if it is anything but SELECT, schema USAGE and
// CONNECT (dbgrants.DashboardPrivilegeViolations): a write privilege, a grant
// option, an EXECUTE, a default privilege, a role membership or attribute. The
// migrations are committed by then, so the run keeps failing until a migration
// revokes it, and the release that over-granted never serves. It fails rather
// than warns because no database can legitimately be in that state: the
// dashboard role is read-only by definition, and the release before this one
// revoked everything it held on every run.
//
// Neither runs when the schema is ahead of this binary: nothing is applied,
// and the newer release's own run does both.

// dashboardRole is dbgrants.DashboardRoleName; a variable only so tests that
// share a Postgres server (and so a role namespace) can each use their own.
var dashboardRole = dbgrants.DashboardRoleName

// DashboardRole reports what a run found of the dashboard role. Nil on a
// Result means the run did not get that far (the schema was ahead).
type DashboardRole struct {
	// Created is true when the role did not exist and this run created it,
	// NOLOGIN.
	Created bool
	// Missing is true when the role does not exist and the migrating role may
	// not create it. No migration this run applied names it.
	Missing bool
	// CanLogin is true when dashboards are on: the deploy made the role
	// login-capable. Off, its grants stay in place and nobody can use them.
	CanLogin bool
	// SecurityDefiner lists the SECURITY DEFINER functions a login-capable
	// role can execute (dbgrants.SecurityDefinerExposure): a way around its
	// column grants. A warning for the caller to log, never a failure.
	SecurityDefiner []string
}

// DashboardRoleMissingError refuses a run whose pending migrations name the
// dashboard role while the database has none and the migrating role may not
// create it. Nothing was applied.
type DashboardRoleMissingError struct {
	Role string
	// Migrations are the pending files that name the role.
	Migrations []string
}

func (e *DashboardRoleMissingError) Error() string {
	return fmt.Sprintf("role %s does not exist and the migrating role may not create it (it has no CREATEROLE), "+
		"but pending migrations grant to it: %s. Nothing was applied. On a ManagedDatabase CNPG creates the role "+
		"when it reconciles the cluster: retry once it has. Elsewhere, have an administrator run `CREATE ROLE %s NOLOGIN`",
		e.Role, strings.Join(e.Migrations, ", "), e.Role)
}

// DashboardPrivilegeError fails a run after which the dashboard role holds
// more than read privileges.
type DashboardPrivilegeError struct {
	Role       string
	Violations []string
}

func (e *DashboardPrivilegeError) Error() string {
	return fmt.Sprintf("role %s may only read (SELECT, schema USAGE, CONNECT), but it holds: %s. "+
		"Revoke them in a new migration (`REVOKE ... FROM %s;`); grant it column SELECTs only. See `forge skill load db/dashboards`",
		e.Role, strings.Join(e.Violations, "; "), e.Role)
}

// dashboardRef is a migration whose SQL names the dashboard role.
type dashboardRef struct {
	version uint
	name    string
}

// scanDashboardRefs lists the migrations in source whose text names role as a
// whole word, case-insensitively. It reads comments and string bodies too: a
// false match only matters when the role is missing and cannot be created,
// and then refusing is the safe answer, where a missed DO-block GRANT would
// leave the schema dirty.
func scanDashboardRefs(fsys fs.FS, dir string, source sourceSet, role string) ([]dashboardRef, error) {
	var out []dashboardRef
	for _, m := range source {
		b, err := fs.ReadFile(fsys, path.Join(dir, m.Name))
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %s: %w", m.Name, err)
		}
		if namesWord(string(b), role) {
			out = append(out, dashboardRef{version: m.Version, name: m.Name})
		}
	}
	return out, nil
}

// namesWord reports whether text holds word, case-insensitively, with no
// identifier byte on either side.
func namesWord(text, word string) bool {
	lower, w := strings.ToLower(text), strings.ToLower(word)
	for i := 0; ; {
		j := strings.Index(lower[i:], w)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(w)
		if (start == 0 || !isIdentByte(lower[start-1])) && (end == len(lower) || !isIdentByte(lower[end])) {
			return true
		}
		i = start + 1
	}
}

// ensureDashboardRole is the run's first step on the dashboard role: create
// it when missing, or refuse the run when it cannot be created and a pending
// migration (one above settled's version) names it.
func ensureDashboardRole(ctx context.Context, conn *sql.Conn, refs []dashboardRef, settled State) (*DashboardRole, error) {
	created, err := dbgrants.EnsureRole(ctx, conn, dashboardRole)
	if err == nil {
		return &DashboardRole{Created: created}, nil
	}
	if !errors.Is(err, dbgrants.ErrRoleNotCreatable) {
		return nil, err
	}
	var pending []string
	for _, r := range refs {
		if !settled.Applied || r.version > settled.Version {
			pending = append(pending, r.name)
		}
	}
	if len(pending) > 0 {
		return nil, &DashboardRoleMissingError{Role: dashboardRole, Migrations: pending}
	}
	return &DashboardRole{Missing: true}, nil
}

// checkDashboardRole is the run's last step on the dashboard role: refuse
// anything but read privileges, and note whether it can log in.
func checkDashboardRole(ctx context.Context, conn *sql.Conn, d *DashboardRole) error {
	if d == nil || d.Missing {
		return nil
	}
	violations, err := dbgrants.DashboardPrivilegeViolations(ctx, conn, dashboardRole)
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return &DashboardPrivilegeError{Role: dashboardRole, Violations: violations}
	}
	_, d.CanLogin, err = dbgrants.RoleLogin(ctx, conn, dashboardRole)
	if err != nil {
		return err
	}
	if d.CanLogin {
		// Informational, like the read-only role's: a failed lookup does not
		// fail a run that succeeded.
		if fns, err := dbgrants.SecurityDefinerExposure(ctx, conn, dashboardRole); err == nil {
			d.SecurityDefiner = fns
		}
	}
	return nil
}
