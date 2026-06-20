---
name: api
description: Write Connect RPC handlers — proto-driven codegen, the thin-translation handler pattern (validate, extract auth, convert proto↔internal, call service, wrap errors via `svcerr.Wrap`), middleware, and testing. Business logic lives in `internal/<svc>/contract.go`, never in handlers.
---

# Connect RPC API Handlers

A Connect RPC handler is **wire-format glue**, not business logic. Every handler in a forge project follows the same six-step shape:

1. Validate the request.
2. Extract auth / user context from the request.
3. Convert proto → internal input type.
4. Call the service (this is where the business logic lives).
5. Convert internal result → proto.
6. Wrap errors with `svcerr.Wrap`.

If a handler grows past those six steps, the extra logic belongs behind the service interface in `internal/<svc>/contract.go`. See `service-layer` for the other half.

## Where handlers live, and what codegen gives you for free

Handlers live in `internal/<svc>/` — the same directory as that service's `contract.go` and its impl. There is no separate top-level `handlers/` tree; the generated handler files (`handlers_gen.go`, `handlers_crud_ops_gen.go`) and the owned ones (`handlers_crud.go`, `validators.go`, `authorizer.go`) sit beside the service they belong to. Each service struct embeds the generated `Unimplemented*Handler` and implements RPC methods:

```go
type UserService struct {
    gen.UnimplementedUserServiceHandler
    deps Deps
}
```

RPCs are defined in `proto/services/<svc>/v1/<svc>.proto`. Naming conventions matter — they trigger auto-generated features:

- **CRUD methods** (`Create<Entity>`, `Get<Entity>`, `List<Entities>`, `Update<Entity>`, `Delete<Entity>`) whose entity has a matching table in the applied `db/migrations/` schema (pluralized snake_case — the CRUD RPCs are the wire half of entity detection, the table is the storage half) → forge generates per-RPC op constructors (request→entity field mapping via the generated `<entity>ToProto`/`<entity>FromProto` conversions, filter→column mapping, response packing, auth/tenant hooks) in `handlers_crud_ops_gen.go` (Tier-1, regenerated every run) and scaffolds thin ~3-line delegations into the user-owned `handlers_crud.go`: `return crud.HandleCreate(s.crudCreateItemOp())(ctx, req)`. The delegations never name entity fields, so schema changes flow through the regenerated ops file and `handlers_crud.go` never rots. To customize an RPC, replace the delegation right in `handlers_crud.go` — the file is yours; `forge generate` only appends shims for newly added CRUD RPCs and never modifies existing content. CRUD RPCs with no matching table generate honest Unimplemented stubs and nothing else — create the table first (`forge add entity` or a hand-written migration).
- **AIP-158 pagination fields** (`page_size`, `page_token`, `next_page_token`) → cursor-based pagination is auto-generated.
- **`optional` filter fields** on List requests → query filters are auto-generated (`search`/`query`/`q` → ILIKE across the table's text columns; any other filter must name a real column of the entity's table or `forge generate` fails loudly). Filter fields must be `optional` in proto, otherwise the generated code can't distinguish "not set" from zero values.
- **`auth_required` annotation** → the per-method policy table in `authorizer_gen.go` is auto-generated. Custom authorization logic (including role checks) goes in `authorizer.go` — `authorizer_gen.go` is regenerated.
- **`idempotency_key` annotation** → signals callers to pass an `Idempotency-Key` header.

**Hand-written handler methods always take priority** — the generator skips any method you've already implemented. After any proto change, run `forge generate`; never hand-edit `gen/` or `*_gen.go` files (fix the proto source instead).

Cross-cutting concerns (auth, logging, recovery, request IDs) live in the forge middleware libraries (`forge/pkg/{authn,authz,middleware,observe}`) plus the thin policy file `internal/middleware/middleware.go`, composed in the binary's `Build` and mounted by its cobra subcommand — not in handlers.

## The canonical handler

