---
title: "Service Communication"
description: "How services communicate in Forge applications"
weight: 20
---

# Service Communication

Forge services communicate via Connect RPC for external calls and through constructor injection for same-process dependencies.

## External Communication (Connect RPC)

External clients and cross-service calls use Connect RPC:

```go
// Connect RPC client (HTTP/JSON compatible)
client := usersv1connect.NewUsersServiceClient(
    http.DefaultClient,
    "http://localhost:8080",
)

resp, err := client.GetUser(ctx, connect.NewRequest(&usersv1.GetUserRequest{
    Id: "123",
}))
```

Connect RPC supports:
- JSON and Protobuf encoding
- HTTP/1.1 and HTTP/2
- gRPC wire compatibility
- Browser-friendly (no proxy needed)

## Service Dependencies

Services declare dependencies in their `Deps` struct and receive them via constructor injection:

```go
type Deps struct {
    DB            *pgxpool.Pool
    Logger        *slog.Logger
    UsersClient   usersv1connect.UsersServiceClient
    Email         email.Contract  // Go interface from internal/email/contract.go
}

type Service struct {
    deps Deps
}

func New(deps Deps) *Service {
    return &Service{deps: deps}
}
```

The generated `pkg/app/wire.go` handles construction and wiring:

```go
// pkg/app/wire.go — GENERATED
func BuildServices(deps *Deps) []Service {
    ordersSvc := ordersservice.New(ordersservice.Deps{
        DB:          deps.DB,
        Logger:      deps.Logger,
        UsersClient: usersv1connect.NewUsersServiceClient(http.DefaultClient, usersURL),
        Email:       deps.Email,
    })
    return []Service{ordersSvc}
}
```

## Service-to-Service Calls

When one service needs to call another, it uses the generated Connect client through its `Deps`:

```go
func (s *Service) CreateOrder(
    ctx context.Context,
    req *connect.Request[ordersv1.CreateOrderRequest],
) (*connect.Response[ordersv1.CreateOrderResponse], error) {
    // Call users service via Connect RPC client
    userResp, err := s.deps.UsersClient.GetUser(ctx, connect.NewRequest(&usersv1.GetUserRequest{
        Id: req.Msg.UserId,
    }))
    if err != nil {
        return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("user not found"))
    }

    // Use internal package via Go interface
    err = s.deps.Email.Send(ctx, userResp.Msg.Email, "Order Created", "Your order has been placed")
    if err != nil {
        s.deps.Logger.Error("failed to send email", "error", err)
    }

    // Process order...
    return connect.NewResponse(&ordersv1.CreateOrderResponse{
        OrderId: order.Id,
    }), nil
}
```

## Request Context

Always propagate context for:
- Cancellation
- Deadlines
- Tracing
- Authentication

```go
func (s *Service) ProcessOrder(
    ctx context.Context,
    req *connect.Request[ordersv1.ProcessOrderRequest],
) (*connect.Response[ordersv1.ProcessOrderResponse], error) {
    // Context propagated to downstream calls
    user, err := s.deps.UsersClient.GetUser(ctx, connect.NewRequest(&usersv1.GetUserRequest{
        Id: req.Msg.UserId,
    }))
    if err != nil {
        return nil, err
    }

    // ...
}
```

## Error Handling

Use Connect error codes:

```go
import "connectrpc.com/connect"

func (s *Service) GetItem(
    ctx context.Context,
    req *connect.Request[itemsv1.GetItemRequest],
) (*connect.Response[itemsv1.GetItemResponse], error) {
    item, err := s.deps.DB.GetItem(ctx, req.Msg.Id)
    if err == sql.ErrNoRows {
        return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("item not found"))
    }
    if err != nil {
        return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("database error"))
    }

    return connect.NewResponse(&itemsv1.GetItemResponse{Item: item}), nil
}
```

## Timeouts and Deadlines

```go
// Set timeout on client side
ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
defer cancel()

response, err := client.SlowOperation(ctx, connect.NewRequest(request))
```

## Best Practices

1. **Always propagate context** — Don't create new contexts
2. **Use Connect error codes** — Not raw errors
3. **Set timeouts** — Prevent hanging calls
4. **Declare dependencies in Deps** — Keep them explicit
5. **Use Go interfaces for internal deps** — via `contract.go`
6. **Use Connect clients for cross-service** — via generated stubs

## See Also

- [Creating Services]({{< ref "creating-services" >}})
- [Testing Strategies]({{< ref "testing-strategies" >}})
