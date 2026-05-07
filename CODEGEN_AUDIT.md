# Codegen audit — library migration candidates (30-04-2026)

**Status update (30-04-2026):** `forge/pkg/authz` has shipped —
`handlers/<svc>/authorizer_gen.go` now emits a thin shim
(~35 lines fixed + 1 row per RPC, was ~110 fixed) that delegates the
matching logic to `forge/pkg/authz`. The library is interface-driven:
the user implements `Decider.Decide(ctx, method, claims) error` and the
generated shim wires up `authz.RolesDecider` populated from
`methodRoles` + `methodAuthRequired`. Public API preserved exactly —
`NewGeneratedAuthorizer()` returns `*GeneratedAuthorizer` (now a type
alias for `*authz.Authorizer`) with `Can` and `CanAccess` methods.
Smoke project: 109 → 65 lines for a 5-RPC service, and lifecycle
changes (panic recovery, claims-context wiring, connect.Error
normalisation) now happen in one library upgrade. See
`migration/v0.x-to-authz-lib` for the upgrade story.

**Status update (30-04-2026):** `forge/pkg/crud` has shipped —
`handlers/<svc>/handlers_crud_gen.go` now emits per-RPC shims that
delegate to one of `crud.HandleCreate` / `HandleGet` / `HandleList` /
`HandleUpdate` / `HandleDelete`. The library carries the lifecycle
(auth + tenant hooks, error envelope, cursor encoding, list pagination
trim, order-by validation); the shim carries the unavoidable per-entity
wiring (proto→entity field copy, repository call site, response packing).
Smoke project: 203 → 189 lines (modest absolute shrink; the substantive
move is that future lifecycle changes happen in one library upgrade
rather than per-template re-renders). See the "shipped (hybrid)" marker
in the table below.

**Status update (30-04-2026):** the `handlers_crud_gen_test.go`
template now emits a `tdd.RunRPCCases` shim with one `tdd.RPCCase`
row per RPC instead of inlining `_, err := svc.<Method>(...); _ = err`
per RPC. Per-RPC line count drops from ~15 to ~9, but the substantive
move is that adding a new failure-mode test is a one-line `tdd.RPCCase`
row in the slice instead of a new `Test<X>_<Mode>` function. The
runner (`tdd.RunRPCCases` / `tdd.TableRPC`) and case type
(`tdd.RPCCase` / `tdd.Case`) live in `forge/pkg/tdd`. See the
"shipped" marker in the table below and `migration/v0.x-to-tdd-rpccases`
for the upgrade story.

**Status update (30-04-2026):** `forge/pkg/contractkit` has shipped —
the mock, middleware, tracing, and metrics templates now emit thin
per-method shims that delegate to the library. Behavioural fingerprints
(mock "...Func not set" error string, slog attribute keys, span name
shape, metric names) are preserved exactly. Forge's own `internal/`
packages were regenerated and the test suite is green. See the four
"shipped" markers below.

**Status update (30-04-2026):** `forge/pkg/auth` and `forge/pkg/tenant`
have shipped — `pkg/middleware/auth_gen.go` is now a ~40-line shim
(was 211 lines) and `pkg/middleware/tenant_gen.go` is a ~38-line shim
(was 106 lines). `pkg/middleware/claims.go` becomes
`type Claims = auth.Claims` (alias) so existing project code keeps
compiling. Behavioural fingerprints preserved: `JWT_SECRET` env-var
fallback, `/Health/` substring skip, skip-list honouring,
deny-by-default tenant claim. `tenant_gen.go` is now generated
unconditionally so `pkg/app/testing.go`'s `ContextWithTenantID` reference
resolves even with multi-tenant disabled. See the two "shipped" markers
in the table below.

**Status update (30-04-2026):** `forge/pkg/testkit` has shipped —
`pkg/app/bootstrap_testing.go.tmpl` no longer inlines `testAuthorizer`,
`newTestDB`, or the `slog.New(...io.Discard)` literal. The generated
file imports `github.com/reliant-labs/forge/pkg/testkit` and
`defaultTestConfig` calls `testkit.DiscardLogger()`,
`testkit.PermissiveAuthorizer{}`, and `testkit.NewSQLiteMemDB(t)`.
`NewTest<Svc>Server` now calls `testkit.NewTestServer(t, register)`.
For multi-tenant projects, `app.WithTestTenant` is a thin re-export of
`testkit.WithTestTenant`. The wiring shim
(`NewTest<Service>` constructing per-service `Deps`, the option
helpers) stays codegen — every project's Deps shape and proto Connect
client constructor is project-specific. Net per-project shrink:
~30% in the boilerplate region of `pkg/app/testing.go`.
See `migration/v0.x-to-testkit` for the upgrade story.

