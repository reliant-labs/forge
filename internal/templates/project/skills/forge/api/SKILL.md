---
name: api
description: Write Connect RPC handlers — proto service definitions, error handling, middleware, and testing.
---

# Connect RPC API Handlers

## Proto Service Definitions

Define RPCs in `proto/services/<svc>/v1/<svc>.proto`. Naming conventions matter — they trigger auto-generated features:

- **CRUD methods** (`Create<Entity>`, `Get<Entity>`, `List<Entities>`, `Update<Entity>`, `Delete<Entity>`) matching a `proto/db/` entity → full handler implementations are auto-generated in `handlers_crud_gen.go`.
- **AIP-158 pagination fields** (`page_size`, `page_token`, `next_page_token`) → cursor-based pagination is auto-generated.
- **`optional` filter fields** on List requests → query filters are auto-generated (`search` → ILIKE, others → exact match).
- **`required_roles` annotation** → per-method RBAC is auto-generated in `authorizer_gen.go`.
- **`idempotency_key` annotation** → signals callers to pass an `Idempotency-Key` header.

Hand-written handler methods always take priority — the generator skips any method you've already implemented.

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

## Error Handling

Use `connect.NewError` with the appropriate code:

- `connect.CodeInvalidArgument` — bad input, failed validation
- `connect.CodeNotFound` — requested resource doesn't exist
- `connect.CodePermissionDenied` — caller lacks permission
- `connect.CodeUnauthenticated` — missing or invalid credentials
- `connect.CodeInternal` — unexpected server errors (log the underlying cause, return a generic message)

Never expose internal details (stack traces, SQL errors) to clients.

## Input Validation

Validate at the handler entry point before any business logic:

```go
if req.Msg.GetEmail() == "" {
    return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("email is required"))
}
```

## Middleware

Middleware lives in `pkg/middleware/` and is wired in `cmd/server.go`. Use it for cross-cutting concerns: auth, logging, recovery, request IDs. Keep handler code focused on business logic.

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

- **Unit tests**: Use generated mocks from `mock_gen.go` to isolate handler logic.
- **Integration tests**: Use a real database to verify queries and transactions.

```bash
forge test
forge test --service users
```

## Common Pitfalls

1. **Stale `gen/`** — Run `forge generate` after any proto change.
2. **Wrong error codes** — `NotFound` vs `InvalidArgument` vs `Internal` matter for clients.
3. **Hand-editing `gen/`** — Changes will be overwritten. Always fix the proto source.
4. **Hand-editing `authorizer_gen.go`** — It's regenerated. Put custom authorization logic in `authorizer.go`.
5. **Non-optional filter fields** — List request filter fields must be `optional` in proto, otherwise the generated code can't distinguish "not set" from zero values.
