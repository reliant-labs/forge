# Example: Full Monorepo

This walkthrough builds a multi-service application with two Go services, a Next.js frontend, database entities, service-to-service communication, and multi-environment Kubernetes deployment. It demonstrates how Forge's Full mode manages a realistic production setup.

## What You'll Build

A task management application with:
- **`api` service** -- Public-facing API that handles authentication and request routing
- **`tasks` service** -- Backend service managing task CRUD with a Postgres database
- **`web` frontend** -- Next.js frontend that calls the `api` service via Connect RPC
- Database entities with the migration-first ORM
- CI/CD pipeline with staging auto-deploy and production manual approval
- KCL-based multi-environment Kubernetes deployment

## Creating the Project

```bash
forge new taskflow \
  --mod github.com/example/taskflow \
  --service api \
  --frontend web

cd taskflow
```

## Adding the Tasks Service

```bash
forge add service tasks --port 8081
```

The project now has two services in `forge.yaml`:

```yaml
services:
  - name: api
    type: GO_SERVICE
    path: services/api
    port: 8080
  - name: tasks
    type: GO_SERVICE
    path: services/tasks
    port: 8081
frontends:
  - name: web
    type: NEXTJS
    path: frontends/web
    port: 3000
```

## Project Structure

```
taskflow/
├── forge.yaml
├── cmd/                                  # Single binary (Cobra CLI)
│   ├── main.go
│   ├── server.go                         # server [services...] command
│   └── version.go
├── proto/
│   ├── config/v1/                        # Config proto
│   ├── services/
│   │   ├── api/v1/api.proto              # API service RPCs
│   │   └── tasks/v1/tasks.proto          # Tasks service RPCs + entity messages
├── gen/                                   # Generated code
│   ├── services/api/v1/apiv1connect/
│   └── services/tasks/v1/tasksv1connect/
├── services/
│   ├── api/
│   │   ├── service.go                    # New(deps), Register(mux, opts)
│   │   └── handlers.go
│   ├── tasks/
│   │   ├── service.go
│   │   └── handlers.go
│   └── mocks/
├── internal/
│   └── db/
│       ├── types.go                      # Type aliases: type Task = tasksv1.Task
│       └── task_orm.go                   # CRUD functions (Create, Get, List, Update, Delete)
├── pkg/
│   └── app/
│       ├── wire.go                       # GENERATED — dependency wiring
│       ├── wire_test.go
│       └── testing.go                    # GENERATED — test helpers
├── frontends/web/
│   ├── src/gen/                          # TypeScript Connect clients
│   ├── package.json
│   └── ...
├── db/
│   ├── queries/                          # sqlc queries
│   └── migrations/
├── deploy/
│   ├── Dockerfile                        # Single multi-stage Dockerfile
│   ├── kcl/
│   │   ├── schema.k
│   │   ├── render.k
│   │   ├── base.k
│   │   ├── dev/main.k
│   │   ├── staging/main.k
│   │   └── prod/main.k
│   ├── k3d.yaml
│   └── docker-compose.yml
├── .github/workflows/
│   ├── ci.yml
│   ├── build-images.yml
│   └── deploy.yml
├── go.work
├── go.mod
├── buf.yaml
└── Taskfile.yml
```

## Defining Proto Contracts

### Tasks Service RPCs

```protobuf
// proto/services/tasks/v1/tasks.proto
syntax = "proto3";

package services.tasks.v1;

option go_package = "github.com/example/taskflow/gen/services/tasks/v1;tasksv1";

import "google/protobuf/timestamp.proto";

service TasksService {
  rpc CreateTask(CreateTaskRequest) returns (CreateTaskResponse);
  rpc GetTask(GetTaskRequest) returns (GetTaskResponse);
  rpc ListTasks(ListTasksRequest) returns (ListTasksResponse);
  rpc UpdateTask(UpdateTaskRequest) returns (UpdateTaskResponse);
  rpc DeleteTask(DeleteTaskRequest) returns (DeleteTaskResponse);
}

// ... message definitions (same as before)
```

### Entity Messages

Entity messages are defined in the service proto alongside RPCs:

```protobuf
// proto/services/tasks/v1/tasks.proto (entity message section)
message Task {
  string id = 1;
  string title = 2;
  string description = 3;
  string status = 4;
  string assignee_id = 5;
  string org_id = 6;
  google.protobuf.Timestamp created_at = 7;
  google.protobuf.Timestamp updated_at = 8;
  google.protobuf.Timestamp deleted_at = 9;
}
```

The type alias and ORM functions are scaffolded in `internal/db/`:

```go
// internal/db/types.go
type Task = tasksv1.Task
```

```go
// internal/db/task_orm.go
func CreateTask(ctx context.Context, db orm.DBTX, task *Task) (*Task, error) { ... }
func GetTaskByID(ctx context.Context, db orm.DBTX, id string) (*Task, error) { ... }
func ListTasks(ctx context.Context, db orm.DBTX, opts ...ListOption) ([]*Task, error) { ... }
```

