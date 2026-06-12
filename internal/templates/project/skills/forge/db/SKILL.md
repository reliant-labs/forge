---
name: db
description: Database work — migrations own the schema, entity types evolve independently from the API, schema is append-only, never migrate down against prod.
emit: both
---

# Database Work

## The key principle

**Migrations are the source of truth for your database schema.** Not API types, not ORM structs — the SQL files in your migrations directory. API contracts and DB schema evolve independently. Codegen pipelines that generate from your API (proto, OpenAPI, GraphQL) should never touch the DB layer — that's your code, evolve it freely.

## Entity type lifecycle

API types and DB types start identical and diverge over time. There's a natural progression:

**Stage 1 — Alias** (when API and DB shape are identical):

```go
type User = apiv1.User
```

No mapping needed; same type everywhere. This is fine until divergence forces concrete types.

**Stage 2 — Concrete struct** (when API and DB diverge):

```go
type User struct {
    ID           string
    Name         string
    Email        string
    // DB-only fields not in the API
    PasswordHash string
    LoginCount   int
    LastLoginAt  time.Time
}
```

This divergence is expected and correct. Your API exposes what clients need. Your DB stores what the domain requires. These are different concerns.

**Stage 3 — Mapper functions** (convert between API and DB types):

```go
func UserToAPI(u *User) *apiv1.User { ... }
func UserFromAPI(u *apiv1.User) *User { ... }
```

The mapper lives near the DB type, not the API type — the DB owns the translation since it's the one diverging.

## Schema evolution patterns

### Adding DB-only fields

Your DB often needs fields the API doesn't expose: audit trails, internal state, denormalized caches, soft-delete tombstones. Add them via migration, update the concrete struct — no API change needed.

### API and DB with different field names or shapes

When DB column names differ from API field names, or when database-native types (`pgtype.Timestamptz`, `decimal.Decimal`) don't round-trip to API wire types cleanly, the concrete struct + mapper pattern handles it. Don't fight the type system — use the mapper.

### Entities that exist only in the DB

Not every table needs an API exposure. Internal bookkeeping tables, junction tables, event logs, queue state — create these with migrations and concrete structs. No API contract, no codegen, no cross-stack changes.

## Migration discipline

- **Migrations are append-only.** Never edit a merged migration — write a new one. Future developers' local state assumes the merged migration is immutable.
- **Always write the down-migration.** Even if you'll never run it in production, your test harness uses it for setup/teardown cycles.
- **Never migrate down against staging or prod.** Roll forward only. If a migration is wrong, write a new migration that undoes it, don't run the down.
- **Keep seed data out of migrations.** Migrations define schema; seeds populate it. Mixing them makes both harder to reason about.

<!-- @forge-only:start -->
## Forge architecture

Forge separates the layers:

```
db/migrations/              # SQL migrations — THE schema source of truth
db/queries/                 # SQL query definitions (for sqlc or manual use)
internal/db/types.go        # Entity types (start as proto aliases, evolve to concrete structs)
internal/db/<entity>_orm.go # Generated ORM CRUD functions for each entity
```

While proto entities drive codegen (greenfield mode, below), `forge generate` regenerates the entity ORM layer — `internal/db/types.go` aliases and `internal/db/<entity>_orm.go`. It never modifies `db/migrations/`, `db/queries/`, or your mappers and custom DB code; the schema itself is always yours.

### Generated ORM semantics

- `Create<Entity>` is a plain `INSERT` — never an upsert. String PKs left unset are generated via `ulid.Make()` at the Create chokepoint.
- With `timestamps: true`, `created_at`/`updated_at` are stamped on create and `updated_at` on update; `created_at` is immutable on update.
- Each entity exports `<Entity>Columns` — the declared-column allowlist. `forge/pkg/crud` validates user-supplied `order_by` against it; an undeclared column is `InvalidArgument`, not a silent no-op.
- Missing rows surface as `errors.Is(err, orm.ErrNoRows)`; `forge/pkg/crud` maps them through `pkg/svcerr` to a clean `NotFound`. All other repo errors map to `Internal` with safe text — no SQL on the wire.
- Generated migrations give string PKs `CHECK (id <> '')`; auto-added timestamp columns use `DEFAULT CURRENT_TIMESTAMP`.

