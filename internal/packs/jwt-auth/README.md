# JWT Auth Pack

Production-ready JWT authentication with JWKS auto-rotation, multi-provider support, and a dev-mode bypass for local development.

## Installation

```bash
forge pack install jwt-auth
```

## What Gets Generated

All files install into `pkg/middleware/auth/jwtauth/` (its own Go subpackage,
package `jwtauth`) so multiple auth packs can coexist without colliding
on filenames in `pkg/middleware/`.

| File | Description |
|------|-------------|
| `pkg/middleware/auth/jwtauth/validator.go` | Core JWT validator (`jwtauth.Validator`) supporting JWKS, static RSA, and HMAC signing modes |
| `pkg/middleware/auth/jwtauth/dev_auth.go` | Dev-mode bypass (`jwtauth.DevAuthEnabled`, `jwtauth.DevClaims`) that injects synthetic claims when `ENVIRONMENT=development` |
| `pkg/middleware/auth/jwtauth/auth_gen.go` | Connect RPC interceptor (`jwtauth.Init`, `jwtauth.Close`, `jwtauth.Interceptor`) — regenerated on `forge generate` |

## Configuration

The pack adds an `auth` section to `forge.yaml`:

```yaml
auth:
  provider: jwt
  jwt:
    signing_method: RS256   # RS256, ES256, or HS256
    jwks_url: ""            # JWKS endpoint for key rotation
    issuer: ""              # Expected token issuer
    audience: ""            # Expected token audience
  dev_mode: true            # Bypass validation in development
```

At runtime, the validator reads these environment variables:

| Variable | Purpose |
|----------|---------|
| `JWT_JWKS_URL` | JWKS endpoint (recommended for production — keys rotate automatically) |
| `JWT_ISSUER` | Expected `iss` claim |
| `JWT_AUDIENCE` | Expected `aud` claim |
| `JWT_SIGNING_METHOD` | Algorithm (defaults to `RS256`) |
| `JWT_HMAC_SECRET` | Shared secret for HMAC signing (simple setups) |
| `JWT_RSA_PUBLIC_KEY` | PEM-encoded RSA public key (static key setups) |

Exactly one of `JWT_JWKS_URL`, `JWT_HMAC_SECRET`, or `JWT_RSA_PUBLIC_KEY` must be set in production.

## Usage

Call `jwtauth.Init` at server startup to initialize the validator, then add `jwtauth.Interceptor()` to your Connect interceptor chain:

```go
import "<module>/pkg/middleware/auth/jwtauth"

if err := jwtauth.Init(logger); err != nil {
    log.Fatal(err)
}
defer jwtauth.Close()

interceptors := connect.WithInterceptors(jwtauth.Interceptor())
```

To run jwt-auth alongside another auth pack (e.g. clerk), compose them with
`connect.WithInterceptors`:

```go
import (
    "<module>/pkg/middleware/auth/jwtauth"
    clerkauth "<module>/pkg/middleware/auth/clerk"
)

jwtauth.Init(logger)
clerkauth.Init(logger)
interceptors := connect.WithInterceptors(
    jwtauth.Interceptor(),
    clerkauth.Interceptor(),
)
```

The interceptor extracts the `Authorization: Bearer <token>` header, validates the JWT, and injects `Claims` into the request context. Handlers access claims via `middleware.ClaimsFromContext(ctx)`.

### Dev Mode

When `ENVIRONMENT=development` (or `dev`), the interceptor skips token validation entirely and injects synthetic admin claims. This lets you develop and test without running an identity provider.

### JWKS Support

In JWKS mode, the validator fetches the provider's JSON Web Key Set on startup and refreshes keys automatically in the background. This supports seamless key rotation from providers like Auth0, Supabase, and Firebase without restarts.

## Dependencies

- `github.com/golang-jwt/jwt/v5`
- `github.com/MicahParks/keyfunc/v3`

## Removal

```bash
forge pack remove jwt-auth
```
