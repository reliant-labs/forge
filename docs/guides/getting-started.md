# Getting Started with Forge

This guide walks through creating a project, adding services and frontends, running locally, and deploying to Kubernetes. By the end, you'll have a working Go + Next.js application running in a local k3d cluster.

## Prerequisites

Install these tools before using Forge:

| Tool | Version | Purpose |
|------|---------|---------|
| [Go](https://go.dev/dl/) | 1.23+ | Service runtime and build toolchain |
| [buf](https://buf.build/docs/installation) | latest | Proto compilation and linting |
| [Node.js](https://nodejs.org/) | 22+ | Next.js frontend development (only needed for Full mode with frontends) |
| [Docker](https://docs.docker.com/get-docker/) | latest | Container builds and local infrastructure |
| [k3d](https://k3d.io/) | latest | Local Kubernetes cluster (only needed for `forge deploy dev`) |
| [KCL](https://kcl-lang.io/docs/user_docs/getting-started/install) | latest | Kubernetes manifest generation (only needed for deploy) |
| [Air](https://github.com/air-verse/air) | latest | Go hot reload during development |

Optional but recommended:
- [Task](https://taskfile.dev/) -- task runner used by generated `Taskfile.yml`
- [golangci-lint](https://golangci-lint.run/) -- Go linting via `forge lint`
- [sqlc](https://sqlc.dev/) -- SQL query code generation

## Installing Forge

```bash
go install github.com/reliant-labs/forge/cmd/forge@latest
```

Verify the installation:

```bash
forge --version
```

## Creating a New Project

The `forge new` command generates a complete project structure. You need to provide a name and a Go module path:

```bash
forge new myproject --mod github.com/example/myproject
```

This creates a Full mode project with a single Go service named `api` on port 8080.

### Including a Next.js Frontend

Use the `--frontend` flag to scaffold a Next.js frontend alongside your Go services:

```bash
forge new myproject --mod github.com/example/myproject --frontend web
```

### What Gets Created

```
myproject/
├── cmd/                         # Single binary (Cobra CLI)
│   ├── main.go                  # Root command + Execute()
│   ├── server.go                # server [services...] command
│   └── version.go               # Build info
├── proto/
│   ├── config/v1/               # Config proto — instantiation contract
│   └── services/
│       └── api/v1/              # Initial service proto
│           └── api.proto
├── services/
│   └── api/                     # Go service implementation
│       ├── service.go           # New(deps), Register(mux, opts), Name()
│       └── handlers.go          # RPC implementations (you write this)
├── pkg/
│   └── app/
│       ├── wire.go              # GENERATED — dependency wiring
│       └── wire_test.go         # GENERATED — test wiring
├── gen/                         # Generated code (separate Go module)
│   └── go.mod
├── deploy/
│   ├── Dockerfile               # Single multi-stage Dockerfile
│   ├── kcl/                     # KCL manifests per environment
│   │   ├── schema.k
│   │   ├── render.k
│   │   ├── base.k
│   │   ├── dev/main.k
│   │   ├── staging/main.k
│   │   └── prod/main.k
│   ├── docker-compose.yml       # Local infrastructure
│   └── k3d.yaml                 # Local cluster config
├── .github/workflows/           # CI/CD pipelines
│   ├── ci.yml
│   ├── build-images.yml
│   └── deploy.yml
├── forge.project.yaml        # Project configuration (source of truth)
├── buf.yaml
├── go.work                      # Go workspace (root + gen/)
├── go.mod
├── Taskfile.yml
└── .gitignore
```

The `forge.project.yaml` file is the project configuration. Every Forge command reads this file to discover services, frontends, environments, and settings.

## Adding a Service

With the project created, add more services using `forge add service`:

```bash
forge add service users --port 8081
```

This creates the proto stub and Go service scaffold, and updates the generated wiring in `pkg/app/wire.go` and `forge.project.yaml`.

The generated proto file is intentionally empty -- you define the RPCs:

```protobuf
// proto/services/users/v1/users.proto
syntax = "proto3";

package services.users.v1;

option go_package = "github.com/myorg/myproject/gen/services/users/v1;usersv1";

service UsersService {
  rpc GetUser(GetUserRequest) returns (User);
  rpc CreateUser(CreateUserRequest) returns (User);
}

message User {
  string id = 1;
  string name = 2;
  string email = 3;
}

message GetUserRequest {
  string id = 1;
}

message CreateUserRequest {
  string name = 1;
  string email = 2;
}
```

You can also add a Next.js frontend:

```bash
forge add frontend dashboard
```

## Creating Internal Packages

For internal boundaries that don't need to be exposed via proto, create internal packages:

```bash
forge package new email
```

This creates:
- `internal/email/contract.go` -- Go interface (the contract)
- `internal/email/service.go` -- implementation scaffold
- `internal/email/mock_gen.go` -- generated mock
- `internal/email/middleware_gen.go` -- generated logging/tracing wrapper

The contract is a Go interface, which can express things proto cannot:

```go
// internal/email/contract.go
package email

type Contract interface {
    Send(ctx context.Context, to, subject, body string) error
    SendBatch(ctx context.Context, messages []Message) <-chan Result
}
```

## Generating Code

After editing proto files or contract interfaces, run the code generator:

```bash
forge generate
```

This runs a multi-step pipeline:

1. **`buf generate`** for Go protobuf + Connect RPC stubs (into `gen/`)
2. **`buf generate`** for TypeScript stubs in any Next.js frontends
3. **Service stub generation** -- creates `service.go` files for new services (non-destructive; won't overwrite existing files)
4. **Mock generation** -- creates mock implementations in `services/mocks/` and `internal/*/mock_gen.go`
5. **Middleware wrapper generation** -- creates logging/tracing wrappers for internal packages
6. **Wiring code** -- updates `pkg/app/wire.go` with construction logic
7. **`sqlc generate`** if `sqlc.yaml` exists
8. **`go mod tidy`** in `gen/`

Watch mode regenerates automatically when proto files change:

```bash
forge generate --watch
```

## Implementing a Service

The generated service stub at `services/users/service.go` has the basic structure. Implement the Connect RPC handler methods:

```go
package usersservice

import (
    "context"

    "connectrpc.com/connect"
    usersv1 "github.com/myorg/myproject/gen/services/users/v1"
)

func (s *Service) GetUser(
    ctx context.Context,
    req *connect.Request[usersv1.GetUserRequest],
) (*connect.Response[usersv1.User], error) {
    // Your business logic here
    return connect.NewResponse(&usersv1.User{
        Id:    req.Msg.Id,
        Name:  "Alice",
        Email: "alice@example.com",
    }), nil
}
```

Services are wired up explicitly in `pkg/app/wire.go` -- there is no manual registration step. Running `forge generate` updates the wiring code automatically.

## Running Locally

```bash
forge run
```

This starts everything needed for local development:

1. **Infrastructure** -- runs `docker compose up -d` from `deploy/docker-compose.yml` (databases, Redis, etc.)
2. **Go binary** -- starts the single binary via [Air](https://github.com/air-verse/air) for hot reload
3. **Next.js frontends** -- runs `npm run dev` for each frontend

Each process gets color-coded log output with a name prefix. Press Ctrl+C to stop everything -- Forge sends SIGTERM to all child processes and tears down the docker-compose infrastructure.

Useful flags:

```bash
forge run --no-infra          # Skip docker-compose (e.g., if DB is already running)
forge run --env staging       # Use staging environment config
```

## Testing

```bash
forge test                    # Unit + integration tests
forge test unit               # Unit tests only
forge test integration        # Integration tests (build tag: integration)
forge test e2e                # End-to-end tests from e2e/
forge test --coverage         # Generate coverage reports
```

The generated test harness in `pkg/app/testing.go` makes it easy to test services with mock dependencies:

```go
func TestCreateUser(t *testing.T) {
    svc, mocks := app.NewTestUsersService(t)

    resp, err := svc.CreateUser(ctx, connect.NewRequest(&usersv1.CreateUserRequest{
        Name:  "Alice",
        Email: "alice@example.com",
    }))
    require.NoError(t, err)
    assert.NotEmpty(t, resp.Msg.Id)
}
```

Test with curl against a running service:

```bash
curl http://localhost:8080/services.users.v1.UsersService/GetUser \
  -H "Content-Type: application/json" \
  -d '{"id": "123"}'
```

## Deploying

Deploy to a Kubernetes environment:

```bash
forge deploy dev
```

For `dev`, this automatically:
1. Ensures a k3d cluster exists (creates one with a local registry at `localhost:5050` if not)
2. Builds the Docker image
3. Pushes the image to the local registry
4. Generates Kubernetes manifests from KCL
5. Applies manifests with `kubectl apply --server-side`
6. Waits for all deployments to roll out

For staging and production:

```bash
forge deploy staging --image-tag sha-abc1234
forge deploy prod --image-tag v1.2.0
```

Preview what would be applied without making changes:

```bash
forge deploy prod --dry-run --image-tag v1.2.0
```

## What's Next

- [Architecture](architecture.md) -- Understand the single binary design, two contract systems, and explicit wiring
- [Database & ORM](database.md) -- Define entities and run migrations
- [KCL Deployments](kcl.md) -- Customize Kubernetes configurations for each environment
- [CI/CD Pipelines](cicd.md) -- Understand and customize the generated GitHub Actions workflows
- [CLI Reference](cli-reference.md) -- Full documentation for every command and flag