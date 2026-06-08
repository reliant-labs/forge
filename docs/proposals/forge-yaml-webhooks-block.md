# Declarative webhook providers via `forge.yaml: webhooks:`

- **Status**: proposed
- **Author**: cp-forge NATS + billing-trio agents (2026-06-08 post-port audit)
- **Related**: `internal/starters/{stripe,twilio,clerk-webhook}` (precedent for one-time business-integration scaffolds), `pkg/runtime` provisioning hook pattern (precedent for "ensure-at-boot" wiring), the existing `forge.yaml services:` block (precedent for the declarative-to-codegen pipeline)
- **Touches**: `internal/config/config.go` (schema), `internal/config/validate.go` (validation), new `internal/codegen/webhook_gen.go` (already partly exists for the starter shape; would extend to handle the declarative form), new `internal/generator/webhook_routes_gen.go.tmpl`.

## Context

Webhooks from third-party providers (Stripe, LiteLLM, Clerk, GitHub, Supabase, …) need the same boilerplate per project:

1. An `AppExtras` field for the handler (`*billing_gateway.StripeWebhookHandler`).
2. Env-gated construction in `pkg/app/setup.go` reading the per-provider secret env var (`STRIPE_WEBHOOK_SECRET`).
3. Route registration via the service's `RegisterHTTP` method (`mux.Handle("/webhooks/stripe", handler)`).
4. Signature verification inside the handler body before any business logic runs.

cp-forge currently does this twice — Stripe in `handlers/billing_gateway` and LiteLLM in `handlers/billing_gateway` — and will need it a third time for Supabase user-sync webhooks and a fourth for GitHub repo-event webhooks. Every instance is a copy-paste of the same wiring with a different secret env name and path.

## The class of friction the current shape produces

- **Wiring drift**: the Stripe wiring lives in three files (AppExtras, setup.go, service.go). A new provider has to mirror the layout exactly; a typo in one of the three (wrong env var name, mismatched path, forgotten setup-gate) doesn't surface until a webhook fails to verify in staging.
- **No central audit**: there's no single place to grep `webhooks: \[` and see every webhook the project exposes. The current grep is "find every `mux.Handle("/webhooks/*"` site".
- **Secret-loading footgun**: forgetting the `STRIPE_WEBHOOK_SECRET` env var in KCL's per-env config means the handler boots with empty bytes and silently rejects every webhook with HMAC mismatch.
- **No regen story**: when forge later adds a webhook-related cross-cutting concern (request-body size cap, IP allowlist by provider, request replay protection), every project's webhook handler has to be touched by hand. A declarative block would let forge own the cross-cutting code via codegen.

## Proposed change

### `forge.yaml` schema

```yaml
webhooks:
  - provider: stripe
    secret_env: STRIPE_WEBHOOK_SECRET     # required — name of env var holding the verification secret
    path: /webhooks/stripe                # required — mux route
    handler: handlers/billing_gateway:StripeWebhookHandler  # required — handler package + type
    optional: false                       # default false; true gates the wiring on secret_env presence
  - provider: litellm
    secret_env: LITELLM_WEBHOOK_SECRET
    path: /webhooks/litellm
    handler: handlers/billing_gateway:LiteLLMWebhookHandler
    optional: true                        # missing secret = log warn, don't register route
  - provider: clerk
    secret_env: CLERK_WEBHOOK_SECRET
    path: /webhooks/clerk
    handler: handlers/user:ClerkWebhookHandler
```

### Generated code

For each entry, `forge generate` emits:

1. **AppExtras field** (in `pkg/app/app_extras.go` if forge owns it, or via `forge:placeholder` marker if user-owned):

   ```go
   // forge:placeholder: billing_gateway.StripeWebhookHandler
   StripeWebhookHandler billing_gateway.StripeWebhookHandler
   ```

2. **Env-gated construction** in a generated `pkg/app/webhooks_gen.go`:

   ```go
   func setupWebhooks(ctx context.Context, app *App, cfg *config.Config, logger *slog.Logger) error {
       if secret := os.Getenv("STRIPE_WEBHOOK_SECRET"); secret != "" {
           app.StripeWebhookHandler = billing_gateway.NewStripeWebhookHandler([]byte(secret), logger)
       } else if /* required: */ false {
           return fmt.Errorf("STRIPE_WEBHOOK_SECRET env var required for stripe webhook")
       } else {
           logger.Warn("webhook secret missing — webhook disabled", "provider", "stripe", "env_var", "STRIPE_WEBHOOK_SECRET")
       }
       // ... same shape per provider ...
       return nil
   }
   ```

