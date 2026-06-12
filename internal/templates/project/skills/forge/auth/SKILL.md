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

## The Claims Struct

`Claims` is the canonical auth payload, defined in `forge/pkg/auth` and aliased in `pkg/middleware/middleware.go` (the thin user-owned auth-policy file — it also owns the claims context key):

```go
// pkg/middleware/middleware.go (user-owned, scaffolded once)
package middleware

import "github.com/reliant-labs/forge/pkg/auth"

type Claims = auth.Claims
```

```go
// forge/pkg/auth/auth.go
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
claims, ok := middleware.ClaimsFromContext(ctx)
if !ok {
    return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no claims"))
}
```

If you need additional claim DATA, prefer the `enrichClaims` hook in `pkg/middleware/middleware.go` — it runs after token validation and can hydrate roles/org/flags onto the validated claims before handlers see them. If you need additional claim FIELDS, add them to `forge/pkg/auth.Claims` so library code (auth interceptor, tenant interceptor) and project code share one type. Project-local extensions are not supported by the alias-based wiring; if you must, replace the `type Claims = auth.Claims` line with a struct that embeds `auth.Claims` and update the callbacks the file passes to the forge libraries.

## The thin policy file (pkg/middleware/middleware.go)

The auth MECHANISM (mode resolution, refusal-to-start, allow-list gate, Bearer parsing, claims plumbing) lives in `forge/pkg/authn`; the authorization interceptor in `forge/pkg/authz`; commodity middlewares (CORS, security headers, request-id, rate limit, idempotency, HTTP stack, audit) in `forge/pkg/middleware`. The project keeps ONE scaffolded-once file, `pkg/middleware/middleware.go`, wiring the four things projects actually customize:

1. **Token validator** — `SetTokenValidator(fn)` installs it; `ValidateToken` dispatches per-request, so install timing is flexible (setup.go, a pack `Init`, or later — but before `cmd/server.go` constructs the chain if you want validate mode).
2. **Identity enricher** — `enrichClaims(ctx, claims)` runs after validation; hydrate roles/org membership/feature flags from your DB here. Errors reject the request.
3. **Allow-list** — `unauthenticatedProcedures`, exact full-procedure strings only.
4. **Dev claims** — `devClaims()` returns the synthetic principal attached while auth is off (dev mode / `AUTH_MODE=none`). The scaffolded default is a fixed dev user (`UserID: "dev-user"`, `Email: "dev@localhost"`, `Role: "admin"`) so claim-demanding handlers (generated CRUD calls `GetUser`) work in dev with zero config; return nil to keep dev passthrough claim-free. Ignored entirely in validate/external-auth modes.

The file also owns `Claims`, the claims context key (`ClaimsFromContext` / `ContextWithClaims`), and the `Authorizer` interface + `DevAuthorizer` that generated code references — so regenerating never churns your handler-facing surface.

Auth mode resolution (in `forge/pkg/authn`, decided ONCE at interceptor construction): validator installed → validate every non-allow-listed request; `MarkExternalAuth()` called (packs do this) → passthrough; `AUTH_MODE=none` → passthrough; dev mode (injected from `cfg.Mode()`) → passthrough; otherwise **the server refuses to start**.

## How auth wiring works

The generated `pkg/middleware/auth_gen.go` is a ~40-line shim over `forge/pkg/auth`:

```go
var generatedAuthConfig = auth.Config{
    Provider:    "jwt",
    JWT:         auth.JWTConfig{SigningMethod: "HS256", ...},
    SkipMethods: []string{...},
}

func GeneratedAuthInterceptor() connect.UnaryInterceptorFunc {
    v, _ := auth.NewValidator(generatedAuthConfig)
    return v.Interceptor(auth.InterceptorOptions{}, ContextWithClaims)
}
```

`auth.NewValidator(cfg)` constructs a JWT/API-key validator. `v.Interceptor(opts, withClaims)` returns a Connect interceptor. The `withClaims` callback (`ContextWithClaims`) lives in `pkg/middleware/middleware.go`.

For API-key or `both` providers, pass a `KeyValidator` implementation:

```go
GeneratedAuthInterceptor(myKeyValidator)
```

`KeyValidator` is aliased to `auth.KeyValidator`; implement `ValidateKey(ctx, key) (*Claims, error)` against your storage.

The library reads `JWT_SECRET` from the environment when `JWTConfig.Secret` is empty (preserves the legacy template behaviour).

## Where the auth interceptor sits in the chain

`cmd/server.go` builds the canonical observability chain via
`observe.DefaultMiddlewares(...)` (recovery → request-id → logging →
tracing → metrics) and appends project-specific interceptors via
`Extras`. Auth is one of those Extras, so failures from the auth
interceptor are still observable (counted, traced, logged):

