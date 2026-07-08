---
name: proto
description: Proto file reference — annotations, CRUD conventions, field rules, and common mistakes.
---

# Proto File Reference

This skill is the annotation + naming reference. The structural conventions
that drive codegen (one service per file, CRUD method shapes) are
**enforced reactively** by `forge lint --conventions` — this skill is the
proactive companion.

Proto is the **wire truth**: services, RPCs, and messages. It is NOT the
schema language — the database schema lives in `db/migrations/` (see the
`db` skill). The two halves meet at entity detection, below.

## Where Proto Files Live

```
proto/services/<service>/v1/<service>.proto    # Service definition (RPCs + messages)
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
the build. Generated code is overwritten — fix issues in `.proto`, never in
`gen/`.

### `forge generate` is the canonical entry — not `buf generate` alone

`buf generate` only emits the `.pb.go` / `_pb.ts` stubs. The forge
descriptors, schema-driven ORM, mocks, frontend hooks, CRUD wiring, and
bootstrap codegen are produced by `forge generate`'s post-buf passes.
Running `buf generate` by itself leaves those projections stale.
**Always run `forge generate`** — it invokes `buf generate` internally.

## No Backwards Compatibility

Proto files in Forge projects are not published APIs with external
consumers. Edit freely — add, rename, remove fields, RPCs, and messages.
Mark removed fields as `reserved`; never reuse field numbers.

## Annotation Reference

```proto
import "forge/v1/forge.proto";
```

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

### Retired: entity / field annotations

`(forge.v1.entity)` and `(forge.v1.field)` are **retired and ignored**.
The option definitions remain in `forge/v1/forge.proto` only as deprecated
tombstones so legacy protos keep compiling; `forge generate` prints a
one-line notice for any message still carrying them. Entities are
projections of the applied `db/migrations/` schema now — there is no
proto-side schema declaration. If your project still has annotated entity
messages (or a `proto/db/` directory), see the
`migrations/proto-entities-to-schema-truth` skill for the flip.

## CRUD RPC Naming Convention — the wire half of an entity

An entity exists when **both halves** exist:

1. a service proto declares the CRUD RPCs below (**wire truth**), and
2. the applied schema in `db/migrations/` has the matching table —
   pluralized snake_case of the entity name (**storage truth**).

CRUD RPCs without a table generate honest nothing (Unimplemented stubs, no
pages, no ORM). Tables without CRUD RPCs are plain schema for hand-written
code. `forge add entity` scaffolds both halves in one step; the messages
and RPCs it writes into the service proto are yours afterwards — the wire
contract evolves independently of the schema.

Use these exact prefixes. For matching methods (with the matching table),
forge generates the per-RPC op constructors and the
`<entity>ToProto` / `<entity>FromProto` conversions in
`handlers_crud_ops_gen.go` (Tier-1, regenerated every run) and scaffolds a
thin delegation into the user-owned `handlers_crud.go`
(`return crud.HandleCreate(s.crudCreateItemOp())(ctx, req)`):

| RPC Name | Generated behavior |
|----------|--------------------|
| `Create<Entity>` | Insert via ORM |
| `Get<Entity>` | Select by ID |
| `List<Entities>` | Paginated list with filters |
| `Update<Entity>` | Update via ORM |
| `Delete<Entity>` | Delete (or soft-delete when the table has `deleted_at`) |

The generated conversions map the **intersection** of wire fields and
columns by name: a wire-only field never reaches the DB, a column-only
field never leaks onto the wire. Add wire-only fields freely.

Hand-written handler methods always win — the generator skips anything
you've already implemented. To customize a generated CRUD RPC, replace
the delegation body in `handlers_crud.go` directly (the file is yours;
new CRUD RPCs are appended, existing content is never modified).

When a request/response shape deliberately deviates from these
conventions (a list keyed by `ticker`+`limit` instead of AIP-158, say),
forge scaffolds an Unimplemented stub into `handlers_crud.go` carrying a
`forge:custom-read-shape: <reason>` comment (including the observed
field list). That is the system working, not an error — the custom
shape is a domain decision and the body is yours to implement, composing
the `pkg/crud`/`pkg/orm` helpers (cursor encode/decode, `WhereEq`/
`WhereILikeAny` filters, column-allowlisted order-by). `forge generate`
prints one warning line per stub it scaffolds, and `forge audit` reports
each under `crud_stubs` until the body lands (the RPC returns
`CodeUnimplemented` until then). Markers written by older forge versions
spell it `FORGE_CRUD_SHAPE_MISMATCH`; audit recognizes both for one
release.

### List Request Conventions (AIP-158)

```proto
message ListTasksRequest {
  int32 page_size = 1;
  string page_token = 2;
  optional string search = 3;   // ILIKE across the table's text columns
  optional bool done = 4;       // Exact-match filter — must name a real column
  string order_by = 5;
  bool descending = 6;
}