```go
import (
    "github.com/reliant-labs/forge/pkg/svcerr"
)

func (s *Service) DoThing(
    ctx context.Context,
    req *connect.Request[apiv1.DoThingRequest],
) (*connect.Response[apiv1.DoThingResponse], error) {
    // 1. Validate request
    if err := validateDoThingRequest(req.Msg); err != nil {
        return nil, connect.NewError(connect.CodeInvalidArgument, err)
    }

    // 2. Extract auth/user context
    claims, err := middleware.ClaimsFromContext(ctx)
    if err != nil {
        return nil, connect.NewError(connect.CodeUnauthenticated, err)
    }

    // 3. Convert proto → internal input
    input := things.DoThingInput{
        UserID: claims.UserID,
        Name:   req.Msg.Name,
    }

    // 4. Call service (business logic)
    result, err := s.deps.Things.DoThing(ctx, input)
    if err != nil {
        return nil, svcerr.Wrap(err)
    }

    // 5. Convert internal result → proto
    return connect.NewResponse(&apiv1.DoThingResponse{
        Id:        result.ID,
        CreatedAt: timestamppb.New(result.CreatedAt),
    }), nil
}
```

That's it. No `db.Begin`, no `slog.Info`, no business decisions. Each step is one or two lines; the whole handler is twenty.

## Error mapping — use `svcerr`, do NOT hand-roll a helper

Every handler uses `svcerr.Wrap(err)` from `github.com/reliant-labs/forge/pkg/svcerr`. The library owns the service-error → connect-error mapping; do **not** re-implement it per service.

```go
import "github.com/reliant-labs/forge/pkg/svcerr"

result, err := s.deps.Things.DoThing(ctx, input)
if err != nil {
    return nil, svcerr.Wrap(err)
}
```

`svcerr.Wrap` does the right thing in three cases:

- **Service returned a wrapped sentinel** (`return nil, svcerr.NotFound("user")`) — wrapped as the matching `*connect.Error` (here `CodeNotFound`).
- **Service returned an existing `*connect.Error`** — passed through unchanged.
- **Anything else** (raw DB error, third-party SDK error, …) — wrapped as `CodeInternal` so internals don't leak to clients. Log the original at the call site if you need it traced.

The package also exposes `svcerr.WithDetail(err, msg)` for the rare case of attaching a structured proto detail to the connect error (e.g., a structured validation-failure description). For the 99% case `svcerr.Wrap(err)` is the only call you make.

Picking the right code matters to clients: `NotFound` vs `InvalidArgument` vs `Internal` drive different client behavior. Never expose internal details (stack traces, SQL errors) to clients — the unknown-error fall-through lands at `CodeInternal` with a generic message.

### Why no per-service helper

Pre-1.7 forge prescribed a per-service `mapServiceError` / `toConnectError` helper. Across the cpnext dogfood pass that produced **four byte-identical copies** of the same switch statement in `internal/{billing,daemon,llm_gateway,org}/handlers.go` — each one mapping the same sentinels to the same Connect codes. The skill earned the duplication. The fix is to ship one mapping, in one library, and have every handler call into it.

`forge lint --conventions` ships a warning (`forgeconv-no-handler-error-mapping`) that flags any service-package file declaring a function whose name and body shape match the old per-service mapper pattern. If you see that warning, replace the helper with `svcerr.Wrap`.

## Domain sentinels live in the service layer

Define your domain failure categories in `internal/<svc>/` using `svcerr` sentinels, not bespoke ones. Two equivalent shapes:

