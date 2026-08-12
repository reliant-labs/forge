---
name: auth
description: Authentication — the owned SetupAuth validator (JWT default with OIDC discovery, Clerk/Firebase variants), the fail-closed interceptor, mixed public/authenticated APIs via auth_required, and how to tell from source whether a server authenticates. Sub-skills: authorization, dev-loop, frontend, api-keys.
---

# Authentication

Authentication is a **middleware, nothing more**: an interceptor validates the bearer token and puts `*Claims` on the context, **fail-closed** — no token, or an invalid one, is rejected before any handler runs. Forge scaffolds the validator choice in OWNED code (`SetupAuth`) and wires the interceptor.

**Which RPCs are callable without credentials is declared in the proto**, per rpc, with `option (forge.v1.method) = { auth_required: false }` — projected into `pkg/middleware/procedures_gen.go`, which the interceptor reads. No second list to keep in step.

Everything downstream reads the context: `middleware.GetUser(ctx)` answers **who the caller is**. What that caller may DO, and which rows they may see, is application policy no annotation can express.

Sub-skills: **`auth/authorization`** (`Enrich`, provisioning), **`auth/dev-loop`** (a token locally, the opt-in dev IdP), **`auth/frontend`** (`AuthProvider`, PKCE), **`auth/api-keys`**.

## Choosing the validator (owned code, not config)

Authentication is CODE, not a `forge.yaml` provider enum. Each service scaffolds one editable file, `internal/app/auth.go`, whose `SetupAuth` returns the validator `cmd serve.go` mounts. Picking a validator is a wiring choice; WHERE this deployment's issuer lives is per-environment DATA on the typed config (declared in `proto/config/v1/config.proto`, pinned in `deploy/kcl/<env>/config.k`). **`SetupAuth` reads no environment variable** — the app has one configuration channel.

```go
// internal/app/auth.go — YOURS (scaffolded once, never regenerated).
func SetupAuth(cfg *config.Config) (func(token string) (*auth.Claims, error), error) {
	jwksURL := cfg.JwtJwksUrl
	// Issuer but no explicit JWKS URL: ask the issuer where its keys are.
	if jwksURL == "" && cfg.JwtIssuer != "" && cfg.JwtSecret == "" {
		meta, err := oauth2.Discover(ctx, nil, cfg.JwtIssuer)
		if err != nil { return nil, fmt.Errorf("discover OIDC issuer %q: %w", cfg.JwtIssuer, err) }
		jwksURL = meta.JWKSURI
	}
	validator, err := auth.NewValidator(auth.Config{
		Provider: "jwt",
		JWT: auth.JWTConfig{
			SigningMethod: cfg.JwtSigningMethod,
			Issuer: cfg.JwtIssuer, Audience: cfg.JwtAudience,
			JWKSURL: jwksURL, Secret: cfg.JwtSecret,
		},
	})
	if err != nil { return nil, fmt.Errorf("build JWT validator: %w", err) }
	return validator.Validate, nil
}
```

**Nothing in the scaffold names an identity provider** — no vendor branch, no default endpoint. Every issuer arrives as a URL on the typed config.

- **Set `jwt_issuer` alone and the JWKS endpoint is DISCOVERED** from `/.well-known/openid-configuration` — one URL instead of two that must agree. Set `jwt_jwks_url` too and it WINS, which is what a containerized dev IdP needs.
- **Keys are fetched at BOOT** and refreshed in the background, so rotation needs no redeploy — and an unreachable issuer or JWKS endpoint REFUSES TO START, naming the URL. Never soften that into a warning: "boots, accepts nothing, reports healthy" is the worst of both.
- **`jwt_jwks_url` and `jwt_secret` are mutually exclusive** — one validator, one key source; both is rejected at startup, not resolved by precedence. For several issuers at once (an IdP migration), compose `auth.Config.TokenValidators`.
- **`jwt_secret` is `sensitive`** — it projects as a `secretKeyRef`, never an inline manifest value: an HS\* value both verifies AND mints. The OIDC client fields are NOT secrets (see the `auth/frontend` skill).
- **`jwt_signing_method` and the key must agree.** Default `RS256`, so a shared-secret `jwt_secret` needs `HS256` — otherwise a correctly-signed token is rejected on `alg` alone, which reads as a bad secret and is not.
- **Swap the validator freely** — arbitrary code, not a fixed enum. A nil validator makes the server **refuse to start**.

### Swapping the IdP

**Most swaps are a CONFIG change, not a code change.** Any standards-compliant issuer — Auth0, Keycloak, Zitadel, Okta, Supabase, a local dev IdP — needs no constructor: point `jwt_issuer` (plus `jwt_jwks_url` if discovery cannot describe it) at the new issuer and the default validator validates against its keys.

Only a NON-STANDARD claim shape needs code — edit `internal/app/auth.go` and return a different validator's `.Validate`:

```go
// Clerk — JWKS + Clerk's org/session claim shape (sub / org_id / org_role → Claims).
// Email isn't in a Clerk token by default; sync it via webhook.
validator, err := auth.Clerk(auth.ClerkOpts{JWKSURL: cfg.JwtJwksUrl, Issuer: cfg.JwtIssuer})

// Firebase — Google's JWKS + Firebase shape (sub = uid); token must match a project's
// aud AND issuer. Add a `firebase_project_ids` config field for the list.
validator, err := auth.Firebase(auth.FirebaseOpts{ProjectIDs: strings.Split(cfg.FirebaseProjectIds, ",")})
```

A constructor needing a value the config lacks gets a config field, never an `os.Getenv`. Other non-standard shapes ride `UserResolver`. Change the frontend `AuthProvider` to match — both halves must name the same issuer.

## The Claims struct

`Claims` is the canonical auth payload — `UserID` / `Email` / `OrgID` / `Role` / `Roles` — defined in `forge/pkg/auth` and aliased in the owned `pkg/middleware/middleware.go` (`type Claims = auth.Claims`), so library and project code name ONE type.

Read it with `middleware.ClaimsFromContext(ctx)` (`(*Claims, ok)`) on an RPC that may be anonymous, or `middleware.GetUser(ctx)` (`(*Claims, error)` — `CodeUnauthenticated` when none) on one that requires a principal. Additional claim DATA rides the `enrichClaims` hook; additional claim FIELDS go on `forge/pkg/auth.Claims`, never a parallel type.

## The wiring file + interceptor chain

The auth MECHANISM (mode resolution, refusal-to-start, allow-list gate, Bearer parsing, claims plumbing) lives in `forge/pkg/authn`. The project keeps ONE scaffolded-once file, `pkg/middleware/middleware.go`, wiring the two things projects customize: the **token validator** (passed EXPLICITLY into `NewAuthInterceptor(AuthDeps{Validate: fn})` — no package global) and the **identity enricher** `enrichClaims`. It also owns `Claims`, the claims context key, and `GetUser`. The allow-list is not among them — it is generated into `procedures_gen.go`.

**How to tell from SOURCE whether this server authenticates** — two facts, both in the repo, neither of them runtime state:

1. the generated `cmd serve.go` builds `middleware.AuthDeps{AnonymousOK: false}` and threads the OWNED `app.SetupAuth(cfg)` validator into `AuthDeps.Validate`;
2. the exempt RPCs are `procedures_gen.go`, projected from the protos' `auth_required` declarations.

Mode resolution happens ONCE at construction: a validator → enforce; `ExternalAuth` → passthrough (another interceptor owns identity); neither → **refuse to start**. **No environment variable can run a forge service without authentication**, and no config value can either — which is why the answer is readable in a diff rather than only observable in production.

Chain order (`observe.Chain`): **recovery → request-id → logging → tracing → metrics**, then **auth → audit → rate-limit**. Auth after observability is deliberate — operators see auth failures in the same dashboards as successful traffic.

### Publishing one RPC (per-RPC allow-list)

`auth_required: false` publishes ONE rpc to unauthenticated callers. It is
per-rpc, not a service posture, and the default is closed.

```proto
service Status {
  rpc GetVersion(GetVersionRequest) returns (GetVersionResponse) {
    option (forge.v1.method) = { auth_required: false };  // published deliberately
  }
  rpc GetDetail(GetDetailRequest) returns (GetDetailResponse) {}  // no annotation = closed
}
```

Opening an rpc is not the only way to serve a second audience: forge declares
multiple frontends and multiple services in `forge.yaml`, so separate audiences
can have separate applications against separate rpcs.

Each lands in the generated set as connect's own `…Procedure` constant, matched EXACTLY — never by substring, so a `HealthReport` rpc cannot ride along with the health probes. An unannotated rpc defaults to `auth_required: true`: silence never publishes an endpoint. The gRPC probes are always allowed; they run before anything can authenticate.

**A public handler gets NO claims — not even from a caller who presented a valid token.** The allow-list is checked BEFORE the Authorization header is read, so the validator is not consulted at all. Handle the anonymous case explicitly:

```go
if claims, ok := middleware.ClaimsFromContext(ctx); ok {
    return s.listFor(ctx, claims.UserID) // personalized
}
return s.listPublic(ctx)                 // anonymous — the normal case here
```

Two ways to get this wrong. `middleware.GetUser(ctx)` returns a `CodeUnauthenticated` **error** (it does not panic), so calling it here 401s every anonymous caller and defeats the annotation. And `claims, _ := ClaimsFromContext(ctx)` followed by `claims.UserID` **nil-panics** on every anonymous call — check `ok`.

To personalize for callers who DO present a token, validate it in the handler, or keep the RPC authenticated and add a separate public one. Not `AnonymousOK: true`: that makes auth non-gating service-wide, so `auth_required: true` gates nothing.

## Forge authenticates; it does not authorize

**Forge ships no authorization.** `auth_required: true` answers "is this caller
signed in", never "may they touch THIS row" — the interceptor rejects callers
with no valid token and then treats every authenticated caller alike. There is
no annotation, config field or generated hook that expresses a policy; designing
and enforcing one is application code.

Two forge-specific facts worth knowing before you write it:

- **Generated CRUD delegations carry no policy.** They compile, pass their
  scaffolded tests, and read as finished, so an unenforced rpc looks identical
  to an enforced one. `db/crud-overrides` covers the seams for putting a policy
  on one.
- **`middleware.GetUser(ctx)` is where the principal comes from.** The
  interceptor already proved the caller has an identity; that call gets it.

## Making a principal an application principal: the `Enrich` seam

The token says who the caller is; your database says what they are. `enrichClaims(ctx, claims)` runs AFTER validation and BEFORE any handler sees the claims — the one chokepoint where an IdP identity becomes an application principal. Hydrate roles/org by `claims.UserID` (the IdP `sub`), never by email.

**`Enrich` can REJECT**, and it is the earliest place you can: a `connect.Error` keeps its code verbatim (`PermissionDenied` stays `PermissionDenied`), a plain error becomes `Unauthenticated`. Right for whole-principal facts (suspended, no local account); wrong for per-resource decisions, which need the row and belong in the handler.

**Request VALIDATION is not its job.** Field rules are enforced separately and earlier by protovalidate, from `buf.validate` annotations on your protos — a malformed request is `InvalidArgument` before any handler or enricher runs. Field checks here would run only on authenticated requests and report a bad email as an auth failure.

**Provisioning is yours too** — your IdP does not populate your `users` table. `auth/authorization` has the enricher recipe with its error codes, plus the public `Register` RPC that inserts on the IdP-verified `sub`, never a body field.

## API keys

A bearer credential you own end to end: your table, your store, `forge/pkg/apikey`'s primitives (SHA-256 hash + indexed prefix + constant-time verify), and an `auth.KeyValidator` wired into `SetupAuth` with `Provider: "both"` to accept a JWT OR a key. Recipe: `auth/api-keys`.

## Dev mode

Dev relaxes only NON-security ergonomics (permissive CORS, verbose errors) — **authentication is enforced in every mode**, and no environment variable turns it off. A local call to a protected RPC needs a real token: mint one against an HS256 `jwt_secret` in the env's secret store (`forge secret set dev JWT_SECRET`), or use the dev IdP, which `forge run` brings up as a host process for any project that declares a frontend. It is Zitadel, and its instance is DECLARED in `idp-steps.yaml` — `forge run` reproduces it, credential included.

Load `auth/dev-loop` before debugging a local 401: it covers the traps that are not your wiring — the dev IdP answers to ONE hostname (`localhost`, which is why its dev loop is `forge run`, not a containerized app), and an OIDC app registered with the default token type mints OPAQUE access tokens no JWKS can validate.

## Frontend wiring

**Forge issues no tokens, so it ships no login form** — no `/auth/login` route, no `/login` page. Identity plugs into two seams: `SetupAuth` validates on the server, `AuthProvider` (`src/lib/auth/provider.ts`) supplies on the client. The PKCE browser flow (`forge/pkg/oauth2`) and its public, non-secret config (`oidc_client_id`, `oidc_redirect_uri`, `oidc_scopes`) are in `auth/frontend`.

## Testing auth

Inject claims: `middleware.ContextWithClaims(ctx, &middleware.Claims{UserID: "user-1", Role: "admin"})`, or `app.AuthedContext(t, testkit.WithUserID(...))`. Omit them to assert the `GetUser` 401 on a handler that requires identity — and that a PUBLIC handler still answers.

## Rules

- Authentication is **fail-closed** (`AnonymousOK: false`): no token, or an invalid one, is rejected before the handler. A nil validator makes the server **refuse to start**.
- **Whether this server authenticates is answerable FROM SOURCE** — `AuthDeps{AnonymousOK: false}` in the generated serve wiring plus the protos' `auth_required` declarations, both readable in a diff. No env var, no config field, and no runtime condition may decide it: an opt-out settable from a shell cannot be reviewed.
- `auth_required: false` on the rpc is the ONE way to publish an endpoint; it is enforced, not documentation. Never bypass auth by removing the interceptor.
- **A public handler gets NO claims, even from a caller who offered a token** — the allow-list is checked before any credential is. Handle both cases: `ClaimsFromContext` and check `ok`, never a blind deref.
- On an authenticated RPC, `middleware.GetUser` gives you the principal — not proof of authentication, which the interceptor already established.
- Custom claim DATA rides `enrichClaims`, not a parallel claim type. Hydrate by the IdP `sub`, never by email.
- **Input validation is protovalidate's job**, via `buf.validate` rules on the proto — not `enrichClaims`, which runs only on authenticated requests and reports failures as auth errors.
- Pin the dev IdP's image to an exact version; a moving tag makes "login broke today" unattributable. Declare its instance (`idp-steps.yaml`) rather than clicking through a console — a setup step that must be RUN is one a teammate's clone will not have.
- **Forge validates tokens; it never issues them.** No login/signup/logout route belongs in a service — implement `AuthProvider` against your IdP. Provisioning is yours: a public `Register` RPC that trusts the verified `sub`, never a body field.
- This skill ends at identity. **Forge ships no authorization** — what a caller may do is yours to design and enforce, and nothing in the framework will flag its absence.
