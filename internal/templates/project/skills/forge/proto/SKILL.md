---
name: proto
description: Proto file reference — annotations, CRUD conventions, field rules, and common mistakes.
---

# Proto File Reference

This skill is the annotation + naming reference. The structural conventions
that drive codegen (one service per file, explicit PK/tenant/timestamp
annotations) are **enforced reactively** by `forge lint --conventions` —
this skill is the proactive companion. See "Enforced by" notes below.

## Where Proto Files Live

```
proto/services/<service>/v1/<service>.proto    # Service definition (RPCs + messages)
proto/services/<service>/v1/entities.proto     # Optional — entity messages alone
proto/forge/v1/forge.proto                     # Forge annotations (vendored, never edited)
proto/shared/v1/types.proto                    # Cross-service shared messages (optional)
```

One service per `.proto` file. The package path is `<project>.services.<service>.v1`.
Cross-service references go through `proto/shared/v1/types.proto` — never
import another service's proto.

**Enforced by:** `forge lint --conventions` → `forgeconv-one-service-per-file`.

## After Editing Protos

```bash
forge generate && forge lint && forge build
```

Rebuilds `gen/` (Go stubs, TypeScript clients, mocks, wiring) and verifies
the build. Does **not** touch handlers, DB layer, or business logic.
Generated code is overwritten — fix issues in `.proto`, never in `gen/`.

### `forge generate` is the canonical entry — not `buf generate` alone

`buf generate` only emits the `.pb.go` / `_pb.ts` stubs. The forge ORM,
descriptors, mocks, frontend hooks, bootstrap wiring, and CRUD scaffolds
are produced by `forge generate`'s post-buf passes that read
`gen/forge_descriptor.json`. Running `buf generate` by itself after
adding a new entity in `proto/db/v1/` will:

  - produce `gen/db/v1/<entity>.pb.go` (proto types only)
  - leave `internal/db/<entity>_orm_gen.go` stale or absent
  - produce a confusing "ORM out of sync" lint warning on the next pass

**Always run `forge generate`** — it invokes `buf generate` internally,
then runs the ORM, descriptor, mock, hook, and bootstrap passes. Use
`buf generate` only when you specifically want stubs without the rest
of the forge surface (rare; mostly for debugging the proto pipeline
itself).

`forge lint` includes a `proto-orm-out-of-sync` check that fires when
`gen/db/v1/` carries a `.pb.go` for an entity that has no
corresponding `internal/db/<entity>_orm_gen.go` — typically a sign
that someone ran `buf generate` instead of `forge generate`.

## No Backwards Compatibility

Proto files in Forge projects are not published APIs with external
consumers. Edit freely — add, rename, remove fields, RPCs, and entities.
Mark removed fields as `reserved`; never reuse field numbers.

## Annotation Reference

```proto
import "forge/v1/forge.proto";
```

### Entity Annotation (`forge.v1.entity`)

Applied to messages to define database entities. **Annotation-only:** a
message becomes a forge entity *only* by carrying `option (forge.v1.entity)`.
There is no auto-detection by field name (`id`, `created_at`, `tenant_id`,
`*_id`) — every semantic must be declared.

```proto
message Task {
  option (forge.v1.entity) = {
    table_name:  "tasks"
    tenant_key:  "org_id"   // multi-tenant isolation; auto-filters queries
    soft_delete: true       // adds deleted_at lifecycle
    timestamps:  true       // manages created_at / updated_at
  };

  string id     = 1 [(forge.v1.field) = { pk: true }];                       // PK MUST be annotated
  string org_id = 2 [(forge.v1.field) = { tenant: true, ref: "orgs.id" }];   // tenant MUST be annotated
  string title  = 3 [(forge.v1.field) = { store: true }];
  string status = 4 [(forge.v1.field) = { store: true }];
}
```

| Option | Type | Purpose |
|--------|------|---------|
| `table_name` | string | Database table name |
| `tenant_key` | string | Tenant column for row-level isolation |
| `soft_delete` | bool | Use `deleted_at` instead of hard delete |
| `timestamps` | bool | Auto-manage `created_at` / `updated_at` |

**Enforced by:** `forgeconv-pk-annotation` (entity must declare `pk: true`),
`forgeconv-timestamps` (`*_at` Timestamp fields need `timestamps: true` or
explicit field annotation), `forgeconv-tenant-annotation` (tenant-shaped
field names need `tenant: true` when entity is tenant-scoped). Run
`forge lint --conventions` to surface violations with exact remediation.

### Field Annotation (`forge.v1.field`)

| Option | Type | Purpose |
|--------|------|---------|
| `pk` | bool | Primary key |
| `tenant` | bool | Tenant scoping column (auto-filters queries) |
| `store` | bool | Persist to database |
| `ref` | string | Foreign key reference (`"table.column"`) |
| `unique` | bool | Unique constraint |
| `index` | bool | Database index |
| `validate` | string | Validation rule |
| `immutable` | bool | Cannot be updated after creation |

### Service / Method Annotations

```proto
service TaskService {
  option (forge.v1.service) = { name: "tasks" version: "v1" };

  rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse) {
    option (forge.v1.method) = { auth_required: true };
  }
}
```