**(a)** Return the package-level sentinel directly (use when there's no helpful detail to attach):

```go
// internal/things/contract.go
import "github.com/reliant-labs/forge/pkg/svcerr"

func (s *svc) GetThing(ctx context.Context, id string) (*Thing, error) {
    row, err := s.db.GetThing(ctx, id)
    if errors.Is(err, sql.ErrNoRows) {
        return nil, svcerr.ErrNotFound
    }
    if err != nil {
        return nil, fmt.Errorf("get thing: %w", err)
    }
    return row, nil
}
```

**(b)** Use the matching constructor when a human-readable detail belongs in the error string:

```go
return nil, svcerr.NotFound("thing")
return nil, svcerr.PermissionDenied("requires org owner")
return nil, svcerr.InvalidArgument("AI access is billed via wallet, not subscription")
return nil, svcerr.FailedPrecondition("organization has no billing account")
```

Both forms preserve the sentinel for `errors.Is` and `svcerr.Code` checks, so handler-side and test-side matching keeps working.

The full sentinel set covers every `connect.Code`:

| svcerr sentinel / constructor | connect.Code | When |
|------------------------------|--------------|------|
| `ErrNotFound` / `NotFound` | `CodeNotFound` | Resource doesn't exist or is filtered out by tenant scoping |
| `ErrAlreadyExists` / `AlreadyExists` | `CodeAlreadyExists` | Unique constraint, idempotency replay mismatch |
| `ErrPermissionDenied` / `PermissionDenied` | `CodePermissionDenied` | Caller is authenticated but not authorized |
| `ErrUnauthenticated` / `Unauthenticated` | `CodeUnauthenticated` | No valid identity |
| `ErrInvalidArgument` / `InvalidArgument` | `CodeInvalidArgument` | Domain invariants violated post-validation |
| `ErrFailedPrecondition` / `FailedPrecondition` | `CodeFailedPrecondition` | System not in required state (e.g. cannot remove last owner) |
| `ErrAborted` / `Aborted` | `CodeAborted` | Optimistic-concurrency / transactional conflict |
| `ErrResourceExhausted` / `ResourceExhausted` | `CodeResourceExhausted` | Rate-limit / quota / plan-limit reached |
| `ErrUnavailable` / `Unavailable` | `CodeUnavailable` | Upstream dependency offline |
| `ErrUnimplemented` / `Unimplemented` | `CodeUnimplemented` | Stubbed RPC or feature-flagged path |
| `context.Canceled` / `context.DeadlineExceeded` | passthrough | `svcerr.ToConnect` maps stdlib context errors |
| anything else | `CodeInternal` | The unknown-error catch-all; opaque to clients |

Add a new sentinel only when the existing set has no representative for the Connect code you need — sentinel sprawl undermines the whole "one mapping, one library" point.

## Attaching structured detail (rare)

When clients legitimately need machine-readable error context — e.g., a validation-failure proto naming the offending field — use `svcerr.WithDetail`:

```go
import (
    "github.com/reliant-labs/forge/pkg/svcerr"
    apiv1 "myproject/gen/things/v1"
)

return nil, svcerr.WithDetail(
    svcerr.InvalidArgument("name must be <= 256 chars"),
    &apiv1.FieldViolation{Field: "name", Reason: "max_length"},
)
```

`WithDetail` is built on `connect.NewErrorDetail`; the proto must be a registered message type. Most handlers never need this — `svcerr.Wrap(err)` is enough.

## Validation helpers

Wire-format validation (required fields, bounds, format) goes in a per-service `validators.go`:

```go
// internal/things/validators.go
func validateDoThingRequest(req *apiv1.DoThingRequest) error {
    if req.Name == "" {
        return errors.New("name is required")
    }
    if len(req.Name) > 256 {
        return errors.New("name must be <= 256 chars")
    }
    return nil
}
```

Validators are pure functions taking `*apiv1.<Method>Request` and returning `error`. They are easy to table-test (see `testing/patterns`). They never touch context, DB, or services.

**Domain-level invariants** ("can this user create a thing in this org?") belong in the service, not the validator. Rule of thumb: if checking the rule requires DB access or external state, it's a service concern.

## When proto and internal types diverge

For a CRUD MVP, proto and internal types often look identical and a tempting shortcut is to pass `req.Msg` straight into the service. **Don't.** At any non-trivial scale they diverge:

- Proto fields are `*string` / `*int32` / `string`; internal types use `string` / `int` / domain enums.
- Proto carries wire concerns (`page_token`, `mask`, oneof wrappers); internal types carry business concerns (`AuthorID`, computed defaults).
- Proto evolves on a wire-compat clock; internal types evolve on a refactor clock.

Convert at the handler boundary. The handler's job is exactly this translation. See `service-layer` for what the internal input/output types should look like.

## Auth context extraction

`middleware.ClaimsFromContext(ctx)` is the canonical extraction. Every authenticated handler does this in step 2 and passes the relevant fields (UserID, OrgID, roles) into the service input — never the raw `*Claims` struct. The service interface should not know your auth provider exists.

For unauthenticated RPCs (health checks, public reads), skip step 2.

## What does NOT belong in a handler

- **DB calls.** No `db.Begin`, no ORM functions, no SQL. Data access (ORM functions from `internal/db/`, transactions for multi-step mutations) lives behind the service — see `service-layer` and `db`.
- **Business decisions.** "Should this user be allowed to do X?" is a service concern; the handler only knows that auth exists.
- **A hand-rolled `mapServiceError` / `toConnectError` helper.** Use `svcerr.Wrap(err)`. The lint warns when you re-roll one.
- **Cross-service orchestration.** "Create a thing AND send an email AND update the audit log" belongs in one service method, not split across the handler.
- **Logging beyond the unknown-error path.** Structured logging happens in middleware (request log) and in the service's tracing wrapper. Handlers stay quiet.
- **Retries, transactions, idempotency.** All service concerns.

## Testing handlers

With this shape, handler unit tests are nearly mechanical:

- Construct a `MockService` from `internal/things/mock_gen.go`.
- Set up `mockSvc.On("DoThing", mock.Anything, expectedInput).Return(result, nil)`.
- Call the handler via the test helper from `internal/app/testing.go`.
- Assert the response or `connect.CodeOf(err)`.

For error-path tests, return a `svcerr.Err*` sentinel (or constructor) from the mock and assert `connect.CodeOf(err)` matches the expected code — the wrap is library-tested, you don't have to re-cover it per service.

See `testing/patterns` Pattern 1 for the table-driven template, and the `tdd.RunRPCCases` runner from `pkg/tdd` for the canonical per-RPC test shape. Integration tests use a real database to verify queries and transactions.

```bash
forge test
forge test --service users
```

## Rules

- Six steps. No business logic in the handler.
- Validators are pure, testable functions in `internal/<svc>/validators.go`.
- Error mapping is `svcerr.Wrap(err)` — always, in every handler. Do not write a per-service helper.
- Define domain failures with `svcerr` sentinels (`svcerr.ErrNotFound` etc.) or constructors (`svcerr.NotFound("user")`) in `internal/<svc>/`.
- Never pass `req.Msg` directly into a service. Always convert to an internal input type.
- Never expose internal error details (SQL, stack traces) to clients. The unknown-error fall-through lands at `CodeInternal` with a generic message.
- Never reach into the DB or other services from a handler.
- Hand-written handler methods take priority over generated CRUD — the generator skips any method you implement. The primary customization path for a CRUD RPC is replacing its delegation in the user-owned `handlers_crud.go`.
- Run `forge generate` after any proto change; never hand-edit `gen/`, `handlers_crud_ops_gen.go`, or `authorizer_gen.go` (custom authorization goes in `authorizer.go`; CRUD customization goes in `handlers_crud.go`).

## Where a handler's collaborators come from (the composition root)

A handler never resolves its own dependencies. Each binary owns a typed composition root — `Build(infra) (*Server, error)` in `internal/app/` — that constructs every service in topological order and hands each one its `Deps` as **interface-typed fields, resolved by type**. A handler's `Deps.Things` is a `things.Service` interface; the handler cannot tell whether `Build` filled it with the real in-process service, a Connect client to another binary, or a mock.

There is no `AppExtras` struct, no string-keyed registry, and no name-matched `wire_gen.go`. The Deps fields are filled in exactly one place — `Build` — so the wiring is plain, compile-checked Go:

```go
// internal/app/build.go
things := things.New(things.Deps{Repo: repo, Logger: log})
srv.MountThings(thingsHandler.New(thingsHandler.Deps{Things: things}))
```

If `repo` does not satisfy `things.Repository`, it does not compile — there is no name-match layer to silently drop a narrow-interface mismatch.

**Deferred / cross-lane typing is handled by the seam, not by a placeholder marker.** When a collaborator's concrete type lands in a sibling lane that hasn't merged, the handler still depends only on the *interface* (`Things things.Service`). The collaborator interface is the seam: the default fill is the real in-process instance, and splitting the service out later — or swapping in a client or a mock — is a one-line change in `Build`, with the handler untouched:

```go
// in-process default:
Things: things.New(things.Deps{Repo: repo}),
// split to its own Deployment later — handler unchanged:
Things: thingsclient.New(conn),
// mock in a test — handler unchanged:
Things: mockThings,
```

Because every dep is an interface filled in one place, "run the app with Things mocked" is a few-line call against `Build` — no framework, no `any`-typed placeholder, no runtime nil hazard. A missing collaborator is a compile error or a loud `validateDeps()` failure at construction, never a typed-zero that quietly no-ops in production.

## Marking optional Deps fields

Most `Deps` fields are required for production traffic — `Build` fills them at construction, `validateDeps()` rejects nil, and per-RPC `if s.deps.X == nil` checks become dead code. The upgrade codemod will strip those checks because they're boilerplate.

A small set of fields are **legitimately optional** — a NATS publisher used only on the rollback path, an audit fallback, an optional gateway feature. For these, tag the field with the `// forge:optional-dep` marker on the line directly above (or as the inline trailing comment after) the declaration:

```go
type Deps struct {
    Logger     *slog.Logger
    Config     *config.Config
    Authorizer middleware.Authorizer
    Repo       Repository

    // NATSPublisher publishes domain events; nil disables the rollback path.
    // forge:optional-dep
    NATSPublisher EventPublisher
}
```

Two things change when the marker is present:

1. **validateDeps()** must NOT include a check for the field. The marker says "nil is OK"; gating it would defeat the design.
2. **Per-RPC nil-checks** like `if s.deps.NATSPublisher != nil { s.deps.NATSPublisher.Publish(...) }` are idiomatic Go for an optional dep and stay. (For *non*-optional deps, drop these checks — `validateDeps()` already gates non-nil at construction.)

`forge lint --conventions` catches misplaced markers (`forgeconv-optional-dep-marker-position`) — the marker only takes effect when attached to a `Deps` struct field, so a typo on the struct or a function docstring fails loudly.

## Extending Repository without breaking sibling fakes (role-interface pattern)

`Repository` is the canonical name for a service's storage interface. The greenfield convention is "one Repository per service, extend as needed" — a Get<Entity>/Create<Entity>/List<Entity> method per RPC, all on the same interface. That works for a single agent owning the whole package.

In a parallel-migration round it does *not* work. Adding a single method to `Repository` atomically breaks every fake Repository in sibling files (test fakes in `internal/<svc>/handlers_test.go`, in-memory fakes in `e2e/`, the generated mock in `internal/<svc>/mock_gen.go`). Agent A adds `GetModelPerformance`; agent B's fakes — or worse, an in-flight rebase carrying stale Repository methods — instantly fail to compile.

**The recommended shape** when adding a new method to an existing Repository in a parallel-migration round is the **opt-in role interface**: declare a small, narrow interface in the file that consumes it, and have the handler type-assert `s.deps.Repo` to that interface at call time.

```go
// internal/api/handlers.go

// ModelPerformanceLister is the narrow read surface for GetModelPerformance.
// It's declared alongside the consuming method so sibling fakes that don't
// implement it can still satisfy Repository — adding a method here does
// NOT break tests that build a *fakeRepo without it.
type ModelPerformanceLister interface {
    GetModelPerformance(ctx context.Context, opts ModelPerformanceOpts) ([]*db.ModelPerformance, error)
}

func (s *Service) GetModelPerformance(
    ctx context.Context,
    req *connect.Request[apiv1.GetModelPerformanceRequest],
) (*connect.Response[apiv1.GetModelPerformanceResponse], error) {
    lister, ok := s.deps.Repo.(ModelPerformanceLister)
    if !ok {
        return nil, connect.NewError(connect.CodeUnimplemented,
            fmt.Errorf("api.GetModelPerformance: Repo does not implement ModelPerformanceLister"))
    }
    rows, err := lister.GetModelPerformance(ctx, ...)
    ...
}
```

Production `*ormRepo` implements both `Repository` and `ModelPerformanceLister`; the assertion is a no-op at runtime. Sibling fakes that haven't grown the new method satisfy the broader `Repository` interface and return `CodeUnimplemented` when the new RPC is called against them — the same outcome as a CRUD shape-mismatch stub.

Once every consumer of the Repository fake adds the new method (or the migration round ends and a polish-round consolidates), promote the role interface back onto the main `Repository` interface: the consuming code stays unchanged, and the type assertion becomes a degenerate `Repository → Repository` that always succeeds.

**When to use this pattern.**

- Adding a method to a Repository that has 2+ fake implementations in sibling lanes.
- Adding a method whose impl is in-flight in a parallel agent's package.
- Standing up a new RPC whose storage shape isn't final (the role interface absorbs the churn while the impl evolves).

**When NOT to use it.**

- Greenfield work where you own both the Repository and every fake. The role-interface adds indirection for no benefit.
- After the migration round ends. Consolidate the role back onto `Repository` so the type assertion goes away.

See `forge lint --conventions` for the matching check that flags a role interface that was added but never promoted — once every consumer implements it the lint surfaces "ready to consolidate" so the indirection doesn't linger.

## When this skill is not enough

- **Designing the service surface** behind the handler — see `service-layer` and `contracts`.
- **Proto-level concerns** (annotations, CRUD naming, pagination shape) — see `proto`.
- **Auth wiring and provider choice** — see `auth` and `packs`.
- **Test patterns** beyond the unit handler test — see `testing/patterns`.
- **Naming conventions** across Go (`PascalCase` types, `camelCase` locals), proto (`snake_case` fields, `PascalCase` messages), and on-disk paths (`snake_case` service directories under `internal/`) — see `architecture` → **Naming conventions**.
