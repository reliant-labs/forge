---
name: db/deploy-migrations
description: How a deployed environment applies schema migrations — migrations embedded in the binary, the rendered migration initContainer, replica races and advisory locking, failure modes, and where AUTO_MIGRATE still fits.
---

# Applying Migrations in a Deployed Environment

## The shape

The app binary **embeds** its migrations: `forge generate` writes
`db/embed.go` (`//go:embed migrations/*.sql` → `forgedb.MigrationsFS`) and
`<binary> db migrate up` applies that embedded set.

This is not a stylistic choice. The production image's runtime stage copies
the binary and nothing else — there is no `db/migrations` directory inside the
container — so a migrator reading `file://db/migrations` could only ever fail
there. Embedding is what makes the image able to migrate itself, and therefore
what makes a deploy-time migration step possible at all.

## The wiring (nothing to do by hand)

```
db/migrations/*.sql
  └─ forge generate
       ├─ db/embed.go                     (the embedded FS)
       └─ deploy/kcl/components_gen.json  {"migrate": {"command": ["/app/<project>","db","migrate","up"]}}
            └─ deploy/kcl/<env>/main.k    migrate = fc.load_migrate(fc.COMPONENTS_GEN)
                 └─ rendered Deployment   initContainers: [migrate]
```

The command is **empty until the project ships its first `.sql`**, and an
empty command renders no init container. A migration step running a command
guaranteed to fail is worse than no step.

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
