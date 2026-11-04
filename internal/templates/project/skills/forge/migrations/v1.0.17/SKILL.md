---
name: v1.0.17
description: Upgrade a project to forge v1.0.17. A `--kind cron` worker now runs its schedule on forge/pkg/cronkit, which claims each tick in the app's Postgres (table forge_cron_ticks, created on first use) so it fires on ONE replica instead of on every replica; `const PerReplica = true` opts a worker back into running everywhere. Workers scaffolded earlier keep firing on every replica until converted. Use when internal/workers/*/worker.go imports github.com/robfig/cron/v3.
version: v1.0.17
detection: grep -rqs --include='*.go' --exclude='*_test.go' 'github.com/robfig/cron/v3' internal/workers && exit 0; exit 1
---

# Upgrading to v1.0.17

## 1. Cron workers fire once per tick, not once per replica

**Use when** a worker under `internal/workers/` imports `github.com/robfig/cron/v3` (it was scaffolded with `--kind cron` before v1.0.17).

### What changed

A cron worker runs inside a long-running workload, so with two replicas every
replica ran every tick: an hourly poller polled twice an hour. A newly
scaffolded cron worker runs on `forge/pkg/cronkit` instead. Before each tick
it inserts (worker name, scheduled tick time) into `forge_cron_ticks` in the
app's Postgres with `ON CONFLICT DO NOTHING`; the replica whose insert lands
runs the tick and the others skip it (a DEBUG log and
`forge_cron_ticks_total{outcome="skipped"}`). The table is bookkeeping that
cronkit creates on first use (`CREATE TABLE IF NOT EXISTS`) as the app's own
role, the one that already runs the migrations at boot. No migration to write.

Nothing changes on upgrade by itself: `worker.go` is yours, and forge never
rewrites it. An unconverted worker keeps firing on every replica.

### Decide per worker

- The job must run once per tick (a poller, a digest, a sweep that calls a
  third party): convert it, below, with `PerReplica = false`.
- The job is meant to run on every pod (an in-process cache refresh, a local
  temp-dir sweep): convert it with `PerReplica = true`, or leave it as it is.

### Convert

1. In `Deps`, add the ORM handle the ticks are claimed on (skip this when the
   project has no `db/migrations`; the worker then fires on every replica and
   says so at boot):

   ```go
   // DB is where ticks are claimed, so a tick fires once across replicas.
   // forge:optional-dep
   DB orm.Context
   ```

2. Beside `const Schedule`, declare the replica policy:

   ```go
   const PerReplica = false
   ```

3. Replace the robfig/cron scheduler: drop the `cron`, `baseCtx` and
   `baseCancel` fields, and make `Start` block in cronkit:

   ```go
   func (w *Worker) Start(ctx context.Context) error {
       return cronkit.Run(ctx, cronkit.Options{
           Name:       w.Name(),
           Schedule:   Schedule,
           PerReplica: PerReplica,
           Claims:     cronkit.Postgres(w.deps.DB),
           Logger:     w.deps.Logger,
       }, w.Run)
   }

   func (w *Worker) Stop(ctx context.Context) error { return nil }
   ```

4. Make `Run` return an error, and drop its own span and `recover`: cronkit
   runs each tick under a root span (`cron.<name>.tick`), logs the returned
   error and recovers a panic. `cronkit.TickTime(ctx)` is the scheduled time,
   the same on every replica, for keying idempotent writes.

   ```go
   func (w *Worker) Run(ctx context.Context) error { … }
   ```

   Update `worker_test.go` where it calls `w.Run(ctx)` (`_ = w.Run(ctx)`).

5. `forge generate`. The new `DB` field resolves to the ORM by type
   (`DB: g.ORM(),` in compose.go's `new<Worker>`); in a legacy `NewComponents`
   project, set it there yourself (`DB: infra.ORM` or what your Infra names
   it). Then `go mod tidy` if robfig/cron is now only indirect.

`Name()` is the claim key: keep it stable, since a renamed worker starts a
fresh claim history (harmless: the next tick is claimed under the new name).

### Behaviour to know

- At most once per tick: a replica that crashes mid-tick loses that tick, and
  a claim that fails (database down) skips the tick there, logged at ERROR
  (`outcome="claim_error"`).
- Ticks on one replica never overlap; a tick that outlives its period skips
  the ticks it covered. robfig/cron used to start an overlapping run instead.
- `@every <d>` is aligned to multiples of d, so it no longer counts from
  process start.

The `workers` skill (Cron-Scheduled Worker) is the full contract.
