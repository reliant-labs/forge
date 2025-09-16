---
title: "Performance Optimization"
description: "Optimizing Forge service performance"
weight: 90
---

# Performance Optimization

Techniques for optimizing Forge service performance.

## Database Query Optimization

### Use Indexes

Define indexes in your proto entities or SQL migrations:

```sql
-- db/migrations/000002_add_indexes.up.sql
CREATE INDEX idx_users_email ON users (email);
CREATE INDEX idx_users_status_created ON users (status, created_at);
```

### Batch Operations

```go
// Bad: N+1 queries
for _, id := range ids {
    user, _ := db.GetUserByID(ctx, s.deps.DB, id)
    users = append(users, user)
}

// Good: Single query
users, _ := db.ListUsers(ctx, s.deps.DB,
    orm.WithWhere("id", orm.In, ids),
)
```

### Connection Pooling

Configure the connection pool via config proto:

```go
// pkg/app/wire.go — configure pool settings
poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseUrl)
if err != nil {
    return nil, err
}
poolConfig.MaxConns = 25
poolConfig.MinConns = 5
pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
```

## Connect RPC Optimization

### Use Binary Protocol

For service-to-service calls, use Protobuf encoding instead of JSON:

```go
client := usersv1connect.NewUsersServiceClient(
    http.DefaultClient,
    "http://users-service:8080",
    connect.WithProtoJSON(), // Or connect.WithGRPC() for gRPC wire format
)
```

### Connection Reuse

Reuse HTTP clients to benefit from connection pooling:

```go
// Good: Shared HTTP client with connection pool
httpClient := &http.Client{
    Transport: &http.Transport{
        MaxIdleConnsPerHost: 10,
        IdleConnTimeout:     90 * time.Second,
    },
}

client := usersv1connect.NewUsersServiceClient(httpClient, serviceURL)
```

## Middleware Optimization

### Skip Expensive Middleware

Use selective middleware application:

```go
interceptors := connect.WithInterceptors(
    middleware.RecoveryInterceptor(),  // Always
    middleware.LoggingInterceptor(logger),
)

// Only add tracing in non-dev environments
if cfg.Environment != "dev" {
    interceptors = connect.WithInterceptors(
        middleware.RecoveryInterceptor(),
        middleware.TracingInterceptor(tracer),
        middleware.LoggingInterceptor(logger),
    )
}
```

## Build Optimization

### Binary Size

The generated Dockerfile already uses:
- `CGO_ENABLED=0` for static linking
- `-ldflags="-s -w"` to strip debug info

### Docker Layer Caching

The generated Dockerfile uses BuildKit mount caches:

```dockerfile
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    go build -o /app ./cmd/
```

## Monitoring

### Add Metrics Interceptor

```go
func MetricsInterceptor(collector *prometheus.HistogramVec) connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            start := time.Now()
            resp, err := next(ctx, req)
            duration := time.Since(start).Seconds()

            collector.WithLabelValues(
                req.Spec().Procedure,
                connectCodeToString(err),
            ).Observe(duration)

            return resp, err
        }
    }
}
```

## Best Practices

1. **Profile before optimizing** — Use `go tool pprof`
2. **Use connection pools** — For database and HTTP clients
3. **Index database queries** — Especially for filtered/sorted columns
4. **Monitor latency** — Add metrics interceptors
5. **Cache selectively** — At the application layer where appropriate
6. **Binary encoding** — Use Protobuf instead of JSON for inter-service calls

## See Also

- [Service Patterns]({{< ref "service-patterns" >}})
- [Database Integration]({{< ref "database-integration" >}})