| Method option | Type | Purpose |
|---------------|------|---------|
| `auth_required` | bool | Require authentication |
| `idempotent` | bool | Mark as idempotent (safe to retry) |
| `timeout` | string | Request timeout |
| `idempotency_key` | bool | Expect `Idempotency-Key` header |

## CRUD RPC Naming Convention

Use these exact prefixes — Forge auto-generates handler implementations
in `handlers_crud_gen.go` for matching methods:

| RPC Name | Generated behavior |
|----------|--------------------|
| `Create<Entity>` | Insert via ORM |
| `Get<Entity>` | Select by ID |
| `List<Entities>` | Paginated list with filters |
| `Update<Entity>` | Update via ORM |
| `Delete<Entity>` | Delete (or soft-delete) |

Hand-written handler methods always win — the generator skips anything
you've already implemented.

### List Request Conventions (AIP-158)

```proto
message ListTasksRequest {
  int32 page_size = 1;
  string page_token = 2;
  optional string search = 3;   // ILIKE filter (auto-generated)
  optional string status = 4;   // Exact-match filter (auto-generated)
}

message ListTasksResponse {
  repeated Task tasks = 1;
  string next_page_token = 2;
}
```

Filter fields **must** be `optional` — otherwise generated code can't
distinguish "not set" from zero values.

## Enum Conventions

```proto
enum TaskStatus {
  TASK_STATUS_UNSPECIFIED = 0;   // Always required — prefix with enum name
  TASK_STATUS_PENDING = 1;
  TASK_STATUS_ACTIVE = 2;
}
```

- First value **must** be `0` and named `<ENUM_NAME>_UNSPECIFIED`.
- All values **must** be prefixed with the enum name in UPPER_SNAKE_CASE.
- Proto fields stay `snake_case` (`created_at`, `org_id`); proto messages / RPCs / services stay `PascalCase`. For the full Go ↔ proto ↔ TS ↔ `forge.yaml` casing table, see **Naming conventions** in `architecture`.

## Proto-driven entities: greenfield vs migrated

Whether proto entities are *authoritative* or *advisory* depends on
project shape. `forge audit` reports which mode you're in via the
`proto_migration_alignment` category.

### Greenfield (proto authoritative)

Default for `forge new`. Proto entity annotations drive codegen:
`forge generate` produces ORM, CRUD handlers, frontend hooks, and (on
first run with no migrations) an initial migration. Proto is the source
of truth; migrations track proto.

### Migrated projects (migrations authoritative)

When you bring an existing schema into forge (`migration/service`,
`migration/cli`), the migrations carry the schema and proto entities
become **advisory documentation**. `forge generate` does not regenerate
your migrations from proto. `forge audit` reports
`migrations authoritative (no proto entities)` if you removed the proto
entities entirely, or flags divergence as
`diverged: N table(s) in migrations not in proto, M in proto not in
migrations` if both exist and disagree.

When divergence is reported, decide:

- **Drop the proto entities** if the migrations are canonical and you
  don't need entity codegen — `forge audit` will then report
  `migrations authoritative (no proto entities)`.
- **Sync proto from DB** with `forge db proto sync-from-db` to bring
  proto back in line with current schema.
- **Roll a migration forward** to bring the schema in line with proto.

See the `db` and `migration` skills for the migration-side view of the
same boundary.

## Common Mistakes

1. **Missing forge import** — Every annotated proto needs `import "forge/v1/forge.proto";`.
2. **Enum without UNSPECIFIED=0** — Proto3 requires the zero value. Name it `<ENUM>_UNSPECIFIED`.
3. **Enum values without prefix** — Use `TASK_STATUS_ACTIVE`, not `ACTIVE`. Proto enums share a namespace.
4. **Non-optional filter fields** — List request filter fields must be `optional`.
5. **Reusing field numbers** — Mark removed fields as `reserved`, never reuse.
6. **Multiple services per file** — Lint-rejected. Use `proto-split`.
7. **Cross-service proto imports** — Hoist shared messages into `proto/shared/v1/types.proto`.
8. **Relying on field-name auto-detection** — `id`, `created_at`, `tenant_id`, `*_id` are NOT magic. Annotate explicitly.

## Rules

- One service per `.proto` file. **Enforced by `forgeconv-one-service-per-file`.**
- Always annotate PKs, tenants, timestamps. **Enforced by `forgeconv-pk-annotation`, `forgeconv-tenant-annotation`, `forgeconv-timestamps`.**
- Filter fields on List requests are always `optional`.
- Removed fields become `reserved`. Never reuse a number.
- Cross-service shared messages live in `proto/shared/v1/types.proto`.
- Run `forge generate && forge lint` after every proto edit.
- Fix issues in proto, not in `gen/` — generated code is overwritten.

## When this skill is not enough

- **Splitting a multi-service file** — see `proto-split`.
- **Designing the Go service surface** behind the proto — see `service-layer`.
- **Handler implementation patterns** — see `api/handlers` and `api`.
- **DB schema lifecycle** (migrations, entity-type evolution) — see `db`.
- **Entity / migration alignment for migrated projects** — see `migration` and `forge audit`'s `proto_migration_alignment` category.