**Status update (30-04-2026):** the per-internal-package
`middleware_gen.go`, `tracing_gen.go`, and `metrics_gen.go` files are
**no longer generated**. Observability moves into
`forge/pkg/observe`: Connect interceptors at the handler boundary
(`LoggingInterceptor`, `TracingInterceptor`, `MetricsInterceptor`,
`RecoveryInterceptor`, `RequestIDInterceptor`) plus opt-in helpers
(`LogCall`, `TraceCall`, `NewCallMetrics`) for inner-call
instrumentation. The canonical chain is one call:
`observe.DefaultMiddlewares(deps)`. Mock stays codegen — the
per-method `Mock<Iface>` is a real grep target. The contract
generator sweeps the three legacy wrappers from any package it
regenerates a mock in, so existing projects upgrade by running
`forge generate` once. See `migration/v0.x-to-observe-libs` for the
upgrade story.

**Scope:** Every distinct `*_gen.go` file shape that a freshly-scaffolded forge
project carries (plus `bootstrap.go`, which is generated but not `_gen` suffixed).
Read-only audit — no source changes.

**Sample project measurements** were taken with a 5-method `internal/emailer`
contract (mix of `error`, `(T, error)`, `(bool, error)` returns) and a single
service. See `/tmp/tmp.dEpInfu73u/biggerscaff` (transient).

**See also:** the parallel agent is implementing `forge/pkg/tdd`. Anything that
defers to a "test scaffold" recommendation below assumes that work — `pkg/tdd`
already exists at `/home/sean/src/reliant-labs/forge/pkg/tdd/{contract,mock,rpc,db,e2e}.go`.

## Summary table

| File shape | Customization | Lines per input | Generic-feasible? | Upgrade pain | Recommendation |
|---|---|---|---|---|---|
| `internal/<pkg>/mock_gen.go` | never | ~7/method + 5 fixed | partial (per-method shim) | 4 | **shipped → forge/pkg/contractkit** (mock stays codegen for greppability) |
| `internal/<pkg>/middleware_gen.go` | never | ~7/method + 10 fixed | partial (per-method shim) | 4 | **shipped (deleted from codegen, lib-replaced)** → forge/pkg/observe (Connect interceptors + opt-in helpers) (30-04-2026) |
| `internal/<pkg>/tracing_gen.go` | never | ~13/method + 13 fixed | partial (per-method shim) | 4 | **shipped (deleted from codegen, lib-replaced)** → forge/pkg/observe (30-04-2026) |
| `internal/<pkg>/metrics_gen.go` | never | ~13/method + 19 fixed | partial (per-method shim) | 4 | **shipped (deleted from codegen, lib-replaced)** → forge/pkg/observe (30-04-2026) |
| `pkg/middleware/auth_gen.go` | never (config-driven) | ~210 fixed (provider-branched) | yes (full library + thin glue) | 5 | **shipped → forge/pkg/auth** (30-04-2026; ~40-line shim) |
| `pkg/middleware/tenant_gen.go` | never (config-driven) | ~106 fixed | yes (full library) | 3 | **shipped → forge/pkg/tenant** (30-04-2026; ~38-line shim) |
| `handlers/<svc>/authorizer_gen.go` | never | ~95 fixed + per-method map entry | partial (data + library) | 4 | **shipped → forge/pkg/authz** (30-04-2026; interface-driven shim, ~35-line fixed + 1 row/RPC) |
| `handlers/<svc>/handlers_gen.go` (Unimplemented stubs) | rare (deleted as you implement) | ~6/RPC | no (per-method signature) | 2 | **stay generated** |
| `handlers/<svc>/handlers_crud_gen.go` | sometimes (TODO comments invite edits, but file is regenerated) | ~50/RPC | partial (delegates already to `pkg/orm`) | 5 | **shipped (hybrid: thin shim + crud lib)** → forge/pkg/crud (30-04-2026) |
| `handlers/<svc>/handlers_crud_test_gen.go` | rare | ~15/RPC → ~9/RPC (one `tdd.RPCCase` row + per-test boilerplate) | yes (rpc table cases via `pkg/tdd`) | 3 | **shipped → emits `tdd.RunRPCCases` shim with `tdd.RPCCase` rows** (30-04-2026) |
| `handlers/<svc>/webhook_routes_gen.go` | never | ~1/webhook + 5 fixed | yes (registration helper) | 1 | **stay generated** (~~currently `//go:build ignore` — already inert~~ — RESOLVED: `stripBuildIgnore` in the renderer drops the directive at write time) |
| `pkg/app/bootstrap.go` | never (use `setup.go`) | ~12/svc + ~6/pkg + ~6/worker + boilerplate | no (per-type wiring) | 5 | **stay generated** — explained below |
| `pkg/app/bootstrap_testing.go` | never | ~10/svc + boilerplate | partial (helpers, not WireUp) | 4 | **shipped (hybrid)** → forge/pkg/testkit (30-04-2026) |

