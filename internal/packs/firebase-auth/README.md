# Firebase Auth Pack

Firebase Authentication for Connect RPC services — JWKS validation
against Google's well-known endpoint, multi-project ID support, and a
dev-mode bypass for local development.

## Installation

```bash
forge pack install firebase-auth
```

## What Gets Generated

All files install into `pkg/middleware/auth/firebase/` (its own Go
subpackage, package `firebase`) so multiple auth packs can coexist
without colliding on filenames in `pkg/middleware/`.

| File | Description |
|------|-------------|
| `pkg/middleware/auth/firebase/validator.go` | Core Firebase JWT validator (`firebase.Validator`) backed by Google's JWKS endpoint with multi-project ID support |
| `pkg/middleware/auth/firebase/dev_auth.go` | Dev-mode bypass (`firebase.DevAuthEnabled`, `firebase.DevClaims`) that injects synthetic claims when no project IDs are set |
| `pkg/middleware/auth/firebase/auth_gen.go` | Connect RPC interceptor (`firebase.Init`, `firebase.Close`, `firebase.Interceptor`) — regenerated on `forge generate` |

## Configuration

The pack adds an `auth` section to `forge.yaml`:

```yaml
auth:
  provider: firebase
  firebase:
    project_ids_env: FIREBASE_PROJECT_IDS
    project_id_env: FIREBASE_PROJECT_ID
  dev_mode: true
```

At runtime, the validator reads these environment variables:

| Variable | Purpose |
|----------|---------|
| `FIREBASE_PROJECT_IDS` | Comma-separated list of accepted Firebase project IDs (multi-tenant) |
| `FIREBASE_PROJECT_ID` | Single project ID shortcut, used when `FIREBASE_PROJECT_IDS` is empty |
| `DEV_USER_ID` / `DEV_USER_EMAIL` / `DEV_USER_ROLE` | Dev-mode synthetic claim overrides |

In production, set `FIREBASE_PROJECT_IDS` (or `FIREBASE_PROJECT_ID`) to
the project IDs whose tokens this service accepts. With neither set,
the pack falls back to dev mode and injects synthetic claims.

## Usage

Call `firebase.Init` at server startup to initialize the validator,
then add `firebase.Interceptor()` to your Connect interceptor chain:

```go
import firebaseauth "<module>/pkg/middleware/auth/firebase"

if err := firebaseauth.Init(logger); err != nil {
    log.Fatal(err)
}
defer firebaseauth.Close()

interceptors := connect.WithInterceptors(firebaseauth.Interceptor())
```

The interceptor extracts the `Authorization: Bearer <token>` header,
validates the JWT against Firebase's JWKS endpoint, and injects
`Claims` into the request context. Handlers access claims via
`middleware.ClaimsFromContext(ctx)`.

### Firebase Claim Shape

Firebase ID tokens carry these standard claims, mapped to
`middleware.Claims`:

| Firebase claim | middleware.Claims field |
|----------------|-------------------------|
| `sub` (or `user_id` / `uid`) | `UserID` |
| `email` | `Email` |
| `org_id` (custom claim) | `OrgID` |
| `role` (custom claim) | `Role` |
| `roles[]` (custom claim) | `Roles` |

`org_id`, `role`, and `roles` are Firebase custom claims — set them via
the Firebase Admin SDK if your app uses them.

### Multi-Project Support

Set `FIREBASE_PROJECT_IDS=projA,projB,projC` to accept tokens from
multiple Firebase projects. The validator enforces both `aud` and `iss`
match the same project. Useful for control-plane services that serve
tenants across multiple Firebase projects.

### Dev Mode

When neither `FIREBASE_PROJECT_IDS` nor `FIREBASE_PROJECT_ID` is set,
the interceptor skips token validation entirely and injects synthetic
claims. This lets you develop and test without configuring a Firebase
project.

## Dependencies

- `github.com/golang-jwt/jwt/v5`
- `github.com/MicahParks/keyfunc/v3`

## Removal

```bash
forge pack remove firebase-auth
```
