---
title: "Creating Middleware"
description: "Building custom middleware for Forge services"
weight: 30
---

# Creating Middleware

Middleware in Forge uses Connect RPC interceptors to provide cross-cutting concerns like logging, authentication, metrics, and more.

## Connect RPC Interceptors

Connect RPC interceptors wrap unary and streaming RPCs:

```go
import "connectrpc.com/connect"

// Unary interceptor
func MyInterceptor() connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            // Before handler
            resp, err := next(ctx, req)
            // After handler
            return resp, err
        }
    }
}
```

## Basic Example: Logging

```go
package middleware

import (
    "context"
    "log/slog"
    "time"

    "connectrpc.com/connect"
)

func Logging(logger *slog.Logger) connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            start := time.Now()
            procedure := req.Spec().Procedure

            logger.Info("request started", "procedure", procedure)

            resp, err := next(ctx, req)

            duration := time.Since(start)
            if err != nil {
                logger.Error("request failed",
                    "procedure", procedure,
                    "duration", duration,
                    "error", err,
                )
            } else {
                logger.Info("request completed",
                    "procedure", procedure,
                    "duration", duration,
                )
            }

            return resp, err
        }
    }
}
```

## Authentication Middleware

```go
package middleware

import (
    "context"

    "connectrpc.com/connect"
)

type contextKey string

const userIDKey contextKey = "user_id"

func Auth() connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            // Extract token from header
            token := req.Header().Get("Authorization")
            if token == "" {
                return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("missing token"))
            }

            // Validate token (simplified)
            userID, err := validateToken(token)
            if err != nil {
                return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid token"))
            }

            // Add user ID to context
            ctx = context.WithValue(ctx, userIDKey, userID)

            return next(ctx, req)
        }
    }
}

// Helper to get user ID from context
func GetUserID(ctx context.Context) (string, bool) {
    userID, ok := ctx.Value(userIDKey).(string)
    return userID, ok
}
```

## Metrics Middleware

```go
package middleware

import (
    "context"
    "time"

    "connectrpc.com/connect"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

var (
    requestDuration = promauto.NewHistogramVec(
        prometheus.HistogramOpts{
            Name: "rpc_request_duration_seconds",
            Help: "RPC request duration in seconds",
        },
        []string{"procedure", "code"},
    )

    requestTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Name: "rpc_requests_total",
            Help: "Total number of RPC requests",
        },
        []string{"procedure", "code"},
    )
)

func Metrics() connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            start := time.Now()

            resp, err := next(ctx, req)

            duration := time.Since(start).Seconds()
            code := "ok"
            if err != nil {
                code = connect.CodeOf(err).String()
            }

            procedure := req.Spec().Procedure
            requestDuration.WithLabelValues(procedure, code).Observe(duration)
            requestTotal.WithLabelValues(procedure, code).Inc()

            return resp, err
        }
    }
}
```

## Rate Limiting Middleware

```go
package middleware

import (
    "context"
    "fmt"
    "sync"

    "connectrpc.com/connect"
    "golang.org/x/time/rate"
)

func RateLimit(requestsPerSecond int) connect.UnaryInterceptorFunc {
    limiters := make(map[string]*rate.Limiter)
    var mu sync.Mutex

    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            procedure := req.Spec().Procedure

            mu.Lock()
            limiter, exists := limiters[procedure]
            if !exists {
                limiter = rate.NewLimiter(rate.Limit(requestsPerSecond), requestsPerSecond)
                limiters[procedure] = limiter
            }
            mu.Unlock()

            if !limiter.Allow() {
                return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("rate limit exceeded"))
            }

            return next(ctx, req)
        }
    }
}
```

## Applying Interceptors

Interceptors are applied when building the Connect handler in `cmd/server.go`:

```go
// cmd/server.go
func runServer(cmd *cobra.Command, args []string) error {
    // Build interceptor chain
    interceptors := connect.WithInterceptors(
        middleware.Recovery(),
        middleware.Logging(logger),
        middleware.Metrics(),
        middleware.Auth(),
    )

    // Build the HTTP mux with all registered services
    mux := http.NewServeMux()

    // Each service registers with the shared interceptors
    userService := services.NewUserService(deps)
    userService.Register(mux, interceptors)

    // Start HTTP server
    return http.ListenAndServe(":8080", mux)
}
```

### Per-Service Interceptors

For service-specific interceptors, apply them at registration time:

```go
// Service with additional auth requirement
adminService := services.NewAdminService(deps)
adminInterceptors := connect.WithInterceptors(
    middleware.Recovery(),
    middleware.Logging(logger),
    middleware.AdminOnly(),
)
adminService.Register(mux, adminInterceptors)
```

## Interceptor Ordering

Interceptors execute in order — first registered is outermost:

```go
connect.WithInterceptors(
    middleware.Recovery(),   // 1. Catches panics (outermost)
    middleware.Logging(),    // 2. Logs requests
    middleware.Auth(),       // 3. Authenticates
    middleware.Metrics(),    // 4. Records metrics (innermost)
)
```

Execution flow:
```
Request → Recovery → Logging → Auth → Metrics → Handler → Metrics → Auth → Logging → Recovery → Response
```

## Generated Middleware for Internal Packages

For internal packages created with `forge package new`, middleware wrappers are automatically generated in `middleware_gen.go`. These wrap the Go interface contract with logging, tracing, and metrics — no manual interceptor setup needed for internal boundaries.

```go
// internal/email/middleware_gen.go — GENERATED
type loggingMiddleware struct {
    next  EmailService  // The contract interface
    logger *slog.Logger
}

func (m *loggingMiddleware) Send(ctx context.Context, to, subject, body string) error {
    m.logger.Info("Send called", "to", to, "subject", subject)
    err := m.next.Send(ctx, to, subject, body)
    if err != nil {
        m.logger.Error("Send failed", "error", err)
    }
    return err
}
```

## Best Practices

1. **Recovery first** - Always put panic recovery as the outermost interceptor
2. **Authentication early** - Authenticate before expensive operations
3. **Metrics last** - Measure after all processing
4. **Thread safety** - Use mutexes for shared state in interceptors
5. **Context values** - Use typed keys for context values
6. **Error handling** - Return Connect error codes (`connect.CodeXxx`)
7. **Performance** - Keep interceptors lightweight

## Testing Middleware

```go
func TestAuthInterceptor(t *testing.T) {
    interceptor := middleware.Auth()

    // Create a test handler
    handler := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
        userID, ok := middleware.GetUserID(ctx)
        if !ok {
            t.Fatal("expected user ID in context")
        }
        t.Logf("user ID: %s", userID)
        return nil, nil
    }

    // Wrap with interceptor
    wrapped := interceptor(handler)

    // Test with valid token
    req := connect.NewRequest(&usersv1.GetUserRequest{})
    req.Header().Set("Authorization", "valid-token")
    _, err := wrapped(context.Background(), req)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }

    // Test without token
    reqNoAuth := connect.NewRequest(&usersv1.GetUserRequest{})
    _, err = wrapped(context.Background(), reqNoAuth)
    if err == nil {
        t.Fatal("expected error for missing token")
    }
    if connect.CodeOf(err) != connect.CodeUnauthenticated {
        t.Fatalf("expected Unauthenticated, got %v", connect.CodeOf(err))
    }
}
```

## See Also

- [Middleware Architecture]({{< ref "../architecture/middleware" >}})
- [Service Patterns]({{< ref "service-patterns" >}})
- [Testing Strategies]({{< ref "testing-strategies" >}})
