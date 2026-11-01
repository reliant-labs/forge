---
name: dashboards
description: Dashboards over your database — the forge_dash role and its lifecycle (ManagedDatabase.dashboards in KCL turns its login on), granting it exactly the columns a panel needs with an ordinary migration, revoking in a later one, the migrator's read-only check, the recorded limits (row cap, timeout, concurrency), kubedial credentials by role, and why the allowlist is Postgres privileges rather than an SQL parser.
---

# Dashboards over your database

Live dashboard panels (a spend chart, a cohort table) can query your application's own Postgres as one role, `forge_dash`. forge owns the role's **lifecycle**; **you** decide what it reads, with ordinary migrations. The summary is in the `db` skill; this is the depth.

| forge does | you do |
|---|---|
| creates `forge_dash` (NOLOGIN) in every database before any migration runs | write a migration that GRANTs it the columns a panel needs |
| lets it log in, with a password Secret and a connection limit, when the database enables dashboards | write a later migration that REVOKEs what a panel no longer needs |
| fails the migration run if the role holds anything but read privileges | keep PII and secrets out of every GRANT |

## 1. The database turns dashboards on (KCL)

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

Enabled, the ManagedDatabase renders `forge_dash` as a CNPG managed role that can log in (no role membership, connection limit `max_concurrency + 1`), with its password in the `<name>-forge-dash` Secret, minted the way `<name>-forge-ro` is. Absent or `enabled = False`, the role still renders, as `ensure: present` with `login: false` and `disablePassword: true`: it cannot log in and has no password.

**Off means NOLOGIN, and your grants stay.** Nothing revokes them: they are your migrations. They are unusable while the role cannot log in, and they serve again the moment it can. Turning dashboards back on flips the same entry, and the `-forge-dash` Secret was kept, so the password does not rotate. It is deliberately not `ensure: absent`: CNPG removes a role with a plain `DROP ROLE`, which Postgres refuses (`2BP01`) once the role holds a grant, so the role would keep its login and its password.

A value out of range fails the render naming the field, and `dashboards` set on both the wrapper and its `spec` is refused. The limits are recorded in the release shape (`databases[].dashboards`, bundle feature `shape-database-dashboards`) and the control plane enforces them from there; an env that records nothing reads as **disabled**, never open.

## 2. Grant a panel its columns (a migration)

```bash
forge db migration new dashboard_spend_by_model
```

```sql
-- forge:backward-compatible — grants only; no schema change
GRANT SELECT (id, created_at, model, cost_cents) ON messages TO forge_dash;
```

That is the whole mechanism. The migration applies in dev, in tests, in CI and in production alike, with dashboards on or off, because the migrator has created the role before it runs. Follow these rules for every GRANT:

1. **Grant exactly the columns the panel's query reads.** Write the panel's SQL first, list every column it touches (in `SELECT`, `WHERE`, `GROUP BY`, `JOIN ... ON`, `ORDER BY`), and grant those. A query naming a column the role lacks fails with `permission denied for table`, which is how you find the one you missed. `SELECT *` fails on any table the role cannot read whole, so do not write panels with it.
2. **Prefer a column list to a whole table.** `GRANT SELECT ON messages TO forge_dash` covers every column the table will EVER have, including a secret one added next quarter. A column list does not grow. Grant a whole table only when every current column is fine to show and the table cannot plausibly gain a sensitive one (a lookup table of plan names).
3. **Never grant PII or secrets.** Not emails, names, phone numbers, addresses, IPs, free text a user typed (titles, notes, prompts, messages), and not tokens, keys, password or token hashes, ciphertext, or anything `forge:secret`. When in doubt, check the table's row in `db/grants/readonly/<schema>.<table>_gen.sql`: the columns listed there as withheld (`forge:secret`, or credential-shaped and undecided) are never for a panel. A panel that needs a person groups by an opaque id (`user_id`), not an email.
4. **Grant base-table columns, not views.** A view reads its tables with its owner's privileges, so granting a view grants every column it selects, whatever the column grants say. If a panel needs an aggregate, write it in the panel's SQL over the granted columns.
5. **Another schema needs USAGE too.** `public` is usable by every role by default; for a table in another schema add `GRANT USAGE ON SCHEMA billing TO forge_dash;`.
6. **Grant nothing else.** Only `SELECT` (on tables, columns and sequences), schema `USAGE` and database `CONNECT` are allowed; see section 4.

### Removing access is a later migration

Never edit or delete the migration that granted it: it has run in every environment. Revoke in a new one:

```sql
-- forge:backward-compatible — revokes a dashboard grant; no schema change
REVOKE SELECT (cost_cents) ON messages FROM forge_dash;   -- one column
REVOKE ALL ON messages FROM forge_dash;                   -- the whole table, column grants included
```

A column a migration drops takes its grant with it. Renaming a column keeps its grant under the new name.

## 3. The role exists before your first migration