(Templates that produce `*_gen.go` flowing into the user project are the
canonical 13 above. Forge's *own* internal `*_gen.go` files at
`internal/codegen/`, `internal/templates/`, `internal/database/` etc. are
themselves outputs of contract gen and benefit transitively from the contract
hybrid recommendation.)

## Per-file deep dives

### `internal/<pkg>/mock_gen.go`

- **What it does:** For every interface in `contract.go`, generates a
  `Mock<Iface>` struct with one `XxxFunc` field per method, plus a method that
  invokes the func or returns `fmt.Errorf("MockService.XxxFunc not set")` /
  zero-value when nil. Compile-time `var _ Iface = (*MockIface)(nil)`.
- **What's mechanical:** All of it. Source: `internal/generator/contract/templates.go::mockTmpl`
  + helpers in `internal/generator/contract/contract_methods.go`.
- **What library could absorb:**
  - `mock.Recorder` for call counting, last-args capture, ordered matchers.
  - `mock.Default[T]()` zero-value helper (already needed by template, today
    it generates expression strings like `nil, fmt.Errorf(...)`).
  - `mock.Call0/Call1/Call2/Call3` shim helpers: `func Call1[A, R0 any](rec *Recorder, name string, fn func(A) (R0, error), a A) (R0, error)`.
- **Generated shim shape (after migration), per-method:**
  ```go
  func (m *MockService) Send(ctx context.Context, to, subject, body string) error {
      return mock.Run0[func(context.Context,string,string,string) error](
          &m.rec, "Send", m.SendFunc, ctx, to, subject, body)
  }
  ```
  Per-method line count drops from ~7 to ~3 and the per-method body becomes a
  pure delegation. The `XxxFunc` field declarations and the type/`var _` lines
  stay in the gen file (they encode the interface signature, which is the only
  thing forge can know that the library cannot).
- **Migration impact:** Sample project's 54-line `mock_gen.go` for 5 methods
  shrinks to ~38 lines. More importantly, error-message style and
  zero-value-by-type semantics move into the library and stop being
  per-template fixups.
- **Risk:** Existing tests (in dogfood + projects) read the
  `MockService.<X>Func not set` error string. A library helper must preserve
  that exact format unless we accept a behavioural change.
- **Recommendation:** **Shipped → `forge/pkg/contractkit`.** Library
  exposes `Recorder` (embedded in every mock), `MockNotSet(mockName,
  method)` for the canonical not-set error, and the `Calls` /
  `CallCount` / `Reset` API. The "MockService.<X>Func not set" error
  string is preserved exactly and locked by
  `TestMockNotSet_FingerprintLocked`. Per-method line count is roughly
  the same as before (3 lines fixed body) but the mock now records
  every call — so net behaviour is added at no per-file cost.

### `internal/<pkg>/middleware_gen.go`

- **What it does:** For each interface, generates `Instrumented<Iface>` that
  logs duration + error per method. Wraps `slog.Logger`.
- **What's mechanical:** All of it. Per-method body is `start := time.Now(); … := mw.inner.X(); mw.logger.Info(...); return …`.
- **What library could absorb:**
  - `middleware.LogCall(logger, "<Method>", start, err)` helper.
  - `middleware.WrapDuration[T](logger, start, attrs...)` deferred helper.
- **Generated shim shape:**
  ```go
  func (mw *InstrumentedService) Send(ctx context.Context, to, subject, body string) error {
      start := time.Now()
      err := mw.inner.Send(ctx, to, subject, body)
      contractkit.LogCall(mw.logger, "Send", start, err)
      return err
  }
  ```
  Per-method line count drops from ~7 to ~5.
- **Migration impact:** Modest; the win is consistency (every package's
  middleware/tracing/metrics share the same observability vocabulary, change in
  one library upgrade) and that the library can grow attributes without
  re-rendering every package.
- **Risk:** None significant — log format is simple and a library helper
  preserves it.
- **Recommendation:** **Shipped → `forge/pkg/contractkit`.** Library
  exposes `LogCallErr(logger, method, start, err)` and
  `LogCall(logger, method, start)` for void methods. The `slog.Info`
  record format is identical to the previous generated line (msg=method,
  attrs duration + error) — locked by `TestLogCallErr_FingerprintLocked`.

