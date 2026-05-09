---
name: adapter
description: Outbound boundary translators. One adapter per third-party system / queue / storage backend; narrow Go interface, vendor-neutral callers.
---

# Adapter

An adapter is the package that translates between your domain and one
external system: a third-party HTTP API, a message broker, a storage
gateway, an OAuth provider. It owns the wire format, the retries, the
timeout policy, and the response mapping. It does not own business
logic — that lives in interactors and services.

```
internal/stripe-adapter/
  contract.go        # // forge:adapter — Service interface, narrow surface
  adapter.go         # Service struct + downstream calls; New(Deps) Service
  adapter_test.go    # httptest stub of the downstream
```

Scaffold one with:

```
forge add package stripe-adapter --type adapter
```

This emits the four files above with the canonical Service / Deps /
New(Deps) Service shape and the `// forge:adapter` marker on
contract.go.

## When to add an adapter

Add an adapter the moment you need to call an external system that
isn't your own database. Symptoms:

- You're about to import an SDK in `pkg/app/setup.go` or in a handler.
- You're about to write `http.NewRequestWithContext(ctx, "POST", "https://api.stripe.com/v1/...", ...)` in a service.
- You're orchestrating two external calls together (that's an
  *interactor* over two adapters, not an unstructured helper).

If the external system is your own first-party Connect service, you
don't need an adapter — depend on the generated client. Adapters exist
for boundaries forge codegen doesn't already manage.

## What goes in

- The HTTP / gRPC / SDK client setup, with timeouts.
- Per-request authentication header construction.
- Retry / circuit-breaker / rate-limit policy.
- Response-body parsing, vendor → domain type mapping.
- Vendor-error → domain-error translation (wrap with svcerr sentinels
  where appropriate).

## What does NOT go in

- Multi-step workflows (validate → fetch → send → audit). Compose
  those in an interactor that depends on this adapter's `Service`.
- Connect RPC handler registration (`NewXxxHandler`). The
  forgeconv-adapter-no-rpc lint rule will warn — adapters are
  outbound-only by convention, so RPC means it's actually a service.
- Business logic — eligibility checks, pricing, dedupe. Those belong
  to the domain.
- Reaching into other adapters or services. Adapters are leaf nodes.

## The Service interface

```go
// internal/stripe-adapter/contract.go
// forge:adapter
//
// stripe-adapter wraps the Stripe Payments API. Callers depend on
// Service; the concrete struct stays unexported.
package stripe_adapter

import "context"

type Service interface {
    HealthCheck(ctx context.Context) error
    CreateCharge(ctx context.Context, in CreateChargeInput) (CreateChargeResult, error)
}

type CreateChargeInput struct {
    AmountMinor int64
    Currency    string
    CustomerID  string
}

type CreateChargeResult struct {
    ChargeID string
}
```

Conventions worth keeping:

- **One method per use case the downstream supports.** Resist mirroring
  the entire vendor SDK; you only owe an interface for what your
  domain actually needs.
- **Domain types in / out, not vendor types.** Even if `*stripe.Charge`
  has 40 fields, this adapter exposes the 1-2 your domain consumes.
- **`context.Context` first arg, always.** Cancellation propagates,
  tracing works.
- **Marker comment `// forge:adapter` on the package doc.** This is
  what the lint rule looks for (and what the next reader looks for).

## How to test

Adapter tests use `net/http/httptest` (or the SDK's record-and-replay
equivalent). The shape:

```go
func TestCreateCharge_OK(t *testing.T) {
    t.Parallel()
    stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/v1/charges" { http.NotFound(w, r); return }
        if got := r.Header.Get("Authorization"); got == "" {
            t.Errorf("missing Authorization header")
        }
        _, _ = w.Write([]byte(`{"id":"ch_test_1"}`))
    }))
    defer stub.Close()

    svc := New(Deps{
        Logger:     slog.Default(),
        HTTPClient: stub.Client(),
        BaseURL:    stub.URL,
    })

    res, err := svc.CreateCharge(context.Background(), CreateChargeInput{...})
    if err != nil { t.Fatalf("CreateCharge: %v", err) }
    if res.ChargeID != "ch_test_1" { t.Fatalf("got %q", res.ChargeID) }
}
```

The point is to exercise the *adapter's* translation logic — request
construction, header injection, response parsing, error mapping —
against a controlled downstream. Your interactor tests will use a mock
of `Service`, never an httptest stub of the downstream.

## Wiring in `pkg/app/setup.go`

```go
func Setup(app *App) error {
    app.Stripe = stripe_adapter.New(stripe_adapter.Deps{
        Logger:     app.Logger,
        HTTPClient: &http.Client{Timeout: 30 * time.Second},
        BaseURL:    app.Config.StripeBaseURL,
    })
    return nil
}
```

The bootstrap template emits the wiring slot; you fill it.

## Library choice is yours

Your adapter wraps whatever client you choose — `net/http` directly,
the vendor SDK, a `connectrpc.com/connect` client, an NATS / Kafka /
Temporal client, an `aws-sdk-go-v2` service client. The forge
convention is the *shape* of the package, not the transport library.

## Rules

- One adapter per outbound boundary. No multi-system "integration"
  packages.
- `// forge:adapter` marker comment on the package doc in
  `contract.go`. Lint enforces this.
- No Connect RPC handlers in an adapter package. Lint enforces this
  (`forgeconv-adapter-no-rpc`).
- Service interface in `contract.go`; concrete `service` struct
  unexported in `adapter.go`; constructor returns the interface.
- Test against an httptest stub (or vendor-SDK equivalent), never
  against the live downstream.
- Adapters are leaf nodes — no other adapters / services in `Deps`.

## When this skill is not enough

- **Composing multiple adapters into a workflow** — see `interactor`.
- **The Service / Deps / New shape itself** — see `service-layer`.
- **Wrapping vendor errors into svcerr sentinels** — see `service-layer`'s
  errors section and `forge/pkg/svcerr`.
- **Webhook ingestion (inbound from a vendor)** — that's a webhook
  handler, not an adapter. See `forge add webhook`.
