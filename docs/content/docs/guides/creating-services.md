---
title: "Creating Services"
description: "Step-by-step guide to creating a new service in Forge"
weight: 31
icon: "add_circle"
---

# Creating Services

This guide walks you through creating a new service from scratch in Forge.

## Overview

Creating a service in Forge follows these steps:

1. Add the service with `forge add service`
2. Define the service contract in proto
3. Generate code with `forge generate`
4. Implement the business logic
5. Test the service

## Step 1: Add the Service

```bash
forge add service todo --port 8081
```

This creates:
- `services/todo/service.go` — scaffold with `New(deps)`, `Register(mux, opts)`, `Name()`
- `services/todo/handlers.go` — placeholder for business logic
- `proto/services/todo/v1/todo.proto` — empty proto service stub
- Updates `pkg/app/wire.go` with construction and registration logic
- Updates `forge.project.yaml`

## Step 2: Define Proto Contract

Edit the generated proto file:

```protobuf
// proto/services/todo/v1/todo.proto
syntax = "proto3";

package services.todo.v1;

option go_package = "github.com/yourcompany/myapp/gen/services/todo/v1;todov1";

service TodoService {
  rpc CreateTodo(CreateTodoRequest) returns (Todo);
  rpc GetTodo(GetTodoRequest) returns (Todo);
  rpc ListTodos(ListTodosRequest) returns (ListTodosResponse);
  rpc UpdateTodo(UpdateTodoRequest) returns (Todo);
  rpc DeleteTodo(DeleteTodoRequest) returns (DeleteTodoResponse);
}

message Todo {
  string id = 1;
  string title = 2;
  string description = 3;
  bool completed = 4;
  int64 created_at = 5;
  int64 updated_at = 6;
}

message CreateTodoRequest {
  string title = 1;
  string description = 2;
}

message GetTodoRequest {
  string id = 1;
}

message ListTodosRequest {
  int32 page_size = 1;
  string page_token = 2;
  bool completed_only = 3;
}

message ListTodosResponse {
  repeated Todo todos = 1;
  string next_page_token = 2;
  int32 total_count = 3;
}

message UpdateTodoRequest {
  string id = 1;
  string title = 2;
  string description = 3;
  bool completed = 4;
}

message DeleteTodoRequest {
  string id = 1;
}

message DeleteTodoResponse {
  bool success = 1;
}
```

## Step 3: Generate Code

```bash
forge generate
```

This creates:

```
gen/
└── services/todo/
    └── v1/
        ├── todo.pb.go              # Proto messages
        └── todov1connect/
            └── todo.connect.go     # Connect RPC stubs

services/
├── todo/
│   ├── service.go                  # Updated with handler interface
│   └── handlers.go                 # Placeholder handlers
└── mocks/
    └── todo_service_mock.go        # Generated mock
```

It also updates `pkg/app/wire.go` with the construction and registration logic.

## Step 4: Implement Business Logic

Edit `services/todo/service.go` to define the `Deps` struct and constructor:

```go
// services/todo/service.go
package todoservice

import (
    "net/http"

    "connectrpc.com/connect"
    todov1connect "github.com/yourcompany/myapp/gen/services/todo/v1/todov1connect"
)

type Deps struct {
    // Add dependencies here as needed
}

type Service struct {
    deps  Deps
    todos map[string]*todov1.Todo // In-memory for this example
}

func New(deps Deps) *Service {
    return &Service{
        deps:  deps,
        todos: make(map[string]*todov1.Todo),
    }
}

func (s *Service) Name() string {
    return "TodoService"
}

func (s *Service) Register(mux *http.ServeMux, opts ...connect.HandlerOption) {
    path, handler := todov1connect.NewTodoServiceHandler(s, opts...)
    mux.Handle(path, handler)
}
```

Then implement the handlers in `services/todo/handlers.go`:

