---
name: db/deploy-migrations
description: How a deployed environment applies schema migrations — migrations embedded in the binary, the rendered migration initContainer, replica races and advisory locking, failure modes, and where AUTO_MIGRATE still fits.
---

# Applying Migrations in a Deployed Environment

## The shape

The app binary **embeds** its migrations: `forge generate` writes
`db/embed_gen.go` (`//go:embed migrations/*.sql` → `forgedb.MigrationsFS`) and
`<binary> db migrate up` applies that embedded set.

This is not a stylistic choice. The production image's runtime stage copies
the binary and nothing else — there is no `db/migrations` directory inside the
container — so a migrator reading `file://db/migrations` could only ever fail
there. Embedding is what makes the image able to migrate itself, and therefore
what makes a deploy-time migration step possible at all.

## The wiring

```
db/migrations/*.sql
  └─ forge generate
       └─ db/embed_gen.go                   (the embedded FS)

deploy/kcl/<env>/main.k                 migrate = ["/app/<project>", "db", "migrate", "up"]
  └─ rendered Deployment                initContainers: [migrate]
```

The `migrate` argv is **yours**. forge scaffolds it once per environment and
never rewrites it, because how a system migrates is an operational decision
that differs per env and changes over time.

Set it to `[]` in any environment that applies migrations out of band — a
DBA-run pipeline, a managed-database console, a separate release train. An
empty command renders no init container, which is the honest answer for an
env that migrates elsewhere. Replace the argv entirely to run a different
tool.

The init container runs the **same image** and the **same env** as the app, so
it reads the same `DATABASE_URL` from the same Secret.

## On Reliant hosting: the managed database

A hosted env's `migrate` job (`before = [fw.BEFORE_ALL]`) runs against the
managed Postgres: `DATABASE_URL = forge.DatabaseRef {name = _db.name}` reaches
it from the platform only, so migrations run there, inside the deploy, never
from your laptop (`forge db migrate up --dsn` cannot reach it). The deploy is
gated on billing like any hosted workload; queued, nothing migrates until it
goes live (`deploy/hosting`).

## Why an initContainer and not a Job

