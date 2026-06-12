---
name: db
description: Database work — SQL migrations are the single schema source of truth; forge generate projects the applied schema into entity structs, ORM, CRUD wiring, and frontend pages.
emit: both
---

# Database Work

## The key principle

**Migrations are the source of truth for your database schema.** Not API types, not ORM structs — the SQL files in your migrations directory. API contracts and DB schema evolve independently: the API describes what crosses the wire, the schema describes what the domain stores. These are different concerns with different evolution clocks.

## Schema evolution patterns

### Adding DB-only fields

Your DB often needs fields the API doesn't expose: audit trails, internal state, denormalized caches, soft-delete tombstones. Add them via migration — no API change needed.

### Entities that exist only in the DB

Not every table needs an API exposure. Internal bookkeeping tables, junction tables, event logs, queue state — create these with migrations and query them with hand-written code. No API contract, no cross-stack changes.

## Migration discipline

- **Migrations are append-only.** Never edit a merged migration — write a new one. Future developers' local state assumes the merged migration is immutable.
- **Always write the down-migration.** Even if you'll never run it in production, your test harness uses it for setup/teardown cycles.
- **Never migrate down against staging or prod.** Roll forward only. If a migration is wrong, write a new migration that undoes it, don't run the down.
- **Keep seed data out of migrations.** Migrations define schema; seeds populate it. Mixing them makes both harder to reason about.

<!-- @forge-only:start -->
## SQL is the schema language

In forge, `db/migrations/*.up.sql` is the **single source of truth** — there is no schema DSL and no proto annotation. `forge generate` applies every up-migration (in lexical order) to an in-memory SQLite shadow database, introspects the resulting tables (columns, types, nullability, PKs, indexes), and projects:

- **Entity structs + ORM** — `internal/db/<entity>_orm.go`. The struct is a projection of the applied schema: `time.Time` for timestamp columns (never `timestamppb`), pointer fields for nullable columns, native slices for array columns.
- **CRUD wiring** — `handlers/<svc>/handlers_crud_ops_gen.go`, including generated `<entity>ToProto` / `<entity>FromProto` conversions between the entity struct and the service-proto wire message.
- **Frontend pages, nav, and mocks** for each entity.

```
db/migrations/              # SQL migrations — THE schema source of truth (yours)
db/queries/                 # SQL query definitions (for sqlc or manual use; yours)
internal/db/<entity>_orm.go # Generated: entity struct + CRUD functions (regenerated)
```

Evolve the schema by writing a migration and re-running `forge generate`; the projections follow. Forge never modifies `db/migrations/` or `db/queries/` — the schema is always yours.

## What makes an entity

An entity exists when **both halves** exist:

1. **Wire truth** — a service proto declares the CRUD RPCs (`Create<X>` / `Get<X>` / `Update<X>` / `Delete<X>` / `List<Xs>`).
2. **Storage truth** — the applied schema has the matching table (pluralized snake_case of the entity name; `Bookmark` → `bookmarks`).

CRUD RPCs without a table generate **nothing** — honest stubs, no pages, no ORM. Tables without CRUD RPCs are **plain schema**: owned by your hand-written code, invisible to the CRUD/frontend projections.

## Starting a new entity

```bash
forge add entity bookmark url:string title:string tags:[]string done:bool
```

This emits `db/migrations/NNNNN_create_bookmarks.up.sql` (+ `.down.sql`) and — once — scaffolds the CRUD messages and RPCs into the service proto. Flags:

