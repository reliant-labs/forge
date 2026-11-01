---
name: v1.0.13
description: Upgrade a project to forge v1.0.13. A table marked forge:dashboard no longer grants its columns to the dashboard role (forge_dash) by default; each column needs its own `forge:dashboard` (allow) or `forge:no-dashboard` / `forge:secret` (deny), and an undecided column is withheld and reported by `forge lint --db-grants`. Use when a migration marks a table forge:dashboard.
version: v1.0.13
detection: git grep -q -l -E "COMMENT ON TABLE .*IS '[^']*forge:dashboard" -- db/migrations 2>/dev/null && exit 0; exit 1
---

# Upgrading to v1.0.13

## A dashboard table's columns are allowed one by one

**Use when** a migration marks a table `COMMENT ON TABLE t IS 'forge:dashboard'`.

**What changed.** The dashboard role (`forge_dash`) used to read every column of a marked table that `forge_ro` may read, minus the `forge:no-dashboard` ones. A column a later migration added to the table was therefore readable by every live panel the moment it existed, whether or not anyone had thought about it: a new email, prompt or phone column became panel-visible by default.

The table marker now only lets the role see the table. A column is granted when it declares `forge:dashboard` itself:

```sql
COMMENT ON COLUMN usage_events.model IS 'forge:dashboard';
COMMENT ON COLUMN usage_events.user_email IS 'forge:no-dashboard';
```

A column with no decision is withheld, so the default fails closed. `forge:no-dashboard` and `forge:secret` are the explicit deny, and they win. A `forge:secret` or undecided credential-shaped column stays withheld from both roles whatever `forge:dashboard` says.

**What generate does.** The first `forge generate` on v1.0.13 rewrites `db/grants/dashboard/<schema>.<table>_gen.sql` for each marked table. A table whose columns declare nothing is granted nothing, and its row lists the undecided columns in a comment. Commit the rewritten files. **Until you add the column markers, `forge_dash` cannot read those tables, and panels over them fail with `permission denied`.** `forge_ro` and `forge db query` are unchanged.

**What you do.** Run `forge lint --db-grants`. Each `forge-db-grants-dashboard-undecided` finding names one column and the migration line that created it. In one new migration, declare every column. `COMMENT ON` replaces the whole comment, so keep any marker already on the column (`'forge:go-type=... forge:dashboard'`). Then run `forge generate` and commit. The rule warns while `forge_version` is v1.0.13 and gates from v1.0.14.

`forge generate` also now fails on a column `forge:dashboard` that does nothing. That covers one on a table that is not itself marked, one on a view, and one beside `forge:no-dashboard` or `forge:secret` in the same comment. So to take a table out of dashboards, clear its columns' markers in the same migration.
