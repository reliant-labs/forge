---
name: frontend
description: The browser half of authentication — the AuthProvider seam, the default credential-less session provider, wiring a real IdP's SDK, why forge scaffolds no first-party sign-in form, and the PKCE browser flow config (client ID, redirect URI, scopes) with forge/pkg/oauth2.
---

# Frontend auth wiring (the AuthProvider seam)

**Forge issues no tokens, so it ships no login form.** There is no `/auth/login`, `/auth/signup`, `/auth/logout` or `/auth/oauth/exchange` handler, and no `/login` page — a scaffold that shipped a form posting to one would be shipping a 404. Identity comes from your IdP; forge ships the seam it plugs into on both sides (`SetupAuth` validates, `AuthProvider` supplies).

The frontend seam is `AuthProvider` in `src/lib/auth/provider.ts` — `getToken` / `getUser` / `isAuthenticated` / `isLoading` / `login` / `logout` / `onAuthStateChange`. `src/app/providers.tsx` injects one into `<AuthContextProvider>`, and everything downstream reads `useAuth()`: the Connect transport takes its bearer token from `getToken` (`setAuthTokenGetter`), and `RuntimeShell` takes the session from the same object. **One credential, one source** — never cache the token twice, or the UI and the requests can disagree about who is signed in.

One provider ships before an IdP exists: `createSessionAuthProvider()` in `src/lib/auth/session-provider.ts` (the default). It holds no credential — forge issues no tokens — and its state is decided by what it was GIVEN:

- **Mock mode** (`NEXT_PUBLIC_MOCK_API` / `VITE_` / `EXPO_PUBLIC_`) — every RPC is answered locally, so the session is a FIXTURE and says so. `login()`/`logout()` move it for real, so a signed-out-first product can build its public/gated routes against it. Only pure mock hands the fixture token out; in `hybrid` a forwarded request carries no Authorization header rather than one no server would accept.
- **A real backend** — signed out, and it warns on first use: this bundle drives a real backend with nothing to present, so anything gated on identity 401s.

**Wiring a real IdP** is one file: implement `AuthProvider` over the provider's SDK and pass it in `providers.tsx`. Clerk → `@clerk/nextjs` (`useAuth().getToken`; sync the user into your table via webhook); Firebase → `firebase/auth` (`getIdToken`, bridge state with `onAuthStateChanged`); Auth0/Supabase → the same shape. Swap the backend validator to match (`auth.Clerk(...)` / `auth.Firebase(...)` in `SetupAuth`) and default to the provider's own hosted components for sign-in — see "First-party sign-in forms" below for the one case forge scaffolds a form instead.

Next.js frontends also ship `src/components/session_nav.tsx` — the signed-in-user dropdown with a sign-out — reading `useAuth()`, so it renders under whichever provider is wired. Mount it in `src/app/layout.tsx` or the sidebar footer. `src/components/ui/login_form.tsx` is the presentational form (an `onSubmit(email, password)` callback, no endpoint of its own) for apps that own a credential flow.

## First-party sign-in forms

Two claims are easy to conflate here; keep them separate. **True:** this app must never POST a password to its own forge backend — forge issues no tokens, there is no `/auth/login` endpoint, and a form field with nowhere legitimate to send it is a liability, not a feature. **False:** "therefore a first-party sign-in form is impossible." It is not. A form that sends the password straight to the IdP's own API over `fetch` never lets this app touch the credential — this app still holds nothing — and PKCE's security properties are unchanged; only the UI moved.

**forge does not scaffold one, and that is a portability judgement rather than a security one.** Driving a hosted sign-in from your own form is proprietary in every implementation: there is no standard for it the way OIDC discovery generalizes the redirect flow, so generated code would fit exactly one vendor and mislead everyone else. `frontends[].auth_mode` accepts only `redirect`; a first-party form is application code you own, against your IdP's own API.

**What it costs, if you build it.** `redirect` is portable across every IdP, and MFA, social sign-in, and password reset all come free because the provider's own hosted pages implement them. A first-party form buys branding — the user never leaves your product — and you inherit responsibility for everything those pages were doing for you. Expect to need a same-origin proxy too: providers commonly answer these APIs with no `Access-Control-Allow-Origin` and ride a session cookie that `SameSite=lax` drops cross-site. `src/components/ui/login_form.tsx` is the presentational form (an `onSubmit(email, password)` callback, no endpoint of its own) if you want a starting point.

**A native frontend has no same-origin proxy to run such a flow through**, so `react-native` signs in with `redirect` (the system browser) or the IdP's own native SDK.

**Against the scaffolded dev IdP specifically**, note that Zitadel's session API (`POST /v2/sessions`) is *not* the shape to build this on: it requires a privileged service-account token, which a browser must never hold, and it returns an encrypted session token (a JWE) rather than a JWT your backend could validate. Signing in through it means a backend that holds the IdP's admin credential and finalizes the OIDC auth request server-side — a different architecture from "the browser talks to the IdP", and one forge deliberately does not generate.

## The PKCE browser flow (when you own the redirect)

An IdP with no usable SDK, or a CLI, needs the flow itself. `forge/pkg/oauth2` implements the client half — RFC 7636 PKCE plus the authorization-code exchange, standard library only. It **obtains** tokens; it never validates them, and the forge server is not a participant in the flow.

Four config fields describe the client (`proto/config/v1/config.proto`):