The initial SQL migration is scaffolded in `db/migrations/00001_init.up.sql`.

## Generating All Code

```bash
forge generate
```

This generates:
- `gen/services/api/v1/` and `gen/services/tasks/v1/` -- Go + Connect stubs
- `frontends/web/src/gen/` -- TypeScript Connect clients
- `pkg/app/wire.go` -- updated wiring code
- `services/mocks/` -- generated mocks

Note: `internal/db/` (type aliases and ORM functions) and `db/migrations/` are scaffolded at project creation time and are NOT regenerated by `forge generate`.

## Implementing the Tasks Service

```go
// services/tasks/service.go
package tasksservice

import (
    "net/http"

    "connectrpc.com/connect"
    "github.com/jackc/pgx/v5/pgxpool"
    tasksv1connect "github.com/example/taskflow/gen/services/tasks/v1/tasksv1connect"
)

type Deps struct {
    DB *pgxpool.Pool
}

type Service struct {
    deps Deps
}

func New(deps Deps) *Service {
    return &Service{deps: deps}
}

func (s *Service) Name() string {
    return "TasksService"
}

func (s *Service) Register(mux *http.ServeMux, opts ...connect.HandlerOption) {
    path, handler := tasksv1connect.NewTasksServiceHandler(s, opts...)
    mux.Handle(path, handler)
}
```

```go
// services/tasks/handlers.go
package tasksservice

import (
    "context"

    "connectrpc.com/connect"
    "github.com/example/taskflow/internal/db"
    tasksv1 "github.com/example/taskflow/gen/services/tasks/v1"
)

func (s *Service) CreateTask(
    ctx context.Context,
    req *connect.Request[tasksv1.CreateTaskRequest],
) (*connect.Response[tasksv1.CreateTaskResponse], error) {
    task := &db.Task{
        Title:       req.Msg.Title,
        Description: req.Msg.Description,
        AssigneeId:  req.Msg.AssigneeId,
    }

    created, err := db.CreateTask(ctx, s.deps.DB, task)
    if err != nil {
        return nil, connect.NewError(connect.CodeInternal, err)
    }

    return connect.NewResponse(&tasksv1.CreateTaskResponse{
        Task: created,
    }), nil
}
```

## Service-to-Service Communication

The API service calls the tasks service through the generated Connect client:

```go
// services/api/service.go
package apiservice

type Deps struct {
    TasksClient tasksv1connect.TasksServiceClient
}

type Service struct {
    deps Deps
}

func New(deps Deps) *Service {
    return &Service{deps: deps}
}
```

```go
// services/api/handlers.go
func (s *Service) CreateTask(
    ctx context.Context,
    req *connect.Request[tasksv1.CreateTaskRequest],
) (*connect.Response[tasksv1.CreateTaskResponse], error) {
    return s.deps.TasksClient.CreateTask(ctx, req)
}
```

The wiring in `pkg/app/wire.go` connects the API service to the tasks service via a Connect client.

## Testing

Use the generated test harness:

```go
func TestCreateTask(t *testing.T) {
    svc, _ := app.NewTestTasksService(t)

    resp, err := svc.CreateTask(ctx, connect.NewRequest(&tasksv1.CreateTaskRequest{
        Title:      "Test Task",
        AssigneeId: "user-1",
    }))
    require.NoError(t, err)
    assert.Equal(t, "Test Task", resp.Msg.Task.Title)
}
```

## Frontend (Next.js)

```typescript
// frontends/web/src/lib/api.ts
import { createConnectTransport } from "@connectrpc/connect-web";
import { createClient } from "@connectrpc/connect";
import { ApiService } from "../gen/services/api/v1/api_connect";

const transport = createConnectTransport({
  baseUrl: process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080",
});

export const apiClient = createClient(ApiService, transport);
```

## Running Locally

```bash
forge run
```

This starts docker-compose infrastructure, the single Go binary with Air hot reload, and the Next.js frontend.

## Deploying

```bash
forge deploy dev                                 # Local k3d
forge deploy staging --image-tag sha-abc1234     # Staging
forge deploy prod --image-tag v1.0.0             # Production
```

## CI/CD Pipeline

The generated workflows handle the full lifecycle:

### On Every PR
`ci.yml` runs lint, test, build, Docker build, verify generated code, and vulnerability scanning.

### On Merge to Main
`build-images.yml` builds and pushes the Docker image with `sha-<short>` tag, then Trivy scans it.

### Staging Auto-Deploy
`deploy.yml` deploys to staging automatically after successful image build on main.

### Production Release
Tag a release to trigger production deployment:

```bash
git tag v1.0.0
git push origin v1.0.0
```

The existing image gets retagged (no rebuild) and deployed to production with manual approval.