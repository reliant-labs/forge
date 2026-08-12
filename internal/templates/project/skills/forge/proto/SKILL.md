---
name: proto
description: Proto file reference — annotations, CRUD conventions, field rules, and common mistakes.
---

# Proto File Reference

Annotation + naming reference. Structural conventions (one service per file,
CRUD method shapes) are enforced by `forge lint --conventions`.

Proto is the **wire truth**: services, RPCs, messages. It is NOT the schema
language — the schema lives in `db/migrations/` (see `db`).

## Where Proto Files Live

```
proto/services/<service>/v1/<service>.proto    # Service definition (RPCs + messages)
proto/forge/v1/forge.proto                     # Forge annotations (vendored, never edited)
proto/shared/v1/types.proto                    # Cross-service shared messages (optional)
```

One service per `.proto` file (`forge lint --conventions` →
`forgeconv-one-service-per-file`). The proto package mirrors the file's path
under `proto/`, with no project prefix: `services.<service>.v1` — `buf lint`'s
`PACKAGE_DIRECTORY_MATCH` rejects any other form. Cross-service references go
through `proto/shared/v1/types.proto`; never import another service's proto.

## After Editing Protos

```bash
forge generate && forge lint && forge build
```

Rebuilds `gen/` (Go stubs, TypeScript clients, mocks, wiring) and verifies the
build. Generated code is overwritten — fix issues in `.proto`, never in `gen/`.

`forge generate` is the canonical entry, never `buf generate` alone: buf emits
only the `.pb.go` / `_pb.ts` stubs, while the descriptors, schema-driven ORM,
mocks, frontend hooks and CRUD wiring come from forge's post-buf passes (it runs
`buf generate` internally).

## No Backwards Compatibility

Proto files in Forge projects are not published APIs with external consumers.
Edit freely — add, rename, remove fields, RPCs and messages. Mark removed fields
as `reserved`; never reuse field numbers.

## Annotation Reference

```proto
import "forge/v1/forge.proto";
```

### Service / Method Annotations

```proto
service TaskService {
  option (forge.v1.service) = { name: "tasks" version: "v1" };

  rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse) {
    option (forge.v1.method) = {
      auth_required: true
      idempotency_key: true
    };
  }
}
```

`forge scaffold entity` writes exactly this onto every scaffolded CRUD RPC.

**Don't guess the option surface — dump it.** `forge project annotations` emits
the authoritative spec — every `forge:*` marker, the proto→column mapping, the
projected `buf.validate` rules, and every `(forge.v1.service)` /
`(forge.v1.method)` option with its type, effect and default — from forge's
compiled descriptors, so it cannot drift:

```bash
forge project annotations --kind method          # readable table
forge project annotations --kind service --json  # machine-readable
forge project annotations --json                 # everything
```

The whole method surface is five options — there are no others, and a name
forge does not know is a **hard `buf generate` failure**, not a warning:

| option | type | what it does |
|---|---|---|
| `auth_required` | bool | whether the caller must be authenticated (see below) |
| `idempotent` | bool | marks the method safe to retry |
| `idempotency_key` | bool | advisory marker for callers; forge does not enforce it |
| `timeout` | Duration | per-method server timeout — `timeout: { seconds: 30 }` |
| `errors` | repeated string | declared error codes, e.g. `errors: ["NotFound"]` |

There is **no `description`** — the prose that documents an rpc is the
ordinary `//` comment above it. Writing one inside the option block fails
with `field description not found` and takes the whole generate down with it.

`auth_required` is **fail-closed and per-rpc**: auth is required even when the
option is unset, and `auth_required: false` publishes *that one rpc* to
unauthenticated callers while its siblings stay closed. There is no service-wide
posture to set. It declares only *whether a caller must be authenticated*; what
an authenticated caller may then do is handler logic against
`middleware.GetUser(ctx)`, checked against the row's own owner column (`auth`,
and `db` for where that column comes from).

### No schema annotations

There is no proto-side schema declaration: entities are projections of the
applied `db/migrations/` schema. `(forge.v1.entity)` and `(forge.v1.field)` are
ignored — delete any that survive in your protos, along with a `proto/db/`
directory if you have one. A column's default is likewise plain SQL (`NOT NULL
DEFAULT <expr>`) in the birth migration, not an annotation.

### Birth markers — the `// forge:*` comments

