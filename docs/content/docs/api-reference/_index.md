---
title: "API Reference"
description: "Complete API reference for Forge"
weight: 80
---

# API Reference

API reference for key Forge patterns and interfaces.

## Service Interface

Every service handler package follows this pattern:

```go
// Deps struct holds all dependencies
type Deps struct {
    DB     *pgxpool.Pool
    Logger *slog.Logger
    // Add service-specific dependencies
}

// Service implements the proto Connect RPC handler interface
type Service struct {
    deps Deps
}

// New constructs the service with injected dependencies
func New(deps Deps) *Service {
    return &Service{deps: deps}
}

// Name returns the service name for registration
func (s *Service) Name() string {
    return "MyService"
}

// Register mounts Connect RPC handlers onto the HTTP mux
func (s *Service) Register(mux *http.ServeMux, opts ...connect.HandlerOption) {
    path, handler := myservicev1connect.NewMyServiceHandler(s, opts...)
    mux.Handle(path, handler)
}
```

## Internal Package Interface

Internal packages define contracts as Go interfaces:

```go
// internal/<name>/contract.go
package mypackage

type Contract interface {
    Method(ctx context.Context, args ...any) (Result, error)
}
```

Generated files:
- `mock_gen.go` — mock implementation for testing
- `middleware_gen.go` — logging/tracing wrapper

## Wiring (pkg/app/)

```go
// pkg/app/wire.go — GENERATED
package app

// BuildDeps constructs all infrastructure and internal packages
func BuildDeps(cfg *configv1.Config) (*Deps, error)

// BuildServices constructs all service handlers
func BuildServices(deps *Deps) []Service

// pkg/app/testing.go — GENERATED
// NewTestXxxService creates a service with mock dependencies
func NewTestXxxService(t *testing.T) (*xxxservice.Service, *TestMocks)

// NewTestXxxServiceServer starts an HTTP test server
func NewTestXxxServiceServer(t *testing.T) (xxxv1connect.XxxServiceClient, func())
```

## Connect RPC Patterns

### Handler Signature

```go
func (s *Service) MethodName(
    ctx context.Context,
    req *connect.Request[myservicev1.MethodNameRequest],
) (*connect.Response[myservicev1.MethodNameResponse], error)
```

### Error Responses

```go
import "connectrpc.com/connect"

return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("resource not found"))
return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("database error"))
```

### Interceptors

```go
// Connect interceptor function
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

## Proto Annotations

### Entity Options (deprecated, ORM-owned)

```protobuf
import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";

message User {
  option (forge.options.v1.entity_options) = {
    table_name: "users"
    timestamps: true
    soft_delete: true
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    not_null: true
  }];
}
```

## HTTP Endpoints

Connect RPC services are accessible via HTTP:

```bash
# Connect protocol (default)
POST /services.users.v1.UsersService/GetUser
Content-Type: application/json

{"id": "123"}
```

## Environment Variables

Environment variables are driven by the config proto. Common patterns:

```bash
DATABASE_URL=postgres://user:pass@localhost/db
PORT=8080
LOG_LEVEL=info
```

## See Also

- [Core Concepts]({{< ref "../core-concepts" >}})
- [Architecture]({{< ref "../architecture" >}})
- [Guides]({{< ref "../guides" >}})
