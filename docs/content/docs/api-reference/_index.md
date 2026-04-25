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

### Entity Messages

Entity messages are defined in the service proto alongside RPCs. They serve as both the API contract and (via type alias) the database type:

```protobuf
// proto/services/users/v1/users.proto
message User {
  string id = 1;
  string email = 2;
  string name = 3;
  string org_id = 4;
  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
}
```

Type aliases in `internal/db/types.go` re-export the proto type: `type User = apiv1.User`. CRUD functions live in `internal/db/user_orm.go`. SQL migrations in `db/migrations/` are the schema source of truth.

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