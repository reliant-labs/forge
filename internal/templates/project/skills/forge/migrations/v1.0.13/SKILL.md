---
name: v1.0.13
description: Upgrade a project to forge v1.0.13. The dashboard role (forge_dash) is granted by ordinary migrations, not annotations - the forge:dashboard / forge:no-dashboard markers, the generated db/grants/dashboard_gen.sql and db/grants/dashboard/, and the forge-db-grants-stale lint rule are gone, and the migrator now creates forge_dash before any migration runs and fails a run after which it holds more than read privileges. Use when the project's migrations carry forge:dashboard or forge:no-dashboard, it commits db/grants/dashboard_gen.sql, or Go code reads the removed dashboard fields of pkg/migratekit or pkg/dbgrants.
version: v1.0.13
detection: test -f db/grants/dashboard_gen.sql -o -d db/grants/dashboard && exit 0; grep -rqsE --include='*.go' --exclude-dir=gen --exclude-dir=vendor --exclude-dir=node_modules 'Dashboard(Applied|RoleDisabled|RoleMissing|GrantsPath|GrantsRowsDir|GrantsFile|Tables)\b|RenderDashboard|ApplyDashboard|DeclaresNoDashboard|TableMarkerDashboard|ColumnMarkerNoDashboard' . && exit 0; exit 1
---

# Upgrading to v1.0.13

**What changed.** forge stopped deciding what the dashboard role reads. `forge_dash` keeps its whole lifecycle (the `ManagedDatabase.dashboards` opt-in, the CNPG managed role and its connection limit, the `<name>-forge-dash` password Secret, NOLOGIN when off), but its access is now granted by your own migrations:

```sql
GRANT SELECT (id, created_at, model, cost_cents) ON messages TO forge_dash;
```

Gone: the `forge:dashboard` table marker, the `forge:no-dashboard` column marker, the generated `db/grants/dashboard_gen.sql` and `db/grants/dashboard/`, the migrator step that applied them, and the `forge-db-grants-stale` lint rule. Unchanged: `forge_ro`, its `forge:secret` / `forge:readable` markers and `db/grants/readonly*`.

New, in every migration run (`db migrate up`, boot-time AutoMigrate, `testkit.NewMigratedPostgresDB`): the migrator creates `forge_dash` NOLOGIN before applying anything when the cluster has none, so a migration may GRANT to it everywhere; and after the migrations it **fails** if `forge_dash` holds anything but `SELECT`, schema `USAGE` and `CONNECT`. The contract and the rules for writing grants: `forge skill load db/dashboards`.

Apply the sections whose "Use when" matches.

## 1. Turn the markers into a GRANT migration

**Use when** a migration carries `forge:dashboard` or `forge:no-dashboard`, or the project commits `db/grants/dashboard_gen.sql`.

On v1.0.13 `forge generate` fails while the applied schema still carries either marker (`forge:dashboard was retired in forge v1.0.13 …`), and it deletes forge's own copies of the old dashboard grants files. So read what they granted **before** regenerating, from git if generate already ran:

```bash
ls db/grants/dashboard/                                   # one file per table
git show HEAD:db/grants/dashboard/public.messages_gen.sql # if generate removed them
```

Each row file holds one row, `('<schema>', '<table>', ARRAY['col', …]::text[])`: the columns forge granted (the marked table's columns `forge_ro` may read, minus the `forge:no-dashboard` ones). A row with `ARRAY[]::text[]` is an unmarked table and grants nothing.

1. `forge db migration new dashboard_grants`.
2. For each row with columns, write `GRANT SELECT (<those columns>) ON <schema>.<table> TO forge_dash;`. Review each list against the rules in `db/dashboards` before you keep it: the old projection granted every readable column of a marked table, and a panel usually needs fewer. Drop any PII or free text.
3. In the same migration, clear the markers, because the retired ones fail generate. `COMMENT ON` replaces the whole comment, so keep any other marker or text it carried:
   ```sql
   COMMENT ON TABLE usage_events IS 'forge:append-only';   -- was 'forge:append-only forge:dashboard'
   COMMENT ON TABLE messages IS NULL;                      -- was 'forge:dashboard'
   COMMENT ON COLUMN chats.title IS NULL;                  -- was 'forge:no-dashboard'
   ```
4. Head the file `-- forge:backward-compatible — dashboard grants and comments only`.
5. Leave the migration that added the markers as it is: it has run everywhere. `forge lint --column-markers` does not report a retired marker in migration text; `forge generate` judges the applied schema.
6. `forge generate`, then commit the migration together with the deletions of `db/grants/dashboard_gen.sql` and `db/grants/dashboard/`. A hand-edited or disowned copy is left for you: delete it.

**Databases already deployed** keep the column grants the old script last applied until this migration runs; nothing revokes them in between. `GRANT` is idempotent, so the migration changes nothing there unless you narrowed a list, and makes a fresh database match. To narrow what a deployed database already holds, revoke explicitly in the same migration (`REVOKE ALL ON <table> FROM forge_dash;` before the `GRANT`): granting fewer columns does not take the others away.

## 2. The Go API lost its dashboard-grants half

**Use when** Go code names any of these.

| Removed | Instead |
|---|---|
| `migratekit.Grants.DashboardApplied`, `.DashboardRoleDisabled`, `.DashboardRoleMissing` | `migratekit.Result.Dashboard` (`*migratekit.DashboardRole`: `Created`, `Missing`, `CanLogin`, `SecurityDefiner`) |
| `migratekit.DashboardGrantsFile`, `migratekit.DashboardGrantsRowsDir` | nothing: there is no file |
| `dbgrants.DashboardTables`, `RenderDashboard`, `RenderDashboardFor`, `RenderDashboardFiles`, `ApplyDashboard`, `DashboardGrantsPath`, `DashboardGrantsRowsDir`, `Table.Dashboard`, `Column.NoDashboard` | grants are migrations; `dbgrants.EnsureDashboardRole` / `EnsureRole` and `dbgrants.DashboardPrivilegeViolations` are the lifecycle and the check |
| `schemadef.TableMarkerDashboard`, `ColumnMarkerNoDashboard`, `Table.Dashboard()`, `Column.DeclaresNoDashboard()` | `schemadef.RetiredMarker` |

A run can now fail two new ways: `*migratekit.DashboardRoleMissingError` (the role does not exist, the migrating role may not create it, and a pending migration names it; nothing was applied, so retry once the role exists) and `*migratekit.DashboardPrivilegeError` (the role holds a non-read privilege; fix with a migration that revokes it).

**A migration runner of your own** (one that applies the files without `migratekit` or `testkit`) must call `dbgrants.EnsureDashboardRole(ctx, db)` before the first file, or a migration that grants to `forge_dash` fails on a fresh database with `role "forge_dash" does not exist`.

## 3. Delete `forge-db-grants-stale` from `lint.rules`

**Use when** `forge.yaml` names `forge-db-grants-stale` under `lint.rules`.

The rule is gone, so the entry is reported as a rule ID nothing has. Delete it.

## 4. `forge db squash` now keeps the dashboard role's grants

**Use when** you squash a forge history (`forge db squash`).

The squash used to dump with `--no-privileges`. It now keeps every `GRANT … TO forge_dash` and drops every other privilege statement, so a baseline that replaces your GRANT migrations still grants what they did. Nothing to do; re-squash if a baseline written by an earlier forge replaced a GRANT migration.
