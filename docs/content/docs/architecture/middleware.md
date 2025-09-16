---
title: "Middleware System"
description: "Connect RPC interceptors for cross-cutting concerns"
weight: 21
icon: "hub"
---

# Middleware System

Forge uses Connect RPC interceptors for cross-cutting concerns. Interceptors are applied to the shared HTTP mux in the generated `cmd/server.go`, so all services automatically get logging, recovery, and any custom middleware.

## Overview

Connect RPC provides a unified interceptor model that works for both unary and streaming RPCs:

```go
type Interceptor interface {
    WrapUnary(next connect.UnaryFunc) connect.UnaryFunc
    WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc
    WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc
}
```

## Built-in Interceptors

### Recovery Interceptor

Catches panics and converts them to Connect errors:

```go
func RecoveryInterceptor() connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            defer func() {
                if r := recover(); r != nil {
                    log.Printf("Panic recovered: %v", r)
                }
            }()
            return next(ctx, req)
        }
    }
}
```

### Logging Interceptor

Logs all method calls with timing:

```go
func LoggingInterceptor(logger *slog.Logger) connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            start := time.Now()
            logger.Info("request", "procedure", req.Spec().Procedure)

            resp, err := next(ctx, req)

            logger.Info("response",
                "procedure", req.Spec().Procedure,
                "duration", time.Since(start),
                "error", err,
            )
            return resp, err
        }
    }
}
```

## Applying Interceptors

Interceptors are applied in the generated `cmd/server.go`:

```go
// cmd/server.go — GENERATED
interceptors := connect.WithInterceptors(
    middleware.RecoveryInterceptor(),
    middleware.LoggingInterceptor(logger),
)

mux := http.NewServeMux()
for _, svc := range services {
    svc.Register(mux, interceptors)
}
```

All services share the same interceptor chain.

## Creating Custom Interceptors

### Authentication

```go
func AuthInterceptor(authService auth.Contract) connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            token := req.Header().Get("Authorization")

            user, err := authService.ValidateToken(ctx, token)
            if err != nil {
                return nil, connect.NewError(
                    connect.CodeUnauthenticated,
                    fmt.Errorf("invalid token: %w", err),
                )
            }

            ctx = context.WithValue(ctx, "user", user)
            return next(ctx, req)
        }
    }
}
```

### Rate Limiting

```go
func RateLimitInterceptor(limiter *RateLimiter) connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            if !limiter.Allow(req.Spec().Procedure) {
                return nil, connect.NewError(
                    connect.CodeResourceExhausted,
                    fmt.Errorf("rate limit exceeded"),
                )
            }
            return next(ctx, req)
        }
    }
}
```

### Tracing

```go
func TracingInterceptor(tracer *Tracer) connect.UnaryInterceptorFunc {
    return func(next connect.UnaryFunc) connect.UnaryFunc {
        return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
            span := tracer.StartSpan(ctx, req.Spec().Procedure)
            ctx = tracer.ContextWithSpan(ctx, span)
            defer span.End()

            resp, err := next(ctx, req)
            if err != nil {
                span.RecordError(err)
            }
            return resp, err
        }
    }
}
```

## Interceptor Order

Interceptors execute in order for requests and reverse order for responses:

```
Request → Recovery → Logging → Auth → Handler → Response
```

Always put Recovery first (outermost) so it catches panics from any other interceptor.

## Internal Package Middleware

Internal packages with `contract.go` get generated middleware wrappers (`middleware_gen.go`) that add logging and tracing at the Go interface boundary. These are applied automatically by the wiring code.

## See Also

- [Creating Services]({{< ref "../guides/creating-services" >}})
- [Explicit Wiring]({{< ref "registry" >}})
