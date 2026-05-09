---
name: proto
description: Proto file reference — annotations, CRUD conventions, field rules, and common mistakes.
---

# Proto File Reference

## Where Proto Files Live

```
proto/services/<service>/v1/<service>.proto    # Service definition (RPCs + messages)
proto/services/<service>/v1/entities.proto     # Entity messages (optional, can inline in service proto)
proto/forge/v1/forge.proto                     # Forge annotations (imported, never edited)
```

One service per proto package. The package path is `<project>.services.<service>.v1`.

## After Editing Protos

Always regenerate after any proto change:

```bash
forge generate && forge build
```

This rebuilds `gen/` (Go stubs, TypeScript clients, mocks, middleware, wiring) and verifies the build. It does **not** touch handlers, DB layer, or business logic.

## No Backwards Compatibility

Proto files in Forge projects are not published APIs with external consumers. Edit freely — add, rename, remove fields, RPCs, and entities as needed. There is no backwards compatibility requirement.

## Annotation Reference

Import forge annotations:

```proto
import "forge/v1/forge.proto";
```

### Entity Annotation (`forge.v1.entity`)

Applied to messages to define database entities:

```proto
message Task {
  option (forge.v1.entity) = {
    table_name: "tasks"
    soft_delete: true
    timestamps: true
  };

  string id = 1 [(forge.v1.field) = { pk: true }];
  string org_id = 2 [(forge.v1.field) = { tenant: true }];
  string title = 3 [(forge.v1.field) = { store: true }];
  string status = 4 [(forge.v1.field) = { store: true }];
}
```

| Option | Type | Purpose |
|--------|------|---------|
| `table_name` | string | Database table name |
| `soft_delete` | bool | Use `deleted_at` instead of hard delete |
| `timestamps` | bool | Auto-manage `created_at` / `updated_at` |

### Field Annotation (`forge.v1.field`)

Applied to individual fields:

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
    idempotent: false
  };
}
```

| Option | Type | Purpose |
|--------|------|---------|
| `auth_required` | bool | Require authentication |
| `idempotent` | bool | Mark as idempotent (safe to retry) |
| `timeout` | string | Request timeout |
| `idempotency_key` | bool | Expect `Idempotency-Key` header |

## CRUD RPC Naming Convention

Use these exact prefixes — Forge auto-generates handler implementations for matching methods:

| RPC Name | Request | Response | Generated behavior |
|----------|---------|----------|--------------------|
| `Create<Entity>` | `Create<Entity>Request` | `Create<Entity>Response` | Insert via ORM |
| `Get<Entity>` | `Get<Entity>Request` | `Get<Entity>Response` | Select by ID |
| `List<Entities>` | `List<Entities>Request` | `List<Entities>Response` | Paginated list with filters |
| `Update<Entity>` | `Update<Entity>Request` | `Update<Entity>Response` | Update via ORM |
| `Delete<Entity>` | `Delete<Entity>Request` | `Delete<Entity>Response` | Delete (or soft-delete) |

### List Request Conventions

For auto-generated pagination and filtering:

```proto
message ListTasksRequest {
  int32 page_size = 1;          // AIP-158 pagination
  string page_token = 2;        // AIP-158 cursor
  optional string search = 3;   // ILIKE filter (auto-generated)
  optional string status = 4;   // Exact-match filter (auto-generated)
}

message ListTasksResponse {
  repeated Task tasks = 1;
  string next_page_token = 2;   // AIP-158 cursor
}
```

Filter fields **must** be `optional` — otherwise generated code can't distinguish "not set" from zero values.

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

1. **Missing forge import** — Every proto using annotations needs `import "forge/v1/forge.proto";`
2. **Enum without UNSPECIFIED=0** — Proto3 requires the zero value. Name it `<ENUM>_UNSPECIFIED`.
3. **Enum values without prefix** — Use `TASK_STATUS_ACTIVE`, not `ACTIVE`. Proto enums share a namespace.
4. **Non-optional filter fields** — List request filter fields must be `optional` for generated code to work.
5. **Reusing field numbers** — Mark removed fields as `reserved`, never reuse the number.
6. **Multiple entities per service** — Keep one service per proto package, one domain per service.
7. **Forgetting to regenerate** — Run `forge generate` after every proto change.

## Rules

- One service per proto package, one handler directory per service.
- Mark removed fields as `reserved` — never reuse field numbers.
- Always `optional` for List filter fields.
- Always regenerate after proto changes: `forge generate && forge build`.
- Fix issues in proto, not in `gen/` — generated code is overwritten.
