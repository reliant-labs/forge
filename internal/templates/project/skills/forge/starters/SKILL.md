---
name: starters
description: Scaffold one-time business-integration starters — Stripe billing, Twilio SMS, Clerk webhook user-sync. The user owns the code after copy. Distinct from packs (which forge maintains centrally).
---

# Forge Starters

Starters are **one-time scaffolds** of opinionated, working code copied into the project. After `forge starter add <name>` runs, the user owns every line of the generated code — forge does **not** track the starter in `forge.yaml`, will **not** re-render anything on `forge generate`, and will **not** run upgrade migrations.

Starters are the right shape for **business integrations** where every project diverges (Stripe billing flow, Twilio SMS workflow, Clerk webhook user-sync). Centrally maintaining that code creates more bugs than it prevents.

For pure **infrastructure** (auth interceptors, JWKS rotation, audit middleware), use **[packs](../../../skills/forge/packs/SKILL.md)** instead.

## Commands

```bash
forge starter list                          # Show available starters
forge starter add <name>                    # Copy starter files into the project
forge starter add <name> --service <svc>    # Route paths into a specific service
forge starter add <name> --force            # Overwrite existing files (default: skip)
```

The command runs once and exits. Forge has no further opinion on the resulting code — edit it, refactor it, delete it, replace its proto entities with your own data model.

## Available Starters

| Starter | What it scaffolds |
|---------|------------------|
| `stripe` | Stripe billing — typed client, webhook signature verification, checkout/payment-intent/customer helpers. No proto entities (you wire to your own data model). |
| `twilio` | Twilio SMS/voice — typed client, HMAC-SHA1 webhook validation, messaging-service contract + impl. No proto entities. |
| `clerk-webhook` | Clerk webhook user-sync — Svix signature verification, user/org/membership event dispatch. Pair with the `clerk` pack for JWKS auth. |

## Starter manifest shape

Each starter ships a `starter.yaml` plus a `templates/` directory:

```yaml
name: stripe
description: Stripe billing scaffold — checkout, webhook, subscription helpers
deps:
  go:
    - github.com/stripe/stripe-go/v82
files:
  - source: stripe_client.go.tmpl
    destination: pkg/clients/stripe/client.go
  - source: stripe_webhook.go.tmpl
    destination: pkg/clients/stripe/webhook.go
notes: |
  After scaffolding, you own this code. The Stripe API surface evolves —
  monitor Stripe's release notes and update accordingly.

  The webhook handler skeleton verifies Stripe-Signature; production deploys
  must set STRIPE_WEBHOOK_SECRET. Configure via your secret manager
  (per-env).
```

Both `source` and `destination` are Go-template strings, so a starter can route into a specific service via `forge starter add stripe --service billing`.

Dependencies are **echoed to the user**, not auto-installed. Starters are user-owned — dependency churn (security bumps, major-version migrations) is the user's call, not forge's.

## What add does

1. Renders each `source` template against `{ModulePath, ProjectName, Service}` into the project at `destination`.
2. Skips files that already exist (use `--force` to override).
3. Prints the dependency hints and post-install notes.
4. Exits. **No `forge.yaml` mutation, no `go mod tidy`, no migration allocation.**

## Pack vs starter — when to reach for which

| | Packs | Starters |
|---|---|---|
| **Lifecycle** | install / upgrade / remove | one-time copy |
| **Tracked in `forge.yaml`?** | yes | no |
| **Re-rendered by `forge generate`?** | yes (the `generate:` block) | never |
| **Auto-installs deps?** | yes | no — echoed for user to add |
| **Right for** | auth interceptors, JWKS, audit, idempotency | Stripe billing, Twilio SMS, Clerk webhook user-sync |

## Rules

- Starters are one-shot. Forge does not roll your customizations back.
- A second `forge starter add` of the same starter skips files that already exist (no clobber). Pass `--force` to opt in.
- Dependencies are listed in the starter notes — `go get` / `npm install` them yourself when you want.
- For pure infrastructure, prefer a pack. Packs win when forge owning the upgrade path is a feature.