## Proto entities: greenfield vs migrated

Two modes, and `forge audit` (`proto_migration_alignment` category) tells you which one you're in:

- **Greenfield (proto authoritative).** A fresh `forge new` with proto entity annotations. `forge generate` produces ORM and CRUD handlers from the proto; on the first run with no migrations it also generates an initial migration from the entities. Proto leads, migrations track.

- **Migrated (migrations authoritative).** When you bring an existing schema into forge via `migration-service` or `migration-cli`, the migrations carry the schema and proto entities become **advisory**. `forge generate` does not regenerate migrations from proto. If you removed proto entities entirely, `forge audit` reports `migrations authoritative (no proto entities)`. If both exist and diverge it flags `diverged: N table(s) in migrations not in proto, M in proto not in migrations` and you decide whether to drop the proto entities, sync them via `forge db proto sync-from-db`, or roll a migration forward to match proto.

In both modes, migrations remain the runtime source of truth. Proto entities are either driving codegen (greenfield) or documenting it (migrated) — the schema itself lives in `db/migrations/`.

## Forge DB commands

### Changing the schema

```
forge db migration new <name>                          # create a new migration pair
forge db migrate up --dsn "$DATABASE_URL"              # apply pending migrations
forge db migrate status --dsn "$DATABASE_URL"          # show what's applied
forge db migrate force <version> --dsn "$DATABASE_URL" # recover from a dirty migration
```

Dev DSN convention:
```
postgres://postgres:postgres@localhost:5432/<project>?sslmode=disable
```

### Inspecting the database

```
forge db introspect --dsn "$DATABASE_URL"   # show live schema
task dev-psql                                # interactive shell
```

### Queries

- Simple queries: add functions in a sibling file you own (e.g. `internal/db/<entity>_queries.go`) using `pgx` directly — `<entity>_orm.go` is regenerated.
- Complex queries: write SQL in `db/queries/` and use sqlc to generate type-safe Go code. Run `forge generate` to pick up sqlc changes.

## Auto-generated features (at scaffold time)

Generated during initial project scaffolding from entity definitions in the plan:

- **Tenant scoping** — entities with a field marked `tenant: true` (the `(forge.v1.field)` annotation) get automatic `WHERE` clause scoping in ORM functions. Field names alone (e.g., `org_id`, `tenant_id`) do NOT trigger scoping — use the explicit annotation. See the `auth` skill for multi-tenant config.
- **Soft delete** — entities with a `deleted_at` field use `SET deleted_at = NOW()` instead of `DELETE`; List/Get exclude soft-deleted rows.
- **Seed data** — deterministic SQL seeds in `db/seeds/0002_*.sql` and JSON fixtures in `db/fixtures/` are generated from entity definitions. Put custom seed data in `db/seeds/0001_*.sql` so regeneration doesn't clobber it.

## Forge-specific rules

- **Don't hand-edit `gen/`** — fix the proto or query, then regenerate.
- **`forge generate` touches only the generated ORM files in the DB layer** (`internal/db/types.go`, `<entity>_orm.go`, from proto entities). You own migrations, queries, and mappers. Evolve them freely.
- **Seed files and fixtures are regenerated** by `forge generate` — put custom seed data in `db/seeds/0001_*.sql`.
- **Entity types are expected to diverge from proto.** Start with aliases, evolve to concrete structs when the domain requires it.

See also: `proto` for the proto-side view of the proto↔DB boundary, `architecture` for the canonical naming-conventions table.
<!-- @forge-only:end -->
