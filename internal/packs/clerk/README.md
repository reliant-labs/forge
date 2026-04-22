# Clerk Pack

Clerk authentication integration with JWKS-based JWT validation, user/organization webhook sync, and a dev-mode bypass.

## Installation

```bash
forge pack install clerk
```

## What Gets Generated

| File | Description |
|------|-------------|
| `pkg/middleware/clerk_auth.go` | Clerk JWT validator using JWKS from your Clerk domain |
| `pkg/middleware/auth_gen.go` | Connect RPC interceptor with Clerk-specific claim extraction (regenerated on `forge generate`) |
| `pkg/clerk/webhook.go` | Webhook router with Svix signature verification for user and org sync events |
| `proto/db/v1/clerk_user.proto` | User entity with `clerk_user_id`, email, name — ready for ORM codegen |

## Configuration

The pack adds a `clerk` auth section to `forge.yaml`:

```yaml
auth:
  provider: clerk
  clerk:
    domain_env: CLERK_DOMAIN
    secret_key_env: CLERK_SECRET_KEY
    publishable_key_env: CLERK_PUBLISHABLE_KEY
    webhook_secret_env: CLERK_WEBHOOK_SECRET
```

| Variable | Purpose |
|----------|---------|
| `CLERK_DOMAIN` | Your Clerk domain (e.g., `example.clerk.accounts.dev`). When unset, dev mode activates. |
| `CLERK_SECRET_KEY` | Backend API key for Clerk SDK calls |
| `CLERK_WEBHOOK_SECRET` | Svix signing secret for webhook verification |

## Usage

### Session Validation

The generated `auth_gen.go` interceptor validates Clerk JWTs automatically. Clerk tokens include organization claims (`org_id`, `org_role`, `org_permissions`) which are mapped to the standard `Claims` struct for use with Forge's auth middleware.

```go
claims := middleware.ClaimsFromContext(ctx)
// claims.UserID  → Clerk user ID (e.g., "user_2x...")
// claims.OrgID   → Active organization ID
// claims.Role    → Organization role (e.g., "org:admin")
// claims.Roles   → Organization permissions + role
```

### Dev Mode

When `CLERK_DOMAIN` is not set, the interceptor injects synthetic claims. Override dev identity with `DEV_USER_ID`, `DEV_ORG_ID`, `DEV_USER_EMAIL`, and `DEV_USER_ROLE` environment variables.

### Webhook Sync

Implement the `ClerkWebhookHandler` interface to sync Clerk events to your database:

```go
router := clerk.NewWebhookRouter(myHandler, "", logger)
mux.Handle("/webhooks/clerk", router)
```

The router handles these events: `user.created`, `user.updated`, `user.deleted`, `organization.created`, `organization.updated`, `organizationMembership.created`, `organizationMembership.deleted`. All payloads are verified with Svix signatures before dispatch.

## Dependencies

- `github.com/clerk/clerk-sdk-go/v2`
- `github.com/svix/svix-webhooks`

## Removal

```bash
forge pack remove clerk
```