| Flag | Effect |
|------|--------|
| `--soft-delete` | add `deleted_at TIMESTAMPTZ` — deletes become UPDATEs, reads filter `IS NULL` |
| `--no-timestamps` | skip the managed `created_at` / `updated_at` columns |
| `--service <name>` | which service proto receives the CRUD RPCs (default: the project's only service) |
| `--no-rpcs` | emit only the migration; do not touch the service proto |
| `--table <name>` | table name override (default: pluralized snake_case of the entity name) |

Field type vocabulary (`name:type`):

| type | SQL column | proto field |
|------|-----------|-------------|
| `string` | `TEXT` | `string` |
| `int` / `int64` | `BIGINT` | `int64` |
| `float` | `DOUBLE PRECISION` | `double` |
| `bool` | `BOOLEAN` | `bool` |
| `time` | `TIMESTAMPTZ` | `google.protobuf.Timestamp` |
| `[]string` | `TEXT[]` | `repeated string` |
| `[]int` | `BIGINT[]` | `repeated int64` |
| `json` | `JSONB` | `string` (JSON text on the wire) |

The generated migration uses `id TEXT PRIMARY KEY CHECK (id <> '')`, `NOT NULL` with type-appropriate defaults on declared fields, and `created_at` / `updated_at TIMESTAMPTZ NOT NULL DEFAULT (now())`. The migration is yours after emission — adjust constraints and defaults freely, then run `forge generate`.

## Behavior by convention — real columns, no annotations

The columns ARE the declaration. The generators read these conventions off the introspected schema:

| columns | behavior |
|---------|----------|
| `deleted_at` | soft delete: DELETE becomes UPDATE, reads filter `IS NULL`, `ListAll*` is the unfiltered variant |
| `created_at` + `updated_at` | managed timestamps (stamped by the ORM) — type-gated: time columns (`TIMESTAMPTZ` et al) or legacy `TEXT` columns count; anything else (epoch integers) stays plain schema |
| `tenant_id` | tenant-scoped rows (auto-filtered queries) |
| text columns | spanned by the generated list `search` filter |
| every column | included in the `order_by` / filter allowlist (`<Entity>Columns`) |

## The portable pg/sqlite subset

Your project's tests **and** forge's shadow introspection apply `db/migrations/*.up.sql` to in-memory SQLite, so table-defining DDL must stay in the portable subset (everything `forge add entity` emits is already in it):

- Parenthesize function defaults: `DEFAULT (now())`, not `DEFAULT now()`.
- No `::type` casts: write `DEFAULT '{}'`, not `DEFAULT '{}'::jsonb` (postgres casts the literal implicitly).
- Native arrays (`TEXT[]`, `BIGINT[]`) are fine.

Postgres-only auxiliary statements (`CREATE EXTENSION` / `FUNCTION` / `TRIGGER` / `VIEW`, `COMMENT`, pg-specific DML) are skipped by the shadow — they can't affect the table/column model. A failing `CREATE TABLE` / `ALTER TABLE` / `DROP TABLE` / `CREATE INDEX` is a **hard `forge generate` error**: silently skipping one would generate an ORM that lies.

## Evolving an entity

Write a migration; the projections follow:

```sql
-- db/migrations/00007_add_bookmark_rating.up.sql
ALTER TABLE bookmarks ADD COLUMN rating BIGINT NOT NULL DEFAULT 0;
UPDATE bookmarks SET rating = 5 WHERE done;   -- data movement is plain SQL
```

```bash
forge generate   # struct, ORM, conversions, pages all pick up the column
```

Data movement (`UPDATE`, backfills) is natively expressible in SQL — something a schema DSL never gave you. Dropping a column is `ALTER TABLE ... DROP COLUMN` plus a regenerate.

## Wire evolution stays proto

The service-proto messages are the **API truth** and evolve independently after the one-time `forge add entity` scaffold. The generated conversions map the **intersection** of wire fields and columns by name: wire-only fields never reach the DB; column-only fields never leak onto the wire. Add a wire-only field to the proto, or a DB-only column via migration — neither side needs to know.

## Generated ORM semantics

- `Create<Entity>` is a plain `INSERT` — never an upsert. String PKs left unset are generated via `ulid.Make()` at the Create chokepoint. **Integer PKs are server-allocated**: the column is omitted from the INSERT and the database-assigned value is scanned back into the struct (`RETURNING` on postgres, `LastInsertId` on sqlite) — any caller-provided value is ignored.
- With stampable `created_at` / `updated_at` columns present, both are stamped on create and `updated_at` on update; `created_at` is immutable on update. Stamps are emitted **in the column's projected type**: time columns get `time.Now().UTC()`, legacy `TEXT` columns get RFC3339Nano text, nullable columns are stamped through their pointer.
- `internal/db/*_orm.go` (and `orm_shared.go`) are Tier-1 self-certifying files: each carries an embedded `forge:hash` marker in its header, so hand-edits trip the drift guard in any clone or worktree. `forge disown internal/db/<entity>_orm.go --reason ...` is the sanctioned one-way exit when the generated CRUD can't express what you need.
- Each entity exports `<Entity>Columns` — the declared-column allowlist. `forge/pkg/crud` validates user-supplied `order_by` against it; an undeclared column is `InvalidArgument`, not a silent no-op.
- Missing rows surface as `errors.Is(err, orm.ErrNoRows)`; `forge/pkg/crud` maps them through `pkg/svcerr` to a clean `NotFound`. All other repo errors map to `Internal` with safe text — no SQL on the wire.
- With a `deleted_at` column, `Delete<Entity>` issues an UPDATE, reads filter `IS NULL`, and a `ListAll<Entities>` variant returns soft-deleted rows too.

## Forge DB commands

### Changing the schema

```
forge add entity <name> [field:type ...]               # new table + CRUD scaffold
forge db migration new <name>                          # create an empty migration pair
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

## Forge-specific rules

- **Don't hand-edit `gen/`** or `internal/db/<entity>_orm.go` — fix the migration or proto, then regenerate.
- **`forge generate` rewrites only the generated entity ORM files** in the DB layer. Migrations, queries, seeds you wrote, and custom query files are yours. Evolve them freely.
- **The schema drives the struct, never the reverse.** Need a field on the entity struct? Write the migration that adds the column.
- **Legacy `(forge.v1.entity)` annotations are retired and ignored.** If a proto message still carries one, `forge generate` prints a notice; delete the annotation (and any `proto/db/` entity files) — see the `migrations/proto-entities-to-schema-truth` skill.

See also: `proto` for the wire half (CRUD RPC naming), `architecture` for the canonical naming-conventions table.
<!-- @forge-only:end -->
