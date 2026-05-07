---
name: db
description: Database work — migrations own the schema, entity types evolve independently, forge generate never touches the DB layer.
---

# Database Work

## The key principle

**Migrations are the source of truth for your database schema.** Not proto, not Go structs — the SQL files in `db/migrations/`. Proto defines your API contracts. Migrations define your persistence. These evolve independently.

`forge generate` does **NOT** touch the database layer. It never modifies `internal/db/`, `db/migrations/`, or `db/queries/`. You own all of this completely — evolve it freely.

## Proto entities: greenfield vs migrated

Two modes of operation, and `forge audit` (`proto_migration_alignment`
category) tells you which one you're in:

- **Greenfield (proto authoritative).** A fresh `forge new` with proto
  entity annotations. `forge generate` produces ORM and CRUD handlers
  from the proto; on the first run with no migrations it also generates
  an initial migration from the entities. Proto leads, migrations track.

- **Migrated (migrations authoritative).** When you bring an existing
  schema into forge via `migration/service` or `migration/cli`, the
  migrations carry the schema and proto entities become **advisory**.
  `forge generate` does not regenerate migrations from proto. If you
  removed proto entities entirely, `forge audit` reports
  `migrations authoritative (no proto entities)`. If both exist and
  diverge it flags
  `diverged: N table(s) in migrations not in proto, M in proto not in
  migrations` and you decide whether to drop the proto entities, sync
  them via `forge db proto sync-from-db`, or roll a migration forward
  to match proto.

In both modes, **migrations remain the runtime source of truth.** Proto
entities are either driving codegen (greenfield) or documenting it
(migrated) — but the schema itself lives in `db/migrations/`. See the
`proto` skill for the proto-side view of the same boundary.

## Architecture

```
db/migrations/              # SQL migrations — THE schema source of truth
db/queries/                 # SQL query definitions (for sqlc or manual use)
internal/db/types.go        # Entity types (start as proto aliases, evolve to concrete structs)
internal/db/<entity>_orm.go # CRUD functions for each entity
```

### Entity type lifecycle

Entity types follow a natural progression:

**Stage 1 — Proto alias** (scaffolded by Forge):
```go
// internal/db/types.go
type User = apiv1.User
```

When API and DB schema are identical, this keeps things simple. No mapping needed — same type everywhere.

**Stage 2 — Concrete struct** (when API and DB diverge):
```go
// internal/db/types.go
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
// internal/db/mappers.go (or in the handler)
func UserToProto(u *User) *apiv1.User {
    return &apiv1.User{
        Id:    u.ID,
        Name:  u.Name,
        Email: u.Email,
    }
}

func UserFromProto(u *apiv1.User) *User {
    return &User{
        ID:    u.Id,
        Name:  u.Name,
        Email: u.Email,
    }
}
```

## Common tasks

### Changing the schema

1. Create a migration:
   ```
   forge db migration new <name>
   ```
2. Write both the `.up.sql` and `.down.sql` in the generated pair.
3. Apply it:
   ```
   forge db migrate up --dsn "$DATABASE_URL"
   ```
4. Update the entity type:
   - If API and DB still match → keep the proto alias.
   - If they diverge → replace the alias with a concrete struct in `internal/db/types.go`.
5. Update ORM functions in `internal/db/<entity>_orm.go` to match.

Dev DSN:
```
postgres://postgres:postgres@localhost:5432/<project>?sslmode=disable
```

### Adding a new entity

1. **Migration first** — create the table:
   ```
   forge db migration new add_invoices_table
   ```
   Write the SQL to create the table.

2. **Entity type** — add to `internal/db/types.go`:
   ```go
   // If the API shape matches your table, use an alias:
   type Invoice = apiv1.Invoice

   // If the table has fields the API doesn't, use a concrete struct:
   type Invoice struct {
       ID          string
       CustomerID  string
       Amount      int64
       // ... DB-specific fields
   }
   ```

3. **ORM functions** — create `internal/db/invoice_orm.go` with CRUD operations.

4. **Proto (if needed)** — if this entity needs API exposure, define the message in proto and run `forge generate`. But not every DB entity needs an API message.

### Adding or updating a query

- Simple queries: add functions in `internal/db/<entity>_orm.go` using `pgx` directly.
- Complex queries: write SQL in `db/queries/` and use sqlc to generate type-safe Go code. Run `forge generate` to pick up sqlc changes.

### Checking what's in the database

```bash
forge db introspect --dsn "$DATABASE_URL"   # Show live schema
forge db migrate status --dsn "$DATABASE_URL"  # Migration status
task dev-psql                                # Interactive shell
```

### Fixing a broken migration

1. Check current state:
   ```
   forge db migrate status --dsn "$DATABASE_URL"
   ```
2. If the migration table is dirty, force to the last good version:
   ```
   forge db migrate force <version> --dsn "$DATABASE_URL"
   ```
3. Fix the SQL and roll forward — don't try to re-run the broken migration.

## Schema evolution patterns

### Adding DB-only fields

Your DB often needs fields the API doesn't expose (audit trails, internal state, denormalized caches). Add them via migration, update the concrete struct — no proto change needed.

### API and DB with different field names or shapes

Proto fields are `snake_case` (e.g. `created_at`, `org_id`) and the generated Go types are `PascalCase` (`CreatedAt`, `OrgID`). Your DB might use different column names or different types (e.g. `pgtype.Timestamptz` instead of `*timestamppb.Timestamp`). Concrete structs + mappers handle this cleanly. See `architecture` for the canonical naming-conventions table.

### Entities that exist only in the DB

Not every table needs a proto message. Internal bookkeeping tables, junction tables, event logs — create these with migrations and concrete structs. No proto, no API, no `forge generate` needed.

## Auto-generated features (at scaffold time)

These are generated during initial project scaffolding based on entity definitions in the plan:

- **Tenant scoping** — Entities with a field explicitly marked `tenant_key: true` get automatic `WHERE` clause scoping in ORM functions. Field names alone (e.g., `org_id`, `tenant_id`) do NOT trigger tenant scoping — use the explicit annotation. See the auth skill for multi-tenant config.
- **Soft delete** — Entities with a `deleted_at` field use `SET deleted_at = NOW()` instead of `DELETE`, and List/Get excludes soft-deleted rows.
- **Seed data** — Deterministic SQL seeds in `db/seeds/0002_*.sql` and JSON fixtures in `db/fixtures/` are generated from entity definitions. Put custom seed data in `db/seeds/0001_*.sql`.

## Rules

- **Migrations are append-only.** Never edit a merged migration — write a new one.
- **Always write the `.down.sql`** — the test harness uses it.
- **Never `forge db migrate down` against staging/prod** — roll forward only.
- **Don't hand-edit `gen/`** — fix the proto or query, then regenerate.
- **Keep seed data out of migrations** — use `db/seeds/`.
- **`forge generate` never touches the DB layer.** You own migrations, entity types, and ORM code. Evolve them freely.
- **Seed files and fixtures are regenerated** by `forge generate` — put custom seed data in `db/seeds/0001_*.sql`.
- **Entity types are expected to diverge from proto.** Start with aliases, evolve to concrete structs when the domain requires it.
