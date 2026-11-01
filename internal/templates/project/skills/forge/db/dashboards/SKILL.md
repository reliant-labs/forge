---
name: dashboards
description: Dashboards over your database — the opt-in forge_dash role (ManagedDatabase.dashboards in KCL), the forge:dashboard table marker and forge:no-dashboard column marker, db/grants/dashboard_gen.sql and its per-table rows, the recorded limits (row cap, timeout, concurrency), kubedial credentials by role, and why the allowlist is Postgres privileges rather than an SQL parser.
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

Enabled, forge declares a second CNPG managed role, `forge_dash` (login, no role membership, connection limit `max_concurrency + 1`), with its password in the `<name>-forge-dash` Secret, minted the same way `<name>-forge-ro` is. Absent or `enabled = False`, the role still renders, but `ensure: present` with `login: false` and `disablePassword: true`: it cannot log in and has no password. It is deliberately not `ensure: absent`, because CNPG removes a role with a plain `DROP ROLE`, which Postgres refuses (`2BP01`) once the role holds a grant; the role would keep its login and password and the migrator would keep granting to it. The migrator never sees the KCL, so the role is the switch: `dashboard_gen.sql` grants only to a role that can log in, and for one that cannot it revokes everything and grants nothing (the run reports `dashboards are off`). Turning dashboards back on flips the same entry, the next migration grants again, and the `-forge-dash` Secret was kept, so the password does not rotate. A value out of range fails the render naming the field, and `dashboards` set on both the wrapper and its `spec` is refused. The limits are recorded in the release shape (`databases[].dashboards`, bundle feature `shape-database-dashboards`) and the control plane enforces them from there; an env that records nothing reads as **disabled**, never open.

## 2. The table opts in (a migration)

```sql
COMMENT ON TABLE usage_events IS 'forge:dashboard';
COMMENT ON COLUMN usage_events.user_email IS 'forge:no-dashboard';
```

`forge:dashboard` is a **table** marker and `forge:no-dashboard` a **column** marker, read the way `forge:no-entity` and `forge:secret` are: by whole token, so either coexists with other markers in the same comment (`'forge:append-only forge:dashboard'`). Declared on the wrong object, or misspelled, they fail `forge generate` and `forge lint --column-markers`. `forge generate` writes `db/grants/dashboard_gen.sql` plus one row per table under `db/grants/dashboard/` (`<schema>.<table>_gen.sql`), and the migrator splices the rows into the script and applies it right after the read-only grants on every run once `forge_dash` exists (a database without the role skips it silently; a script that fails fails the run). Commit them; `forge lint --db-grants` reports a file stale or missing when the markers and the files disagree.

What `forge_dash` can read is a strict subset of what `forge_ro` can:

```
tables marked forge:dashboard  ∩  columns forge_ro may read  −  forge:no-dashboard columns
```

So a `forge:secret` column, and a credential-shaped column nobody decided (`db/query`), is never reachable by a panel whatever `forge:no-dashboard` says. A view is never granted, even one marked: the catalog cannot say which base column a view exposes. Taking the last marker off a table revokes on the next run; the script is a REVOKE then GRANT, not a delta. The REVOKE reads what the catalog holds for the role (column, view, sequence, routine and schema privileges, and `ALTER DEFAULT PRIVILEGES` entries naming it), not what the declaration lists, so a column that gains `forge:no-dashboard` or `forge:secret` loses its access on the next run. A privilege the migrating role cannot revoke fails the run instead of standing silently. Privileges `PUBLIC` holds and role memberships are not the script's to remove.

### SECURITY DEFINER functions are a way around the column grants

The boundary holds for every expression the **role** evaluates. A `SECURITY DEFINER` function evaluates its body as its **owner**, so a column the role has no `SELECT` on is readable through one the role may execute, and Postgres grants `EXECUTE` on every new function to `PUBLIC` by default. `forge_ro` has the same exposure. Writes stay blocked (the caller's transaction is `READ ONLY` and holds inside the function); the column boundary does not.

forge generates no such function, so this is a limit rather than a hole in the script, but it is real. After the grants, each migration run lists the `SECURITY DEFINER` functions either role can execute and logs a warning naming them (it never fails the run). Fix it at the function: `REVOKE EXECUTE ON FUNCTION f(...) FROM PUBLIC;`, or do not write a definer function over a table with a `forge:secret` column.

### Not bounded by the privileges

The role cannot set per-role GUCs, and inside a read-only transaction `set_config('work_mem', ...)` and `set_config('statement_timeout', '0', true)` still succeed, so the privileges alone do not bound a query's memory. `dbquery`'s guard refuses `SET`, `RESET`, `set_config()` and the functions that run SQL text they are handed (`ts_stat`, `ts_rewrite`, `query_to_xml`, `dblink`, `crosstab`, ...), and the query service that runs panels sets its limits with `SET LOCAL` per query. A denylist over SQL is a speed bump, not a boundary: also set a cluster-level `temp_file_limit`. The script also tries `ALTER ROLE ... SET default_transaction_read_only`; under the application's owner role that is always a notice, and the caller's explicit `READ ONLY` transaction is the boundary.

## Why the allowlist is Postgres privileges, not an SQL parser

A panel runs arbitrary SQL, so something must decide which tables and columns it may touch. A parser-based allowlist (extract the table names, check them, forward the query) is a second SQL grammar that disagrees with Postgres's about subqueries, CTEs, `select *`, `row_to_json(t)`, `upper(col)`, set-returning functions and whatever the next release adds; every disagreement is a hole, and the parser can only ever be as complete as the day it was written. A column the role has no `SELECT` privilege on is refused by the server however the SQL reaches it, which is a check that cannot be out-parsed. So the parser (if any) is a convenience for good error messages and the privileges are the boundary, and the same reasoning keeps the role out of views and out of every role membership (`pg_read_all_data` would make the column grants moot).

Per-query bounds (row cap, statement timeout, concurrency) are the caller's: whatever connects as `forge_dash` (`pkg/dbquery` over `pkg/dbquery/kubedial`, `Client.CredentialsFor(ctx, db, kubedial.RoleDashboard)`) applies them from the recorded shape. `kubedial.PolicyRulesFor(kubedial.RoleDashboard, dbs...)` is the Kubernetes Role such a caller needs, and it names `<name>-forge-dash` and not `<name>-forge-ro`: a leaked panel runner reaches the narrower role only.