### `internal/<pkg>/tracing_gen.go`

- **What it does:** Per-interface `Traced<Iface>` that wraps each method in an
  OTel span, propagating an existing `context.Context` arg if present, falling
  back to `context.Background()` otherwise. Records errors as span events.
- **What's mechanical:** All of it. Source: `templates.go::tracingTmpl`.
- **What library could absorb:**
  - `tracing.Start(ctx, name) (context.Context, trace.Span)` — already exists
    in OTel, but a forge-ish helper that flags errors and captures duration
    saves three lines per method.
  - `tracing.RecordCall(span, err)` deferred helper.
- **Generated shim shape:**
  ```go
  func (tw *TracedService) Send(ctx context.Context, to, subject, body string) error {
      ctx, span := tw.tracer.Start(ctx, "Service.Send")
      defer span.End()
      err := tw.inner.Send(ctx, to, subject, body)
      contractkit.RecordSpanError(span, err)
      return err
  }
  ```
  Per-method line count drops from ~13 to ~5.
- **Migration impact:** Substantial — `tracing_gen.go` is the second-largest
  per-method file (84 lines for 5 methods). Halving it cuts gen output and
  removes the `_ = ctx` dance that the current template does for context-less
  methods.
- **Risk:** `context.Background()` fallback for context-less methods is a
  minor concession to the current template design — those methods don't
  propagate a parent span. Library can keep that semantic.
- **Recommendation:** **Shipped → `forge/pkg/contractkit`.** Library
  exposes `TraceStart(ctx, tracer, name)` (substitutes
  context.Background() when ctx is nil) and `RecordSpanError(span,
  err)` (no-op on nil err). Span name uses the
  `<Iface>.<Method>` form preserved from the previous template.

### `internal/<pkg>/metrics_gen.go`

- **What it does:** Per-interface `Metric<Iface>` that records call count,
  error count, and duration histogram per method. Each method gets identical
  boilerplate with three OTel calls and an attribute set.
- **What's mechanical:** All of it. Source: `templates.go::metricsTmpl`.
- **What library could absorb:**
  - `metrics.Recorder` struct that holds the three instruments and exposes
    `Recorder.Record(ctx, method string, start time.Time, err error)`.
  - Constructor helper `metrics.New(meter, packageName)` returns the recorder
    and creates the three instruments once.
- **Generated shim shape:**
  ```go
  func (mtw *MetricService) Send(ctx context.Context, to, subject, body string) error {
      start := time.Now()
      err := mtw.inner.Send(ctx, to, subject, body)
      mtw.rec.Record(ctx, "Send", start, err)
      return err
  }
  ```
  Per-method line count drops from ~13 to ~4.
- **Migration impact:** Same as tracing — halves a per-method file.
- **Risk:** Naming convention (`<package>.calls/errors/duration`) becomes a
  library default. Non-breaking if preserved; future-flexible if we add a
  config struct.
- **Recommendation:** **Shipped → `forge/pkg/contractkit`.** Library
  exposes `Metrics` struct + `NewMetrics(meter, packageName)` which
  creates the three instruments using the canonical
  `<package>.{calls,errors,duration}` names. Per-method recording is
  three short calls: `RecordCall(ctx, method)`,
  `RecordDuration(ctx, method, start)`, `RecordError(ctx, method, err)`
  — `nil` ctx is allowed and substitutes `context.Background()`
  internally.

### `pkg/middleware/auth_gen.go`

- **What it does:** Generates a Connect interceptor that authenticates either
  by JWT, API key, or both. The body is a giant tree of `{{- if eq .Provider
  "jwt" -}}` switches with provider-specific helpers (JWT parsing, JWKS,
  keyFunc, claim extraction).
- **What's mechanical:** Almost all of it. The per-project inputs are
  effectively the auth config struct (provider, JWT signing method, JWKS URL,
  issuer, audience, API-key header) and the skip-method list.
- **What library could absorb:** Almost everything. The provider-specific
  paths (validateJWT, getStringClaim, authenticateAPIKey, etc.) are pure
  library code. The skip-method list and provider selection become a config
  struct passed to a library constructor.
