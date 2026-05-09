---
name: handlers
description: Thin-translation handler pattern — validate, extract auth, convert proto↔internal, call service, wrap errors via `svcerr.Wrap`. Business logic lives in `internal/<svc>/contract.go`, never in handlers.
---

# Handler = Thin Translation Layer

A Connect RPC handler is **wire-format glue**, not business logic. Every handler in a forge project follows the same six-step shape:

1. Validate the request.
2. Extract auth / user context from the request.
3. Convert proto → internal input type.
4. Call the service (this is where the business logic lives).
5. Convert internal result → proto.
6. Wrap errors with `svcerr.Wrap`.

If a handler grows past those six steps, the extra logic belongs behind the service interface in `internal/<svc>/contract.go`. See `service-layer` for the other half.

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

### Why no per-service helper

Pre-1.7 forge prescribed a per-service `mapServiceError` / `toConnectError` helper. Across the cpnext dogfood pass that produced **four byte-identical copies** of the same switch statement in `handlers/{billing,daemon,llm_gateway,org}/handlers.go` — each one mapping the same sentinels to the same Connect codes. The skill earned the duplication. The fix is to ship one mapping, in one library, and have every handler call into it.

`forge lint --conventions` ships a warning (`forgeconv-no-handler-error-mapping`) that flags any handler-tree file declaring a function whose name and body shape match the old per-service mapper pattern. If you see that warning, replace the helper with `svcerr.Wrap`.

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
// handlers/things/validators.go
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

- **DB calls.** No `db.Begin`, no ORM functions, no SQL. Those live behind the service.
- **Business decisions.** "Should this user be allowed to do X?" is a service concern; the handler only knows that auth exists.
- **A hand-rolled `mapServiceError` / `toConnectError` helper.** Use `svcerr.Wrap(err)`. The lint warns when you re-roll one.
- **Cross-service orchestration.** "Create a thing AND send an email AND update the audit log" belongs in one service method, not split across the handler.
- **Logging beyond the unknown-error path.** Structured logging happens in middleware (request log) and in the service's tracing wrapper. Handlers stay quiet.
- **Retries, transactions, idempotency.** All service concerns.

## Testing handlers

With this shape, handler unit tests are nearly mechanical:

- Construct a `MockService` from `internal/things/mock_gen.go`.
- Set up `mockSvc.On("DoThing", mock.Anything, expectedInput).Return(result, nil)`.
- Call the handler via the test helper from `pkg/app/testing.go`.
- Assert the response or `connect.CodeOf(err)`.

For error-path tests, return a `svcerr.Err*` sentinel (or constructor) from the mock and assert `connect.CodeOf(err)` matches the expected code — the wrap is library-tested, you don't have to re-cover it per service.

See `testing/patterns` Pattern 1 for the table-driven template, and the `tdd.RunRPCCases` runner from `pkg/tdd` for the canonical per-RPC test shape.

## Rules

- Six steps. No business logic in the handler.
- Validators are pure, testable functions in `handlers/<svc>/validators.go`.
- Error mapping is `svcerr.Wrap(err)` — always, in every handler. Do not write a per-service helper.
- Define domain failures with `svcerr` sentinels (`svcerr.ErrNotFound` etc.) or constructors (`svcerr.NotFound("user")`) in `internal/<svc>/`.
- Never pass `req.Msg` directly into a service. Always convert to an internal input type.
- Never expose internal error details (SQL, stack traces) to clients. The unknown-error fall-through lands at `CodeInternal` with a generic message.
- Never reach into the DB or other services from a handler.
- Hand-written handler methods take priority over generated CRUD — the generator skips any method you implement.

## Cross-lane type placeholders (`forge:placeholder`)

Parallel agent lanes often need to declare an `AppExtras` field whose typed `Repository` / `Client` / `Provider` lives in a sibling lane that has not yet landed. The historical workaround was to type the field as `any` and add an inline cast in `wire_gen.go` (the `castUserRepo` shim that shipped to cpnext was the canonical example). That bridge typed the field at the wrong layer and silently masked the case where the sibling lane *never* landed — the worker registered with `Repo == nil` and quietly no-op'd in production.

