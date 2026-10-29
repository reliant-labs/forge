---
name: migration-timeouts
description: Why a migration never waits forever for a lock — the default lock_timeout (5s), statement_timeout (30m) and retry (5 attempts), the per-migration `-- forge:lock-timeout` / `-- forge:statement-timeout` header directives, CREATE INDEX CONCURRENTLY and its INVALID-index cleanup, and the run-wide flags.
---

# Lock and statement timeouts

`ALTER TABLE` takes an ACCESS EXCLUSIVE lock, which waits behind any session
that has touched the table — a slow report, a transaction an application left
open. Postgres queues lock requests, so while the migration waits **every later
query on that table queues behind the migration**, plain `SELECT`s included. One
long reader plus one `ALTER TABLE` is an outage for as long as the reader runs.
The migrator therefore never waits unboundedly:

| Setting | Default | Meaning |
|---|---|---|
| `lock_timeout` | `5s` | Longest one attempt waits for a lock. Bounds the pile-up behind the DDL. |
| `statement_timeout` | `30m` | Backstop against a runaway statement. Generous so a real backfill is not cut off. |
| attempts | `5` | Tries of a migration whose lock timed out, the first included. Waits 1s, 2s, 4s, 8s (±25% jitter, capped at 15s) between them. |

A timeout in the migrator is not a failure of the migration, it is the lock
being busy, so a timed-out attempt is rolled back completely (one file is one
transaction) and tried again; each retry is logged at WARN with the migration,
attempt and wait. When every attempt times out, the run fails with a
`*migratekit.LockTimeoutError` naming the migration and what to do, and the
schema is left **clean at the previous version**: nothing was applied, so the
next run is simply another try, with no `force` and no dirty flag. Only lock
timeouts are retried; any other error fails on the first attempt.

**Per-migration override** — a header line in the `.up.sql`, a Go duration
(`500ms`, `30s`, `5m`, `1h30m`) or `off`; a rationale may follow:

```sql
-- forge:lock-timeout 30s
-- forge:statement-timeout 2h — backfills 40M rows
```

A file's directive replaces the run's value for that file only. A malformed or
doubled directive fails the run before anything is applied, the way a malformed
`forge:adopts` does. Raise `statement-timeout` for a backfill that legitimately
runs long (better still, backfill in batches, one short transaction each); raise
`lock-timeout` only for a migration that must wait out something known and
brief. Run-wide values: `db migrate up --lock-timeout 10s --statement-timeout 1h
--lock-attempts 3` (the scaffolded binary's `db migrate up` has the same flags),
or `migratekit.Options.Run` / `migratekit.AutoMigrateWith` from Go.

**`CREATE INDEX CONCURRENTLY`** must be the only statement in its file, so it
runs outside a transaction block and the limits are set on the session, not with
`SET LOCAL`. It does not hold the table's lock for long, but it waits for every
transaction older than itself to end, so an idle-in-transaction session stalls
it and it times out like any DDL. A timed-out build leaves its index `INVALID`;
the migrator **drops it before each retry** (`DROP INDEX CONCURRENTLY`), so a
retry never meets "already exists" and a `CREATE INDEX CONCURRENTLY IF NOT EXISTS`
never quietly keeps a broken index. Only indexes the attempts created, or that
the file names, are dropped — another invalid index in the database is left
alone. If the blocker also stops the drop, the `LockTimeoutError` lists the
index (`Leftover`) and the next run drops it first.

The limits are set on the migration's session and **reset when it ends**,
including on failure, because `AutoMigrate` borrows a connection from the
application's pool.
