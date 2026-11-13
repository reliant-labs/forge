# `forge db analyze`: empirical analysis of migrations and queries

Status: accepted (owner, 2026-11). Phase 1 in progress.

## Problem

`forge lint --migration-safety` (`internal/linter/migrationlint`) reads migration SQL as
text. It cannot tell whether an `ALTER COLUMN … TYPE` rewrites the table, which lock a
statement takes, or whether a constraint validation scans the whole table: those depend on
the column types, the Postgres version and the existing schema. Nothing at all looks at the
queries an app runs: a new filter or join with no index merges unnoticed, and the generated
list search over text columns is the likeliest sequential scan in every project.

## Decision

forge owns the mechanism and a stable JSON document; reviewers (the Reliant `pr-review`
workflow, or any user's) consume it.

- **Deterministic findings gate**, and every new rule warns first through the lint rollout
  table (`internal/cli/lint/lint_rollout_table.go`).
- **LLM judgement is advisory**: the reviewer reads the JSON (plans, locks, sizes, file:line)
  and comments; it never blocks on its own verdict.
- **The database it runs against is configurable.** The default is a scratch Postgres
  (`forge/pkg/pgtest`), which needs nothing. A project can point it at a database with real
  data and real statistics, typically an anonymized snapshot of prod, and get answers that
  depend on size (durations, real plans).
- It needs Postgres, so it is **never in a fast PR tier** (`task ci`). It runs in the review
  lane, `ci:full`, or on demand.

## Configuration

```yaml
# forge.yaml
database:
  analyze:
    target:
      # scratch (default): pgtest's embedded Postgres (or FORGE_TEST_POSTGRES_URL),
      #   migrated to the base, then the change applied.
      # dsn: a database you provide, e.g. an anonymized prod snapshot.
      kind: dsn
      # The NAME of the env var holding the DSN. A DSN is never written in forge.yaml.
      dsn_env: FORGE_ANALYZE_DATABASE_URL
      statement_timeout: 60s   # per statement; a timeout is itself a finding
      lock_timeout: 5s
    # Size hints for a scratch target (it has no rows). Ignored when the target has
    # statistics of its own (a dsn target that has been ANALYZEd).
    table_rows:
      orders: 40000000
    large_table_rows: 1000000  # at or above this a table is "large"
    small_table_rows: 10000    # below this, scan/index findings are dropped
```

Flags override per run: `--target scratch|dsn`, `--dsn-env NAME`, `--base REV`, `--json`.
Rule severities go through `lint.rules` like every other rule (`{db-analyze-…: off}`).

### Safety of a `dsn` target

The target is never changed:

- Everything runs in **one session, one transaction, always rolled back**: `BEGIN`,
  `SET LOCAL statement_timeout/lock_timeout`, apply pending migrations statement by
  statement, measure, `EXPLAIN`, `ROLLBACK`. Postgres DDL is transactional.
- A statement that cannot run in a transaction (`CREATE INDEX CONCURRENTLY`, a file marked
  no-transaction) is **not executed**. Its lock is known statically (`SHARE UPDATE
  EXCLUSIVE`) and is reported as such, with `executed: false`. For the query pass the index
  is created non-concurrently inside the rolled-back transaction so plans can use it.
- The DSN is refused when it reconciles (`databaseIdentity`, `internal/cli/db_target.go`)
  with a database a non-development deploy env declares. Point it at a snapshot, not prod.
- On a snapshot with data, a rewrite or a scan really runs (bounded by
  `statement_timeout`) and is rolled back. That cost is the point: it measures it.

A `clone` isolation mode (`CREATE DATABASE … TEMPLATE`) was considered and left out of v1:
a rolled-back transaction gives the same answers without a full copy.

## What is analyzed

`--base REV` (default: the merge-base with the remote default branch) splits the migration
directory:

- **scratch**: migrations unchanged since base are applied first as setup (a pgtest
  template keyed by their content, so the setup is reused); migrations added or changed
  since base are applied one statement at a time and analyzed.
- **dsn**: every migration the target's `schema_migrations` lacks is analyzed (it will run
  against prod-shaped data). A target ahead of the tree (versions the tree does not have) is
  an error (exit 2).

An empty set is a verdict, not a silent pass: the document says scanned 0, and why.

## Phase 1: migrations

Per executed statement, inside the transaction:

| Measured | How |
|---|---|
| locks taken | diff of `pg_locks WHERE pid = pg_backend_pid()` before/after, mapped to relation names and modes |
| table/index rewrite | `pg_class.relfilenode` of every user relation before/after |
| scans | `pg_stat_xact_user_tables` (`seq_scan`, `seq_tup_read`) delta |
| duration | wall time (meaningful on a dsn target with data) |
| size | `reltuples` on a target with statistics, else `table_rows`, else unknown |

