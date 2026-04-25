# Stripe Pack

Stripe payment integration with typed API client, webhook handler with signature verification and idempotency, and proto entity definitions for payment tracking.

## Installation

```bash
forge pack install stripe
```

## What Gets Generated

| File | Description |
|------|-------------|
| `pkg/stripe/client.go` | Typed Stripe client with methods for payment intents, customers, and checkout sessions |
| `pkg/stripe/webhook.go` | Webhook handler with Stripe signature verification and event-driven dispatch |
| `handlers/webhooks/stripe_webhook_gen.go` | Route registration for `/webhooks/stripe` (regenerated on `forge generate`) |
| `proto/db/v1/stripe_entities.proto` | Proto entities for `StripeCustomer`, `StripePaymentIntent`, `StripeSubscription`, `StripeInvoice` with indexes and foreign keys |

## Configuration

```yaml
stripe:
  webhook_secret_env: STRIPE_WEBHOOK_SECRET
  api_key_env: STRIPE_SECRET_KEY
  publishable_key_env: STRIPE_PUBLISHABLE_KEY
```

| Variable | Purpose |
|----------|---------|
| `STRIPE_SECRET_KEY` | Stripe API secret key (required) |
| `STRIPE_WEBHOOK_SECRET` | Webhook signing secret from your Stripe dashboard (required for webhooks) |
| `STRIPE_PUBLISHABLE_KEY` | Frontend publishable key |

## Usage

### API Client

```go
client, err := stripe.NewClient()

pi, err := client.CreatePaymentIntent(ctx, stripe.CreatePaymentIntentParams{
    Amount:   2000,
    Currency: "usd",
    CustomerID: "cus_xxx",
})
```

The client exposes `CreatePaymentIntent`, `CapturePaymentIntent`, `CancelPaymentIntent`, `CreateCustomer`, `UpdateCustomer`, `DeleteCustomer`, and `CreateCheckoutSession`.

### Webhook Handling

Implement the `StripeEventHandler` interface (or embed `NoopEventHandler` to handle only events you care about):

```go
type MyHandler struct {
    stripe.NoopEventHandler
}

func (h *MyHandler) HandlePaymentIntentSucceeded(ctx context.Context, event stripe.Event) error {
    var pi stripe.PaymentIntent
    stripe.ParseEventData(event, &pi)
    // process successful payment
    return nil
}
```

Register the webhook route:

```go
webhooks.RegisterStripeWebhookRoutes(mux, &MyHandler{})
```

### Signature Verification & Idempotency

The webhook handler verifies every request against the `Stripe-Signature` header using Stripe's official library. It also includes a built-in idempotency store that deduplicates events by ID, preventing double-processing on retries. The default in-memory store works for single instances; implement the `IdempotencyStore` interface for multi-instance deployments.

## Dependencies

- `github.com/stripe/stripe-go/v82`

## Removal

```bash
forge pack remove stripe
```
