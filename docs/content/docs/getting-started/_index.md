---
title: "Getting Started"
description: "Install Forge, create your first project, and understand the basic workflow"
weight: 10
icon: "play_arrow"
---

# Getting Started with Forge

This guide walks you through installing the Forge CLI, creating a project, adding a service, and deploying it locally. By the end you will have a running Go service backed by proto definitions, with hot reload, a Docker image, and Kubernetes manifests.

## Prerequisites

Forge orchestrates several tools. Install these before you begin:

- **Go 1.23+** — [go.dev/dl](https://go.dev/dl/)
- **buf** — proto linter and code generator: `go install github.com/bufbuild/buf/cmd/buf@latest`
- **Docker** — for building container images
- **Task** (optional, recommended) — task runner: `go install github.com/go-task/task/v3/cmd/task@latest`

For local Kubernetes deployment you will also need:

- **k3d** — lightweight k3s in Docker: [k3d.io](https://k3d.io)
- **KCL** — configuration language for manifests: [kcl-lang.io](https://kcl-lang.io)
- **kubectl** — Kubernetes CLI

## Installation

```bash
go install github.com/reliant-labs/forge/cmd/forge@latest
```

Verify the install:

```bash
forge --version
```

## Creating a Project

`forge new` scaffolds an entire project: Go modules, proto directories, a single-binary entrypoint with Cobra CLI, Docker setup, KCL manifests, CI workflows, and a project configuration file.

```bash
forge new myproject --mod github.com/myorg/myproject
cd myproject
```

The `--mod` flag sets the Go module path. This is used in all generated import paths, proto `go_package` options, and buf configuration.

You can also create a project with an initial Next.js frontend:

```bash
forge new myproject --mod github.com/myorg/myproject --frontend dashboard
```

### What Gets Created

```
myproject/
├── cmd/                         # Single binary (Cobra CLI)
│   ├── main.go                  # Root command + Execute()
│   ├── server.go                # server [services...] — HTTP server
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
│       ├── wire_test.go         # GENERATED — test wiring
│       └── testing.go           # GENERATED — test helpers
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

This creates the proto stub and Go service scaffold, and updates the wiring in `pkg/app/wire.go` and `forge.project.yaml`.

The generated proto file is intentionally empty — you define the RPCs:

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

## Creating Internal Packages

For internal boundaries that don't need proto, create internal packages:

```bash
forge package new email
```

This creates `internal/email/` with a `contract.go` (Go interface), implementation scaffold, generated mock, and middleware wrapper.

## Adding a Frontend

```bash
forge add frontend dashboard
```

## Generating Code

After editing proto files, run the code generator:

```bash
forge generate
```

This runs a multi-step pipeline:

1. **`buf generate`** for Go protobuf + Connect RPC stubs (into `gen/`)
2. **`buf generate`** for TypeScript stubs in any Next.js frontends
3. **Service stub generation** — creates `service.go` files for new services (non-destructive; won't overwrite existing files)
4. **Mock/middleware generation** — creates mocks for API services and internal packages
5. **Wiring code update** — regenerates `pkg/app/wire.go`
6. **`sqlc generate`** if `sqlc.yaml` exists
7. **`go mod tidy`** in `gen/`

Watch mode regenerates automatically when proto files change:

```bash
forge generate --watch
```

## Implementing a Service

The generated service stub at `services/users/service.go` has the basic structure. You implement the Connect RPC handler methods in your service:

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

Services are wired up explicitly in `pkg/app/wire.go` — there is no manual wiring step. Running `forge generate` updates the wiring code.

## Running Locally

```bash
forge run
```

This starts everything needed for local development:

1. **Infrastructure** — runs `docker compose up -d` from `deploy/docker-compose.yml` (databases, Redis, etc.)
2. **Go binary** — starts the single binary via [Air](https://github.com/air-verse/air) for hot reload
3. **Next.js frontends** — runs `npm run dev` for each frontend

Each process gets color-coded log output with a name prefix. Press Ctrl+C to stop everything — Forge sends SIGTERM to all child processes and tears down the docker-compose infrastructure.

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
forge test e2e                # End-to-end tests
forge test --coverage         # Generate coverage reports
```

Use the generated test harness for service tests:

```go
func TestCreateUser(t *testing.T) {
    svc, mocks := app.NewTestUsersService(t)

    resp, err := svc.CreateUser(ctx, connect.NewRequest(&usersv1.CreateUserRequest{
        Email: "user@test.com",
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
3. Pushes to the local registry
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

## Project Configuration

The `forge.project.yaml` file drives all CLI behavior. Here is an example after adding a few services:

```yaml
name: myproject
module_path: github.com/myorg/myproject
version: "0.1.0"
mode: full
hot_reload: true

services:
  - name: api
    type: GO_SERVICE
    path: services/api
    port: 8080
  - name: users
    type: GO_SERVICE
    path: services/users
    port: 8081

frontends:
  - name: dashboard
    type: nextjs
    path: frontends/dashboard
    port: 3000

environments:
  - name: dev
    type: local
    registry: localhost:5050
    namespace: myproject-dev
  - name: staging
    type: cloud
    registry: ghcr.io/myorg
    namespace: myproject-staging
  - name: prod
    type: cloud
    registry: ghcr.io/myorg
    namespace: myproject-prod
```

## Next Steps

- **[Architecture]({{< relref "../architecture" >}})** — understand single binary design, two contract systems, and explicit wiring
- **[KCL Deployment Guide]({{< relref "../guides/kcl" >}})** — customize Kubernetes manifests
- **[Database Integration]({{< relref "../guides/database-integration" >}})** — migration-first database workflow
- **[CI/CD Guide]({{< relref "../guides/ci-cd" >}})** — understand the generated GitHub Actions workflows
- **[CLI Reference]({{< relref "../reference/cli" >}})** — full command reference