---
title: "Best Practices"
description: "Best practices for Forge development"
weight: 110
---

# Best Practices

Recommended practices for building robust Forge applications.

## Proto-First Development

1. **Always define external APIs in proto first** - Before implementing
2. **Use semantic versioning** - /v1, /v2 for API versions
3. **Run proto-breaking checks** - Before merging changes
4. **Document in proto** - Comments become documentation
5. **Enforce with linter** - Use `forge lint --contract`

## Two Contract Systems

1. **Proto contracts** for external boundaries - Config and API protos
2. **Go interface contracts** for internal boundaries - `contract.go`
3. **Don't use proto for internal packages** - Go interfaces are richer
4. **Keep contracts minimal** - Only what consumers need
5. **Generated middleware wraps contracts** - Logging, tracing, mocking

## Service Design

1. **Single responsibility** - One service, one purpose
2. **Declare dependencies in `Deps` struct** - Not global variables
3. **Use constructor injection** - Via `New(deps Deps) *Service`
4. **Return Connect RPC error codes** - For all errors
5. **Validate input** - At service boundary

## Database

1. **Migration-first** - SQL migrations are the source of truth
2. **Use forge-orm** (`pkg/orm/`) - For generated CRUD from migrated schema
3. **Use transactions** - For multi-step operations
4. **Add indexes** - For queried fields
5. **Include timestamps** - created_at, updated_at

## Testing

1. **Use the generated test harness** - `app.NewTestXxxService(t)`
2. **Use table-driven tests** - For validation
3. **Mock swapping via config** - Test config defaults to mocks
4. **Integration tests** - For critical paths
5. **Aim for 80%+ coverage** - Measure and improve

## Middleware

1. **Recovery first** - Catch panics
2. **Authentication early** - Before business logic
3. **Metrics last** - Measure everything
4. **Keep lightweight** - Minimal overhead
5. **Use Connect interceptors** - `connect.UnaryInterceptorFunc`

## Performance

1. **Use connection pooling** - Configure database connections
2. **Batch operations** - Avoid N+1 queries
3. **Add indexes** - For frequently queried fields
4. **Cache appropriately** - Read-heavy workloads
5. **Set timeouts** - Prevent hanging requests

## Security

1. **Validate all input** - Never trust client data
2. **Use authentication** - For protected endpoints
3. **Rate limiting** - Prevent abuse
4. **Sanitize errors** - Don't leak internal details
5. **Use TLS** - In production

## Deployment

1. **Environment configs** - KCL per-env files
2. **Health checks** - For all services
3. **Graceful shutdown** - Handle signals
4. **Monitoring** - Export metrics
5. **Structured logging** - JSON format

## Code Organization

```
myproject/
├── cmd/                    # Cobra CLI (single binary)
│   ├── main.go
│   ├── server.go           # server [services...] command
│   └── version.go
├── services/               # Proto API service handlers
│   └── <name>/
│       ├── service.go      # New(deps), Register(mux, opts)
│       └── handlers.go     # RPC implementations
├── internal/               # Internal package boundaries
│   └── <name>/
│       ├── contract.go     # Go interface contract
│       ├── service.go      # Implementation
│       ├── mock_gen.go     # Generated mock
│       └── middleware_gen.go
├── pkg/app/
│   ├── wire.go             # Generated wiring
│   └── testing.go          # Generated test harness
├── proto/                  # Proto definitions
│   ├── config/v1/
│   └── services/<name>/v1/
├── gen/                    # Proto-generated code (separate module)
├── deploy/
│   ├── Dockerfile          # Single Dockerfile
│   └── kcl/                # KCL manifests
└── db/migrations/          # SQL migrations
```

## Common Pitfalls

1. **Defining internal APIs in proto** - Use Go interface contracts (`contract.go`) instead
2. **Global state** - Use `Deps` struct and constructor injection
3. **Ignoring errors** - Always handle errors
4. **No timeouts** - Set context timeouts
5. **No tests** - Write tests as you go
6. **Blocking in handlers** - Use goroutines for long tasks
7. **Not using transactions** - For multi-step DB operations
8. **Skipping `forge generate`** - Always regenerate after proto or contract changes

## See Also

- [Creating Services]({{< ref "creating-services" >}})
- [Testing Strategies]({{< ref "testing-strategies" >}})
- [Performance]({{< ref "performance" >}})