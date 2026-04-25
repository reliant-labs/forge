# JWT Auth Pack

Production-ready JWT authentication with JWKS auto-rotation, multi-provider support, and a dev-mode bypass for local development.

## Installation

```bash
forge pack install jwt-auth
```

## What Gets Generated

| File | Description |
|------|-------------|
| `pkg/middleware/jwt_validator.go` | Core JWT validator supporting JWKS, static RSA, and HMAC signing modes |
| `pkg/middleware/dev_auth.go` | Dev-mode bypass that injects synthetic claims when `ENVIRONMENT=development` |
| `pkg/middleware/auth_gen.go` | Connect RPC interceptor that wires the validator into your request pipeline (regenerated on `forge generate`) |

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

Call `InitAuth` at server startup to initialize the validator, then add `GeneratedAuthInterceptor()` to your Connect interceptor chain:

```go
if err := middleware.InitAuth(logger); err != nil {
    log.Fatal(err)
}
defer middleware.CloseAuth()

interceptors := connect.WithInterceptors(middleware.GeneratedAuthInterceptor())
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
