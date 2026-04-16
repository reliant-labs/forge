# Forge Extension Plans

## Overview

These plans extend Forge with new capabilities that make it a better fit for production Go systems. Each extension follows Forge's existing patterns: template-based scaffolding, contract-driven generation, config tracking in `forge.project.yaml`, and wiring through `bootstrap.go`.

Priority order based on breadth of impact across production systems.

---

## 1. Worker Component Type (`forge add worker`)

### Problem
Most production backends have async/background work: queue consumers, scheduled jobs, event processors. Currently Forge only scaffolds HTTP services and frontends. Teams hand-roll workers with inconsistent patterns.

### Design

A worker is a long-running process that doesn't serve HTTP but participates in the single-binary lifecycle. It has:
- A `Start(ctx context.Context) error` method (blocks until ctx cancelled)
- A `Stop(ctx context.Context) error` method (graceful drain)
- Health reporting via the existing health endpoint
- Access to the same `Deps` injection as services

#### Config tracking

```yaml
# forge.project.yaml
services:
  - name: order-processor
    type: worker          # new type alongside "go_service"
    path: handlers/order-processor
```

#### Files generated

| File | Purpose |
|---|---|
| `handlers/<name>/worker.go` | Worker struct, Deps, Start/Stop, New() |
| `handlers/<name>/worker_test.go` | Test harness with context cancellation |

#### Template: `worker/worker.go.tmpl`

```go
package {{.Name | lower}}

import (
    "context"
    "log/slog"
    "{{.Module}}/pkg/config"
)

type Deps struct {
    Logger *slog.Logger
    Config *config.Config
}

type Worker struct {
    deps Deps
}

func New(deps Deps) *Worker {
    return &Worker{deps: deps}
}

func (w *Worker) Name() string { return "{{.Name}}" }

// Start blocks until ctx is cancelled. Implement your processing loop here.
func (w *Worker) Start(ctx context.Context) error {
    w.deps.Logger.Info("worker started", "worker", w.Name())
    <-ctx.Done()
    return nil
}

// Stop is called during graceful shutdown. Drain in-flight work here.
func (w *Worker) Stop(ctx context.Context) error {
    w.deps.Logger.Info("worker stopping", "worker", w.Name())
    return nil
}
```

#### Bootstrap integration

Workers are constructed in `bootstrap.go` like services but instead of `Register(mux)`, they're collected into a `[]Worker` slice. The `cmd/server.go` starts them in goroutines and manages their lifecycle alongside the HTTP server.

```go
// In bootstrap.go
type Worker interface {
    Name() string
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

#### cmd/server.go changes

```go
// Start workers
for _, w := range app.Workers {
    go func(w app.Worker) {
        if err := w.Start(ctx); err != nil {
            logger.Error("worker failed", "worker", w.Name(), "error", err)
        }
    }(w)
}