You author an entity's proto message and birth it with `forge scaffold` (or
`forge scaffold entity <name> --from-proto <svc>` for one named message) — the
only birth path forge has. **Comment markers** shape what birth emits. They are
ordinary `//` comments, not `(forge.v1.*)` options, read by the raw-proto
scanner at birth only and inert once the table exists. Message-level markers go
in the leading comment; field-level markers take a leading comment **or** a
trailing one after the field's `;` (inline `[...]` options in between are fine).

**Managed fields are declared, not assumed.** Birth writes `id`, `created_at`
and `updated_at` (plus `deleted_at` under `forge:soft-delete`) into your message
at field numbers above its high-water mark, one time, and they are yours
thereafter. Declare them yourself if you prefer — birth leaves a field the
message already has alone. Either way the message describes its own shape, which
is what the ORM row type, the CRUD envelopes and the generated edit pages
project.

Run `forge project annotations --kind entity` / `--kind field` for the current
marker list and what each emits. In practice: `forge:entity` tablizes a message;
`forge:soft-delete` and `forge:append-only` tablize *and* shape the lifecycle
(opt-in soft delete; an immutable ledger with no Update/Delete);
`forge:read-only` and `forge:secret` trim a field off the born write and read
sides respectively.

```proto
// forge:soft-delete
message Product {
  string name = 1;
  double price = 2 [(buf.validate.field).double.gte = 0];  // forge:read-only
  string api_token = 3;      // forge:secret
  bool requires_prescription = 4;
  // birth appends: id, created_at, updated_at, deleted_at
}
```

Storage-side semantics (what each marker adds to the owned migration, the
proto→column type mapping, and the entity-shaping flags `--soft-delete` /
`--no-timestamps` / `--table`) live in `db`.

**Spell a marker wrong and nothing happens — quietly.** An unrecognized marker
is just a proto comment: it compiles, the birth exits zero, and the field keeps
its default behavior, so a mistyped `forge:read-only` leaves the field
client-writable. `forge generate` cannot catch this, since there is no error to
raise; `forge lint --proto-markers` flags any `forge:*` in a proto comment that
matches no known marker, naming the file, the line and the closest real marker:

```
⚠ [forge-proto-markers] proto/services/orders/v1/orders.proto:8
    → "forge:server-set" was renamed to "forge:read-only" and is no longer
      recognized — this comment currently does NOTHING (the field stays
      client-writable). Rewrite it as "forge:read-only".
```

Warnings only: it never fails the build, since an unrecognized marker might be
a future forge version's or prose that happens to contain `forge:`. It runs in
the default `forge lint` pass whenever a `proto/` tree exists.

### Getting annotation syntax right: run `forge generate`

It's a fast, exact oracle. Draft the proto (for a new entity, mark it
`// forge:entity` and let `forge scaffold` write the migration), run it, read
the error, fix, repeat. It fails **loudly** on a bad List filter (a field naming
no real column), a missing import, a malformed message, or a migration postgres
rejects. Don't plan the annotation surface up front.

## CRUD RPC Naming Convention — the wire half of an entity

An entity needs both a service proto declaring the CRUD RPCs below and a
matching table in the applied schema (pluralized snake_case of the entity
name); see `db` for the storage half. `forge scaffold entity` writes both in one
step, and the messages and RPCs it puts in the service proto are yours
afterwards — the wire contract evolves independently of the schema.

Use these exact prefixes. For matching methods (with the matching table), forge
generates the per-RPC op constructors and the `<entity>ToProto` /
`<entity>FromProto` conversions in `handlers_crud_ops_gen.go` (Tier-1,
regenerated every run) and scaffolds a thin delegation into the user-owned
`handlers_crud.go` (`return crud.HandleCreate(s.crudCreateItemOp())(ctx, req)`):

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

Hand-written handler methods always win — the generator skips anything you've
already implemented. To customize a generated CRUD RPC, replace the delegation
body in `handlers_crud.go` (the file is yours; new CRUD RPCs are appended,
existing content never modified).

