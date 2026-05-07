---
name: packs
description: Install, remove, and manage Forge packs — pre-built infrastructure scaffolds for auth, audit, and other concerns forge maintains centrally. Distinct from starters (one-time business-integration copies).
---

# Forge Packs

Packs are pre-built **infrastructure** scaffolds that add working code to your project (auth middleware, JWKS rotation, audit interceptors, idempotency stores). Pack code lives in your project but forge maintains the install/upgrade lifecycle — `forge pack list` shows what's installed and `forge generate` re-renders the regeneratable bits.

Packs are the right shape when:
- The behavior is **infrastructure** (auth interceptor, JWKS refresh, audit middleware, idempotency keys, rate-limit primitives)
- Forge owning the upgrade path is a feature, not a tax (security fixes, library bumps, codegen alignment)
- Every project wants approximately the same thing

Packs are the **wrong** shape when every project diverges (Stripe billing logic, Twilio SMS workflows, Clerk webhook user-sync). For those, use **[starters](../../../skills/forge/starters/SKILL.md)** instead — `forge starter add <name>` is a one-time copy, the user owns the result.

## Commands

```bash
forge pack install <name>    # Install a pack (tracked in forge.yaml)
forge pack remove <name>     # Remove a pack
forge pack list              # Show available and installed packs
```

Always run `forge generate` after installing or removing a pack.

## Available Packs

| Pack | What it adds |
|------|-------------|
| `jwt-auth` | JWT validation with JWKS, dev-mode bypass, RS256 by default |
| `clerk` | Clerk JWKS validation + Connect interceptor (auth side only — for the webhook user-sync side use the `clerk-webhook` starter) |
| `firebase-auth` | Firebase Authentication — JWKS validation against Google's certs |
| `api-key` | API key lifecycle — create, list, revoke, rotate with secure hashing |
| `audit-log` | Audit logging with DB persistence — full RPC metadata + a `ListAuditEvents` read-side RPC |
| `nats` | NATS JetStream client + publisher + durable pull consumer with backoff/retry/DLQ |
| `data-table` | Generic React table wired to TanStack Query and forge's auto-generated `useEntities` hooks (frontend pack) |
| `auth-ui` | Login/signup/session UI; pairs with `jwt-auth`, `clerk`, or `firebase-auth` via `--config provider=…` (frontend pack) |

## Pack vs starter — when to reach for which

| | Packs | Starters |
|---|---|---|
| **Lifecycle** | install / upgrade / remove | one-time copy, then user owns it |
| **Tracked in `forge.yaml`?** | yes (`packs:` list) | no |
| **Re-rendered by `forge generate`?** | yes (the `generate:` block) | no, ever |
| **Auto-installs deps?** | yes (`go get`, `npm install`) | no — deps are echoed for the user to add |
| **Right for** | auth interceptors, JWKS, audit, idempotency | Stripe billing, Twilio SMS, Clerk webhook user-sync |

If a pack's templates have business logic that diverges per project, it should be a starter instead.

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
- For business integrations (Stripe, Twilio, Clerk webhook user-sync), reach for `forge starter add` rather than expecting a pack.