The ordering guarantee is **Kubernetes' own**. It holds under `kubectl apply
-f -`, under Argo CD, under Flux, and under `forge env deploy` alike — no
applier has to cooperate, and there is no Job-name-per-release or
Job-immutability bookkeeping. A Job in a plain apply stream is not ordered
against the Deployments at all.

## Failure modes, and what each does

- **Replicas race the same migration.** Safe, whichever way each one migrates.
  migratekit takes one session advisory lock per database, shared by
  `db migrate up` (the init container, the dev loop's migrate job) and
  `AutoMigrate` (a server booting with `AUTO_MIGRATE`). One replica applies;
  the others wait, then find nothing to do, and every one exits 0. The lock
  is migratekit's own because golang-migrate's lock key differs between those
  two entry points, so on its own it never made them wait for each other.
- **A migration fails.** The init container exits non-zero → CrashLoopBackOff
  → the rollout stalls, with the OLD pods still serving. That is the correct
  outcome: shipping code that needs a schema the database does not have is
  worse than not shipping.
- **The schema is dirty** (a previous migration failed partway). `db migrate
  up` refuses and names the version. Applying more on top is how a
  half-applied migration becomes a corrupted one. Clear it deliberately:
  `forge db migrate force <version> --dsn ...`.
- **Every pod start re-runs it** (scale-up, node eviction). Intended, and
  cheap: a no-op is one connection, one lock, one version read.
- **The schema is AHEAD of the binary** — the previous release, mid-deploy.
  See below.

## Old code on a new schema — every deploy, not just incidents

Every rolling deploy runs the PREVIOUS release's code against the NEXT
release's schema for a while: the new release migrates first, and the old
ReplicaSet keeps serving — and keeps starting pods on a reschedule or a
scale-up — until the rollout completes. So every migration must leave a schema
the previous release still works on. That is expand/contract: add in release N,
stop reading the old shape in N, drop it in N+1.

A migration states that it is the safe (expand) kind in its `.up.sql`:

```sql
-- forge:backward-compatible — additive column; the previous release never reads it
ALTER TABLE plans ADD COLUMN retired_at TIMESTAMPTZ;
```

The migrator (`forge/pkg/migratekit`: the scaffolded `db migrate up` and
`AutoMigrate`) records each version's declaration in `schema_migrations_compat`
as it applies. A binary that meets a schema AHEAD of it — an old pod booting
mid-deploy — exits 0 and says so (`SCHEMA AHEAD OF THIS BINARY`) when every
unknown version was declared compatible, and refuses to start
(`*migratekit.SchemaAheadError`) when one was not. That refusal is the point:
old code does not get to guess on a schema nobody vouched for.

It also records WHICH file each version was, in `schema_migrations_applied`,
and before applying anything checks those against the files the binary embeds
under the same numbers. A version applied from a different file — typically a
branch's migration run against a shared database and renumbered before merge —
is `*migratekit.MigrationMismatchError`, and nothing runs: the recorded version
no longer describes the schema. The error names both files and the manual
reconcile. Only the name after the version prefix is compared, so editing an
old migration's comments or re-padding its number is not a mismatch.

### A migration the schema has already passed

golang-migrate tracks one number and applies only migrations **above** it, so a
migration numbered *below* the current version can never run — it looks there,
finds nothing pending, and reports the schema current. With timestamp versions
this happens exactly one way: two branches are cut, A allocates an earlier
timestamp than B, and **B merges and deploys first**. Main now carries A's
migration at a version the database has already passed, and A's SQL silently
never executes. The first symptom is a query for a column that does not exist,
in a crash-looping pod far from the migration that owns it.

So before applying anything, every embedded version at or below the schema's
version must be recorded in `schema_migrations_applied`. One that is not is
`*migratekit.MissingMigrationError`, and nothing runs.

**The fix is to re-version the file** with `forge db migration rebase <file>`,
which the error spells out: the migration has not run anywhere, so renaming it
to a timestamp newer than the schema's version is safe and is all that is
needed. Rebase allocates the new version above both the directory's max and
the default branch's, and **refuses** a migration that has already merged —
that one is recorded in a database under its current name, so it must keep its
version and be repaired by a new forward migration instead. Forge deliberately does **not** apply
it out of order — that would make the schema depend on merge order, so
A-then-B and B-then-A would produce different databases from the same commit
with nothing reporting which one you got. `forge lint`'s
`non-timestamp-migration-version` rule catches most of these at PR time, before
a deploy can refuse.

Versions applied before this record existed are backfilled **once**, when the
table is created, from the schema's own version — so an existing database
adopts the check without refusing. Those rows carry no filename: which file an
old database ran under a given number is the one thing it cannot vouch for.

There is no stepping a schema back, and no rollback of a release either — see
"Roll forward only" below.

## Adopting a database another tool migrated

A project moving onto forge from **goose** squashes goose's history into ONE
baseline migration. Two kinds of database then meet the same binary:

- a **fresh** one, which applies the baseline as SQL;
- one **goose already migrated**, whose schema already IS the baseline. Running
  the baseline there would fail on its first `CREATE TABLE` and leave the
  schema dirty.

The baseline says which history it replaces, in its own header, listing EVERY
version goose recorded. The line repeats to wrap; the lists accumulate:

```sql
-- forge:adopts goose 20260211000002 20260211000003 20260212100000
-- forge:adopts goose 20261011194043 20261011203444
CREATE TABLE chats (...);
```

On the normal path — `db migrate up` and `AutoMigrate` alike, so the init
container and a desktop install both get it with no step to run — a database
golang-migrate has never recorded anything in, but which has a
`goose_db_version` table, is checked against that list:

| goose's applied set | What happens |
|---|---|
| exactly the listed versions | **Adopted.** The baseline is recorded as applied (`schema_migrations` + `schema_migrations_applied`) and none of its SQL runs. Later migrations then apply on the same run. `Result.Adopted` is set, and `AutoMigrate` logs `ADOPTED` at WARN. |
| missing a listed version | `*migratekit.AdoptionError` (Missing). The database is BEHIND, and this binary no longer carries that migration. Run the last release that still migrates with goose, then this one. |
| holds a version the list lacks | `*migratekit.AdoptionError` (Unexpected). Either the baseline was cut before that migration merged (re-cut it), or it is a branch's migration on a shared database (reconcile the schema, then delete its `goose_db_version` row). |
| no `goose_db_version` table | Not a goose database; the baseline runs as SQL. |

Every refusal changes nothing and names the versions involved. The check is
**set membership**, never "the newest version matches". goose applies out of
order (`WithAllowMissing`), so a database can hold the newest version and
still lack an older one. A version counts as applied when its **latest** row
says `is_applied`, which is how goose reads its own table, so a rolled-back
version counts as missing.

Properties you can rely on:

- **Once only.** After golang-migrate records a version, goose's table is
  never read again. Re-runs are ordinary no-ops.
- **goose's table is left alone.** Rolling back to the last goose release is
  safe: goose finds every version it knows applied and does nothing.
- **Concurrent replicas are safe.** The check-and-record runs under
  migratekit's run lock, so one replica adopts and the rest find it done.
- **Only the first migration may declare it.** A goose database has run none
  of the project's other migrations. A misplaced or malformed declaration is
  refused at `Open` and at boot on every database, so the first test run
  fails. `forge lint` reports it at review time as `invalid-adopts-directive`.

Generate the list from the goose files; never type it. goose's version is the
filename's numeric prefix:

```bash
ls <goose-dir>/*.sql | xargs -n1 basename | cut -d_ -f1 | sort -n | xargs -n8 | sed 's/^/-- forge:adopts goose /'
```

Build the baseline body with `pg_dump --schema-only --no-owner
--no-privileges` from a throwaway database the goose history built, excluding
`goose_db_version`. Then strip pg_dump's session prelude and psql commands:

```bash
grep -v -e '^SET ' -e '^SELECT pg_catalog.set_config' -e '^\\' dump.sql
```

Leaving them in breaks things, and measurably. The migrator runs the
baseline on its own session, so `set_config('search_path', '', false)`
persists on that session. The next migration in the same run then fails with
`relation "…" does not exist`, and under `AutoMigrate` the connection returns
to the app's pool with an empty `search_path`. `\restrict` (pg_dump ≥ 16.10
and its sibling minor releases) is a psql command, so it fails as SQL.

Prove the baseline before shipping it. Build one database with the last goose
release, and let the new binary adopt it. Build a second fresh database from
the baseline. `pg_dump --schema-only` both, excluding each tool's bookkeeping
tables, and diff them. The declaration says the two schemas are equal; only
that diff shows it.

`forge project migrate import --from goose` is the other path: it renumbers
goose's history into new forge versions. It suits only a project whose
databases can be rebuilt, because a database goose already migrated would run
every imported migration again.

## Where AUTO_MIGRATE still fits

`AUTO_MIGRATE=true` migrates **in-process at startup**. It is not a duplicate
of the init container — it serves the HOST loop (`forge env up`), where there is
no pod and therefore no init container. The scaffold sets it true in `dev`'s
`config.k` and leaves it false everywhere else.

Do not turn it on in a cloud env to "make migrations work". Replicas racing to
migrate on startup, each already serving its readiness probe, is not a
migration strategy.

## Verifying

`forge doctor --signal deploy` fails **Deploy Migrations** for any environment
that ships `.sql` migrations with no way to apply them. It accepts a migration
Job, a migration initContainer, a migrate command, or `AUTO_MIGRATE=true` — it
asserts a path exists, not which one.

See also: `db` for authoring migrations, `deploy` for the rollout.

## Roll forward only — there is no down

Forge writes no down migration, runs none, and fails lint on a new one.
`forge db migration new`, `forge scaffold` entity births, `forge db squash` and
`forge project migrate import --from goose` all write `.up.sql` only; neither
`forge db migrate` nor the scaffolded `<binary> db migrate` has a `down`.

Why: a down script claims to undo a release and cannot. By the time anyone
would run it, the release has written rows in the new shape, other services
have read them, and jobs have acted on them. It was written before any of that,
it is untested in the state it would run in, and the moment it is needed is
mid-incident.

What to do instead:

- **Hotfix forward.** A bad migration is repaired by the next migration,
  written against the state the database is actually in.
- **Expand, then contract.** Add the new shape (nullable column, new table) and
  backfill; switch readers and writers in a release; drop the old shape in a
  LATER release. At every step the previous release still works against the
  new schema.
- **Roll the app forward too.** There is no release rollback: a bad release is
  fixed by a new one, cut and promoted like any other. Binding an env to an
  OLDER release is possible (an ordinary `forge env deploy`, labelled
  `direction BEHIND`), but it undoes nothing — the older code then runs on the
  newer schema, which is safe only if every migration since was
  expand-only and marked `-- forge:backward-compatible`.

### Existing down files

`forge lint`'s `no-down-migration` rule (in the migration-safety lane) errors on
every `*.down.sql` and every goose `-- +goose Down` section with SQL in it,
whenever it was written. There is no grandfather line: forge never runs a down
file, so deleting one is always safe — delete them, along with any test or CI
job that executes them.