```go
// services/todo/handlers.go
package todoservice

import (
    "context"
    "fmt"
    "sync"
    "time"

    "connectrpc.com/connect"
    "github.com/google/uuid"
    todov1 "github.com/yourcompany/myapp/gen/services/todo/v1"
)

var mu sync.RWMutex

func (s *Service) CreateTodo(
    ctx context.Context,
    req *connect.Request[todov1.CreateTodoRequest],
) (*connect.Response[todov1.Todo], error) {
    if req.Msg.Title == "" {
        return nil, connect.NewError(
            connect.CodeInvalidArgument,
            fmt.Errorf("title is required"),
        )
    }

    now := time.Now().Unix()
    todo := &todov1.Todo{
        Id:          uuid.New().String(),
        Title:       req.Msg.Title,
        Description: req.Msg.Description,
        Completed:   false,
        CreatedAt:   now,
        UpdatedAt:   now,
    }

    mu.Lock()
    s.todos[todo.Id] = todo
    mu.Unlock()

    return connect.NewResponse(todo), nil
}

func (s *Service) GetTodo(
    ctx context.Context,
    req *connect.Request[todov1.GetTodoRequest],
) (*connect.Response[todov1.Todo], error) {
    mu.RLock()
    todo, exists := s.todos[req.Msg.Id]
    mu.RUnlock()

    if !exists {
        return nil, connect.NewError(
            connect.CodeNotFound,
            fmt.Errorf("todo %s not found", req.Msg.Id),
        )
    }

    return connect.NewResponse(todo), nil
}

// ... implement remaining handlers
```

## Step 5: Test the Service

Use the generated test harness:

```go
// services/todo/handlers_test.go
package todoservice_test

import (
    "context"
    "testing"

    "connectrpc.com/connect"
    todov1 "github.com/yourcompany/myapp/gen/services/todo/v1"
    "github.com/yourcompany/myapp/pkg/app"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestCreateTodo(t *testing.T) {
    svc, _ := app.NewTestTodoService(t)
    ctx := context.Background()

    resp, err := svc.CreateTodo(ctx, connect.NewRequest(&todov1.CreateTodoRequest{
        Title:       "Buy groceries",
        Description: "Milk, eggs, bread",
    }))
    require.NoError(t, err)
    assert.NotEmpty(t, resp.Msg.Id)
    assert.Equal(t, "Buy groceries", resp.Msg.Title)
    assert.False(t, resp.Msg.Completed)
}

func TestGetTodo_NotFound(t *testing.T) {
    svc, _ := app.NewTestTodoService(t)
    ctx := context.Background()

    _, err := svc.GetTodo(ctx, connect.NewRequest(&todov1.GetTodoRequest{
        Id: "nonexistent",
    }))

    assert.Error(t, err)
    assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}
```

Run tests:

```bash
go test ./services/todo/...
```

### Manual Testing

Start the server:

```bash
forge run
```

Test with curl:

```bash
# Create a todo
curl http://localhost:8081/services.todo.v1.TodoService/CreateTodo \
  -H "Content-Type: application/json" \
  -d '{"title": "Buy groceries", "description": "Milk, eggs, bread"}'

# Get the todo (use the ID from above)
curl http://localhost:8081/services.todo.v1.TodoService/GetTodo \
  -H "Content-Type: application/json" \
  -d '{"id": "123e4567-e89b-12d3-a456-426614174000"}'

# List all todos
curl http://localhost:8081/services.todo.v1.TodoService/ListTodos \
  -H "Content-Type: application/json" \
  -d '{}'
```

## Adding Dependencies

If your service needs to call other services or use internal packages, add them to the `Deps` struct:

```go
type Deps struct {
    DB    *pgxpool.Pool
    Email email.Contract  // Go interface from internal/email/contract.go
}

func (s *Service) CreateTodo(ctx context.Context, req *connect.Request[todov1.CreateTodoRequest]) (*connect.Response[todov1.Todo], error) {
    // Use dependencies
    todo, err := db.CreateTodo(ctx, s.deps.DB, &db.Todo{
        Title: req.Msg.Title,
    })
    if err != nil {
        return nil, connect.NewError(connect.CodeInternal, err)
    }

    // Notify via email
    s.deps.Email.Send(ctx, "admin@example.com", "New todo", todo.Title)

    return connect.NewResponse(toProto(todo)), nil
}
```

The wiring in `pkg/app/wire.go` is updated by `forge generate` to pass the dependencies.

## Next Steps

- **[Service Communication]({{< relref "service-communication" >}})** — Learn how services call each other
- **[Testing Strategies]({{< relref "testing-strategies" >}})** — Advanced testing techniques
- **[Database Integration]({{< relref "database-integration" >}})** — Connect to a real database