// On shutdown, stop workers with timeout
for _, w := range app.Workers {
    w.Stop(shutdownCtx)
}
```

#### Selective startup

`forge run myproject server order-processor` — workers are selectable by name just like services. The `BootstrapOnly()` function includes/excludes workers by name.

### Implementation steps

1. Add `worker/*.tmpl` templates to `internal/templates/`
2. Update embed directive in `templates.go`
3. Add `RenderWorkerTemplate()` to `templates.go`
4. Add `forge add worker` CLI command in `internal/cli/add.go`
5. Update `bootstrap.go.tmpl` to construct and collect workers
6. Update `cmd-server.go.tmpl` to start/stop workers
7. Update `BootstrapOnly()` to filter workers by name
8. Add worker type to `ServiceConfig` validation
9. Add tests

---

## 2. Event Bus Package Pattern (`forge package new --kind eventbus`)

### Problem
Event-driven communication between services/workers is common but there's no standard pattern in Forge. Teams create ad-hoc pub/sub with no contracts, no test doubles, no generated wiring.

### Design

A specialized internal package that provides a typed event bus contract. The implementation (NATS, Kafka, Redis, in-memory) is left to the user, but the contract, mock, and wiring are generated.

#### Files generated

| File | Purpose |
|---|---|
| `internal/eventbus/contract.go` | `Publisher` and `Subscriber` interfaces |
| `internal/eventbus/service.go` | Deps struct, in-memory implementation (default) |
| `internal/eventbus/events.go` | Event type registry scaffold |

#### Contract

```go
package eventbus

import "context"

// Event is the base event envelope.
type Event struct {
    ID      string
    Type    string
    Payload []byte
}

// Publisher publishes events to topics.
type Publisher interface {
    Publish(ctx context.Context, topic string, event Event) error
}

// Subscriber subscribes to events on topics.
type Subscriber interface {
    Subscribe(ctx context.Context, topic string, handler func(ctx context.Context, event Event) error) error
}

// Service combines both interfaces for convenience.
type Service interface {
    Publisher
    Subscriber
}
```

This follows the existing contract.go pattern but with a richer starting point. `forge generate` picks it up and generates mocks, middleware (logging/tracing wrappers), and wiring — exactly like any other internal package.

The in-memory implementation serves as both the default dev implementation and the test double. Users replace it with NATS/Kafka/etc by implementing the same interface.

### Implementation steps

1. Add `internal-package/eventbus/` templates to `internal/templates/`
2. Add `--kind` flag to `forge package new` CLI command
3. When `--kind eventbus`, use the specialized templates instead of the generic ones
4. The generated files are still scanned by `forge generate` for contract-based codegen (mocks, middleware, tracing)

---

## 3. Webhook Ingestion Pattern (`forge add webhook`)

### Problem
Consuming webhooks from external services (Stripe, GitHub, Twilio, etc.) follows a repetitive pattern: verify signature, dedup, parse, dispatch. Teams re-implement this every time with subtle bugs (missing idempotency, wrong signature verification).

### Design

A webhook endpoint is a specialized HTTP handler attached to a service. It generates the boilerplate and provides a handler interface for the user to implement.

#### Files generated

| File | Purpose |
|---|---|
| `handlers/<service>/webhooks.go` | HTTP handler, signature verification, idempotency |
| `handlers/<service>/webhooks_test.go` | Test harness with replay support |
| `db/migrations/XXX_create_webhook_events.up.sql` | Idempotency table |

#### Template: webhook handler

```go
// RegisterHTTP is already part of every service. Webhooks register here.
func (s *Service) RegisterHTTP(mux *http.ServeMux, middleware func(http.Handler) http.Handler) {
    mux.Handle("/webhooks/{{.Name}}", middleware(http.HandlerFunc(s.handleWebhook)))
}

func (s *Service) handleWebhook(w http.ResponseWriter, r *http.Request) {
    body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB max
    if err != nil {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }

    // Verify signature (implement in verifySignature)
    if err := s.verifyWebhookSignature(r, body); err != nil {
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return
    }

    // Idempotency check
    eventID := r.Header.Get("{{.IDHeader}}")
    if eventID == "" {
        eventID = r.Header.Get("X-Request-ID")
    }
    if processed, _ := s.isWebhookProcessed(r.Context(), eventID); processed {
        w.WriteHeader(http.StatusOK)
        return
    }

    // Dispatch
    if err := s.processWebhook(r.Context(), r.Header, body); err != nil {
        s.deps.Logger.Error("webhook processing failed", "error", err, "event_id", eventID)
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }

    s.markWebhookProcessed(r.Context(), eventID)
    w.WriteHeader(http.StatusOK)
}
```

The user implements `verifyWebhookSignature()` and `processWebhook()` — the contract is clear, the boilerplate is handled.

#### Idempotency migration

```sql
CREATE TABLE IF NOT EXISTS webhook_events (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_webhook_events_source ON webhook_events(source);
```

### Implementation steps

1. Add `webhook/*.tmpl` templates
2. Add `forge add webhook <name> --service <service>` CLI command
3. Generate webhook handler files into the target service directory
4. Generate migration file with auto-incrementing version
5. Update the service's `RegisterHTTP` to include the webhook route
6. Generate test file with helper to replay recorded payloads

---

## 4. Auth Middleware Generation (Proto Annotations)

### Problem
Forge already scaffolds auth middleware (`middleware-auth.go.tmpl`, claims, authorizer interface) but doesn't generate the actual JWT validation or API key checking logic from configuration. Teams manually implement token validation in every `Authorizer`.

### Design

Extend the existing `service.proto` and `method.proto` annotations to drive auth code generation. The proto annotations already have `auth_required` and `auth_provider` fields — we generate working interceptor logic from them.

#### Proto annotations (already exist, need generation support)

```protobuf
// forge/options/v1/service.proto — already defined
message AuthConfig {
    bool auth_required = 1;
    string auth_provider = 2; // "jwt", "api_key", "both"
}

// forge/options/v1/method.proto — already defined
message MethodOptions {
    bool auth_required = 1;
}
```

#### Config additions

```yaml
# forge.project.yaml
auth:
  provider: jwt           # "jwt" | "api_key" | "both"
  jwt:
    issuer: ""            # validated in token
    audience: ""          # validated in token
    jwks_url: ""          # for RS256/ES256 key rotation
    signing_method: ES256 # HS256, RS256, ES256
  api_key:
    header: X-API-Key     # which header to check
```

#### Generated code

When `auth.provider` is set in config, `forge generate` produces a working `middleware/auth_gen.go` that:
- Validates JWTs (signature, expiry, issuer, audience)
- Extracts claims into the existing `Claims` struct
- Checks API keys against a pluggable store (interface in contract)
- Skips auth for methods annotated with `auth_required: false`

The generated code calls into a user-implementable `TokenValidator` interface so teams can customize validation without editing generated files.

### Implementation steps

1. Read existing auth proto annotations during `forge generate`
2. Parse `forge.project.yaml` auth config
3. Generate `middleware/auth_gen.go` with JWT/API key validation based on config
4. Generate `internal/auth/contract.go` with `TokenValidator` and `KeyStore` interfaces
5. Update per-service `authorizer.go.tmpl` to use claims from context instead of environment check
6. Add integration test template that tests auth flows

---

## 5. Operator Component Type (`forge add operator`)

### Problem
Kubernetes operators (controllers) follow a highly formulaic pattern: CRD types → controller with `Reconcile()` → RBAC → leader election → health checks. This is perfect for codegen but currently requires kubebuilder or hand-rolling.

### Design

An operator is a component type that generates a controller-runtime based reconciler. It participates in the single binary lifecycle but runs a controller manager instead of serving HTTP.

#### Files generated

| File | Purpose |
|---|---|
| `handlers/<name>/controller.go` | Controller struct, Reconcile(), SetupWithManager() |
| `handlers/<name>/controller_test.go` | envtest-based test harness |
| `handlers/<name>/types.go` | CRD Go types (spec, status) |
| `deploy/crds/<name>.yaml` | CRD manifest |

#### Template: controller

```go
package {{.Name | lower}}

import (
    "context"
    "log/slog"
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Deps struct {
    Logger *slog.Logger
    Client client.Client
}

type Controller struct {
    deps Deps
}

func New(deps Deps) *Controller {
    return &Controller{deps: deps}
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
    c.deps.Logger.Info("reconciling", "name", req.Name, "namespace", req.Namespace)

    // TODO: Implement reconciliation logic
    return reconcile.Result{}, nil
}

func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&{{.TypeName}}{}).
        Complete(c)
}
```

#### Bootstrap integration

Operators register with a controller manager instead of an HTTP mux. The `cmd/server.go` template conditionally creates a manager when operators are present.

### Implementation steps

1. Add `operator/*.tmpl` templates
2. Add `forge add operator <name>` CLI command
3. Generate CRD types, controller, test harness, and CRD manifest
4. Update `bootstrap.go.tmpl` to setup controller manager when operators exist
5. Update `cmd-server.go.tmpl` to start manager alongside HTTP server
6. Add `controller-runtime` to generated `go.mod` only when operators are present
7. Generate RBAC manifests in `deploy/`

---

## 6. External Client Package Pattern (`forge package new --kind client`)

### Problem
Every production app integrates with 3-10 external APIs. Without a standard pattern, external clients are scattered, untestable (no interface), and inconsistently configured.

### Design

A specialized internal package with a richer contract scaffold: an interface for the external API, a Deps struct with HTTP client and config, and a health check method.

#### Files generated

| File | Purpose |
|---|---|
| `internal/<name>/contract.go` | Client interface with methods + `HealthCheck()` |
| `internal/<name>/client.go` | HTTP client implementation scaffold |
| `internal/<name>/client_test.go` | Test with httptest server |

#### Contract

```go
package {{.Name}}

import "context"

type Service interface {
    // HealthCheck verifies connectivity to the external service.
    HealthCheck(ctx context.Context) error

    // TODO: Add your client methods here.
}
```

The implementation file includes:
- HTTP client with configurable timeout and base URL
- Standard error wrapping
- Context propagation
- Config wiring (base URL and API key from env)

`forge generate` handles mocks, tracing, and middleware wrappers via the standard contract scanning.

### Implementation steps

1. Add `internal-package/client/` templates
2. Handle `--kind client` in `forge package new`
3. Generate client implementation scaffold with HTTP client
4. Generate test file with httptest server pattern
5. Wire config for base URL and API key

---

## 7. Multi-Tenancy Primitives

### Problem
B2B SaaS backends almost always need tenant isolation. The pattern is formulaic: extract tenant from auth context, scope all queries, isolate test data. Without framework support, tenant scoping bugs are a common source of data leaks.

### Design

Not a new component type — instead, a set of additions across existing Forge features:

#### Tenant context middleware

When `auth.multi_tenant: true` in config, generate a middleware that extracts `org_id` (or configurable claim name) from the JWT claims and injects it into context. The existing `Claims` struct gets a `TenantID` field.

#### ORM query scoping

When an entity proto message has a field annotated with `[(forge.options.v1.field).tenant_key = true]`, the generated CRUD methods automatically filter by that field using the tenant ID from context. Generated queries include `WHERE org_id = $tenant_id` without the developer needing to remember.

#### Test harness

The generated test helpers include `WithTenant(ctx, tenantID)` to set up isolated test contexts.

### Implementation steps

1. Add `multi_tenant` option to auth config in `forge.project.yaml`
2. Add `tenant_key` to field proto options
3. Update claims middleware to extract tenant ID
4. Update ORM generation to add tenant scoping to CRUD queries
5. Update test harness templates with tenant helpers

---

## Implementation Priority & Dependencies

```
1. Worker Component Type         — independent, high impact
2. Event Bus Package Pattern     — independent, high impact
3. Webhook Ingestion Pattern     — independent, medium-high impact
4. Auth Middleware Generation     — independent, medium-high impact (builds on existing scaffolding)
5. Operator Component Type       — independent, medium impact
6. External Client Package       — independent, medium impact
7. Multi-Tenancy Primitives      — depends on Auth (#4) and ORM, lower priority
```

Items 1-6 are fully independent of each other and can be implemented in parallel.

---

## Implementation Status

All 7 extensions have been implemented:

| # | Extension | Status | Key Command |
|---|-----------|--------|-------------|
| 1 | Worker Component Type | **Done** | `forge add worker <name>` |
| 2 | Event Bus Package | **Done** | `forge package new <name> --kind eventbus` |
| 3 | Webhook Ingestion | **Done** | `forge add webhook <name> --service <svc>` |
| 4 | Auth Middleware Generation | **Done** | Auto-generated via `forge generate` when `auth.provider` is set |
| 5 | Operator Component Type | **Done** | `forge add operator <name> [--group] [--version]` |
| 6 | External Client Package | **Done** | `forge package new <name> --kind client` |
| 7 | Multi-Tenancy Primitives | **Done** | Auto-generated via `forge generate` when `auth.multi_tenant.enabled` is set |