message ListTasksResponse {
  repeated Task tasks = 1;
  string next_page_token = 2;
  int32 total_count = 3;
}
```

Filter fields **must** be `optional` — otherwise generated code can't
distinguish "not set" from zero values.

`search` / `query` / `q` are the fuzzy-search filters: they span the
table's non-PK text columns via `orm.WhereILikeAny`. Any other filter
field must name a real column of the entity's table — `forge generate`
fails loudly otherwise. A user-supplied `order_by` is validated against
the table's declared-column allowlist (`<Entity>Columns`); an undeclared
column returns `InvalidArgument`, not a silent no-op.

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

## RPCs owned by another service/repo — import, don't re-scaffold

The protos above are the ones **your** service owns. For an RPC owned by
*another* service or repo (you're a client of it, not its implementer), the
remote proto is the single source of truth — **import it and generate only a
CLIENT.** Pull it in via a pinned `task protos` copy or a buf BSR dependency;
regenerate the client when the upstream version bumps.

Never hand-copy the remote `.proto` into your `proto/services/` and let forge
scaffold a server for it. That produces a dead handler package: every method
is `CodeUnimplemented`, `Deps` holds only `Logger`/`Config` with no domain
collaborators, and the wiring boots a service nobody calls. If you find
yourself staring at that shape — an all-`Unimplemented` handler for RPCs you
never meant to serve — you meant to *import the upstream proto and generate a
client*, not own the server.

## Common Mistakes

1. **Missing forge import** — Every proto using `(forge.v1.method)` / `(forge.v1.service)` needs `import "forge/v1/forge.proto";`.
2. **Enum without UNSPECIFIED=0** — Proto3 requires the zero value. Name it `<ENUM>_UNSPECIFIED`.
3. **Enum values without prefix** — Use `TASK_STATUS_ACTIVE`, not `ACTIVE`. Proto enums share a namespace.
4. **Non-optional filter fields** — List request filter fields must be `optional`.
5. **Reusing field numbers** — Mark removed fields as `reserved`, never reuse.
6. **Multiple services per file** — Lint-rejected. Use `proto-split`.
7. **Cross-service proto imports** — Hoist shared messages into `proto/shared/v1/types.proto`.
8. **Declaring schema in proto** — There is no entity annotation. Columns come from `db/migrations/`; if a CRUD RPC has no matching table, nothing is generated. Use `forge add entity` (or write the migration) first.

## Rules

- One service per `.proto` file. **Enforced by `forgeconv-one-service-per-file`.**
- Proto declares the wire; `db/migrations/` declares the schema. Entity = CRUD RPCs + matching table.
- Filter fields on List requests are always `optional`.
- Removed fields become `reserved`. Never reuse a number.
- Cross-service shared messages live in `proto/shared/v1/types.proto`.
- RPCs owned by another repo: **import the upstream proto and generate a client** — never hand-copy it and scaffold a server you won't implement.
- Run `forge generate && forge lint` after every proto edit.
- Fix issues in proto, not in `gen/` — generated code is overwritten.

## When this skill is not enough

- **Splitting a multi-service file** — see `proto-split`.
- **Designing the Go service surface** behind the proto — see `service-layer`.
- **Handler implementation patterns** — see `api`.
- **DB schema lifecycle** (migrations, conventions, the portable subset) — see `db`.
- **Retiring legacy entity annotations** — see `migrations/proto-entities-to-schema-truth`.
