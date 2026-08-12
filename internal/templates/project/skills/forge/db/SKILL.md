---
name: db
description: Database work — SQL migrations are the single schema source of truth; forge generate projects the applied schema into entity structs, ORM, CRUD wiring, and frontend pages.
emit: both
---

# Database Work

## The key principle

**Migrations are the source of truth for your database schema.** Not API types, not ORM structs — the SQL files in `db/migrations/`. The API describes what crosses the wire; the schema describes what the domain stores. Different concerns, different clocks, so they evolve independently:

- **DB-only fields** — audit trails, internal state, denormalized caches, tombstones. Add via migration; no API change.
- **DB-only tables** — bookkeeping, junction tables, event logs, queue state. Create with a migration, query by hand; no API contract, no cross-stack change.

## Migration discipline

- **Migrations are append-only.** Never edit a merged migration — write a new one. Everyone else's local state assumes it is immutable.
- **Always write the down-migration.** Your test harness uses it for setup/teardown even if prod never runs it.
- **Never migrate down against staging or prod.** Roll forward only: a wrong migration is undone by a new migration, not by running the down.
- **Keep seed data out of migrations.** Migrations define schema; seeds populate it.

<!-- @forge-only:start -->
## SQL is the schema language

There is no schema DSL and no proto annotation: `db/migrations/*.up.sql` is the **single source of truth**. `forge generate` applies every up-migration (lexical order) to an ephemeral postgres shadow database, introspects the resulting tables (columns, types, nullability, PKs, indexes), and projects:

- **Entity structs + ORM** — `internal/db/<entity>_orm.go`, mirroring the applied schema: `time.Time` for timestamp columns (never `timestamppb`), pointers for nullable columns, native slices for arrays.
- **CRUD wiring** — `internal/handlers/<svc>/handlers_crud_ops_gen.go` (beside the service's `contract.go` + handlers), including `<entity>ToProto` / `<entity>FromProto` conversions to the service-proto wire message.
- **Frontend pages, nav, and mocks** for each entity.

`db/migrations/` and `db/queries/` are yours and forge never modifies them; `internal/db/<entity>_orm.go` is regenerated.

## What makes an entity

An entity exists when **both halves** exist:

1. **Wire truth** — a service proto declares the CRUD RPCs (`Create<X>` / `Get<X>` / `Update<X>` / `Delete<X>` / `List<Xs>`).
2. **Storage truth** — the applied schema has the matching table (pluralized snake_case; `Bookmark` → `bookmarks`).

CRUD RPCs without a table generate **nothing** — honest stubs, no pages, no ORM. Tables without CRUD RPCs are **plain schema**: yours to query by hand, invisible to the CRUD/frontend projections.

## Starting a new entity

**The proto is the only place an entity is declared.** Write the message under a leading `// forge:entity` comment, then run bare `forge scaffold` (`--dry-run` plans; `--service <svc>` narrows): it births every marked message — missing CRUD quintet + owned migration pair — and generates, stubbing each custom RPC pb-through. The marker is consumed at birth only; once the table exists it is inert and evolution is a new migration. Envelope messages are refused even when marked.

There is no CLI field grammar (`forge scaffold entity <name> <field:type ...>` is refused, printing the equivalent message for you): everything an entity needs — enums, `optional` for nullable, `buf.validate` rules, `// forge:read-only` / `// forge:secret` / `// forge:append-only`, and real foreign keys — can only be spelled in the message.

`forge project annotations --kind field --json` is the **authoritative annotation spec** — the proto→column mapping a birth applies, every marker, and the `buf.validate` rules with their db/zod effects. It derives from forge's own renderer, recognizers and descriptors, so trust it over any table, including this one. A rule it omits (`const`, `uuid`, CEL) is still wire-enforced, just not projected.

## Birthing one named entity

Bare `forge scaffold` births every marked message. To birth one — or to birth a message whose CRUD quintet already exists but whose table does not — name it:

```bash
forge scaffold entity order --from-proto tasks
forge scaffold entity --from-proto tasks.Order
forge scaffold entity --from-proto tasks         # batch: quintets with no applied table
```

The `--from-proto <svc>` batch form sweeps `// forge:entity`-marked messages too. Birth is **one-time**: the migration is yours the moment it is written, and forge never re-reads the proto to regenerate or edit it.

**There is no import path** (`--from-schema` is refused). To put a wire contract on a table that already exists, hand-write the matching proto message and run `forge generate` — it projects the ORM and CRUD wiring from the applied schema without writing a migration.

### Birth markers — schema hardening baked into your owned migration

Everything a marker adds lands in the **user-owned birth migration**, never a forge-owned `_gen` file. Entity-level markers tablize the message, so a marked message needs no separate `// forge:entity` line. Run `forge project annotations --kind entity` (and `--kind field`) for the full list, placement and effect. The storage-side nuances:

- **`// forge:append-only`** — a real **DB trigger** in the birth migration raises on any `UPDATE`/`DELETE`, so no bug or compromised caller can rewrite history; `.down.sql` drops the trigger + function. The CRUD quintet completes to Create/Get/List only.
- **`// forge:secret`** — the column stays real schema truth and settable on Create/Update; only the read path (`<entity>ToProto`) drops it.
- **`// forge:read-only`** — the input-side mirror: readable but not client-writable (status, computed price, a lifecycle timestamp). It removes the field from the write envelopes and nothing more, so give the column a DB `DEFAULT` or set it in your handler, or the first insert lands the zero. Mark it up front — the AIP-134 `Update<Entity>Request` wraps the whole entity, so excluding the field at birth also keeps it out of the born `new/page.tsx`, edit form and `handlers_crud_test.go`, and hand-stripping it later breaks those scaffold-once files.
- **`// forge:soft-delete`** — **OPT-IN**: unmarked entities get no `deleted_at` and hard-delete. Beyond the read filter, `ListAll<Entities>` returns tombstones. The `--soft-delete` flag and a message already carrying a `deleted_at` field do the same thing.

Proto → SQL, in both birth forms: scalars to `NOT NULL` columns with zero defaults, `optional` to nullable, enums to `TEXT` + `CHECK (col IN (...))` (DEFAULT = first non-`_UNSPECIFIED` member), repeated scalars to arrays, maps/nested messages to `JSONB`, and an `*_id` string whose stem resolves to a real entity to `TEXT` + an applied `REFERENCES` and index. oneof/`Any`/cross-package fields become TODO comment lines, never silently dropped. Envelopes — Request/Response messages, anything carrying `page_size`/`page_token`/`update_mask` — are refused.

### Column write policy — ownership, immutability, concurrency

Whether a full-replace UPDATE may touch a column is schema truth, declared as a
`COMMENT ON COLUMN` in the migration that owns it and introspected by `forge
generate`. Three decisions live there, all in `db/write-policy`:

- **Row ownership** — forge stores none of its own. If rows belong to a user, an
  org or an account, that is a column you write in the birth migration; adding
  it later is a migration plus a backfill.
- **`forge:immutable`** — keeps a full-replace Update from zeroing a column the
  request never carried (a server-assigned owner id, a Stripe customer ID, a
  creation stamp).
- **`forge:version`** — opt-in optimistic concurrency. Without it the last
  writer wins silently.

### Two routes to the same parent

When a table reaches the same parent two ways — `estimates.customer_id`
directly, and `estimates.job_id → jobs.customer_id` — both foreign keys are
individually valid and the two routes can still name different rows. Declare
which one wins with a `COMMENT ON CONSTRAINT ... 'forge:ref ...'` in a
migration, the same `COMMENT ON` vocabulary as `forge:immutable` above on a
constraint instead of a column. `forge db seed` refuses to write rows where an
undeclared pair disagrees. The vocabulary, the sweep query that finds every
diamond at once, and which route to pick are in `db/seeding`.

## Behavior by convention — real columns, no annotations

The columns ARE the declaration. The generators read these off the introspected schema:

| columns | behavior |
|---------|----------|
| `deleted_at` | soft delete: DELETE becomes UPDATE, reads filter `IS NULL`, `ListAll*` is the unfiltered variant |
| `created_at` + `updated_at` | ORM-managed timestamps — type-gated: time columns (`TIMESTAMPTZ` et al) or legacy `TEXT` count; anything else (epoch integers) stays plain schema |
| text columns | spanned by the generated list `search` filter |

## Just write postgres

forge is postgres-pinned. Your tests **and** forge's shadow introspection apply `db/migrations/*.up.sql` **verbatim** to a real ephemeral postgres (no Docker — a cached embedded-postgres binary, or a server via `FORGE_TEST_POSTGRES_URL`). There is no portability subset to honor: anything postgres accepts works — `::type` casts, schema-qualified names, `JSONB`, native arrays, generated/identity columns.

Statements the bare ephemeral DB can't satisfy (`CREATE EXTENSION` for an uninstalled extension, a nonexistent role) are skipped by the shadow — they can't affect the table/column model. A failing `CREATE TABLE` / `ALTER TABLE` / `DROP TABLE` / `CREATE INDEX` is a **hard `forge generate` error**, since skipping one would generate an ORM that lies.

## Evolving an entity

Write a migration; the projections follow. Data movement is plain SQL in the same file.

```sql
-- db/migrations/00007_add_bookmark_rating.up.sql
ALTER TABLE bookmarks ADD COLUMN rating BIGINT NOT NULL DEFAULT 0;
UPDATE bookmarks SET rating = 5 WHERE done;
```

```bash
forge generate   # struct, ORM, conversions, pages all pick up the column
```

Wire evolution stays proto: service-proto messages are the **API truth** and evolve independently after the one-time scaffold. The generated conversions map the **intersection** of wire fields and columns by name — wire-only fields never reach the DB, column-only fields never reach the wire.

## Generated ORM semantics

- `Create<Entity>` is a plain `INSERT`, never an upsert. Unset string PKs are generated via `ulid.Make()` at the Create chokepoint. **Integer PKs are server-allocated**: the column is omitted from the INSERT and the database-assigned value is scanned back via `RETURNING` — any caller-provided value is ignored.
- With stampable `created_at` / `updated_at` columns, both are stamped on create and `updated_at` on update; `created_at` is immutable on update. Stamps use the column's projected type: time columns get `time.Now().UTC()`, legacy `TEXT` columns get RFC3339Nano text, nullable columns are stamped through their pointer.
- `internal/db/*_orm.go` (and `orm_shared.go`) are Tier-1 self-certifying: each carries an embedded `forge:hash` marker, so hand-edits trip the drift guard in any clone or worktree. `forge project disown internal/db/<entity>_orm.go --reason ...` is the sanctioned one-way exit.
- Each entity exports `<Entity>Columns`, the declared-column allowlist. `forge/pkg/crud` validates user-supplied `order_by` against it; an undeclared column is `InvalidArgument`, not a silent no-op.
- `Get<Entity>ByID` answers a missing row with `svcerr.NotFound("<entity>")` — return or `svcerr.Wrap` it, never re-derive it. `Update`/`Delete` still give `orm.ErrNoRows`. Other repo errors → `Internal`, no SQL on the wire.

## Diverging from generated CRUD

`handlers_crud_ops_gen.go` is Tier-1 and keeps regenerating. To customize one RPC — or to attach a per-caller policy to one — override an exported op field in your owned `handlers_crud.go` instead of disowning the ops file. Load the `db/crud-overrides` skill for the seams.

## Forge DB commands

Each takes `--dsn "$DATABASE_URL"`:

```
forge db migration new <name>      # create an empty migration pair
forge db migrate up                # apply pending migrations
forge db migrate status            # show what's applied
forge db migrate force <version>   # recover from a dirty migration
forge db introspect                # show live schema
task dev-psql                      # interactive shell (no --dsn)
```

Dev DSN convention: `postgres://postgres:postgres@localhost:5432/<project>?sslmode=disable`

Deployed envs migrate themselves — load the `db/deploy-migrations` skill.

`forge db seed apply|status|reset` materializes deterministic dev rows from the applied schema, and `db/seeds/vocab.yaml` teaches it your domain's vocabulary — load the `db/seeding` skill before seeding.

### Queries

- Simple: `internal/db/<entity>_repo_ext.go` — the custom-query seam forge scaffolds beside every entity, once, and never rewrites. Its header names the tools: the generated repo delegates with `orm.QueryOption` filters, raw SQL via `db.Bun()`, and the `orm.Context` that runs a method in or out of a transaction.
- Complex: write SQL in `db/queries/` and use sqlc. Run `forge generate` to pick up sqlc changes.

## Forge-specific rules

- **Draft, then `forge generate` — it is the fast, exact oracle.** Let `forge scaffold entity` write the first-draft migration + proto (or hand-write one), then generate, read the error, fix, repeat. A failing `CREATE TABLE`/`ALTER TABLE`, a missing import, or a List filter naming no real column fails loudly. Don't reverse-engineer forge's `internal/**` or exhaustively plan first.
- **The schema drives the struct, never the reverse.** Need a field on the entity struct? Write the migration that adds the column. Don't hand-edit `gen/` or `internal/db/<entity>_orm.go` — those are the only DB-layer files generate rewrites.

See also: `proto` for the wire half (CRUD RPC naming), `architecture` for the naming-conventions table.
<!-- @forge-only:end -->
