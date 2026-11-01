package dbgrants

// # The dashboard role
//
// [DashboardRoleName] is the role live dashboard panels query as. It is opt-in
// per database (ManagedDatabase.dashboards in KCL), per table and per column,
// and what it may read is a SUBSET of what [RoleName] may read:
//
//	forge:dashboard tables  ∩  forge:dashboard columns  ∩  columns forge_ro may read
//
// # Every column is an explicit allow
//
// The table marker lets the role see a table; it does not let it read the
// table's columns. Each column is allowed by its own `forge:dashboard`
// comment, and `forge:no-dashboard` (or `forge:secret`) is the explicit
// deny. A column that declares neither is withheld. That is what makes the
// default fail closed: a column a later migration adds to a marked table (an
// email, a prompt) is unreadable by panels until someone decides it, instead
// of readable because nobody did. `forge lint --db-grants` reports every
// undecided column (rule forge-db-grants-dashboard-undecided), so the
// omission is loud, not silent; [DashboardUndecided] is what it reads.
//
// The allowlist lives in Postgres privileges, not in an SQL parser. A panel's
// query is arbitrary SQL, and any filter that reads it (a table allowlist, a
// blocklist of functions) is a second SQL grammar that disagrees with
// Postgres's about `select * from (select …)`, a CTE, a view or
// `upper(secret_col)`. A column the role has no SELECT privilege on is refused
// by the server whatever the SQL says, which is the only check that cannot be
// out-parsed. The same reasoning is why the role is never granted a view.
//
// The script is [RenderFor] with [DashboardRoleName] over [DashboardTables], so
// it is the same convergence as forge_ro's (REVOKE, then GRANT exactly the
// declared columns), no-ops when the role does not exist, and tries to set
// default_transaction_read_only (a notice, not a failure, when the migrating
// role may not: the caller's READ ONLY transaction is the boundary).
//
// # Switching dashboards off
//
// Off is a role that CANNOT LOG IN. The deploy renders forge_dash as
// `ensure: present, login: false, disablePassword: true` (pkg/deploy), never
// `absent`: CNPG removes a role with a plain DROP ROLE, which Postgres refuses
// (2BP01) once the role holds a grant, so an absent role would keep its login
// and password. The script reads the role's own rolcanlogin as the switch:
// while it is false the script revokes everything and grants nothing, and it
// grants again the run after the role can log in. The migrator never sees the
// KCL; the role it finds is the declaration.

// DashboardTables projects tables (every applied base table, as
// [Table]/[Column] declare them) onto the dashboard role: the same tables, each
// with only the columns the role may read.
//
// An unmarked table stays in the result with no columns rather than being
// dropped. The script revokes per SCHEMA it declares, so keeping the table is
// what makes removing the last forge:dashboard marker from a schema take the
// access away on the next run instead of leaving the old grant behind.
//
// On a marked table a column is granted only when it declares forge:dashboard,
// and it is judged by [Column.Disposition] as well, so a forge:secret column
// or a credential-shaped one nobody decided is withheld from the dashboard role
// exactly as from the read-only role, whatever forge:dashboard says.
// forge:no-dashboard wins over forge:dashboard. A column with no decision at
// all is withheld and listed in the result's [Table.Undeclared].
func DashboardTables(tables []Table) []Table {
	out := make([]Table, 0, len(tables))
	for _, t := range tables {
		d := Table{Schema: t.Schema, Name: t.Name, Dashboard: t.Dashboard}
		if t.Dashboard {
			for _, c := range t.Columns {
				switch {
				case c.Dashboard && !c.NoDashboard && c.Disposition() == Granted:
					d.Columns = append(d.Columns, c)
				case dashboardUndecided(c):
					d.Undeclared = append(d.Undeclared, c.Name)
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// dashboardUndecided: a column of a marked table that nobody decided for the
// dashboard role — no forge:dashboard, no forge:no-dashboard, no forge:secret.
func dashboardUndecided(c Column) bool { return !c.Dashboard && !c.NoDashboard && !c.Secret }

// DashboardUndecided returns every column of a forge:dashboard table that
// carries no dashboard decision (see [DashboardTables]), sorted: the columns
// the dashboard role is refused without anyone having said so. A
// credential-shaped column forge_ro withholds as undecided is included: the
// dashboard decision is a separate one, and forge:no-dashboard or forge:secret
// makes it.
func DashboardUndecided(tables []Table) []Finding {
	var out []Finding
	for _, t := range tables {
		if !t.Dashboard {
			continue
		}
		for _, c := range t.Columns {
			if dashboardUndecided(c) {
				out = append(out, Finding{Schema: t.Schema, Table: t.Name, Column: c.Name})
			}
		}
	}
	sortFindings(out)
	return out
}

// RenderDashboard is the dashboard role's grants script. tables is the same
// set [Render] takes (every base table, marked or not); it is deterministic,
// like [Render].
func RenderDashboard(tables []Table) (string, error) {
	return RenderDashboardFor(DashboardRoleName, tables)
}

// RenderDashboardFor is [RenderDashboard] for a named role, which roles being
// cluster-wide only tests need. It is the one place the login gate is on: see
// "Switching dashboards off" above.
func RenderDashboardFor(role string, tables []Table) (string, error) {
	return assembled(renderFiles(role, DashboardTables(tables), true))
}

// RenderDashboardFiles is [RenderDashboard] as a project commits it; see
// [Files].
func RenderDashboardFiles(tables []Table) (Files, error) {
	return renderFiles(DashboardRoleName, DashboardTables(tables), true)
}