| field | why it is not a secret |
| --- | --- |
| `oidc_client_id` | Ships in the bundle and rides every authorization request's query string. An identifier, not a credential. |
| `oidc_redirect_uri` | Public by construction, and compared LITERALLY by the issuer — the registered allow-list is what stops a code being redirected to an attacker's host. (The scaffolded dev IdP globs it instead, so a dev frontend keeps an ephemeral port; that relaxation is local-only — see `auth/dev-loop`.) |
| `oidc_scopes` | Requested capability names. `openid` must be present or the request is bare OAuth 2.0, not OIDC. `offline_access` is what earns a refresh token — see "Staying signed in" below. |
| `jwt_issuer` | The one URL the endpoints are DERIVED from, via `oauth2.Discover`. |

**Which side reads them matters.** A browser bundle cannot read the Go server's config object: it takes these values from its own build-time public env (`NEXT_PUBLIC_OIDC_*` / `VITE_OIDC_*`), which is where the scaffolded frontend provider looks. The Go fields above are for a server-side or CLI participant in the flow. The two must agree on the SAME issuer and client id — that is the coupling to check when a login works in one and not the other. Public env is fine for exactly these values and for nothing else: it is inlined into the bundle, so a token or secret placed there is published.

**A public client has no client secret — that is the point of PKCE.** The per-attempt code verifier replaces it, so there is no confidential value to leak from a browser. `oauth2.TokenRequest.ClientSecret` exists for CONFIDENTIAL (server-side) clients only; the browser flow leaves it empty. Do not add a client-secret config field for a browser client, and if you add one for a confidential client, mark it `sensitive: true`.

```go
v, _ := oauth2.NewVerifier()          // persist v.Secret() + state server-side;
state, _ := oauth2.NewState()         // they must survive the redirect
meta, err := oauth2.Discover(ctx, nil, cfg.JwtIssuer) // one URL -> all endpoints
u, _ := oauth2.AuthRequest{
    Endpoint: meta.AuthorizationEndpoint, ClientID: cfg.OidcClientId,
    RedirectURI: cfg.OidcRedirectUri, Scopes: strings.Split(cfg.OidcScopes, ","),
    State: state, Challenge: v.Challenge(),
}.URL()
// ...on return, CHECK STATE BEFORE TRUSTING ANYTHING:
if err := oauth2.CompareState(state, r.URL.Query().Get("state")); err != nil {
    return err // errors.Is(err, oauth2.ErrStateMismatch) — treat as CSRF
}
tok, err := (&oauth2.Exchanger{}).Exchange(ctx, oauth2.TokenRequest{
    Endpoint: meta.TokenEndpoint, ClientID: cfg.OidcClientId,
    RedirectURI: cfg.OidcRedirectUri, Code: r.URL.Query().Get("code"), Verifier: v,
})
```

`Verifier` and `Token` redact themselves under `fmt`, `slog` and `encoding/json` — marshalling one FAILS rather than emitting the secret, so a verifier cannot end up in a log line by accident. Get the value out deliberately with `.Secret()`.

## Staying signed in past the access token's hour

An access token typically lives an hour. Without a refresh, the session simply stops: `getToken()` returns null, the transport sends no `Authorization` header, the server rejects, and the user is **silently signed out mid-task** — which is worst precisely when they have unsaved work.

The scaffolded provider refreshes ON DEMAND inside `getToken()` when the token is expired or within `REFRESH_SKEW_MS` (30s) of it. Three things about it are load-bearing:

- **`offline_access` AND `prompt=consent`, together.** OIDC Core §11 permits the authorization server to IGNORE `offline_access` unless the prompt contains `consent`, and strict providers do it **silently** — the login succeeds, the token response just arrives with no `refresh_token` and a granted `scope` that quietly dropped it. Providers differ: Zitadel v4.16.2 (the dev IdP) is lenient and returns a refresh token without the prompt (verified); implementations built on node oidc-provider are not. The scaffold DERIVES the prompt from the scopes (`promptFor()`) so the two cannot drift. Sending it costs nothing where it is not required — do not delete it because the dev IdP tolerates its absence.
- **Single-flight.** Ten concurrent RPCs must trigger exactly ONE refresh. Providers ROTATE the refresh token on every use, and a re-presented old one is treated as theft — at worst revoking the whole grant and signing the user out completely. Some allow a few seconds' grace, so removing the coalescing can **appear to work**; Zitadel v4.16.2 allows none (verified: a replayed token is rejected immediately, though it spares the newest one). `refreshInFlight` in `oidc-provider.ts` is that mechanism.
- **Store the rotated token.** `refreshToken()` (browser) and `Exchanger.Refresh` (Go) both return the token to present NEXT — rotated when the provider rotated, otherwise the one just used. Store it unconditionally; there is no branch to get wrong.

On failure the scaffold **fails closed and does not retry**: it clears the session, emits signed-out, and warns. Whether that should instead redirect is app policy, documented at `onRefreshFailed` in your own `oidc-provider.ts`. Do NOT add a background timer that refreshes on a schedule — that keeps an abandoned tab's session alive indefinitely.

## Rules

- **One credential, one source.** The transport's `getToken` and the UI's session must read the same provider object.
- Never put a token in the bundle's public env (`NEXT_PUBLIC_*` / `VITE_*` / `EXPO_PUBLIC_*`) — those are inlined at build time and readable by anyone who loads the page.
- Check `state` before exchanging a code, always. An unchecked `state` is a CSRF hole, not a formality.
- The access token your backend validates must be a **JWT**. Some IdPs issue an OPAQUE access token unless the client requests a registered API resource/audience — an opaque string will never validate against a JWKS, however correct the rest of the wiring is. Request the audience, and set `jwt_audience` to match.
