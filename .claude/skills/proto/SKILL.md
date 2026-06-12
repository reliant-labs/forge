---
name: proto
description: Proto file reference — annotations, CRUD conventions, field rules, and common mistakes.
---

# Proto File Reference

Proto is the **wire truth**: services, RPCs, and messages. It is NOT the
schema language — the database schema lives in `db/migrations/` (see the
`db` skill). The two halves meet at entity detection, below.

## Where Proto Files Live

```
proto/services/<service>/v1/<service>.proto    # Service definition (RPCs + messages)
proto/forge/v1/forge.proto                     # Forge annotations (imported, never edited)
```

One service per proto package. The package path is `<project>.services.<service>.v1`.

## After Editing Protos

Always regenerate after any proto change:

```bash
forge generate && forge build
```

This rebuilds `gen/` (Go stubs, TypeScript clients, mocks, wiring) and verifies the build. It does **not** touch handlers or business logic.

## No Backwards Compatibility

Proto files in Forge projects are not published APIs with external consumers. Edit freely — add, rename, remove fields, RPCs, and messages as needed. Mark removed fields as `reserved`; never reuse field numbers.

## Annotation Reference

Import forge annotations:

```proto
import "forge/v1/forge.proto";
```

### Service Annotation (`forge.v1.service`)

```proto
service TaskService {
  option (forge.v1.service) = {
    name: "tasks"
    version: "v1"
  };
}
```

### Method Annotation (`forge.v1.method`)

```proto
rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse) {
  option (forge.v1.method) = {
    auth_required: true
    idempotency_key: true
  };
}
```

| Option | Type | Purpose |
|--------|------|---------|
| `auth_required` | bool | Require authentication |
| `idempotent` | bool | Mark as idempotent (safe to retry) |
| `timeout` | string | Request timeout |
| `idempotency_key` | bool | Expect `Idempotency-Key` header |

### Retired: entity / field annotations

`(forge.v1.entity)` and `(forge.v1.field)` are **retired and ignored**.
Their definitions remain in `forge/v1/forge.proto` only as deprecated
tombstones so legacy protos keep compiling; `forge generate` prints a
notice for any message still carrying them. There is no proto-side
schema declaration: entities are projections of the applied
`db/migrations/` schema, and behavior (soft delete, timestamps, tenant
scoping) is read off real columns (`deleted_at`,
`created_at`/`updated_at`, `tenant_id`). To retire the annotations in an
older project, see the `migrations/proto-entities-to-schema-truth`
project skill.

## CRUD RPC Naming Convention — the wire half of an entity

An entity exists when **both halves** exist: a service proto declares
these CRUD RPCs (wire truth), AND the applied schema in `db/migrations/`
has the matching table — pluralized snake_case of the entity name
(storage truth). CRUD RPCs without a table generate honest nothing
(stubs, no pages, no ORM); tables without CRUD RPCs are plain schema for
hand-written code. `forge add entity` scaffolds both halves in one step.

| RPC Name | Request | Response | Generated behavior |
|----------|---------|----------|--------------------|
| `Create<Entity>` | `Create<Entity>Request` | `Create<Entity>Response` | Insert via ORM |
| `Get<Entity>` | `Get<Entity>Request` | `Get<Entity>Response` | Select by ID |
| `List<Entities>` | `List<Entities>Request` | `List<Entities>Response` | Paginated list with filters |
| `Update<Entity>` | `Update<Entity>Request` | `Update<Entity>Response` | Update via ORM |
| `Delete<Entity>` | `Delete<Entity>Request` | `Delete<Entity>Response` | Delete (or soft-delete when the table has `deleted_at`) |

The scaffolded messages are yours afterwards — the wire contract evolves
independently of the schema. Generated `<entity>ToProto` /
`<entity>FromProto` conversions map the intersection of wire fields and
columns by name: wire-only fields never reach the DB, column-only fields
never leak onto the wire.

### List Request Conventions

For auto-generated pagination and filtering:

```proto
message ListTasksRequest {
  int32 page_size = 1;          // AIP-158 pagination
  string page_token = 2;        // AIP-158 cursor
  optional string search = 3;   // ILIKE across the table's text columns
  optional bool done = 4;       // Exact-match filter — must name a real column
  string order_by = 5;
  bool descending = 6;
}

message ListTasksResponse {
  repeated Task tasks = 1;
  string next_page_token = 2;   // AIP-158 cursor
  int32 total_count = 3;
}
```

Filter fields **must** be `optional` — otherwise generated code can't distinguish "not set" from zero values. `order_by` is validated against the table's declared-column allowlist; an undeclared column returns `InvalidArgument`.

## Enum Conventions

```proto
enum TaskStatus {
  TASK_STATUS_UNSPECIFIED = 0;   // Always required — prefix with enum name
  TASK_STATUS_PENDING = 1;
  TASK_STATUS_ACTIVE = 2;
  TASK_STATUS_COMPLETED = 3;
}
```

- First value **must** be `0` and named `<ENUM_NAME>_UNSPECIFIED`
- All values **must** be prefixed with the enum name in UPPER_SNAKE_CASE

## Common Mistakes

1. **Missing forge import** — Every proto using `(forge.v1.method)` / `(forge.v1.service)` needs `import "forge/v1/forge.proto";`
2. **Enum without UNSPECIFIED=0** — Proto3 requires the zero value. Name it `<ENUM>_UNSPECIFIED`.
3. **Enum values without prefix** — Use `TASK_STATUS_ACTIVE`, not `ACTIVE`. Proto enums share a namespace.
4. **Non-optional filter fields** — List request filter fields must be `optional` for generated code to work.
5. **Reusing field numbers** — Mark removed fields as `reserved`, never reuse the number.
6. **Declaring schema in proto** — There is no entity annotation. Columns come from `db/migrations/`; CRUD RPCs without a matching table generate nothing. Create the table first (`forge add entity` or a migration).
7. **Forgetting to regenerate** — Run `forge generate` after every proto change.

## Rules

- One service per proto package, one handler directory per service.
- Proto declares the wire; `db/migrations/` declares the schema. Entity = CRUD RPCs + matching table.
- Mark removed fields as `reserved` — never reuse field numbers.
- Always `optional` for List filter fields.
- Always regenerate after proto changes: `forge generate && forge build`.
- Fix issues in proto, not in `gen/` — generated code is overwritten.
