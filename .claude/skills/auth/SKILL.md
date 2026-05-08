---
name: auth
description: Authentication, authorization, and multi-tenancy — forge.yaml config, JWT, API keys, RBAC, dev mode, and tenant scoping.
---

# Authentication & Authorization

Forge generates a layered auth system from proto annotations and `forge.yaml` config. Authentication (who are you?) is handled by interceptors; authorization (can you do this?) is handled by per-service authorizers.

## Auth Providers

Set the provider in `forge.yaml`:

```yaml
auth:
  provider: jwt       # "jwt", "api_key", "both", "none"
  jwt:
    signing_method: RS256
    jwks_url: "https://your-idp.com/.well-known/jwks.json"
    issuer: "https://your-idp.com/"
    audience: "your-api"
  api_key:
    header: X-API-Key  # default
```

- **jwt** — Validates Bearer tokens from the Authorization header using JWKS. Install the `jwt-auth` pack for production-ready validation.
- **api_key** — Validates API keys from a custom header (default `X-API-Key`). Install the `api-key` pack for key lifecycle management.
- **both** — Accepts either JWT or API key per request. The interceptor tries JWT first, then falls back to API key.
- **none** — No authentication. All requests proceed unauthenticated.

## Pack Layout: Nested Per-Pack Subpackages

Auth packs install their middleware code into a **per-pack Go subpackage**, nested under `pkg/middleware/auth/` (auth providers) or `pkg/middleware/audit/` (audit), never directly into `pkg/middleware/` itself. This is collision-by-construction — two auth packs can never overwrite each other's `auth_gen.go`, because the files live in different directories with different package decls. The extra `auth/` and `audit/` layer keeps the middleware tree organized as more packs ship.

| Pack | Subpackage path | Package decl | Key symbols |
|------|-----------------|--------------|-------------|
| `jwt-auth` | `pkg/middleware/auth/jwtauth/` | `jwtauth` | `Init`, `Close`, `Interceptor`, `DevAuthEnabled`, `Validator` |
| `clerk` | `pkg/middleware/auth/clerk/` | `clerk` | `Init`, `Close`, `Interceptor`, `DevAuthEnabled`, `Validator` |
| `api-key` | `pkg/middleware/auth/apikey/` | `apikey` | `NewValidator`, `Validator` (implements `middleware.KeyValidator`) |
| `audit-log` | `pkg/middleware/audit/auditlog/` | `auditlog` | `Interceptor` (DB-backed) |

External-service clients (e.g., `stripe`, `twilio`) install under `pkg/clients/<service>/`. Each pack declares its `subpath:` in `pack.yaml`; `forge pack list` shows the column so you can see at a glance which subtree a pack will touch.

The shared `Claims` type and `ContextWithClaims` / `ClaimsFromContext` helpers stay in `pkg/middleware/`. Pack subpackages import them.

### Composing two auth packs

The canonical observability chain is built by
`forge/pkg/observe.DefaultMiddlewares(...)` (recovery → request-id →
logging → tracing → metrics). Auth interceptors slot in as `Extras`,
so failures from auth still get logged / traced / counted by the
canonical chain. Example for jwt-auth + clerk together:

```go
// cmd/server.go (regenerated; the Extras pattern is forge-level convention)
import (
    "connectrpc.com/connect"
    "github.com/reliant-labs/forge/pkg/observe"

    "<module>/pkg/middleware/auth/jwtauth"
    clerkauth "<module>/pkg/middleware/auth/clerk"
)

projectInterceptors := []connect.Interceptor{
    jwtauth.Interceptor(),
    clerkauth.Interceptor(),
}
interceptors := observe.DefaultMiddlewares(observe.DefaultMiddlewareDeps{
    Logger: logger,
    Tracer: tracer,    // optional — nil disables tracing
    Meter:  meter,     // optional — nil disables metrics
    Extras: projectInterceptors,
})
opts := []connect.HandlerOption{connect.WithInterceptors(interceptors...)}
```

The order matters: within `Extras`, the first interceptor that
successfully attaches `Claims` to the context wins. For
JWT-or-Clerk-OR-fallback flows, write a small composite in `pkg/app/`
that tries each in turn — Forge intentionally ships no built-in
chooser.

`observe.DefaultMiddlewares` is the entry point; the full ordering
rationale lives in `forge/pkg/observe/middleware.go`. Custom chains
that want auth-first (or that omit certain layers entirely) can build
their `[]connect.Interceptor` directly without going through
`DefaultMiddlewares`.

## The Claims Struct

All auth flows produce the same canonical `Claims` struct in `pkg/middleware/claims.go`:

```go
type Claims struct {
    UserID string   `json:"user_id"`
    Email  string   `json:"email"`
    OrgID  string   `json:"org_id"`
    Role   string   `json:"role"`
    Roles  []string `json:"roles"`
}
```

Retrieve claims in handlers:

```go
claims, err := middleware.GetUser(ctx)
if err != nil {
    return nil, err  // already a CodeUnauthenticated connect error
}
```