- **Generated shim shape (after migration):**
  ```go
  // pkg/middleware/auth_gen.go (~30 lines; data only)
  package middleware
  import "github.com/reliant-labs/forge/pkg/auth"
  var generatedAuthConfig = auth.Config{
      Provider:      "{{.Provider}}",
      SigningMethod: "{{.JWT.EffectiveSigningMethod}}",
      Issuer:        "{{.JWT.Issuer}}",
      Audience:      "{{.JWT.Audience}}",
      JWKSURL:       "{{.JWT.JWKSURL}}",
      APIKeyHeader:  "{{.APIKey.EffectiveAPIKeyHeader}}",
      SkipMethods:   []string{ {{- range .SkipMethods}}"{{.}}",{{- end}} },
  }
  func GeneratedAuthInterceptor(kv KeyValidator) connect.UnaryInterceptorFunc {
      return auth.NewInterceptor(generatedAuthConfig, kv, ContextWithClaims)
  }
  ```
- **Library shape (`forge/pkg/auth`):** `Config`, `NewInterceptor`,
  `validateJWT`, `authenticateAPIKey`, `getStringClaim`, `getStringSliceClaim`,
  `KeyValidator` interface stub.
- **Migration impact:** 211-line template → ~30-line generated shim + ~250-line
  library. Crucially, fixing JWKS bugs / adding new signing methods is a
  library change, not a regen for every project.
- **Risk:**
  - The `Claims` type currently lives in `pkg/middleware/claims.go`
    (project-local). Library auth needs a `Claims` shape — either depend on
    `pkg/middleware.Claims` (creates cycle) or library defines its own and
    project re-exports/wraps. The cleanest split is: `pkg/auth.Claims`
    canonical, `pkg/middleware.Claims` becomes alias.
  - `os.Getenv("JWT_SECRET")` direct calls are present in the current template
    — library should accept the secret via config, not env, or expose an
    optional env-loading helper. Behavioural-change risk.
- **Recommendation:** **Migrate.** Highest single-file ROI. Complementary to
  the existing `jwt-auth` and `clerk` pack templates that overlap with this
  same logic.

### `pkg/middleware/tenant_gen.go`

- **What it does:** Provides `ContextWithTenantID`, `TenantIDFromContext`,
  `RequireTenantID`, `TenantInterceptor`, `extractTenantClaim`. Only the claim
  field name and column name are project-specific.
- **What's mechanical:** All of it.
- **What library could absorb:** Everything — make `TenantInterceptor` accept
  a config struct: `tenant.Config{ ClaimField, ColumnName, ExtractClaim func(*Claims, string) string }`.
- **Generated shim shape:**
  ```go
  // pkg/middleware/tenant_gen.go (~10 lines)
  package middleware
  import "github.com/reliant-labs/forge/pkg/tenant"
  func TenantInterceptor() connect.UnaryInterceptorFunc {
      return tenant.NewInterceptor(tenant.Config{
          ClaimField: "{{.ClaimField}}",
          ColumnName: "{{.ColumnName}}",
      })
  }
  ```
- **Migration impact:** 106-line template → ~10-line shim + ~80-line library.
- **Risk:** Same `Claims`-type circularity as auth; same fix.
- **Recommendation:** **Migrate.** Pair with auth.

### `handlers/<svc>/authorizer_gen.go`

- **What it does:** Builds two per-service maps (`methodRoles`,
  `methodAuthRequired`) from proto annotations and exposes
  `GeneratedAuthorizer.CanAccess(ctx, procedure)`. The Can/CanAccess body is
  identical for every project; only the maps differ.
- **What's mechanical:** Map population (must come from proto). The 50-line
  switch-style logic body is fixed.
- **What library could absorb:** The matching logic. The maps stay in the
  generated file because they're per-project data.
- **Generated shim shape:**
  ```go
  // handlers/<svc>/authorizer_gen.go (~25 lines: maps + thin constructor)
  package <svc>
  import "github.com/reliant-labs/forge/pkg/authz"
  var methodRoles = map[string][]string{ /* ... */ }
  var methodAuthRequired = map[string]bool{ /* ... */ }
  type GeneratedAuthorizer = authz.RolesAuthorizer
  func NewGeneratedAuthorizer() *authz.RolesAuthorizer {
      return authz.NewRolesAuthorizer(methodAuthRequired, methodRoles)
  }
  ```
- **Migration impact:** 105-line template → ~25-line shim + ~80-line library.
- **Risk:** Tests rely on `NewGeneratedAuthorizer()` returning a struct with
  specific behaviour for the empty-procedure and unknown-procedure cases — a
  test must be added in the library to lock those (the
  `TestAuthorizerDenyByDefault` test in `unit_test.go.tmpl` would still pass
  via the type alias).
- **Recommendation:** **Hybrid → `forge/pkg/authz`.** Medium ROI; chiefly
  consolidates the deny-by-default policy into one place where security
  reviews can land.

