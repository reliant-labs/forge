---
name: v1.0.16
description: Upgrade a project to forge v1.0.16. A `--kind cron` worker now runs its schedule on forge/pkg/cronkit, which claims each tick in the app's Postgres (table forge_cron_ticks, created on first use) so it fires on ONE replica instead of on every replica; `const PerReplica = true` opts a worker back into running everywhere. Workers scaffolded earlier keep firing on every replica until converted. Use when internal/workers/*/worker.go imports github.com/robfig/cron/v3. Also: forge/pkg/releasehealth.ServiceHealth gained an Endpoints slice, so it is no longer comparable with ==; use when Go code imports forge/pkg/releasehealth. Also: a scaffolded UpdateX over a table owned by its own primary key (COMMENT ON COLUMN <t>.id IS 'forge:owner') stamped the key, moving an update aimed at another row onto the caller's; use when a migration declares forge:owner on an id column. Also: forge/pkg/schemadef.ForeignKey gained KeyColumns / RefKeyColumns slices (a composite foreign key's whole key), so it is no longer comparable with ==; use when Go code imports forge/pkg/schemadef.
version: v1.0.16
detection: grep -rqs --include='*.go' --exclude='*_test.go' 'github.com/robfig/cron/v3' internal/workers && exit 0; grep -rqs --include='*.go' --exclude-dir=gen --exclude-dir=vendor --exclude-dir=node_modules 'forge/pkg/releasehealth"' . && exit 0; grep -rqs --include='*.go' --exclude-dir=gen --exclude-dir=vendor --exclude-dir=node_modules 'forge/pkg/schemadef"' . && exit 0; grep -rqsiE --include='*.up.sql' "on column [a-z0-9_.\"]*\.\"?id\"? is '[^']*forge:owner" db/migrations && exit 0; exit 1
---

# Upgrading to v1.0.16

## 1. Cron workers fire once per tick, not once per replica

**Use when** a worker under `internal/workers/` imports `github.com/robfig/cron/v3` (it was scaffolded with `--kind cron` before v1.0.16).

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

## 2. `releasehealth.ServiceHealth` is no longer comparable

**Use when** Go code imports `github.com/reliant-labs/forge/pkg/releasehealth`.

### What changed

`ServiceHealth` gained `Endpoints []EndpointHealth`: release health now judges
p95 latency per endpoint (`ServiceHealth.LatencyRatio`), so a window that
merely holds a different mix of endpoints does not read as the service getting
slower. A struct with a slice field cannot be compared with `==` or used as a
map key.

### What to do

Nothing, unless your code compares two `ServiceHealth` values with `==`/`!=`
or keys a map by one; the compiler names each such site. Compare the fields
you mean instead (`a.Service == b.Service`), or `reflect.DeepEqual` in a test.
Leaving `Endpoints` nil keeps the old service-level latency verdict.

## 3. A table owned by its own key: UpdateX stamped the key

**Use when** a migration declares the primary key as the owner column, e.g.
`COMMENT ON COLUMN tenants.id IS 'forge:owner'` (a table whose rows are owned by
themselves).

### What changed

The owner-scoping wrapper forge scaffolds into `handlers_crud.go` stamps the
owner column onto an Update's entity, `e.<Owner> = owner`. When the owner column
is the key, that line is `e.ID = owner`: an update aimed at another row was
silently moved onto the caller's own, and came back as that row's constraint
error (`already_exists … tenants_slug_key`) rather than NotFound. A newly
scaffolded Update over such a table predicates on the key and never stamps it.

`handlers_crud.go` is yours, so forge does not rewrite an existing one.

### What to do

In each `Update<Entity>` of such a table, delete the `op.Entity` override that
assigns `e.ID = owner`, together with its `build := op.Entity` line. Keep the
`op.Persist` (and `op.PersistMasked`) override: its `orm.WhereEq("id", owner)`
is what scopes the write. `forge project audit` prints the corrected wrapper
for an unscoped one.

### Also new: declaring the owner at birth

`// forge:owner` on a proto field (`// forge:read-only forge:owner` above
`string tenant_id = 2;`) writes the `COMMENT ON COLUMN … IS 'forge:owner'` into
the entity's birth migration, so `forge scaffold` births `handlers_crud.go`
already scoped. A `forge:read-only` NOT NULL owner column no longer needs
`forge:fill=handler` while its Create is authenticated (the wrapper stamps it);
a `forge:fill=handler` added only to get past "can never be populated" can stay
or go.

## 4. `schemadef.ForeignKey` is no longer comparable

**Use when** Go code imports `github.com/reliant-labs/forge/pkg/schemadef`.

### What changed

`ForeignKey` gained `KeyColumns []string` and `RefKeyColumns []string`: a
composite constraint (`FOREIGN KEY (tenant_id, strategy_id) REFERENCES
strategies (tenant_id, id)`) is still reported as one entry per column, and
each entry now carries the whole key, so a reader can tell that all its columns
name the same parent row. Both are nil for a single-column reference.
Introspection also pairs the key's columns by position: a composite used to
come back as the cross product of its columns (strategy_id → strategies.tenant_id
as well as → strategies.id), and now yields exactly one entry per column. A
struct with a slice field cannot be compared with `==` or used as a map key.

### What to do

Nothing, unless your code compares two `ForeignKey` values with `==`/`!=` or
keys a map by one; the compiler names each such site. Compare the fields you
mean instead (`a.Column == b.Column && a.RefTable == b.RefTable`), or
`reflect.DeepEqual` in a test. Code that deduplicated the old cross-product
rows of a composite can drop that workaround.