`forge:placeholder` makes the deferred-typing intent explicit. Declare the AppExtras field as `any` AND tag it with the target type the marker promises will land:

```go
// pkg/app/app_extras.go
type AppExtras struct {
    // UserRepo is owned by the user-handler lane; type lands when that
    // lane merges. Tighten this declaration to user.Repository at merge
    // time — the marker becomes a no-op once the field type matches.
    // forge:placeholder: user.Repository
    UserRepo any
}
```

Both the comment shape (matches `// forge:optional-dep`) and the struct-tag shape (`UserRepo any \`forge:placeholder:"user.Repository"\``) are accepted.

Three things change when the marker is present:

1. **wire_gen** emits a typed `resolveUserRepo(app) user.Repository` accessor at file scope. Each consuming `wireXxxDeps` calls the accessor instead of `app.UserRepo`, so the typed Deps field receives a typed value. The accessor compiles whether the AppExtras field is still `any` (during the cross-lane port) or already typed `user.Repository` (after tightening) — `any(app.UserRepo).(user.Repository)` is a no-op in the latter case.
2. **`forge generate` ERRORS** when an AppExtras field carries the marker but is still typed `any`. The build halts with a message naming the field, the current type, and the target type — the user knows exactly which declaration to tighten.
3. **`forge lint --wire-coverage` ERRORS** on the same condition, even when `wire_gen.go` is missing (the placeholder error fires off `app_extras.go` directly so a `forge generate`-refused-to-write state still gets diagnosed).

Once you tighten the AppExtras declaration from `UserRepo any` to `UserRepo user.Repository`, the marker becomes a no-op — the build-time gate stops firing, the runtime accessor is still emitted, and the type assertion in `resolveUserRepo` becomes a degenerate `any(typed).(typed)` that always succeeds.

If `app.UserRepo` is nil at runtime (e.g. the `setup.go` wiring forgot to assign it), the accessor panics with a clear message. That's deliberate — silent nil passthrough is the bug class the marker exists to surface.

Use `forge:placeholder` only when the typed value originates from a sibling parallel-agent lane. For genuinely optional fields, use `forge:optional-dep` instead — see the next section.

## Marking optional Deps fields

Most `Deps` fields are required for production traffic — wire_gen sources them from `*App` at startup, `validateDeps()` rejects nil, and per-RPC `if s.deps.X == nil` checks become dead code. The upgrade codemod will strip those checks because they're boilerplate.

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

Three things change when the marker is present:

1. **wire_gen** emits the typed-zero assignment silently — no inline `// TODO: wire NATSPublisher`, no contribution to the `UNRESOLVED FIELDS` header. The user explicitly opted in to "may be nil"; warning every regenerate is noise.
2. **validateDeps()** must NOT include a check for the field. The marker says "nil is OK"; gating it would defeat the design.
3. **Per-RPC nil-checks** like `if s.deps.NATSPublisher != nil { s.deps.NATSPublisher.Publish(...) }` are idiomatic Go and the upgrade codemod leaves them alone.

`forge lint --conventions` catches misplaced markers (`forgeconv-optional-dep-marker-position`) — the marker only takes effect when attached to a `Deps` struct field, so a typo on the struct or a function docstring fails loudly. `forge lint --wire-coverage` reports any non-optional unresolved Deps fields so they don't accumulate silently.

## When this skill is not enough

- **Designing the service surface** behind the handler — see `service-layer` and `contracts`.
- **Proto-level concerns** (annotations, CRUD naming, pagination shape) — see `proto`.
- **Auth wiring and provider choice** — see `auth` and `packs`.
- **Test patterns** beyond the unit handler test — see `testing/patterns`.
- **Naming conventions** across Go (`PascalCase` types, `camelCase` locals), proto (`snake_case` fields, `PascalCase` messages), and on-disk paths (`snake_case` directories under `handlers/`) — see `architecture` → **Naming conventions**.
