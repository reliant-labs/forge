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

There is no stepping a schema back, and no rollback of a release either — see
"Roll forward only" below.

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
- **Roll the app forward too.** There is no release rollback: a bad release is
  fixed by a new one, cut and promoted like any other. Binding an env to an
  OLDER release is possible (an ordinary `forge env promote`, labelled
  `direction BEHIND`), but it undoes nothing — the older code then runs on the
  newer schema, which is safe only if every migration since was
  expand-only and marked `-- forge:backward-compatible`.

### Down files that predate the policy

`forge lint`'s `no-down-migration` rule (in the migration-safety lane) errors on
every `*.down.sql` and every goose `-- +goose Down` section with SQL in it. A
project that wrote them before the rule grandfathers its history with one
reviewable line; anything newer still fails:

```yaml
database:
  migration_safety:
    down_files_allowed_until: "00092"   # at or below: one folded warning; above: error
```

The line is hand-written on purpose — a baseline stamped automatically would
grandfather whatever was written a minute before the stamp. Forge never runs
the grandfathered files, so deleting them is always safe.
