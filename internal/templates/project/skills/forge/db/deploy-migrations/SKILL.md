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

## Why an initContainer and not a Job

The ordering guarantee is **Kubernetes' own**. It holds under `kubectl apply
-f -`, under Argo CD, under Flux, and under `forge env deploy` alike — no
applier has to cooperate, and there is no Job-name-per-release or
Job-immutability bookkeeping. A Job in a plain apply stream is not ordered
against the Deployments at all.

## Failure modes, and what each does

- **Replicas race the same migration.** Safe. golang-migrate's postgres driver
  takes a session advisory lock: one replica applies, the rest block, then
  find nothing to do. Every one exits 0.
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
- **The schema is AHEAD of the binary** — a rollback. See below.

## Rolling back across a migration

A rollback runs an OLDER release's binary against a database a NEWER release
already migrated. An older migrator sees a version it does not embed and fails
(`no migration found for version N`), which used to make the rollback deploy die
at its own migrate step. Two things now handle it:

**1. The deploy skips the migration.** `forge env promote <old> --to <env>
--rollback`, then `forge env deploy <env>`: when the env's current ledger entry
is a rollback, the deploy does NOT apply its pre-rollout Jobs (the standalone
migrate Job). It prints `ROLLBACK: skipping N pre-rollout Job(s): …` and
`--json` records `promotion_rollback` and `skipped_pre_rollout_jobs`. The older
release's migrations are already applied (the ledger refuses a rollback to a
release the env never ran), and the older binary has no down SQL for the newer
ones, so running its migrate step could only no-op or fail. Post-rollout Jobs
still run. The next forward promote migrates again as normal.

This covers the STANDALONE migrate Job (`forge.CronJob{schedule = ""}`, or a
`kind = "job"` workload with no `before`). A migration lowered to an
initContainer (`before = [fw.BEFORE_ALL]`) is part of the pod and is not
skipped; there the older binary's own migrator decides (point 2).

**2. The migrator classifies a schema ahead of it** (`forge/pkg/migratekit`,
the scaffolded `db migrate up` and `AutoMigrate`). A migration may declare, in
its `.up.sql`:

```sql
-- forge:backward-compatible — additive column; the previous release never reads it
ALTER TABLE plans ADD COLUMN retired_at TIMESTAMPTZ;
```

The migrator records each version's declaration in `schema_migrations_compat`
as it applies. An older binary built on this migratekit that meets a schema
ahead of it exits 0 and says so (`SCHEMA AHEAD OF THIS BINARY`) when every
unknown version was declared compatible, and fails with the runbook
(`*migratekit.SchemaAheadError`) when not. A binary built BEFORE this has no
such handling. That is why the deploy-side skip exists.

**What neither does: make the older code correct on the newer schema.**
Skipping the migration runs the older code against the CURRENT schema. That is
safe when the newer migrations were expand-only (added columns and tables the
old code ignores). When one removed or changed something the old code uses, do
NOT roll the app back across it — there is no down to run, and the schema is
never stepped back. Roll forward instead: ship a hotfix release built on the
current schema.

Author migrations expand/contract so rolling the app back is always safe: add
in release N, stop reading the old shape in N, drop it in N+1.

## Where AUTO_MIGRATE still fits

`AUTO_MIGRATE=true` migrates **in-process at startup**. It is not a duplicate
of the init container — it serves the HOST loop (`forge run`), where there is
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
- **Roll back the app, never the schema.** `forge env deploy --rollback`
  re-points an environment at an earlier release's images and runs no SQL — and
  the migrate Job only ever runs `db migrate up`. Expand-then-contract is what
  makes the older release safe to run.

### Existing down files

`forge lint`'s `no-down-migration` rule (in the migration-safety lane) errors on
every `*.down.sql` and every goose `-- +goose Down` section with SQL in it,
whenever it was written. There is no grandfather line: forge never runs a down
file, so deleting one is always safe — delete them, along with any test or CI
job that executes them.