Every migration run (`db migrate up`, the server's boot-time AutoMigrate, a test database) first creates `forge_dash` as `NOLOGIN NOINHERIT` when the cluster has none, and otherwise leaves the existing role exactly as it is. On a ManagedDatabase, CNPG declares the role too and **adopts** the one the migrator created: its reconciler finds a declared role in the catalog and ALTERs it to the declared attributes (login, password, connection limit, comment) instead of creating it.

One role may not create it: a ManagedDatabase's own application role has no `CREATEROLE`, and CNPG's managed roles can reconcile after the first migration Job starts. The run then continues if no pending migration names `forge_dash`. If one does, the run refuses **before applying anything** (`role forge_dash does not exist and the migrating role may not create it … Nothing was applied`), so the schema is not left dirty; the Job's retry, after CNPG has created the role, succeeds. On a database you run yourself with a migrating role that lacks `CREATEROLE`, have an administrator run `CREATE ROLE forge_dash NOLOGIN` once.

## 4. The migrator's one check: read privileges only

After the migrations, every run reads what `forge_dash` holds from the catalog and **fails** if it is anything but `SELECT`, schema `USAGE` and database `CONNECT`:

- `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER` on a table or column, `USAGE`/`UPDATE` on a sequence, `CREATE` on a schema, `CREATE`/`TEMPORARY` on the database;
- any privilege held `WITH GRANT OPTION`;
- `EXECUTE` granted to the role itself (PUBLIC's default `EXECUTE` is not counted; see SECURITY DEFINER below);
- an `ALTER DEFAULT PRIVILEGES ... TO forge_dash` entry, which would hand it every table created later instead of the columns you named;
- membership in any role (`pg_read_all_data` would make every column grant moot), and the `SUPERUSER`, `CREATEROLE`, `CREATEDB`, `REPLICATION` and `BYPASSRLS` attributes.

The error names each privilege. The offending migration has already committed, so every run fails until a new migration revokes it (`REVOKE INSERT ON messages FROM forge_dash;`): the release that over-granted never serves. Test databases (`testkit.NewMigratedPostgresDB`, `app.NewMigratedTestDB`) apply the same check, so `task test` catches it before review does.

To see what a panel could read, act as the role in a test against a migrated database: `SET ROLE forge_dash;` then run the panel's SQL.

### SECURITY DEFINER functions are a way around the column grants

The boundary holds for every expression the **role** evaluates. A `SECURITY DEFINER` function evaluates its body as its **owner**, so a column the role has no `SELECT` on is readable through one the role may execute, and Postgres grants `EXECUTE` on every new function to `PUBLIC` by default. `forge_ro` has the same exposure. Writes stay blocked (the caller's transaction is `READ ONLY` and holds inside the function); the column boundary does not.

forge generates no such function, but it is real. While dashboards are on, each migration run lists the `SECURITY DEFINER` functions `forge_dash` (or `forge_ro`) can execute and logs a warning naming them (it never fails the run). Fix it at the function: `REVOKE EXECUTE ON FUNCTION f(...) FROM PUBLIC;`, or do not write a definer function over a table with a sensitive column.

### Not bounded by the privileges

The role cannot set per-role GUCs, and a panel runs in a session where `set_config('work_mem', ...)` and `set_config('statement_timeout', '0', true)` succeed, so the row cap, the statement timeout and the concurrency cap do not bound a query's memory. `pkg/dbquery`, which runs panels, sets its timeouts for the panel's own transaction and keeps a client-side deadline behind them; set a cluster-level `temp_file_limit` as well. The caller's explicit `READ ONLY` transaction is what keeps a panel from writing, beside the privileges.

## Why the allowlist is Postgres privileges, not an SQL parser

A panel runs arbitrary SQL, so something must decide which tables and columns it may touch. A parser-based allowlist (extract the table names, check them, forward the query) is a second SQL grammar that disagrees with Postgres's about subqueries, CTEs, `select *`, `row_to_json(t)`, `upper(col)`, set-returning functions and whatever the next release adds; every disagreement is a hole, and the parser can only ever be as complete as the day it was written. A column the role has no `SELECT` privilege on is refused by the server however the SQL reaches it, which is a check that cannot be out-parsed. So the parser (if any) is a convenience for good error messages and the privileges are the boundary, and the same reasoning keeps the role out of views and out of every role membership.

Per-query bounds (row cap, statement timeout, concurrency) are the caller's: whatever connects as `forge_dash` (`pkg/dbquery` over `pkg/dbquery/kubedial`, `Client.CredentialsFor(ctx, db, kubedial.RoleDashboard)`) applies them from the recorded shape. `kubedial.PolicyRulesFor(kubedial.RoleDashboard, dbs...)` is the Kubernetes Role such a caller needs, and it names `<name>-forge-dash` and not `<name>-forge-ro`: a leaked panel runner reaches the narrower role only.

## Checklist: adding a panel

1. Write the panel's SQL. List every column it reads.
2. Drop any column that is PII, free text or a secret; change the panel to group by an id instead.
3. `forge db migration new dashboard_<panel>`; write one `GRANT SELECT (<columns>) ON <table> TO forge_dash;` per table, with `-- forge:backward-compatible — grants only`.
4. `task test:short -- ./internal/<pkg>/...` (or any test that migrates a database): the migrator's read-only check runs there.
5. Turn dashboards on for the database in KCL if they are not already.
