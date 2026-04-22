---
name: packs
description: Install, remove, and manage Forge packs — pre-built integrations for auth, payments, SMS, and more.
---

# Forge Packs

Packs are pre-built integrations that add working code to your project (auth, payments, email, audit). Unlike external dependencies, pack code lives in your project and is yours to customize.

## Commands

```bash
forge pack install <name>    # Install a pack
forge pack remove <name>     # Remove a pack
forge pack list              # Show available and installed packs
```

Always run `forge generate` after installing or removing a pack.

## Available Packs

| Pack | What it adds |
|------|-------------|
| `jwt-auth` | JWT validation with JWKS, dev-mode bypass, RS256 by default |
| `clerk` | Clerk authentication — JWKS validation, user sync webhooks, session management |
| `api-key` | API key lifecycle — create, list, revoke, rotate with secure hashing |
| `stripe` | Stripe payments — webhook handler, payment intents, customer sync |
| `twilio` | Twilio SMS/voice — send messages, delivery webhooks, message tracking |
| `audit-log` | Audit logging with DB persistence — tracks who did what with full RPC metadata |

## Pack Config in forge.yaml

Each pack adds a config section to `forge.yaml`. For example, after installing `jwt-auth`:

```yaml
packs:
  - jwt-auth

auth:
  provider: jwt
  jwt:
    signing_method: RS256
    jwks_url: ""
    issuer: ""
    audience: ""
  dev_mode: true
```

Edit these values to match your auth provider (Auth0, Supabase, Firebase, etc.).

## What install does

1. Renders template files into your project (e.g., `pkg/middleware/jwt_validator.go`)
2. Adds Go dependencies (`go get`)
3. Records the pack in `forge.yaml` under `packs:`
4. Runs `go mod tidy`

## File ownership

Pack files have overwrite policies:
- **always** — Regenerated on every `forge generate`. Do not hand-edit.
- **once** — Written on install only. Yours to customize.

Files marked `always` are re-rendered on every `forge generate` to stay in sync with your proto and config changes. Customize behavior in the companion "once" files.

## Rules

- Always run `forge generate` after installing or removing a pack.
- Do not hand-edit files marked `overwrite: always` — they are regenerated.
- A pack cannot be installed twice — remove it first to re-install.
- Some packs depend on each other (e.g., `api-key` sets `provider: both` which implies JWT). Check config defaults after install.