When a request/response shape deliberately deviates (a list keyed by
`ticker`+`limit` instead of AIP-158, say), forge scaffolds an Unimplemented stub
into `handlers_crud.go` carrying a `forge:custom-read-shape: <reason>` comment
with the observed field list. That is the system working, not an error — the
body is yours to implement, composing the `pkg/crud`/`pkg/orm` helpers (cursor
encode/decode, `WhereEq`/`WhereILikeAny` filters, column-allowlisted order-by).
`forge generate` prints one warning line per stub, and `forge project audit`
reports each under `crud_stubs` until the body lands; the RPC returns
`CodeUnimplemented` until then.

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

**The request's fields ARE the filter surface.** To filter or scope a list by
anything else — an enum `status`, a foreign key, an owner `<owner>_id` — add
that field to the List request naming the real column, and forge wires it end
to end:

```proto
// added by hand to the scaffolded ListPrescriptionsRequest:
optional string status = 3;      // enum facet — exact-match on the status column
optional string patient_id = 4;  // FK / owner scope — exact-match on patient_id
```

Never fetch a page and filter client-side — that truncates past the page cap.
`forge scaffold entity` writes the `page_size`/`page_token`/`search`/`bool`
facets/`order_by`/`descending` set; enum/FK/owner facets you add by hand.

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
- Proto fields stay `snake_case` (`created_at`, `org_id`); messages / RPCs / services stay `PascalCase`. Full casing table: **Naming conventions** in `architecture`.

## RPCs owned by another service/repo — import, don't re-scaffold

For an RPC owned by *another* service or repo you are a client, not its
implementer: the remote proto is the source of truth — **import it (pinned
`task protos` copy or a buf BSR dep) and generate only a CLIENT.** Never
hand-copy it into `proto/services/` and scaffold a server: every method
`CodeUnimplemented` and a `Deps` holding only `Logger`/`Config` is the tell.

## Common Mistakes

1. **Missing forge import** — Every proto using `(forge.v1.method)` / `(forge.v1.service)` needs `import "forge/v1/forge.proto";`.
2. **Enum without UNSPECIFIED=0** — Proto3 requires the zero value. Name it `<ENUM>_UNSPECIFIED`.
3. **Enum values without prefix** — Use `TASK_STATUS_ACTIVE`, not `ACTIVE`. Proto enums share a namespace.
4. **Non-optional filter fields** — List request filter fields must be `optional`.
5. **Reusing field numbers** — Mark removed fields as `reserved`, never reuse.
6. **Multiple services per file** — Lint-rejected (`forgeconv-one-service-per-file`). Use `proto-split`.
7. **Cross-service proto imports** — Hoist shared messages into `proto/shared/v1/types.proto`.
8. **Declaring schema in proto** — There is no entity *annotation*; the `// forge:entity` marker is a birth instruction, not a schema declaration. Columns come from `db/migrations/`; if a CRUD RPC has no matching table, nothing is generated. Mark the message and run `forge scaffold` (or write the migration) first.
9. **`min_len == max_len` on a fixed-length code** — `buf lint` rejects it (exit 100). A fixed length takes `string.len = N` (ISO currency 3, country 2, NPI 10), which still projects to `CHECK (char_length(col) = N)` at birth; a single fixed literal takes `string.const = "X"`, which is wire-only. Reserve `min_len`/`max_len` for a genuine range.

## Rules

- **Draft, then `forge generate`** — the fast, exact oracle; read its error and fix. Never reverse-engineer forge's source for syntax.
- Proto declares the wire; `db/migrations/` declares the schema. Entity = CRUD RPCs + matching table.
- RPCs owned by another repo: **import the upstream proto and generate a client** — never hand-copy it and scaffold a server you won't implement.
- Run `forge generate && forge lint` after every proto edit; fix issues in proto, not in `gen/` — generated code is overwritten.

## When this skill is not enough

- Splitting a multi-service file — `proto-split`.
- Designing the Go service surface behind the proto — `service-layer`.
- Handler implementation patterns — `api`.
- DB schema lifecycle, and the schema as the entity truth — `db`.