```go
// Constructed up front in runServer; an unconfigured auth provider is a
// startup error, not a per-request surprise.
authInterceptor, err := middleware.NewAuthInterceptor(cfg.Mode().IsDev())
if err != nil {
    return fmt.Errorf("auth configuration: %w", err)
}
projectInterceptors := []connect.Interceptor{
    authInterceptor,                    // ← here
    fmw.AuditInterceptor(logger, middleware.ClaimsFromContext),
}
interceptors := observe.DefaultMiddlewares(observe.DefaultMiddlewareDeps{
    Logger: logger,
    Extras: projectInterceptors,
})
```

The default ordering puts auth-after-observability deliberately —
operators want auth failures in the same dashboards as successful
traffic. To put auth first, drop it from `Extras` and prepend it onto
the result of `DefaultMiddlewares` directly. See
`forge/pkg/observe/middleware.go` for the full ordering rationale.

## Unauthenticated Endpoints

The auth interceptor checks an allow-list before requiring auth (exact procedure match only — the gate lives in `forge/pkg/authn`). To allow additional unauthenticated endpoints, add them to the `unauthenticatedProcedures` map in `pkg/middleware/middleware.go`:

```go
var unauthenticatedProcedures = map[string]struct{}{
    "/grpc.health.v1.Health/Check": {},
    "/grpc.health.v1.Health/Watch": {},
    "/myapp.v1.PublicService/GetStatus": {},  // add here
}
```

## RBAC via Proto Annotations

Annotate RPC methods with `auth_required` in your proto (there is no
`required_roles` annotation — role logic is code, not proto):

```proto
rpc CreateProject(CreateProjectRequest) returns (CreateProjectResponse) {
  option (forge.v1.method) = {
    auth_required: true
  };
}
```

`forge generate` produces `handlers/<svc>/authorizer_gen.go` with the per-method policy table (auth-required flags and declared error codes). Customize access control — including role checks — in `handlers/<svc>/authorizer.go` (yours to edit; delegates to the generated authorizer by default).

## Dev Mode

`forge run` defaults the children to `ENVIRONMENT=development` when nothing else sets it (per-env config or your shell always wins), so the canonical dev command never boots the server in production mode — where an unconfigured auth provider would refuse to start.

In dev mode (or `AUTH_MODE=none`) the auth interceptor runs in passthrough and attaches the synthetic principal from `devClaims()` in `pkg/middleware/middleware.go` to every request, so handlers and generated CRUD that demand claims via `middleware.GetUser` work with zero auth config. To disable, return `nil` from `devClaims()` — dev requests then carry no claims and claim-demanding RPCs return Unauthenticated. The dev principal is only consulted in passthrough mode; installing a validator or registering external auth makes `pkg/authn` ignore it.

Note the split: the `jwt-auth` pack is for REAL JWT validation (JWKS, issuer/audience checks) — it is not part of the dev path. You do not need any pack to develop locally.

In development (`cfg.Environment == "development"`), bootstrap also wires a `DevAuthorizer` that allows all requests. This is logged with a WARN at startup. Never use `DevAuthorizer` in production.

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

When multi-tenant is enabled, entities with a field explicitly marked `tenant: true` (the `(forge.v1.field)` annotation) are automatically scoped — generated CRUD handlers include `WHERE <tenant_col> = $tenantID` in every query. The `tenant` annotation must be set explicitly; field names like `org_id` or `tenant_id` are NOT auto-detected.

## Frontend wiring (auth-ui pack)

The `auth-ui` frontend pack pairs with each auth backend pack to install
opinionated login / signup / session UI. Pick the backend first, then
pick the matching `--config provider=…`:

```bash
# Default — pairs with the jwt-auth backend pack
forge pack install auth-ui                       # provider defaults to jwt-auth

# Pair with the clerk backend pack (pulls in @clerk/nextjs)
forge pack install auth-ui --config provider=clerk

# Pair with the firebase-auth backend pack (pulls in firebase)
forge pack install auth-ui --config provider=firebase-auth
```

The pack installs into every frontend declared in `forge.yaml` at
`src/components/auth/`. It ships:

- `LoginForm` — email/password form (or Clerk/Firebase wrapper) with
  `react-hook-form` + `zod` validation.
- `SignupForm` — registration form, where supported by the provider.
- `SessionNav` — header avatar dropdown with sign-out and an optional
  tenant switcher.
- `DevModeBanner` — visible warning when
  `NEXT_PUBLIC_AUTH_DEV_MODE=true`, mirroring the backend pack's
  `dev_mode: true` setting.
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