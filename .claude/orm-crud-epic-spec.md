# Forge epic: postgres-pinned, Bun-powered, library-maximized data layer

Branch: **feat/gateways**. **No backwards compat** (forge unreleased) — delete old shapes.
Binary: `cmd/forge` → `/tmp/forge-bin/forge`. Engine choice ratified by Sean: **Bun** (`uptrace/bun`),
not sqlc (sqlc is the SQL-first alternative; rejected because forge's custom reads are dynamic and
it'd own the model structs forge projects conventions onto).

**COORDINATION**: the component-model epic (`.claude/component-epic-spec.md`) is landing on the same
branch in parallel (components:/kind:/ports:, deploy-as-data→KCL). The two are orthogonal in subject
(data layer vs config/deploy) but both regenerate downstream. **Phase 5 (downstream fallout) of THIS
epic must run AFTER the component epic's downstream phase**, so cp-forge/kalshi each regenerate ONCE
onto the combined new forge, not twice into conflicting shapes.

Five phases, hard order (1→2→3→4 sequential; 5 after, coordinated):
- **1. Postgres-pin + kill SQLite** — foundation.
- **2. Bun engine** — replace the hand-rolled runtime ORM.
- **3. Library-maximize CRUD** — generics over Bun; codegen shrinks to per-type glue.
- **4. Tier-2 content** — wired custom-RPC scaffolds, not Unimplemented stubs.
- **5. Downstream** — cp-forge + kalshi onto Bun (coordinate with component epic).

Process rules (EVERY worktree agent): first `git reset --hard $(git rev-parse feat/gateways)` and
confirm HEAD; `cd` explicitly in every Bash; COMMIT EARLY per coherent step; test tiers =
`go test -short ./...` inner loop, package tests before commit, plain e2e gate
`go test -tags e2e -count=1 ./internal/cli/` once at end after committing. THE SAFETY NET for this
epic is the **executed CRUD lifecycle gate** (scaffold → real DB → boot → CRUD roundtrip) — it must
stay green at every phase, now against REAL postgres. Commits end
`Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

## 1. Postgres-pin + kill SQLite

Pin the database to postgres everywhere; delete SQLite (and the unused mysql option).
- Config: `database.driver` pinned to `postgres` (remove `sqlite`/`mysql`/sqlite-as-`none` paths;
  keep `none` = no database). `EffectiveDatabaseDriver` returns postgres. Remove the
  `sqlc_enabled` flag (Bun is the engine; SQL-first is not the path).
- `pkg/orm`: delete `SqliteDialect` and the entire dialect abstraction (Bun is pg-only for us).
- `internal/schemadef`: replace the in-memory modernc SQLite shadow (`:memory:`) with a REAL
  ephemeral postgres for generate-time introspection — **embedded-postgres** (a downloaded/cached
  real pg binary, no Docker; e.g. `fergusstrange/embedded-postgres`) as the default, falling back to
  a detected running postgres (the project's dev docker-compose) or testcontainers when Docker is
  present. Introspect via the existing `pkg/orm` `PostgresDialect.IntrospectColumnsQuery` /
  `IntrospectIndexesQuery`. DELETE `normalizeForShadow` and the pg→portable hacks — real pg needs
  none of it. This fixes the schema-qualified-DDL bug class (cp-forge fr-b4b775f629) at the root.
- `pkg/testkit`: in-memory SQLite test harness → embedded postgres (shared instance, per-test
  schema or txn isolation). Generated tests + the corpus must run against real pg.
- Gate (executed lifecycle) now boots against real pg — it already did for the boot step; make the
  introspection/test path pg too.

DONE = no `sqlite`/`modernc`/`:memory:` references remain (outside historical comments); generate +
the executed lifecycle gate green against embedded/real postgres; cp-forge's `controlplane.`-schema
migration introspects cleanly (the bug that froze its ORM is gone).

## 2. Bun engine

Replace `pkg/orm`'s hand-rolled runtime query/CRUD engine (~bulk of its 9,667 LOC:
`query.go`, `crud.go`, `cursor.go`, `filter.go`, `dialect.go`, `model.go`, `repository.go`,
`client.go`, `internal_query.go`) with **Bun**. KEEP the schema-truth machinery that codegen needs
(`introspect.go` — now real-pg, `differ.go`, `schema.go`, `migration.go`) and the thin type helpers
generated code references (`nulltime.go`, `array.go` StringArray/Int64Array/ArrayValue, `json.go`,
`errors.go` ErrNoRows/UnknownFieldError, the `TypeText/TypeJSONB/...` constants used at generate
time).

THE CONTRACT generated code + `pkg/crud` call today (preserve or cleanly migrate): `orm.Context`
(handle, 98×), `orm.QueryOption` + `WithWhere/WithOrderBy/WithLimit` + `WhereEq/WhereILike/
WhereIsNull/WhereILikeAny` + operators (`GreaterThan/Asc/Desc`), `NewQueryBuilder`,
`DecodeCursor/EncodeCursor`, `ValidateOrderBy`, `NewClientWithDB/Client`.

KEY DECISION (make it, document it): expose Bun directly vs keep a thin forge facade. Recommended:
`orm.Context` becomes/wraps `bun.IDB`; generated CRUD ops are reimplemented on Bun's query builder;
keep a SMALL set of forge helpers Bun doesn't provide (cursor encode/decode, ValidateOrderBy column
allowlist, the array/nulltime types, the typed errors). The RAW-SQL ESCAPE HATCH is Bun itself
(`bun.Raw`, `db.NewRaw`, `db.QueryContext`) — exposed to user-owned handlers. Generated model structs
gain Bun struct tags (`bun:"..."`), projected from introspection (conventions unchanged:
deleted_at→soft-delete, created_at/updated_at, tenant_id, text→search).

`pkg/crud` (the 1,234-LOC CRUD lifecycle library) reimplemented over Bun: List/Get/Create/Update
(incl. AIP-134 mask)/Delete, pagination (cursor), soft-delete, tenant-scoping — all over Bun's API.

DONE = executed lifecycle gate green (real CRUD roundtrip via Bun against real pg); masked update
still non-clobbering; the hand-rolled engine files deleted; `go test -race` green.

## 3. Library-maximize CRUD (packages over codegen — "both", pushed)

forge already does both (pkg/crud library + thin generated shims, H1). Push further with generics:
a generic `crud.Handler[Model, Proto]` parameterized by (Bun handle, ToProto/FromProto conversions,
entity metadata) so the GENERATED code per entity shrinks to: the Bun-tagged model struct (from
introspection), the two conversion funcs, and a registration line. Lifecycle/pagination/soft-delete/
tenant-scoping/query execution all live in `pkg/crud` + Bun. The irreducible per-type glue
(conversions, column metadata) stays codegen — type-safe, no reflection in the hot path.

DONE = generated per-entity handler/ops files measurably smaller (report the before/after LOC per
entity); lifecycle gate green; no behavior change.

## 4. Tier-2 content: wired custom-RPC scaffolds

Tier-2 scaffold-then-own ALREADY EXISTS and works (`handlers_crud.go` is scaffolded once, user owns
forever, forge only appends shims for NEW rpcs — "yours: scaffolded once, never touched again").
The gap is CONTENT: a custom (non-CRUD-shape) RPC scaffolds a `CodeUnimplemented` stub tagged
`forge:custom-read-shape` + reason. Change the BODY to a WIRED starting point: request fields mapped
onto a Bun query-builder skeleton (best-effort from the RPC's request shape) + the ToProto/FromProto
conversions already in place + a `// TODO: refine this query` marker — so the user edits working code,
not a blank. Keep the scaffold-once-then-own mechanism (O1 self-cert ownership makes "generated start
→ user-owned on edit" clean). Also: scaffold the standard AIP-132 List-with-filter/order/paginate as
a real Tier-1 handler where the RPC matches that shape (covers many "custom" lists, e.g. sign-predicate
filters), so fewer RPCs fall to the custom path at all.

DONE = a scaffolded custom-read RPC compiles and runs a real (if naive) query out of the box; a
filtered-List RPC gets a working generated filter/order/paginate handler; lifecycle gate green.

## 5. Downstream (after component epic's downstream; coordinate)

- **kalshi**: regenerate onto Bun. Its 6 hand-written raw-SQL RPCs are the acid test — re-express the
  ones that fit Bun's builder (joins/aggregates/latest-per-market via Bun); keep raw SQL (now
  `bun.Raw`) where genuinely custom. Verify the four schema-truth tables introspect via real pg
  (no more SQLite shadow). `task test` green.
- **cp-forge**: regenerate onto Bun; its `controlplane.`-schema ORM (frozen by the SQLite-shadow bug)
  now regenerates from real-pg introspection — verify the previously-frozen `internal/db` comes back
  to life. Build/test/audit/idempotent.
- Both: this is the proof the engine swap holds on real production-shaped projects.