If you need additional claim fields (e.g., `TenantID`, `Plan`), extend the `Claims` struct rather than creating a separate type.

## Unauthenticated Endpoints

The auth interceptor checks an allow-list before requiring auth. To allow additional unauthenticated endpoints, add them to the `unauthenticatedProcedures` map in `pkg/middleware/auth.go`:

```go
var unauthenticatedProcedures = map[string]struct{}{
    "/grpc.health.v1.Health/Check": {},
    "/grpc.health.v1.Health/Watch": {},
    "/myapp.v1.PublicService/GetStatus": {},  // add here
}
```

## RBAC via Proto Annotations

Annotate RPC methods with `required_roles` in your proto:

```proto
rpc CreateProject(CreateProjectRequest) returns (CreateProjectResponse) {
  option (forge.v1.method) = {
    auth_required: true
  };
}
```

`forge generate` produces `handlers/<svc>/authorizer_gen.go` with role mappings. Customize access control in `handlers/<svc>/authorizer.go` (yours to edit; delegates to the generated authorizer by default).

## Dev Mode

In development (`cfg.Environment == "development"`), bootstrap wires a `DevAuthorizer` that allows all requests. This is logged with a WARN at startup. Never use `DevAuthorizer` in production.

## Multi-Tenant Config

Enable row-level tenant isolation in `forge.yaml`:

```yaml
auth:
  provider: jwt
  multi_tenant:
    enabled: true
    claim_field: org_id    # JWT claim to extract tenant ID from (default: "org_id")
    column_name: org_id    # DB column for tenant scoping (default: "org_id")
```

Run `forge generate` after changing this config.

**How it works at runtime:** The `TenantInterceptor` (runs after `AuthInterceptor`) extracts the tenant ID from claims and injects it into context. Use `middleware.RequireTenantID(ctx)` or `middleware.TenantIDFromContext(ctx)` in handlers.

| claim_field | Claims field used |
|-------------|-------------------|
| `org_id` (default) | `claims.OrgID` |
| `user_id` / `sub` | `claims.UserID` |
| `email` | `claims.Email` |

When multi-tenant is enabled, entities with a field explicitly marked `tenant_key: true` in the plan are automatically scoped — generated CRUD handlers include `WHERE <tenant_col> = $tenantID` in every query. The `tenant_key` must be set explicitly; field names like `org_id` or `tenant_id` are NOT auto-detected.

## Frontend wiring (auth-ui pack)

The `auth-ui` frontend pack pairs with each auth backend pack to install
opinionated login / signup / session UI. Pick the backend first, then
pick the matching `--config provider=…`:

```bash
forge pack install auth-ui                              # default → jwt-auth
forge pack install auth-ui --config provider=clerk      # pulls in @clerk/nextjs
forge pack install auth-ui --config provider=firebase-auth  # pulls in firebase
```

The pack installs into every frontend declared in `forge.yaml` at
`src/components/auth/`. It ships:

- `LoginForm` / `SignupForm` — react-hook-form + zod validated forms (or
  Clerk/Firebase wrappers, depending on provider).
- `SessionNav` — header avatar dropdown with sign-out and an optional
  tenant switcher.
- `DevModeBanner` — visible warning when
  `NEXT_PUBLIC_AUTH_DEV_MODE=true`, mirroring the backend pack's
  `dev_mode: true` flag.
- `auth-store.ts` — Zustand store: `{user, session, isLoading,
  isAuthenticated}`. Subscribe to slices, never the whole store.

Wire in `src/app/layout.tsx`:

```tsx
import { DevModeBanner, SessionNav } from "@/components/auth";

export default function RootLayout({ children }) {
  return (
    <html><body>
      <DevModeBanner />
      <header className="flex items-center justify-between border-b px-6 py-3">
        <span className="font-bold">My App</span>
        <SessionNav />
      </header>
      <main>{children}</main>
    </body></html>
  );
}
```

For the `jwt-auth` variant, also rehydrate the persisted token at app
boot — see the rendered `src/components/auth/README.md` for the
`HydrateAuth` snippet. Clerk and Firebase variants manage rehydration
internally.

## Testing Auth

Inject claims into context directly in tests:

```go
ctx := middleware.ContextWithClaims(context.Background(), &middleware.Claims{
    UserID: "user-1",
    Email:  "test@example.com",
    OrgID:  "tenant-123",
    Role:   "admin",
})
```

For tenant context: `ctx = middleware.ContextWithTenantID(ctx, "tenant-123")`.

## Rules

- Never bypass auth by removing the interceptor — add procedures to the allow-list instead.
- Extend the `Claims` struct for custom fields; do not create parallel claim types.
- `authorizer_gen.go` is regenerated on every `forge generate` — customize auth logic in `authorizer.go`.
- Always use `connect.CodeUnauthenticated` for missing/invalid credentials and `connect.CodePermissionDenied` for insufficient roles.
- `TenantInterceptor` must come AFTER `AuthInterceptor` in the interceptor chain.
- `tenant_gen.go` is regenerated by `forge generate` — do not hand-edit.