3. **Route registration** in a generated `pkg/app/webhook_routes_gen.go`:

   ```go
   func registerWebhookRoutes(mux *http.ServeMux, app *App, logger *slog.Logger) {
       if app.StripeWebhookHandler != nil {
           mux.Handle("/webhooks/stripe", app.StripeWebhookHandler)
       }
       if app.LiteLLMWebhookHandler != nil {
           mux.Handle("/webhooks/litellm", app.LiteLLMWebhookHandler)
       }
       // ... per provider ...
   }
   ```

   Called once from generated bootstrap.go after the Connect mux is built.

4. **Skeleton handler interface** (Tier-2; scaffold-once, user owns):

   ```go
   // handlers/billing_gateway/stripe_webhook.go (scaffolded once, user-owned)
   type StripeWebhookHandler struct {
       secret []byte
       logger *slog.Logger
   }

   func NewStripeWebhookHandler(secret []byte, logger *slog.Logger) *StripeWebhookHandler {
       return &StripeWebhookHandler{secret: secret, logger: logger}
   }

   func (h *StripeWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
       // 1) Read body (capped at 64 KiB by default — forge can enforce this in a middleware layer).
       // 2) Verify signature (project fills in — forge can't pick the algorithm for every provider).
       // 3) Decode event.
       // 4) Dispatch to business logic.
   }
   ```

5. **KCL projection**: each entry's `secret_env` automatically projects into the relevant env's secret-only ConfigMap projection (the same machinery `environments[].config.sensitive` already uses), so forgetting to set `STRIPE_WEBHOOK_SECRET` in KCL is a forge.yaml ↔ KCL cross-check error at `forge generate` time, not a runtime HMAC-mismatch surprise.

## Why this isn't a starter

`internal/starters/stripe` and `internal/starters/twilio` are one-time-copy *business-integration* starters — the user owns the resulting code, forge never touches it. They fit the "shipping Stripe customer-portal code" story but not the "wiring up a new webhook route" story.

A webhook block in `forge.yaml` is the wiring layer: it solves the "I have this Stripe verification logic, please wire it into bootstrap + setup + routes" problem. The starter still provides the verification logic; the `webhooks:` block provides the wiring around it. Both ship — starter owns business code, codegen owns wiring code.

## What this would mean for cp-forge

The current Stripe + LiteLLM wiring (three files × two providers = six edit points) collapses to four lines of `forge.yaml`. Adding Supabase user-sync becomes a one-line forge.yaml change + the actual Supabase verification handler.

The verification handler itself stays user-owned (it's business logic — webhook secret rotation, event-type dispatch, idempotency keys, replay protection are all project-specific). The boilerplate around it stops being copy-pasted.

## Why proposal-only right now

The other forge agent has 156 dirty files including the `internal/generator/webhook_gen.go` file (already a target for whatever they're doing) plus build-target and external-build work that touches the same `internal/cli/audit_*` + `internal/cli/build_*` paths a webhook codegen pass would need to coordinate against. Landing a `webhooks:` block now would force a coordination dance against six pre-existing in-flight changes; landing it after the sibling work settles makes it a focused ~300-line change (forge.yaml schema + validator + codegen + template + audit-pass extension).

## Open questions

1. **Signature verification: forge-owned or user-owned?** The current proposal puts it in the user-owned handler. An alternative is per-provider middleware in forge (`forge/pkg/webhooks/stripe`, `forge/pkg/webhooks/clerk`), called before the handler runs. Risk: each provider has its own quirks (Stripe re-verifies on tolerance, Clerk uses svix headers, GitHub uses HMAC-SHA256 of the raw body) that don't generalise cleanly. Recommendation: keep verification user-owned; forge provides a `forge/pkg/webhookutil` helper library with the common primitives (HMAC compare, body cap, idempotency cache) that the user composes.
2. **Where does `handler:` resolve to?** The proposal uses `handlers/billing_gateway:StripeWebhookHandler` — a Go-package-path + identifier. Alternative: just the identifier, with forge searching for the type across handler packages. The explicit-path form is more verbose but unambiguous; favoured because cp-forge already has two Stripe-related types in two different packages and a search would pick the wrong one.
3. **Multiple webhooks per provider?** GitHub apps can have webhooks for repo events AND organization events at different paths. Each gets its own `webhooks:` entry — `provider: github-repo`, `provider: github-org`. The provider name is opaque to forge; it's used for the codegen identifier suffix and log attributes, not for any provider-specific logic.
