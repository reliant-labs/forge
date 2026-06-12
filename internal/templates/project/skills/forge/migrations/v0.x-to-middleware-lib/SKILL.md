---
name: v0.x-to-middleware-lib
description: Migrate pkg/middleware from ~25 scaffolded mechanism files to the forge libraries (pkg/authn, pkg/authz, pkg/middleware, pkg/observe) plus ONE thin user-owned policy file. Optional — old copies keep working; adopt to start receiving security fixes.
relevance: migration
---

# Migrating pkg/middleware to the forge middleware libraries

Use this skill when your project's `pkg/middleware/` contains the old
scaffolded mechanism files (`auth.go`, `cors.go`, `recovery.go`,
`ratelimit.go`, …) and you want to converge on the library shape that
fresh scaffolds use.

**This migration is OPTIONAL and never forced.** Your copies are
user-owned; nothing breaks if you keep them. The trade-off: field
evidence across downstream repos showed 23 of 25 of these files stay
byte-identical to the templates forever — photocopies of
security-critical code that drift and never receive fixes. The library
versions are versioned with forge and keep getting fixes.

## 1. What changed

Fresh scaffolds now emit exactly TWO files under `pkg/middleware/`:

- `middleware.go` — the thin, user-owned auth-policy file. It wires the
  four things projects actually customize: the token validator
  (`SetTokenValidator` / `ValidateToken`), the identity enricher
  (`enrichClaims`), the unauthenticated allow-list
  (`unauthenticatedProcedures`), and dev-claims behaviour
  (`devClaims`). It also owns `Claims` (alias of `auth.Claims`), the
  claims context key, and the `Authorizer` interface + `DevAuthorizer`.
- `middleware_test.go` — tests for that policy wiring only.

(The codegen siblings `auth_gen.go` / `tenant_gen.go` /
`auth_validator.go` are unchanged — still regenerated from forge.yaml.)

The mechanisms moved into libraries:

| Old project file | Library replacement |
|------------------|---------------------|
| `auth.go` (interceptor, modes, refusal) | `forge/pkg/authn` — `authn.NewInterceptor(authn.Policy{...})`; the thin file's `NewAuthInterceptor(devMode)` calls it |
| `authz.go` `AuthzInterceptor` | `forge/pkg/authz` — `authz.Interceptor(checker)`; the `Authorizer` interface + `GetUser` + `Action*` consts stay in the thin file |
| `permissive_authz.go` | `DevAuthorizer` in the thin file |
| `claims.go` | `Claims` alias + context key in the thin file |
| `cors.go` | `forge/pkg/middleware.CORSMiddleware` |
| `security_headers.go` | `forge/pkg/middleware.SecurityHeadersMiddleware` + `DefaultSecurityHeadersConfig` |
| `requestid.go` | `forge/pkg/middleware.RequestIDMiddleware` (shares `pkg/observe`'s context key) |
| `ratelimit.go` | `forge/pkg/middleware.RateLimitInterceptor(opts, middleware.ClaimsFromContext)` |
| `idempotency.go` (Connect) | `forge/pkg/middleware.IdempotencyInterceptor` |
| `audit.go` | `forge/pkg/middleware.AuditInterceptor(logger, middleware.ClaimsFromContext)` |
| `http.go` (`HTTPStack`, `HTTPAuth`) | `forge/pkg/middleware.HTTPStack(logger, middleware.ClaimsFromContext)`, `HTTPAuth(validate, withClaims)` |
| `logging.go`, `recovery.go`, `trace_handler.go`, `logevents.go` | `forge/pkg/observe` (`LoggingInterceptor`, `RecoveryInterceptor`, `DefaultMiddlewares`) — serverkit already wires these |
| `redact.go` | `forge/pkg/middleware.Redact` |

Claims-aware library pieces take YOUR `middleware.ClaimsFromContext`
as a callback — the claims context key stays project-owned, so
generated handlers and authorizers keep compiling unchanged.

## 2. Detection

```bash
# Old shape: mechanism files exist in the project tree.
ls pkg/middleware/ | grep -E 'auth\.go|cors\.go|recovery\.go|ratelimit\.go'

# New shape: one thin policy file delegating to pkg/authn.
grep -l "forge/pkg/authn" pkg/middleware/middleware.go
```

## 3. Migration steps

1. **Preserve your policy.** Diff your `pkg/middleware/auth.go` against
   the template-era original. The only diffs projects actually have are
   policy: a custom validator install site, claims enrichment, extra
   allow-list entries, dev-claims injection. Note them.
2. **Scaffold the thin file.** Copy `middleware.go` +
   `middleware_test.go` from a fresh `forge new` scaffold (or run
   `forge generate` after bumping forge — the scaffold writes them only
   if absent). Re-apply your policy: allow-list entries into
   `unauthenticatedProcedures`, enrichment into `enrichClaims`,
   dev-claims into `devClaims`.
3. **Update call sites** in `cmd/server.go` / `pkg/app/services_gen.go`
   — both are Tier-1 and regenerate to the library calls automatically
   on `forge generate`. Hand-written call sites map per the table above
   (note the claims-lookup argument on `AuditInterceptor`,
   `RateLimitInterceptor`, `HTTPStack`).
4. **Delete the old mechanism files** (all the table's left column,
   plus their `_test.go` siblings). The thin file + libraries replace
   them completely.
5. **Tidy and verify.**

   ```bash
   go mod tidy        # drops hashicorp/golang-lru + x/time if unused
   go build ./... && go test ./...
   forge audit        # no orphan warnings; checksums for the new pair
   ```

## 4. Behaviour contracts that must survive

Verify after migrating (the library tests cover these, but your wiring
can still break them):

- **Refusal-without-validator** — `ENVIRONMENT=production` + no
  validator + no pack + no `AUTH_MODE=none` → the server REFUSES to
  start.
- **`AUTH_MODE=none`** — explicit opt-out still serves unauthenticated.
- **401-on-empty-Authorization** — with a validator installed, a
  missing `Authorization` header on a non-allow-listed procedure is
  `CodeUnauthenticated`, never a silent pass-through.
- **Allow-list-only-unauthenticated** — exact procedure match only; a
  `HealthReport` RPC must not ride along with `Health/Check`.
- **Dev mode injection** — dev passthrough is driven by
  `cfg.Mode().IsDev()` handed to `NewAuthInterceptor`, not by the
  interceptor re-reading the environment.

## 5. Rollback

Your old files are in git history; the thin file and the old mechanism
files cannot coexist (duplicate symbols), so rollback is
`git checkout -- pkg/middleware/ && go mod tidy`.
