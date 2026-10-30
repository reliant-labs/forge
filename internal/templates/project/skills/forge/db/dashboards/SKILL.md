---
name: dashboards
description: Dashboards over your database — the opt-in forge_dash role (ManagedDatabase.dashboards in KCL), the forge:dashboard table marker and forge:no-dashboard column marker, db/grants/dashboard_gen.sql, the recorded limits (row cap, timeout, concurrency), kubedial credentials by role, and why the allowlist is Postgres privileges rather than an SQL parser.
---

# Dashboards over your database

Live dashboard panels (a spend chart, a cohort table) can query your application's own Postgres. It is **opt-in at two levels** and off by default: a database enables it, and a table is marked into it. Nothing else is reachable. The summary is in the `db` skill; this is the depth.

## 1. The database opts in (KCL)

```python
forge.ManagedDatabase {
    name = "orders"
    dashboards = forge.DatabaseDashboards {
        enabled = True          # default False
        row_cap = 1000          # 1-10000: most rows one panel query returns
        timeout = "5s"          # "1s".."10s": statement timeout of one panel query
        max_concurrency = 2     # 1-4: panel queries at once
    }
}
```

Enabled, forge declares a second CNPG managed role, `forge_dash` (login, no role membership, connection limit `max_concurrency + 1`), with its password in the `<name>-forge-dash` Secret, minted the same way `<name>-forge-ro` is. Absent or `enabled = False`, the role renders `ensure: absent`: switching dashboards off removes the login rather than orphaning it. A value out of range fails the render naming the field, and `dashboards` set on both the wrapper and its `spec` is refused. The limits are recorded in the release shape (`databases[].dashboards`, bundle feature `shape-database-dashboards`) and the control plane enforces them from there; an env that records nothing reads as **disabled**, never open.

## 2. The table opts in (a migration)

```sql
COMMENT ON TABLE usage_events IS 'forge:dashboard';
COMMENT ON COLUMN usage_events.user_email IS 'forge:no-dashboard';
```

`forge:dashboard` is a **table** marker and `forge:no-dashboard` a **column** marker, read the way `forge:no-entity` and `forge:secret` are: by whole token, so either coexists with other markers in the same comment (`'forge:append-only forge:dashboard'`). Declared on the wrong object, or misspelled, they fail `forge generate` and `forge lint --column-markers`. `forge generate` writes `db/grants/dashboard_gen.sql`, and the migrator applies it right after `readonly_gen.sql` on every run once `forge_dash` exists (a database without the role skips it silently; a script that fails fails the run). Commit it; `forge lint --db-grants` reports it stale or missing when the markers and the file disagree.

What `forge_dash` can read is a strict subset of what `forge_ro` can:

```
tables marked forge:dashboard  ∩  columns forge_ro may read  −  forge:no-dashboard columns
```

So a `forge:secret` column, and a credential-shaped column nobody decided (`db/query`), is never reachable by a panel whatever `forge:no-dashboard` says. A view is never granted, even one marked: the catalog cannot say which base column a view exposes. Taking the last marker off a table revokes on the next run; the script is a REVOKE then GRANT, not a delta. The REVOKE reads what the catalog holds for the role (column, view, sequence, routine and schema privileges, and `ALTER DEFAULT PRIVILEGES` entries naming it), not what the declaration lists, so a column that gains `forge:no-dashboard` or `forge:secret` loses its access on the next run. A privilege the migrating role cannot revoke fails the run instead of standing silently. Privileges `PUBLIC` holds and role memberships are not the script's to remove: keep both empty of anything sensitive.

## Why the allowlist is Postgres privileges, not an SQL parser

A panel runs arbitrary SQL, so something must decide which tables and columns it may touch. A parser-based allowlist (extract the table names, check them, forward the query) is a second SQL grammar that disagrees with Postgres's about subqueries, CTEs, `select *`, `row_to_json(t)`, `upper(col)`, set-returning functions and whatever the next release adds; every disagreement is a hole, and the parser can only ever be as complete as the day it was written. A column the role has no `SELECT` privilege on is refused by the server however the SQL reaches it, which is a check that cannot be out-parsed. So the parser (if any) is a convenience for good error messages and the privileges are the boundary, and the same reasoning keeps the role out of views and out of every role membership (`pg_read_all_data` would make the column grants moot).

Per-query bounds (row cap, statement timeout, concurrency) are the caller's: whatever connects as `forge_dash` (`pkg/dbquery` over `pkg/dbquery/kubedial`, `Client.CredentialsFor(ctx, db, kubedial.RoleDashboard)`) applies them from the recorded shape. `kubedial.PolicyRulesFor(kubedial.RoleDashboard, dbs...)` is the Kubernetes Role such a caller needs, and it names `<name>-forge-dash` and not `<name>-forge-ro`: a leaked panel runner reaches the narrower role only.

