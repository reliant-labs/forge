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
  - leave `internal/db/<entity>_orm.go` stale or absent
  - produce a confusing "ORM out of sync" lint warning on the next pass

**Always run `forge generate`** — it invokes `buf generate` internally,
then runs the ORM, descriptor, mock, hook, and bootstrap passes. Use
`buf generate` only when you specifically want stubs without the rest
of the forge surface (rare; mostly for debugging the proto pipeline
itself).

`forge lint` includes a `proto-orm-out-of-sync` check that fires when
`gen/db/v1/` carries a `.pb.go` for an entity with no matching
`.pb.orm.go` sibling (or one older than the `.pb.go`) — typically a
sign that someone ran `buf generate` instead of `forge generate`.

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
    table:       "tasks"    // optional; inferred (snake_case, pluralized) when empty
    soft_delete: true       // adds deleted_at lifecycle
    timestamps:  true       // manages created_at / updated_at
    indexes:     [{ fields: ["org_id", "status"] }]
  };

  string id     = 1 [(forge.v1.field) = { pk: true }];                       // PK MUST be annotated
  string org_id = 2 [(forge.v1.field) = { tenant: true, ref: "orgs.id" }];   // tenant MUST be annotated
  string title  = 3 [(forge.v1.field) = { validate: { required: true, max_length: 200 } }];
  string status = 4;        // plain fields persist by default — no annotation needed
}
```

| Option | Type | Purpose |
|--------|------|---------|
| `table` | string | Database table name (inferred from message name when empty) |
| `soft_delete` | bool | Use `deleted_at` instead of hard delete |
| `timestamps` | bool | Auto-manage `created_at` / `updated_at` |
| `indexes` | IndexDef list | `{ name, fields, unique }` — composite/single-column indexes |
| `middleware` | string list | Repository middleware: `"tracing"`, `"metrics"`, `"logging"` |

Tenant isolation is declared on the FIELD (`tenant: true` below) — there
is no entity-level tenant option.

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
| `store` | StoreAs enum | Storage for complex/non-scalar types: `STORE_AS_JSONB`, `STORE_AS_TEXT`, `STORE_AS_BLOB`, `STORE_AS_TABLE`. Unset = inferred from the proto type — plain scalar fields need no `store` at all |
| `ref` | string | Foreign key reference (`"table.column"`) |
| `unique` | bool | Unique constraint |
| `index` | bool | Database index |
| `default_value` | string | SQL default expression (`"0"`, `"'active'"`, `"NOW()"`) |
| `skip` | bool | Exclude this field from code generation |
| `validate` | ValidationRules | Message, not a string: `validate: { required: true, format: "url", min_length: 3 }`. Fields: `required`, `min_length`, `max_length`, `pattern`, `format`, `min`, `max`, `allowed_values`, `custom` |
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
| `timeout` | Duration | Server-side timeout: `timeout: { seconds: 30 }` |
| `idempotency_key` | bool | Expect `Idempotency-Key` header |
| `errors` | string list | Declared Connect error codes (`"NotFound"`, `"InvalidArgument"`) |

## CRUD RPC Naming Convention

Use these exact prefixes. For matching methods, forge generates the
per-RPC op constructors (request→entity mapping, filters, response
packing) in `handlers_crud_ops_gen.go` (Tier-1, regenerated every run)
and scaffolds a thin delegation into the user-owned `handlers_crud.go`
(`return crud.HandleCreate(s.crudCreateItemOp())(ctx, req)`):

| RPC Name | Generated behavior |
|----------|--------------------|
| `Create<Entity>` | Insert via ORM |
| `Get<Entity>` | Select by ID |
| `List<Entities>` | Paginated list with filters |
| `Update<Entity>` | Update via ORM |
| `Delete<Entity>` | Delete (or soft-delete) |

Hand-written handler methods always win — the generator skips anything
you've already implemented. To customize a generated CRUD RPC, replace
the delegation body in `handlers_crud.go` directly (the file is yours;
new CRUD RPCs are appended, existing content is never modified).

When a request/response shape deviates from these conventions, forge
scaffolds an Unimplemented stub into `handlers_crud.go` carrying a
`FORGE_CRUD_SHAPE_MISMATCH: <reason>` comment (including the observed
field list); `forge audit` reports it under `crud_stubs` until you
implement the body.

### List Request Conventions (AIP-158)

```proto
message ListTasksRequest {
  int32 page_size = 1;
  string page_token = 2;
  optional string search = 3;   // ILIKE across the entity's string columns
  optional string status = 4;   // Exact-match filter — must name a declared entity column
}

message ListTasksResponse {
  repeated Task tasks = 1;
  string next_page_token = 2;
}
```

Filter fields **must** be `optional` — otherwise generated code can't
distinguish "not set" from zero values.

`search` / `query` / `q` are the fuzzy-search filters: they span the
entity's non-PK string columns via `orm.WhereILikeAny`. Any other
filter field must name a declared entity column — `forge generate`
fails loudly otherwise. A user-supplied `order_by` is validated against
the entity's declared-column allowlist (`<Entity>Columns`); an
undeclared column returns `InvalidArgument`, not a silent no-op.

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

When you bring an existing schema into forge (`migration-service`,
`migration-cli`), the migrations carry the schema and proto entities
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
- **Handler implementation patterns** — see `api`.
- **DB schema lifecycle** (migrations, entity-type evolution) — see `db`.
- **Entity / migration alignment for migrated projects** — see `migration` and `forge audit`'s `proto_migration_alignment` category.
