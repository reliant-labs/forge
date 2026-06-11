---
name: forge-libraries
description: One-page index of every forge/pkg/* subpackage. Read this BEFORE porting a utility from an existing codebase — the equivalent may already exist.
---

# Forge Libraries (`github.com/reliant-labs/forge/pkg/*`)

Forge ships a set of public Go libraries under `github.com/reliant-labs/forge/pkg/`. They're independent of any particular forge project — you can `go get` them into any Go module that wants a Connect-RPC stack with the same conventions.

**Read this skill before porting a utility from another codebase.** The migration of `control-plane` to forge surfaced four would-be re-implementations that already existed here, plus several that *should* have been adopted but weren't because the porter didn't know what was available.

The list is short. Skim it once; remember it forever.

| Package | Purpose | When to adopt |
|---------|---------|---------------|
| `pkg/auth` | Authentication primitives: JWT validators, dev-mode bypass, context-claims plumbing. Used by the `jwt-auth` pack. | Need to validate a bearer token / Supabase HMAC / static RSA key on the server side. Don't write your own JWT validator. |
| `pkg/authz` | Authorization library: thin shim that powers per-service `authorizer_gen.go`. `Can(ctx, claims, action, resource)` + role/membership checks. | You're enforcing "user X can perform action Y on resource Z." Don't write a parallel authz package. |
| `pkg/config` | Project config primitives: env loading, default merging, structured-config support. Used by every `pkg/config/config.go` forge generates. | You need typed env-var loading with defaults. The forge-generated `pkg/config/config.go` already uses this — extend it via your proto/config schema, don't reach into `pkg/config` directly. |
| `pkg/contractkit` | Test-mock support library: `Recorder` for call counts, `MockNotSet(...)` standard error string, `RecordedCall` helpers. Every forge-generated `mock_gen.go` embeds `contractkit.Recorder`. | You're writing a hand-rolled mock for an interface forge didn't generate one for. Use `contractkit.Recorder` so call recording is uniform. |
| `pkg/controller` | Kubernetes controller-runtime utilities: generic `Reconciler[T]` base (fetch/NotFound/finalizer lifecycle), `Result.Done()/Requeue(d)/Stop()`, predicates (`SkipDeletion`, `HasAnnotation`, `HasLabel`), `Backoff` exponential, `ClusterClientManager` for multi-cluster, `controllertest.New(t)` envtest harness. | Building a Kubernetes operator. Use `forge add operator` + `forge add crd` to scaffold; both pull in this library. |
| `pkg/crud` | RPC-level CRUD lifecycle helpers (`HandleCreate`, `HandleGet`, `HandleList`, `HandleUpdate`, `HandleDelete`) consuming the per-RPC op constructors generated in `handlers_crud_ops_gen.go`; the user-owned `handlers_crud.go` delegates to them. Validates `order_by` against the entity's `<Entity>Columns` allowlist (undeclared column → `InvalidArgument`); maps `orm.ErrNoRows` through `pkg/svcerr` to a clean `NotFound` and every other repo error to `Internal` with safe text — no SQL on the wire. | You implemented Service-layer CRUD methods and the handler just maps proto↔domain. Forge auto-wires this; don't bypass it. |
| `pkg/dialects` | Database-dialect glue: Postgres / SQLite / etc. abstractions consumed by `pkg/orm`. | You're customizing the ORM behavior for a specific DB. Otherwise — leave it alone, the ORM uses it transparently. |
| `pkg/middleware` | Connect interceptor and HTTP middleware primitives. Provides `Claims`, `ContextWithClaims`, recovery, logging, and audit middleware that forge regenerates `pkg/middleware/` from. | Writing custom middleware that fits the forge claims/auth model. Don't define a parallel `type Claims` in your project. |
| `pkg/observe` | Cross-cutting observability. Connect interceptors: `LoggingInterceptor`, `TracingInterceptor`, `MetricsInterceptor`, `RecoveryInterceptor`, `RequestIDInterceptor`, plus `DefaultMiddlewares(deps)` for the canonical chain. Per-method helpers: `TraceCall`, `LogCall`, `NewCallMetrics`. | Wiring tracing/logging/metrics for your services. The forge scaffold already wires `DefaultMiddlewares` for you; reach in here only for opt-in per-method instrumentation. |
| `pkg/orm` | Forge's typed ORM: codegen-driven entity types + query builder, consumed by the generated `internal/db/<entity>_orm.go`. Each entity exports `<Entity>Columns` (the declared-column allowlist used for `order_by` validation); `orm.WhereILikeAny` powers multi-column `search`/`query`/`q` filters; `orm.NullTime` is the tolerant nullable-timestamp scanner; missing rows surface as `errors.Is(err, orm.ErrNoRows)`. Used when `forge.yaml: features.orm: true` and protos have `forge:entity` annotations. | Building a CRUD service backed by Postgres/SQLite with proto-driven entities. Don't hand-write `*sql.DB` scan boilerplate if forge can generate it. (Exception: if your source uses a hand-rolled DAO, see the `migration-service` skill's "wide DAO" section.) |
| `pkg/svcerr` | Canonical service-error → Connect-error mapping. 19 sentinels (NotFound, PermissionDenied, ResourceExhausted, PlanLimit, InsufficientBalance, Expired, …) + matching constructors + `Wrap(err)` for handlers. Read the package doc — this is the single biggest "I almost ported it before realizing it already existed" trap from the migration. | You're returning errors from service-layer code that handlers will wrap into `connect.Error`. Always. There's no reason to define a parallel sentinel set in your project. |
| `pkg/tdd` | Test-driven-development helpers: `RunRPCCases` for table-driven Connect handler tests, contract-mock test patterns. | Writing handler unit tests by hand. (CRUD lifecycle coverage comes from the scaffold-once, user-owned `handlers_crud_test.go`, which asserts real executed semantics against your migrations on an in-memory DB.) |
| `pkg/tenant` | Multi-tenant context plumbing: `WithTenant(ctx, id)`, `TenantFromContext(ctx)`. Used by forge's tenant middleware. | Your service is multi-tenant and needs the tenant ID at the contract layer. |
| `pkg/testkit` | Common test-harness pieces: discard logger, in-memory SQLite, httptest harness, permissive authorizer, `WithTestTenant`. Used by forge-generated `pkg/app/testing.go`. | Writing an integration test that needs an in-process forge app. Don't roll your own bootstrap-testing — extend testkit. |

## Decision rule: adopt-or-port

When porting a utility from an existing codebase to forge:

1. **Search this list first.** If a forge package covers the surface, adopt it. The migration of control-plane to forge skipped two ports outright (svcerr, tracing) because forge equivalents existed and were strict supersets.
2. **If forge covers ~80%**, adopt the forge package and add a thin project-local extension for the missing 20%. Don't fork forge into your project tree.
3. **If forge doesn't cover it**, write your project-local package (under `internal/<name>/` or `pkg/<name>/` per the architecture skill). Don't bend forge's package to fit a domain it wasn't designed for.

## What's NOT here

- **HTTP / Connect transport plumbing** — `connectrpc.com/connect` itself, not forge. Forge doesn't wrap Connect; it embraces it.
- **Database drivers** — `jackc/pgx`, `mattn/go-sqlite3`, etc. Forge doesn't ship its own driver; `pkg/orm` and `pkg/dialects` build on top.
- **OTel SDK init** — that lives in each project's `cmd/otel.go` (not in `pkg/observe`). `observe` is the *interceptor* layer; the SDK bootstrap is per-project so you can customize sampling, resource attrs, etc.
- **Stripe / Twilio / NATS clients** — those ship as packs (`forge pack install`) or starters (`forge starter add`), not as `pkg/*` libraries.

## When this skill is not enough

- Implementation details of any individual package — read the package's own godoc.
- The forge codegen pipeline (what generates `pkg/middleware/*` etc.) — see the `architecture` skill.
- When to write a custom adapter vs. extend a forge package — see the `adapter` skill.
