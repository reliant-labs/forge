---
name: api
description: Write Connect RPC handlers — the pb-through model (every RPC is a method on the handler package's `*Service` working with wire types + `s.deps`), CRUD via `// forge:entity` + `pkg/crud`, error handling with `svcerr.Wrap`, and testing.
---

# Connect RPC API Handlers

Every RPC is a method on the handler package's `*Service` in `internal/handlers/<svc>/`, working with the generated **wire (pb) types** and `s.deps`. There is ONE shape — "pb-through". A **CRUD** RPC (a `// forge:entity` message with a matching table) delegates to the generic `pkg/crud` runtime through an owned shim; a **custom** RPC is a method you implement in place on `*Service`. Keep it thin and predictable:

1. Validate the request. 2. Extract auth / user context. 3. Do the work through `s.deps` (the repo, or a domain collaborator for real orchestration). 4. Pack the result into the response proto. 5. Wrap errors with `svcerr.Wrap`.

For a simple RPC, step 3 is a direct `s.deps.Repo` call. When an RPC grows real orchestration you want to unit-test transport-free (or reuse), extract it into a **standalone package** (`forge scaffold package <name>`) called through `s.deps` — forge's by-type DI wires it. There is no per-service domain package and no generated proto↔domain converters; the handler owns any wire→domain mapping explicitly. See `service-layer` and `contracts`.

## Where handlers live, and what codegen gives you

Everything for a service lives in ONE `internal/handlers/<svc>/` directory. The concrete `*Service` + `deps` is in `service.go`; one custom RPC per `rpc_<name>.go`; CRUD delegations in the owned `handlers_crud.go` beside the generated `handlers_crud_ops_gen.go`; `validators.go` is yours. There is no `contract.go` in a handler package — the `*Service` methods **are** the handlers. The struct embeds the generated `Unimplemented*Handler` and carries `deps` (`type Service struct { gen.UnimplementedUserServiceHandler; deps Deps }`).

RPCs are defined in `proto/services/<svc>/v1/<svc>.proto`. Naming conventions trigger auto-generated features:

- **CRUD methods** (`Create<Entity>`, `Get<Entity>`, `List<Entities>`, `Update<Entity>`, `Delete<Entity>`) whose entity has a matching table → forge generates per-RPC op constructors (request↔entity + filter→column mapping, response packing) in `handlers_crud_ops_gen.go` and scaffolds thin ~3-line delegations into the owned `handlers_crud.go`: `return crud.HandleCreate(s.crudCreateItemOp())(ctx, req)`. The delegations never name entity fields, so schema changes flow through the regenerated ops file and never rot; to customize, replace the delegation right in `handlers_crud.go` (it's yours; `forge generate` only appends shims for new CRUD RPCs). A CRUD RPC with no matching table generates an honest Unimplemented stub — create the table first (`forge scaffold entity`).
- **AIP-158 pagination fields** (`page_size`, `page_token`, `next_page_token`) → cursor pagination auto-generated. **`idempotency_key`** → signals callers to pass an `Idempotency-Key` header.
- **`optional` filter fields** on List requests → query filters auto-generated (`search`/`query`/`q` → ILIKE across text columns; any other filter must name a real column or `forge generate` fails loudly). Filter fields must be `optional` in proto, else codegen can't distinguish "not set" from zero.
- **`auth_required` annotation** → informational metadata only (feeds `forge project map`/`graph`); **it gates nothing at runtime.** The auth interceptor establishes identity; what a caller may then do is handler logic you write against `middleware.GetUser(ctx)`. See `auth`.

**Hand-written methods always take priority** — the generator skips any method you've implemented. After any proto change run `forge generate`; never hand-edit `gen/` or `*_gen.go`. Cross-cutting concerns (auth, logging, recovery, request IDs) live in the interceptor chain (`observe.Chain`), not in handlers.

## The canonical handler

```go
func (s *Service) DoThing(
    ctx context.Context, req *connect.Request[apiv1.DoThingRequest],
) (*connect.Response[apiv1.DoThingResponse], error) {
    if err := validateDoThingRequest(req.Msg); err != nil {          // 1. validate
        return nil, connect.NewError(connect.CodeInvalidArgument, err)
    }
    claims, err := middleware.ClaimsFromContext(ctx)                 // 2. extract auth
    if err != nil {
        return nil, connect.NewError(connect.CodeUnauthenticated, err)
    }
    result, err := s.deps.Things.DoThing(ctx, things.DoThingInput{   // 3+4. convert + do the work
        UserID: claims.UserID, Name: req.Msg.Name,
    })
    if err != nil {
        return nil, svcerr.Wrap(err)                                 // 5. wrap errors
    }
    return connect.NewResponse(&apiv1.DoThingResponse{               // pack the response
        Id: result.ID, CreatedAt: timestamppb.New(result.CreatedAt),
    }), nil
}
```

This delegates to a `Things` domain package; a simpler RPC would query `s.deps.Repo` right here.

## Error mapping — use `svcerr`, do NOT hand-roll a helper

Every handler uses `svcerr.Wrap(err)` from `github.com/reliant-labs/forge/pkg/svcerr`. The library owns the service-error → connect-error mapping; do **not** re-implement it per service. It handles three cases:

| input | result |
|---|---|
| a wrapped sentinel (`svcerr.NotFound("user")`) | the matching `*connect.Error`, **message preserved** |
| an existing `*connect.Error` | passed through untouched |
| anything else (raw DB / SDK error) | `CodeInternal`, message replaced with `internal server error`, original kept as a server-only cause |

Row 3 is why: `connect.Error.Message()` is literally `err.Error()`, so an error whose text you did not write is text you publish to an anonymous caller — postgres writes things like `relation "accounts" does not exist (SQLSTATE 42P01) dsn=postgres://app:s3cr3t@db:5432/prod`. `svcerr.Wrap` redacts it; the original stays reachable server-side via `svcerr.Cause(err)`, `errors.Is`/`errors.As`, and the logging interceptor logs it with the request id, so do not re-log it at the call site.

To keep a useful client message **and** the diagnostic, use `svcerr.WithCause` — client sees the summary, the log gets the driver error:

```go
return nil, svcerr.Wrap(svcerr.WithCause(svcerr.Internal("create thing failed"), err))
```

Redaction applies only to that fallback: codes you choose (`InvalidArgument`, `NotFound`, …) keep the message you gave them, because it is part of your API. So never put a driver error inside one — `svcerr.InvalidArgument(err.Error())` opts out of every protection above.

`forge lint --conventions` warns (`forgeconv-no-handler-error-mapping`) on a re-rolled per-service mapper.

### Domain sentinels

Return domain failures with `svcerr` sentinels (not bespoke ones) — the bare sentinel (`return nil, svcerr.ErrNotFound`) when there's no detail, or the constructor (`svcerr.NotFound("thing")`, `svcerr.FailedPrecondition("no billing account")`) when a human-readable detail belongs in the string. Both preserve the sentinel for `errors.Is` / `svcerr.Code`. The full set covers every `connect.Code`:

| svcerr sentinel / constructor | connect.Code + when |
|---|---|
| `ErrNotFound` / `NotFound` | `CodeNotFound` — missing, or hidden by row-scoping WHERE |
| `ErrAlreadyExists` / `AlreadyExists` | `CodeAlreadyExists` — unique constraint, idempotency replay |
| `ErrPermissionDenied` / `PermissionDenied` | `CodePermissionDenied` — authenticated but not authorized |
| `ErrUnauthenticated` / `Unauthenticated` | `CodeUnauthenticated` — no valid identity |
| `ErrInvalidArgument` / `InvalidArgument` | `CodeInvalidArgument` — domain invariants violated post-validation |
| `ErrFailedPrecondition` / `FailedPrecondition` | `CodeFailedPrecondition` — system not in required state |
| `ErrAborted` / `Aborted` | `CodeAborted` — concurrency / transactional conflict |
| `ErrResourceExhausted` / `ResourceExhausted` | `CodeResourceExhausted` — rate-limit / quota / plan-limit |
| `ErrUnavailable` / `Unavailable` | `CodeUnavailable` — upstream dependency offline |
| `ErrUnimplemented` / `Unimplemented` | `CodeUnimplemented` — stubbed / feature-flagged |

Add a new sentinel only when the set has no representative for the code you need. (`context.Canceled`/`DeadlineExceeded` pass through `svcerr.ToConnect`.)

### Structured detail and routing codes (both rare)

`svcerr.WithDetail(err, proto)` attaches a machine-readable proto (e.g. a `FieldViolation`) — most handlers never need it. `svcerr.WithReason(err, "no_active_subscription")` attaches a stable snake_case code the frontend ROUTES on (upsell, redirect to billing) instead of brittle message TEXT; `svcerr.Wrap` carries it to the wire as `x-forge-error-reason` metadata. Generated CRUD already stamps one on EVERY error it returns (`duplicate`, `reference_in_use`, `not_found`, … — the `crud.Reason*` constants); reuse those names where the meaning matches. Both preserve `errors.Is`/`svcerr.Code`.

## Validation helpers

Wire-format validation (required fields, bounds, format) goes in a per-service `validators.go` as pure functions taking `*apiv1.<Method>Request` and returning `error` — easy to table-test, never touching context, DB, or services:

```go
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

**Domain-level invariants** ("can this user create a thing in this org?") belong in the service — if checking the rule needs DB access or external state, it's a service concern.

## When proto and internal types diverge

For a CRUD MVP proto and internal types often look identical, and passing `req.Msg` straight into the service is tempting. **Don't** — they diverge at scale (proto `*string`/`page_token`/oneofs vs internal `string`/domain enums; different evolution clocks). Convert at the handler boundary; that translation IS the handler's job. See `service-layer`.

`middleware.ClaimsFromContext(ctx)` is the canonical auth extraction: pass the relevant fields (UserID, OrgID, roles) into the service input — never the raw `*Claims`; the service interface should not know your auth provider exists. Unauthenticated RPCs skip step 2.

## What does NOT belong in a `*Service` method

A `*Service` method reads and writes its own tables through `s.deps`. Out of the method body:

- **Resolving your own dependencies.** Collaborators arrive in `Deps`, filled once by `NewComponents` — never construct a repo, open a pool, or dial another service.
- **A hand-rolled `mapServiceError` helper** (use `svcerr.Wrap`), and **reaching into another service's storage** (call a peer through its `Deps` interface, never its tables).
- **Cross-cutting middleware** (auth enforcement, logging, recovery, request IDs) — those live in the interceptor chain; log only on the unknown-error path. **Logic that deserves transport-free tests or reuse** → a standalone package the handler calls via `s.deps`, not a god-method.
- **A server for an RPC another repo owns.** If you're only a *client*, import the upstream proto and generate a client — never hand-copy it and scaffold a handler. Every method `CodeUnimplemented` with `Deps` carrying only `Logger`/`Config` is the tell. See `proto`.

## Testing handlers

The scaffold gives you one test file per RPC (`handlers_scaffold_<rpc>_test.go`, no shared helper) built on `tdd.RunRPCCases` (`pkg/tdd`), run against the real `*Service` wired by the test harness (`<pkg>.NewTest<Svc>(t)`, from that service's own `helpers_gen_test.go`) — no mock of the handler itself. Each row declares a request, an expected outcome (`Check` or a `WantErr` code), and an optional setup hook; the scaffold row asserts `CodeUnimplemented` and self-destructs the moment you implement the RPC. When a handler delegates to a domain package, fake that collaborator through its generated `mock_gen.go` (set the `XxxFunc` field), pass it via `<pkg>.With<Svc>Deps(...)`, return a `svcerr.Err*` sentinel to exercise error paths, and assert `connect.CodeOf(err)`. Run `task test`. See `testing/patterns` Pattern 1.

## Extending Repository without breaking fakes

Load the `api/role-interface` skill for the opt-in role-interface pattern — adding a `Repository` method in a parallel-migration round without breaking every sibling fake.

## Deps: where they come from, and optional fields

The explicit composition (`NewComponents`) fills each `Deps` field as an **interface, resolved by type** — the handler can't tell the real in-process service from a Connect client or a mock (see `architecture` → **The composition root**). Most fields are required — `validateDeps()` rejects nil, so per-RPC `if s.deps.X == nil` checks are dead code (the codemod strips them). A **legitimately optional** field (a NATS publisher used only on the rollback path) is tagged `// forge:optional-dep` directly above the declaration; then `validateDeps()` must NOT check it and its `if s.deps.X != nil { ... }` guards stay. See `service-layer`.

## Adding or customizing an RPC

- **New custom RPC:** declare it in the proto and run `forge generate` (or `forge scaffold rpc <service> <Name>`, which self-heals a stale descriptor and writes a correctly-signed stub); generate emits a pb-through method returning `CodeUnimplemented` for you to fill in place (no generated `<Name>Input`/`Result` types or converters). After a batch of proto edits, `forge scaffold` does the whole chain in one phased run (`--dry-run` plans).
- **Customizing a CRUD RPC:** replace its delegation in the owned `handlers_crud.go` — access-control / row-scoping `WHERE` goes there, never in `handlers_crud_ops_gen.go`.

## When this skill is not enough

A transport-agnostic domain/use-case layer behind the handler → `service-layer`, `contracts`, `interactor`. Proto-level concerns (annotations, CRUD naming, pagination) → `proto`. Auth wiring, access control, row scoping → `auth`. Test patterns → `testing/patterns`. Naming conventions across Go / proto / on-disk paths → `architecture` → **Naming conventions**.
