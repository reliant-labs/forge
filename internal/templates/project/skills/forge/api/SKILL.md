---
name: api
description: Write Connect RPC handlers — error handling, validation, middleware, database access, and testing.
---

# Connect RPC API Handlers

## Handler Anatomy

Handlers live in `handlers/<svc>/service.go`. Each service struct embeds the generated `Unimplemented*Handler` and implements RPC methods:

```go
type UserService struct {
    gen.UnimplementedUserServiceHandler
    db  *db.Queries
}

func (s *UserService) GetUser(ctx context.Context, req *connect.Request[gen.GetUserRequest]) (*connect.Response[gen.GetUserResponse], error) {
    // validate, execute, respond
}
```

## Generated Types

All request/response types and interfaces come from `gen/`. **Never hand-edit files in `gen/`** — fix the `.proto` source and regenerate:

```bash
forge generate
```

If behavior seems stale, regenerate first. This is the most common cause of confusing type errors.

## Error Handling

Use `connect.NewError` with the appropriate code:

- `connect.CodeInvalidArgument` — bad input, failed validation
- `connect.CodeNotFound` — requested resource doesn't exist
- `connect.CodePermissionDenied` — caller lacks permission
- `connect.CodeUnauthenticated` — missing or invalid credentials
- `connect.CodeInternal` — unexpected server errors (log the underlying cause, return a generic message)

Never expose internal details (stack traces, SQL errors) to clients.

## Input Validation

Validate at the handler entry point before any business logic or database call:

```go
if req.Msg.GetEmail() == "" {
    return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("email is required"))
}
```

Validate all required fields, format constraints, and business rules early. Return `InvalidArgument` with a clear message.

## Middleware

Middleware lives in `pkg/middleware/` and is wired into the HTTP stack in `cmd/server.go`. Use middleware for cross-cutting concerns: auth, logging, recovery, request IDs. Keep handler code focused on business logic.

## Database Access

Use sqlc-generated queries from your `db` package. For multi-step mutations, wrap in a transaction:

```go
tx, err := pool.Begin(ctx)
if err != nil {
    return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to begin transaction"))
}
defer tx.Rollback(ctx)
qtx := s.db.WithTx(tx)
// ... use qtx for queries ...
tx.Commit(ctx)
```

## Testing

- **Unit tests**: Use generated mocks from `mock_gen.go` to isolate handler logic from the database.
- **Integration tests**: Use a real database to verify queries and transactions end-to-end.

```bash
forge test
forge test --service users
```

## Idempotency Keys

Mutating RPCs (Create / Update / Delete) should be annotated with `idempotency_key = true` in the proto so callers know to pass a per-request key. The generated client and docs surface this expectation; the handler is responsible for enforcing it.

```proto
rpc Create(CreateRequest) returns (CreateResponse) {
  option (forge.options.v1.method_options) = {
    auth_required: true
    idempotency_key: true
  };
}
```

Consumers pass the key via the `Idempotency-Key` request header (case-insensitive). Read it from `req.Header().Get("Idempotency-Key")` in the handler, look up any prior result keyed by `(caller, method, key)`, and either replay the stored response or proceed and persist the result. A short TTL (24h is typical) bounds storage.

Read-only methods (Get, List, Search) are naturally idempotent and do not need a key.

## Common Pitfalls

1. **Stale `gen/`** — Run `forge generate` after any proto change. Type mismatches usually mean you forgot.
2. **Wrong error codes** — `NotFound` vs `InvalidArgument` vs `Internal` matter for clients. Choose deliberately.
3. **Missing auth checks** — Ensure auth middleware covers all routes, or validate in the handler.
4. **Hand-editing `gen/`** — Changes will be overwritten. Always fix the proto source.
5. **Ignoring `idempotency_key`** — If a method is annotated with `idempotency_key = true`, the handler must read and honor the `Idempotency-Key` header. Otherwise the annotation is misleading.