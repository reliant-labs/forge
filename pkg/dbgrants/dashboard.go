package dbgrants

// # The dashboard role
//
// [DashboardRoleName] is the role live dashboard panels query as. It is opt-in
// per database (ManagedDatabase.dashboards in KCL) and per table, and what it
// may read is a SUBSET of what [RoleName] may read:
//
//	forge:dashboard tables  ∩  columns forge_ro may read  −  forge:no-dashboard columns
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
// declared columns), no-ops when the role does not exist, and sets
// default_transaction_read_only.

// DashboardTables projects tables (every applied base table, as
// [Table]/[Column] declare them) onto the dashboard role: the same tables, each
// with only the columns the role may read.
//
// An unmarked table stays in the result with no columns rather than being
// dropped. The script revokes per SCHEMA it declares, so keeping the table is
// what makes removing the last forge:dashboard marker from a schema take the
// access away on the next run instead of leaving the old grant behind.
//
// Columns are judged by [Column.Disposition] first, so a forge:secret column
// or a credential-shaped one nobody decided is withheld from the dashboard role
// exactly as from the read-only role, whatever forge:no-dashboard says.
func DashboardTables(tables []Table) []Table {
	out := make([]Table, 0, len(tables))
	for _, t := range tables {
		d := Table{Schema: t.Schema, Name: t.Name, Dashboard: t.Dashboard}
		if t.Dashboard {
			for _, c := range t.Columns {
				if c.Disposition() == Granted && !c.NoDashboard {
					d.Columns = append(d.Columns, c)
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// RenderDashboard is the dashboard role's grants script. tables is the same
// set [Render] takes (every base table, marked or not); it is deterministic,
// like [Render].
func RenderDashboard(tables []Table) (string, error) {
	return RenderFor(DashboardRoleName, DashboardTables(tables))
}
