---
name: adapter
description: Outbound boundary translators. One adapter per third-party system / queue / storage backend; narrow interface, vendor-neutral callers.
emit: both
---

# Adapter

An adapter is the package that translates between your domain and one external system: a third-party HTTP API, a message broker, a storage gateway, an OAuth provider. It owns the wire format, the retries, the timeout policy, and the response mapping. It does not own business logic — that lives one layer up.

## When to add an adapter

Add an adapter the moment you need to call an external system that isn't your own database. Symptoms:

- You're about to import a vendor SDK in your application bootstrap or in a handler.
- You're about to write `POST https://api.stripe.com/v1/...` (or the equivalent in your stack) inline in a service.
- You're orchestrating two external calls together — that's an interactor over two adapters, not an unstructured helper.

If the external system is your own first-party API that you already generate clients for, you don't need an adapter — depend on the generated client. Adapters exist for boundaries you don't already manage.

## What goes in

- The HTTP / gRPC / SDK client setup, with timeouts.
- Per-request authentication header construction.
- Retry / circuit-breaker / rate-limit policy.
- Response-body parsing, vendor → domain type mapping.
- Vendor-error → domain-error translation.

## What does NOT go in

- Multi-step workflows (validate → fetch → send → audit). Compose those in an interactor that depends on this adapter's interface.
- Business logic — eligibility checks, pricing, dedupe. Those belong to the domain.
- Reaching into other adapters or services. Adapters are leaf nodes; they don't know about each other.

## The narrow interface

The adapter exposes ONE narrow interface in the language of your domain — not the full vendor SDK. Conventions worth keeping:

- **One method per use case the downstream supports.** Resist mirroring the entire vendor SDK; you only owe an interface for what your domain actually needs.
- **Domain types in / out, not vendor types.** Even if the vendor SDK has 40 fields per object, your adapter exposes the 1-2 your domain consumes.
- **Cancellation propagates.** Always accept a context / cancellation token as the first argument so timeouts and tracing work end-to-end.

Illustrative shape (Go):

```go
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

Same shape in any language: a narrow interface in domain types, hiding the vendor's wire format.

## How to test

Adapter tests use an in-process HTTP test server (or the SDK's record-and-replay equivalent) to stand in for the vendor. The point is to exercise the *adapter's* translation logic — request construction, header injection, response parsing, error mapping — against a controlled downstream.

```go
func TestCreateCharge_OK(t *testing.T) {
    stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/v1/charges" { http.NotFound(w, r); return }
        if got := r.Header.Get("Authorization"); got == "" {
            t.Errorf("missing Authorization header")
        }
        _, _ = w.Write([]byte(`{"id":"ch_test_1"}`))
    }))
    defer stub.Close()

    svc := New(Deps{HTTPClient: stub.Client(), BaseURL: stub.URL})

    res, err := svc.CreateCharge(context.Background(), CreateChargeInput{...})
    if err != nil { t.Fatalf("CreateCharge: %v", err) }
    if res.ChargeID != "ch_test_1" { t.Fatalf("got %q", res.ChargeID) }
}
```

The interactor that calls this adapter in production mocks the interface, never the downstream HTTP server.

## Library choice is yours

Your adapter wraps whatever client you choose — raw HTTP, the vendor SDK, an RPC client, a queue / streaming client. The convention is the *shape* of the package (narrow interface in domain types), not the transport library.

## Rules

- **One adapter per outbound boundary.** No multi-system "integration" packages.
- **Narrow interface in domain types.** Don't expose vendor types or mirror the vendor SDK.
- **Test against a stub server.** Never against the live downstream.
- **Adapters are leaf nodes.** No other adapters / services in the dep struct.

## When a consumer must register onto the adapter (two-phase wiring)

Adapters are leaf nodes at construction, but occasionally a downstream consumer registers a callback / sink / subscriber onto the adapter after both exist (e.g. an event-bus adapter receiving subscribers from services built later). Don't add the consumer to the adapter's `Deps` — that inverts the leaf rule, and constructor topo-ordering alone deadlocks on this shape.

Use **construct-then-register** inside `NewComponents` (`forge disown internal/app/compose.go` first to hand-own the construction site): build the adapter, build the consumer, then call the register/subscribe setter. It's an ordinary method call after both ends exist — not a framework seam.

```go
bus := eventbus.New(eventbus.Deps{Logger: log})
svc := orders.New(orders.Deps{Bus: bus})  // consumer holds the adapter interface
bus.Subscribe("order.created", svc.OnOrderCreated)  // phase two
```

There is no `PostBootstrap` / `post_bootstrap.go` seam — late registration is plain Go in the disowned `compose.go`. See the `interactor` skill for the canonical two-phase shape.

<!-- @forge-only:start -->
## Forge scaffolding

Scaffold an adapter with:

```
forge add adapter stripe-adapter
```

This emits the canonical package layout:

```
internal/stripe-adapter/
  contract.go        # // forge:adapter — Service interface, narrow surface
  adapter.go         # Service struct + downstream calls; New(Deps) Service
  adapter_test.go    # httptest stub of the downstream
```

Plus a `cache.go` stub for any local caching the adapter needs (delete it if you don't). `forge add package <name> --type adapter` resolves to the same code path.

## Marker comment and lint enforcement

Every adapter package's `contract.go` carries a `// forge:adapter` marker comment on the package doc. The marker tells the next reader the package's role in the architecture, and one lint rule enforces invariant:

- `forgeconv-adapter-no-rpc` — adapter packages must not register Connect RPC handlers. Adapters are outbound-only; an RPC means it's actually a service.

## Wiring: construct in the explicit composition

An adapter is a leaf built in `internal/app/compose.go` `NewComponents` (off the owned `internal/app/providers.go` `Infra`) and passed to consumers as an **interface**. No `Setup(app *App)`, no name-matched `App` fields — just construct it and hand it down.

```go
// internal/app/compose.go
stripe := stripeadapter.New(stripeadapter.Deps{
    HTTPClient: &http.Client{Timeout: 30 * time.Second},
    Cfg:        infra.Cfg.Stripe,  // scalars travel as one typed Config block
})
bill := billing.New(billing.Deps{Charges: stripe})  // consumer sees stripeadapter.Service, not the concrete type
```

Because the consumer depends on the interface, swapping the real adapter for a mock (tests) or a different backend is a one-line change here — the consumer is untouched.

## When this skill is not enough (forge sub-skills)

- **Composing multiple adapters into a workflow** — see `interactor`.
- **The Service / Deps / New shape itself** — see `service-layer`.
- **Wrapping vendor errors into svcerr sentinels** — see `service-layer`'s errors section and `forge/pkg/svcerr`.
- **Webhook ingestion (inbound from a vendor)** — that's a webhook handler, not an adapter. See `forge add webhook`.
<!-- @forge-only:end -->
