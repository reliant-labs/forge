# Clerk Pack

Clerk authentication integration with JWKS-based JWT validation and a
dev-mode bypass. The pack ships only the auth-side machinery — the
JWKS rotation and Connect interceptor are pure infrastructure that
benefit from forge keeping them up to date.

For the **webhook side** of Clerk integration (user sync, org sync,
membership events), use the `clerk-webhook` starter — that's a
one-time scaffold rather than a tracked pack, because every project
customizes the user-sync code anyway.

```bash
forge pack install clerk
forge starter add clerk-webhook --service api   # if you also need user sync
```

## Installation

```bash
forge pack install clerk
```

## What Gets Generated

Auth code installs into `pkg/middleware/auth/clerk/` (its own Go subpackage,
package `clerk`) so multiple auth packs can coexist without colliding on
filenames in `pkg/middleware/`.

| File | Description |
|------|-------------|
| `pkg/middleware/auth/clerk/validator.go` | Clerk JWT validator (`clerk.Validator`) using JWKS from your Clerk domain |
| `pkg/middleware/auth/clerk/dev_auth.go` | Dev-mode bypass (`clerk.DevAuthEnabled`, `clerk.DevClaims`) when `CLERK_DOMAIN` is unset |
| `pkg/middleware/auth/clerk/auth_gen.go` | Connect RPC interceptor (`clerk.Init`, `clerk.Close`, `clerk.Interceptor`) — regenerated on `forge generate` |

## Configuration

The pack adds a `clerk` auth section to `forge.yaml`:

```yaml
auth:
  provider: clerk
  clerk:
    domain_env: CLERK_DOMAIN
    secret_key_env: CLERK_SECRET_KEY
    publishable_key_env: CLERK_PUBLISHABLE_KEY
```

| Variable | Purpose |
|----------|---------|
| `CLERK_DOMAIN` | Your Clerk domain (e.g., `example.clerk.accounts.dev`). When unset, dev mode activates. |
| `CLERK_SECRET_KEY` | Backend API key for Clerk SDK calls |

## Usage

### Session Validation

The generated `pkg/middleware/auth/clerk/auth_gen.go` interceptor validates Clerk
JWTs automatically. Clerk tokens include organization claims (`org_id`,
`org_role`, `org_permissions`) which are mapped to the standard `Claims`
struct for use with Forge's auth middleware. Wire it in `pkg/app/setup.go`:

```go
import (
    "connectrpc.com/connect"

    clerkauth "<module>/pkg/middleware/auth/clerk"
)

clerkauth.Init(logger)
defer clerkauth.Close()
interceptors := connect.WithInterceptors(clerkauth.Interceptor())
```

Then in handlers:

```go
claims := middleware.ClaimsFromContext(ctx)
// claims.UserID  → Clerk user ID (e.g., "user_2x...")
// claims.OrgID   → Active organization ID
// claims.Role    → Organization role (e.g., "org:admin")
// claims.Roles   → Organization permissions + role
```

### Dev Mode

When `CLERK_DOMAIN` is not set, the interceptor injects synthetic claims. Override dev identity with `DEV_USER_ID`, `DEV_ORG_ID`, `DEV_USER_EMAIL`, and `DEV_USER_ROLE` environment variables.

### Webhook User-Sync (separate scaffold)

```bash
forge starter add clerk-webhook --service api
```

This drops a Svix-verified webhook router into `pkg/clerk/webhook.go`. You
own the file thereafter. Implement `ClerkWebhookHandler` to persist users,
orgs, and memberships into your own data model — every project's user
table differs, so forge does not own a proto entity for this.

## Dependencies

- `github.com/clerk/clerk-sdk-go/v2`
- `github.com/MicahParks/keyfunc/v3`
- `github.com/golang-jwt/jwt/v5`

## Removal

```bash
forge pack remove clerk
```