A relation created earlier in the same migration run is "new" and exempt: locking or
rewriting an empty table nobody reads yet is free.

Rules (each warn-first via the rollout table; IDs prefixed `db-analyze-`):

- `db-analyze-table-rewrite`: a pre-existing table or its indexes were rewritten.
- `db-analyze-access-exclusive-lock`: a pre-existing table held `ACCESS EXCLUSIVE` (blocks
  reads and writes) across a statement that also scanned or rewrote it.
- `db-analyze-validation-scan`: a full scan of a pre-existing table under a lock that blocks
  writes (`ADD CONSTRAINT` without `NOT VALID`, `SET NOT NULL` without a `CHECK`).
- `db-analyze-blocking-index-build`: `CREATE INDEX` without `CONCURRENTLY` on a
  pre-existing table (`SHARE` lock: blocks writes for the whole build).
- `db-analyze-statement-timeout`: a statement exceeded `statement_timeout` on the target.

Findings below `small_table_rows` (when the size is known) are dropped. The textual
`--migration-safety` lane stays: it is fast and needs no Postgres; this lane is the
empirical complement.

## Phase 2: query capture

`forge/pkg/orm` installs one more bun query hook **only when `FORGE_QUERY_CAPTURE_DIR` is
set** (zero cost otherwise). Running the tests with it set
(`FORGE_QUERY_CAPTURE_DIR=… task test:short`) records each distinct query shape: the SQL
with `$n` placeholders (forge binds values, `pkg/crud/bind.go`), normalized (whitespace,
`IN` lists), its fingerprint, and its caller: the first stack frame outside bun,
`database/sql` and forge's own `pkg/orm` and `pkg/crud`. One JSONL file per process.

Queries no test runs are invisible. That is the intended incentive.

## Phase 3: query plans

`forge db analyze queries --capture DIR` selects the shapes whose caller is in a file
changed since base (`--all` for every one), applies the head schema on the target in the
rolled-back transaction, and explains each with `EXPLAIN (GENERIC_PLAN, FORMAT JSON)`
(Postgres 16+; older is exit 2) twice: as is, and with `SET LOCAL enable_seqscan = off`.

- `db-analyze-query-no-usable-index`: with `enable_seqscan = off` the plan still seq-scans
  a table under a filter or join condition, so no index can serve the predicate. True at
  any size, so it works on an empty scratch database.
- `db-analyze-query-large-seqscan` (needs real statistics): the normal plan seq-scans a
  table at or above `large_table_rows`.

Whether a scratch target can borrow row counts (Postgres 18's
`pg_restore_relation_stats`) is a spike: the planner also scales by the relation's
on-disk size, which is zero on an empty table. See `db-analyze-stats-spike.md`.

## Phase 4: review integration (control-plane)

`pr-review` runs `forge db analyze --json` when a diff touches `db/migrations/`, the ORM
package or `*_repo_ext.go`, and gives the document to the reviewer model with the diff.
Deterministic findings are reported as such; the model's judgement ("this table is hot,
this lock is unacceptable") is a comment.

## Later: production feedback

`auto_explain` and `pg_stat_statements` on hosted Postgres, surfaced through `forge obs`,
show the plan regressions pre-merge analysis cannot.

## Output document (`--json`)

```json
{
  "schema": "forge.db-analyze/v1",
  "target": {"kind": "scratch", "identity": "localhost:5432/forge_analyze_x", "server_version": "18.1", "has_statistics": false},
  "base": "origin/main@abc123",
  "migrations": [{
    "file": "db/migrations/20261101_add_status.up.sql", "version": 20261101, "analyzed": true,
    "statements": [{
      "index": 0, "line": 3, "sql": "ALTER TABLE orders ALTER COLUMN status TYPE text",
      "executed": true, "duration_ms": 12,
      "locks": [{"relation": "orders", "mode": "AccessExclusiveLock"}],
      "rewrites": [{"relation": "orders", "kind": "table"}],
      "scans": [{"relation": "orders", "seq_scan": 1, "seq_tup_read": 0}],
      "rows": {"orders": {"value": 40000000, "source": "table_rows"}}
    }]
  }],
  "queries": [],
  "findings": [{"rule": "db-analyze-table-rewrite", "severity": "warning", "file": "…", "line": 3,
                "relation": "orders", "message": "…", "fix": "…"}],
  "summary": {"migrations_scanned": 1, "statements_executed": 1, "queries_scanned": 0, "skipped": []},
  "exit_code": 0
}
```

Exit codes: **0** nothing gating; **1** a gating finding; **2** could not analyze (no
Postgres, Postgres too old, target ahead of the tree, refused DSN). Never "passed" when
nothing was examined.
