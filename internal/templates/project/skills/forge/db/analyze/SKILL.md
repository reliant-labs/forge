---
name: analyze
description: Measure what a migration does on a real postgres with forge db analyze — the locks it takes, the tables it rewrites or scans, how long it runs — inside one always-rolled-back transaction; scratch vs dsn (anonymized snapshot) targets, database.analyze config, the five db-analyze-* rules, the forge.db-analyze/v1 JSON document and exit codes.
---

# Measuring migrations: `forge db analyze`

`forge lint --migration-safety` reads migration SQL as text. It cannot know
whether `ALTER COLUMN … TYPE` rewrites the table, which lock a statement takes,
or whether a constraint validation scans every row: that depends on the column
types, the existing schema and the postgres version. `forge db analyze` runs the
migrations and measures them. Keep both: the lint is fast and needs no
postgres; this is the empirical complement.

```bash
forge db analyze            # scratch target, migrations changed since the merge-base
forge db analyze --json     # the forge.db-analyze/v1 document
forge db analyze --base HEAD~3
FORGE_ANALYZE_DATABASE_URL=postgres://… forge db analyze --target dsn
```

It needs postgres, so it is never in a fast PR tier: run it on demand, in a
review lane, or in a full gate.

## What it does

Everything runs in **one session, one transaction, always rolled back**:
`BEGIN`, `SET LOCAL statement_timeout / lock_timeout`, each pending migration
applied **statement by statement**, each measured, `ROLLBACK`. Postgres DDL is
transactional, so the target is never changed.

Per executed statement:

| Measured | How |
|---|---|
| locks | `pg_locks` of the session, before vs after |
| rewrites | `pg_class.relfilenode` of every user relation, before vs after |
| scans | `pg_stat_xact_user_tables` (`seq_scan`, `seq_tup_read`) delta |
| duration | wall time (meaningful on a target with data) |
| size | `reltuples` on a target with statistics, else `table_rows`, else unknown |

A relation created earlier in the run is **new** and exempt from every rule:
locking or rewriting a table nobody reads yet is free.

A statement that cannot run in a transaction (`CREATE INDEX CONCURRENTLY`,
`DROP INDEX CONCURRENTLY`, `VACUUM` …) is **not executed**: it is listed with
`executed: false` and its lock is reported statically (`SHARE UPDATE
EXCLUSIVE`, which blocks neither reads nor writes).

A migration file runs as one transaction in production, so a lock one statement
takes is held through every later statement **of the same file** — the rules
count it. A lock an earlier file took was released when that file committed, and
is not counted (it shows with `held_before: true`).

## Targets

| `--target` | Runs on | Analyzes |
|---|---|---|
| `scratch` (default) | pgtest's embedded postgres (or `FORGE_TEST_POSTGRES_URL`), at `database.postgres_version` | migrations added or changed since `--base` (default: merge-base with the remote default branch); the unchanged ones are applied first as setup, from a cached template |
| `dsn` | a database you provide — an **anonymized snapshot of prod** | every migration its `schema_migrations` lacks |

On a scratch target tables are empty: rewrites, locks and validation scans are
still observed exactly (they do not depend on rows), but durations are not
meaningful and sizes come only from `table_rows` hints. On a snapshot with data a
rewrite or a scan really runs (bounded by `statement_timeout`) and is rolled
back — that cost is the measurement.

A dsn target is **refused** when it is the database a non-development env
declares (`deploy/kcl/<env>/`), or when its DSN names no server and database.
A target ahead of the tree, or dirty, cannot be analyzed (exit 2).

## Configuration

```yaml
# forge.yaml
database:
  analyze:
    target:
      kind: dsn                          # scratch (default) | dsn
      dsn_env: FORGE_ANALYZE_DATABASE_URL  # the env var's NAME; never a DSN in forge.yaml
      statement_timeout: 60s             # per statement (default 60s); a timeout is a finding
      lock_timeout: 5s                   # default 5s; another session's lock stops the run (exit 2)
    table_rows:                          # size hints when the target cannot size a table
      orders: 40000000
    large_table_rows: 1000000
    small_table_rows: 10000              # findings on a table known to be smaller are dropped
```

Flags override per run: `--target`, `--dsn-env`, `--base`, `--dir`, `--json`.

## Rules

Each warns first through forge's lint rollout and is dialled through
`lint.rules` like any forge rule (`{db-analyze-table-rewrite: off}`).

| Rule | Fires when | Fix |
|---|---|---|
| `db-analyze-table-rewrite` | a pre-existing table or its indexes were rewritten (`int`→`bigint`, a volatile `DEFAULT random()`) | add the column with no or a constant default; widen only binary-compatibly (`varchar(10)`→`varchar(20)`/`text`); otherwise expand/contract: new column, batched backfill, switch |
| `db-analyze-access-exclusive-lock` | ACCESS EXCLUSIVE (blocks reads too) held across a scan or rewrite of a pre-existing table | keep the long part out from under it (below) |
| `db-analyze-validation-scan` | a full scan under a lock that blocks writes: `ADD CONSTRAINT` without `NOT VALID`, `SET NOT NULL` with no valid `CHECK`, a foreign key (scans both tables) | `ADD CONSTRAINT … NOT VALID`, then `VALIDATE CONSTRAINT` **in a later migration file**; for NOT NULL: `CHECK (c IS NOT NULL) NOT VALID`, validate, then `SET NOT NULL` (postgres skips the scan) |
| `db-analyze-blocking-index-build` | an index built on a pre-existing table without `CONCURRENTLY` (SHARE lock blocks writes) | `CREATE INDEX CONCURRENTLY` alone in its file; for a UNIQUE/PK constraint, build the index concurrently then `ADD CONSTRAINT … USING INDEX` |
| `db-analyze-statement-timeout` | a statement exceeded `statement_timeout` on the target | batch it, or split it so the long part takes no blocking lock |

Measured on postgres 18 (and pinned by forge's tests): `varchar` widening, a
constant default, `DEFAULT now()`, `ADD COLUMN … NOT NULL DEFAULT 0` and any
`NOT VALID` constraint neither rewrite nor scan; `VALIDATE CONSTRAINT` scans
under SHARE UPDATE EXCLUSIVE, which blocks no writes — unless the same file's
`NOT VALID` statement already holds ACCESS EXCLUSIVE. `TRUNCATE` is left to the
textual lane's `destructive-change`.

## The document (`--json`)

`schema: forge.db-analyze/v1`. `target` (`kind`, `identity` host:port/db — never
credentials — `server_version`, `has_statistics`), `base`, `migrations[]` (each
`statements[]` with `line`, `sql`, `executed`, `duration_ms`, `locks[]`,
`rewrites[]`, `scans[]`, `rows{}`), `queries[]` (the query-plan pass; empty
here), `findings[]` (`rule`, `severity`, `file`, `line`, `relation`, `message`,
`fix`, `rows`, `note` for a warn-first finding), `summary`
(`migrations_scanned`, `statements_executed`, `reason` when nothing was scanned),
`exit_code`, `error`.

Exit codes: **0** nothing gating; **1** a gating finding; **2** could not
analyze (no postgres, target ahead of the tree or dirty, refused DSN, a
statement that failed, another session's lock). An empty analysis is exit 0
with `migrations_scanned: 0` and a `reason` — read the reason, it is never a
silent pass.

A reviewer (`pr-review`) reads the document: deterministic findings are
reported as such; judgement about a specific table ("this one is hot") is a
comment, not a gate.

The mechanism is a library, `forge/pkg/dbanalyze` (`forge project libraries
dbanalyze`): a session holding the rolled-back transaction, the measured
executor, the rules and the document types.