### `handlers/<svc>/handlers_gen.go` (Unimplemented stubs)

- **What it does:** For every RPC that doesn't yet have a real implementation
  in the package, emits a `(s *Service) Foo(...) (...)` stub returning
  `connect.NewError(CodeUnimplemented, ...)`. The next `forge generate`
  detects real implementations and drops the stub.
- **What's mechanical:** Stub generation. But each stub has the *real proto
  method's signature* — that's where forge can't be replaced.
- **Why generics don't help:** The whole point is to give the user a method on
  `*Service` matching the proto signature so the rest of the package compiles.
  A library can't synthesize methods on user types. (Embedding a generic
  `Unimplemented[Req, Resp]` would technically satisfy the interface, but it
  defeats the entire UX of "gen drops the stub when you implement it" because
  the embedded method would still satisfy the interface and the user's real
  method would silently shadow it without ever causing a stub-removal trigger.)
- **Recommendation:** **Stay generated.** Real reason: the *purpose* of the
  file is to provide one method-on-type per RPC, and Go has no way to express
  "supply a method on this type only when the user hasn't" outside of code
  generation. A library import would either (a) require the user to embed the
  generic and break the auto-removal UX, or (b) require a separate
  satisfies-interface check that adds complexity for no compression.

### `handlers/<svc>/handlers_crud_gen.go`

- **What it does:** For each CRUD RPC, emits a full handler with auth check,
  tenant check, ORM call, response packing. The biggest per-RPC file (~50-100
  lines per RPC depending on List with pagination/filters/orderBy).
- **What's mechanical:** Mostly mechanical, but every section has user-facing
  TODOs ("apply req.Msg.UpdateMask...", "TODO: PROTO_NAME requires custom
  mapping"). The file is regenerated, so user edits don't survive — TODOs
  point at the generated code as a starting point but the user's actual
  business logic goes in a sibling file (e.g. `service_create_custom.go`).
- **Why a generic doesn't replace it cleanly:**
  - The handler ties together connect.Request/Response, the entity type, and
    the per-field mapping (`Kind: scalar/timestamp/enum/wrapper`). A
    `crud.Handler[Req, Resp, Entity]` library helper *can* express the
    happy-path skeleton, but it can't express the per-field mapping table
    (`req.Msg.X → entity.Y`) without reflection.
  - Reflection would add a runtime cost on every request and lose the
    compile-time check that the proto field actually maps to the entity field.
- **What COULD migrate (library extensions):** The List handler's pagination
  / filter / order-by chain is already half-generic via `pkg/orm`. An
  additional `crud.List[Filter, Entity]` helper that reads the (already
  per-RPC) `opts` slice and runs the query could shave ~10 lines per List
  handler.
- **Recommendation:** **Shipped → `forge/pkg/crud` (hybrid).** The
  CRUD lifecycle (auth, tenant, error mapping, cursor encoding/decoding,
  page-size clamp, page-of-+1 trim, order-by validation,
  `connect.Code*` selection) moved into `pkg/crud`. The shim stays
  generated because it carries the unavoidable per-entity wiring: the
  `req.Field -> entity.Field` copy table, the `db.Create<Name>` /
  `db.List<Name>` call sites, and the response field name. Per-method
  the body becomes a single `crud.HandleX(crud.XOp[Req,Resp,Ent]{...})(ctx, req)`
  where the struct literal supplies closures for Auth/Tenant/Entity/
  Persist/Pack and (for List) PageToken/PageSize/OrderBy/Filters/Query/
  EntityID. **Locked behavioural fingerprints** (preserved verbatim):
  `"<op> <entity>: %w"` envelope at CodeInternal/CodeNotFound,
  `"invalid page token"` at CodeInvalidArgument,
  `"update <entity>: <field> is required"` at CodeInvalidArgument, and
  the default 50/clamp-100 page size. Behaviour is locked by tests in
  `pkg/crud/crud_test.go`.

### `handlers/<svc>/handlers_crud_test_gen.go`

- **What it does:** Per CRUD RPC, emits a `TestUnit_<Name>` test that
  constructs the service via the test factory, makes one call with mostly-zero
  inputs, swallows errors. The file is largely a placeholder for the user to
  flesh out.
- **What's mechanical:** All of it. The per-RPC body is 4-12 lines of canned
  test invocation with `_ = err`.
- **What library could absorb:** Reduce each per-RPC test to a single
  `tdd.RPCCase` row driven by `tdd.TableRPC(t, svc, cases)` (the
  `forge/pkg/tdd` library being implemented in flight). Forge generates the
  rows; the table-runner is library code.
- **Migration impact:** 103-line template → ~25-line "list of rows + one
  TableRPC call". Rate of change goes from "edit each test by hand" to "edit
  one slice".
- **Risk:** Lifecycle tests (per-row Setup/seed) need to be expressible via
  `RPCCase.Setup`. The current `pkg/tdd/rpc.go` should be checked for
  parity.
- **Recommendation:** **Shipped → `forge/pkg/tdd` (`RunRPCCases` shim)**
  (30-04-2026). The runner (`tdd.RunRPCCases` aliasing `tdd.TableRPC`)
  and case type (`tdd.RPCCase` aliasing `tdd.Case`) live in
  `forge/pkg/tdd`. Per-RPC the generated test body is now ~9 lines:
  `t.Parallel(); svc := app.NewTest<Pkg>(t); tdd.RunRPCCases(t,
  []tdd.RPCCase[pb.Req, pb.Resp]{ {…} }, svc.<Method>)`. The
  FORGE_SCAFFOLD marker still seeds one row per RPC so the file
  remains forge-owned until the user clears every marker (the
  `writeScaffoldFile` ownership boundary is unchanged). **Locked
  behavioural fingerprints** (preserved verbatim): `connect.CodeOf(err)`-
  based error matching, declared-order Setup execution, default
  `context.Background()` for nil `Case.Ctx`. Behaviour is locked by
  tests in `pkg/tdd/rpc_test.go` and the rendered-shape fingerprints
  by `internal/codegen/crud_gen_test.go::TestGenerateCRUDTests_BasicGeneration`.
  See `migration/v0.x-to-tdd-rpccases` for the upgrade story.

### `handlers/<svc>/webhook_routes_gen.go`

- **What it does:** Registers webhook HTTP routes on the mux. 13-line
  template, one line per webhook.
- **~~Currently inert:~~ RESOLVED:** The template still begins with
  `//go:build ignore` (so it is not compiled as part of forge itself), but
  `templates.stripBuildIgnore` runs inside `RenderFromFS` and drops the
  directive at render time. The written `webhook_routes_gen.go` files in
  user projects therefore start at the `// Code generated …` line and
  participate in compilation as expected.
- **Recommendation:** **Stay generated.** Real reason: trivial size, one line
  per webhook; library import would not save lines.

### `pkg/app/bootstrap.go`

- **What it does:** Wires every service, worker, operator, internal package
  with their constructors, dev-mode authorizer swap, fallible-vs-infallible
  branches, mux registration, BootstrapOnly variant. 480-line template — by
  far the largest single file.
- **What's mechanical:** Almost all of it.
- **Why generics don't help:** Each service's constructor takes a
  per-package-typed `<pkg>.Deps{}`. There is no shared constructor signature
  that a library could call generically without reflection. Even with
  reflection, the dev-mode authorizer swap is a per-service decision that
  benefits from being explicit.
- **What COULD partially migrate:**
  - `WorkerInstance`/`OperatorInstance` lifecycle wrappers (lines 60-100 of
    template) are pure plumbing — could move to `forge/pkg/app/lifecycle`.
  - The `RunOperators` controller-manager startup is generic — could move to
    `forge/pkg/app/operator.RunManager(ctx, controllers, opts)`.
- **Recommendation:** **Stay generated.** Real reason: the per-package
  constructor invocations are the file's whole purpose, and Go's lack of
  type-parameter-method-enumeration means there is no compression to be
  gained from a library here. Compromise: extract `WorkerInstance` and
  `RunOperators` helpers to `forge/pkg/lifecycle` to shrink the template by
  ~50 lines (low ROI; do last).

### `pkg/app/bootstrap_testing.go`

- **What it does:** `NewTest<Service>(t)` factories, `WithLogger`, `WithDB`,
  `WithAuthorizer`, `With<Service>Deps` options, plus an httptest harness. 261
  lines.
- **What's mechanical:** ~70% — the option pattern is identical for every
  project; the per-service factory is per-project.
- **What library could absorb:**
  - `testkit.WithLogger`, `testkit.WithDB`, `testkit.WithAuthorizer` options
    on a generic `testkit.Config[Deps]`.
  - The httptest server + mux registration in `New<Service>(...)` →
    `testkit.NewServer(t, registerFn)`.
  - The permissive `testAuthorizer` → already lives in `pkg/middleware` or
    moves to `pkg/authz` per the authorizer migration.
- **Generated shim shape:** Per service, ~15 lines instead of ~50.
- **Recommendation:** **Hybrid → `forge/pkg/testkit`** (or merge into
  `pkg/tdd`). Coordinate with the parallel agent.

## Migration sequencing (recommended order)

Order by ROI / risk and library dependencies:

1. **`forge/pkg/contractkit`** (mock + middleware + tracing + metrics).
   Highest aggregate ROI — covers four files per `internal/` package, and
   forge's *own* repo has 16+ such packages so the dogfood win is immediate.
   No upstream library deps.
2. **`forge/pkg/auth` + `forge/pkg/tenant`** (auth_gen.go + tenant_gen.go).
   Largest single-file wins. Must precede authorizer migration so they share
   `Claims`. Touches the `pkg/middleware/claims.go` ownership question — get
   that decision before starting.
3. **`forge/pkg/authz`** (authorizer_gen.go). Medium win, low risk; depends
   on `Claims` decision from step 2.
4. **`forge/pkg/tdd` integration → handlers_crud_test_gen.go.** Coordinate
   with the parallel agent. Defer template rewrite until `pkg/tdd` is settled.
5. **`forge/pkg/testkit`** (bootstrap_testing.go). Optional consolidation
   with `pkg/tdd`; lower ROI but clean.
6. **`forge/pkg/crud` extensions** (handlers_crud_gen.go shrink). Lowest ROI
   among "do something" items; do only if the previous wins reveal a clean
   `crud.Paginate` signature.

## Risks / open questions

1. **`Claims` ownership.** Today `pkg/middleware/claims.go` lives in the
   user's project. A library `auth`/`tenant` needs a `Claims` shape, and we
   need to decide whether (a) library defines `Claims` and the project
   middleware aliases, (b) project keeps `Claims` and library is generic over
   it via type parameter, (c) something else. Affects every auth-touching
   migration — needs user decision before step 2.
2. **Behavioural fingerprint preservation.** Several generated files have
   behaviour that callers might depend on:
   - Mock's `"MockService.<X>Func not set"` exact error string.
   - Authorizer's exact deny-on-empty-procedure path (`TestAuthorizerDenyByDefault`).
   - Auth interceptor's `os.Getenv("JWT_SECRET")` direct read.
   The migration needs a test pass that locks each fingerprint *before*
   refactoring, or an explicit user blessing to change them.
3. **Per-package mocks vs shared `pkg/contractkit/mock`.** The mock currently
   lives in the same package as the contract (so it can return zero values of
   package-private types). A library helper that generates the mock from the
   outside hits Go visibility rules. Hybrid (per-method shim stays in package)
   sidesteps this, but the shim count grows linearly with method count — at
   ~3 lines per method it's still less than today's ~7. Confirm the user
   prefers hybrid over a separate `mocks` subpackage.
4. ~~**Webhook gen `//go:build ignore`.**~~ **RESOLVED.** The renderer
   strips the directive at write time (`templates.stripBuildIgnore`
   inside `RenderFromFS`), so user projects' `webhook_routes_gen.go`
   files compile and the routes wire up. Verified via
   `TestRenderTemplate_StripsBuildIgnoreFromRenderedOutput` and an
   end-to-end `forge new` + `forge add webhook` + `forge generate`
   smoke run.

## Estimated effort

Rough hours per migration, assuming each includes: library code, library tests,
template edits, dogfood verification (`forge generate` on forge's own repo
followed by `go build ./... && go test ./...`).

| Migration | Library LOC | Template churn | Dogfood verify | Hours |
|---|---|---|---|---|
| `forge/pkg/contractkit` (4 files in one library) | ~400 | 4 templates | 16+ packages × 4 files | 12-16 |
| `forge/pkg/auth` | ~250 | 1 template (heavy) | 1 file | 8-10 |
| `forge/pkg/tenant` | ~80 | 1 template | 1 file | 3-4 |
| `forge/pkg/authz` | ~80 | 1 template | per-service | 4-6 |
| `pkg/tdd` integration (test_gen template rewrite) | ~50 (additive) | 1 template | per-service | 4-5 |
| `forge/pkg/testkit` | ~150 | 1 template | per-service | 6-8 |
| `forge/pkg/crud` extensions | ~80 | 1 template (light) | per-service | 4-5 |
| **Total if all done** | ~1090 | 9 templates | full forge dogfood | **41-54** |

The contractkit migration is the only one strictly required to validate the
overall library-import pattern; it pays back its own cost via forge's own
internal usage (`internal/codegen`, `internal/database`, `internal/config`,
`internal/templates`, `internal/docs`, `internal/doctor`, `internal/debug`,
`internal/packs`, `internal/generator/contract` all carry the four `*_gen.go`
files today).
