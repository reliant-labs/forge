# F0b research: the login broker and native public-client PKCE

Status: research only, no code changed. Scope: the server, config and IdP facts that F0b
(system-browser PKCE, secure storage, refresh, logout, account isolation) depends on.
Line numbers are from this worktree.

## 1. What "the login broker" is

There are three layers. The control plane is a fourth, outside this repo.

| Layer | Where | Role |
|---|---|---|
| IdP | Zitadel, the dev engine `HostInfra.engine = "zitadel"` (`kcl/schema.k:1502,1573-1622`). Hosted: an instance the control plane converges | Issues JWTs. One port serves OIDC, `/v2` and `/management/v1` (`pkg/devidp/zitadel.go` `Client` doc) |
| Declaration | `forge.ManagedIdentity` / `IdentityApplication` / `IdentityLogin` (`kcl/schema.k:6701-6760, 6956-7145`) → release shape `ShapeIdentity` (`pkg/release/identity.go:40-41, 590`) | Declares roles and applications. `kind` is only `"native_broker" \| "oidc_web"` (`schema.k:6743`). `IdentityLogin.mode = "hosted"` is "PHASE 2 … nothing converges it yet" (`schema.k:6727-6732`) |
| Reconciler | `pkg/identity` (`spec.go`, `apps.go`, `reconcile.go`, `store.go`). Dev projection: `internal/devidentity/devidentity.go:407-447` | Converges Zitadel apps and publishes `Output` (issuer, audience=ProjectID, scopes, JWKS, per-app client_id/redirects; `store.go:160-264`) into typed config |
| Broker | Generated server code `internal/templates/project/app-login-broker.go.tmpl`, built on `pkg/devidp` (`flow.go`, `login.go`) | The product's own sign-in API. The server drives the whole OIDC flow |

**How the web app signs in today, end to end.** Mode `native` is the default. The name
means "the app's own form", not a mobile app.

1. The SPA's sign-in screen POSTs `{email,password,totp}` to `POST /auth/login`
   (`app-login-broker.go.tmpl:74-81, 276`). The browser never talks to the IdP (`pkg/devidp/flow.go:15-51`).
2. The server calls `devidp.Client.SignIn`, which runs these steps:
   - `BeginFlow` (`flow.go:116`) makes the server-side PKCE S256 pair and calls `/authorize` without following the redirect.
   - `CreateSession` (`login.go:114`) uses mandatory password/TOTP checks. They are not caller-chosen, to block impersonation (`login.go:46-60`).
   - `FinalizeAuthRequest` (`login.go:191`) needs the **broker machine token** (`IAM_LOGIN_CLIENT`, `config.proto.tmpl:443-485`).
   - `RedeemCode` (`flow.go:241`) is a public client with no secret (`flow.go:238`).
3. **The token lives in an HttpOnly, SameSite=Lax cookie `<name>_session` whose value IS the
   access token** (`tmpl:84-104, 390-410`). The response body carries only display claims (`identityFrom`, `tmpl:417`).
4. RPCs read the credential through `credentialFrom` in this order: Bearer header, then X-API-Key, then the session cookie
   (`internal/templates/project/middleware.go.tmpl:177-234, 270`; `pkg/authn/authn.go:226-256`).
5. **Refresh: none.** "the refresh token is discarded rather than used". The cookie expires with the
   access token, about 1h (`tmpl:380-389`). **Logout** only clears the cookie (`tmpl:366-377`). It does not revoke anything at the IdP and does not end the session.
6. Other routes: `GET /auth/session`, `/auth/options`, `/auth/password-policy`, `POST /auth/register|forgot|reset`
   (`tmpl:149-210`).

## 2. Can it serve a native public client today?

**Zitadel itself can.** Code+PKCE S256 with no secret, custom-scheme redirects, refresh tokens,
`/oauth/v2/revoke`, `/oidc/v1/end_session` and discovery are all standard Zitadel endpoints.
`pkg/identity/client.go:316` already reads `/.well-known/openid-configuration`. Zitadel rotates refresh tokens on each use, and the old token stops working.

**forge's library half-supports it:**
- `identity.AppNative` exists (`spec.go:232`). It maps to `OIDC_APP_TYPE_NATIVE` with auth method NONE, grants
  `AUTHORIZATION_CODE` + `REFRESH_TOKEN`, JWT access tokens and the role assertion (`apps.go:39-63`).
- Redirect and post-logout URIs are compared literally. `DevMode` turns them into globs and allows http (`spec.go:244-255`).
- `offline_access` is in the default `oidc_scopes` (`config.proto.tmpl:427-428`).

**What is missing:**
- **No declaration path to `AppNative`.** KCL accepts only `native_broker|oidc_web` (`schema.k:6743`), and so does the release
  validation (`release/identity.go:590`). The dev projection switch has no native case (`devidentity.go:430-445`).
  Nobody constructs `identity.AppNative`. The only uses are the two kinds above (`devidentity.go:432,441`).
- **The broker is cookie-only and has no refresh.** The native app cannot reuse `/auth/login`. It would get a cookie that holds an
  access token and no refresh token. It is also the wrong security model: a password posted to the API and no system browser.
- **Native in the IdP's eyes means "user goes to /authorize in a system browser".** The `native_broker` app sets
  `LoginURI` (login V2, `apps.go:65-70`), so `/authorize` for **that** client redirects into the web app's
  `/auth/sign-in?authRequest=…`. Nothing in the web flow handles an `authRequest` id that a third party started.
  `SignIn` always starts its own flow. Two options follow:
  - (a) A native app has login V1. Zitadel then renders its own hosted pages, which is the unconverged "hosted" mode and a seam in the product's sign-in UI.
  - (b) A native app's `LoginURI` points at the web `/auth/sign-in`. The broker then needs a new route that finalizes a **caller-supplied**
    `authRequest` with a verified session and 302s to the app's `myapp://` callback. That route is new and security-critical (see `login.go:46-60`).
- **No revoke/end-session wiring** anywhere in the broker, and no published `end_session`/`revocation` endpoint in `Output`.
- **No universal-link support.** No apple-app-site-association or assetlinks serving.

**Dev:** `forge env up dev` runs Zitadel on a host port (`devidentity.go` derives origins from the env). The iOS simulator shares
the host's loopback, so `http://localhost:<port>` works. The **Android emulator needs `10.0.2.2`**. Zitadel resolves
the instance from the Host header (`zitadel.go` `Client.Host`), so the external-domain mismatch has to be designed for. Options are `adb reverse` or a device-reachable dev origin. Physical devices need a LAN or tunnel origin.
`DevMode: true` is already forced for every dev app (`devidentity.go:415`), so a `myapp://*` glob would be accepted.

## 3. `pkg/oauth2`

`pkg/oauth2` is an RFC-tight **client** library: S256 verifier/challenge (`oauth2.go:87-245`), `AuthRequest.URL`
(`authorize.go:80`), code exchange (`exchange.go`), refresh with no verifier (`refresh.go:18`, a redacting
`RefreshToken` type), discovery with `SupportsS256` (`discovery.go:54-152`), and an RFC 8252 loopback login
(`loopback.go:60-69`, `127.0.0.1:0`). It is Go, so the TS app cannot import it. Use it as the **reference spec and test
oracle**: the same parameter set, the reserved-param guards, the state check and the redaction discipline. It is also the server-side
building block if the broker ever proxies refresh. Do not hand-roll a TS OAuth client. Use `expo-auth-session`'s PKCE
plus discovery, and copy the invariants from `pkg/oauth2`'s tests.

## 4. Bearer validation: will a native token be accepted?

`pkg/auth` validates the signature against the JWKS (live or the published snapshot). It enforces `iss` and `aud` only when they are set
(`auth.go:142-146, 542-546`), and reads roles from `urn:zitadel:iam:org:project:<aud>:roles`. In Zitadel the
audience is the **ProjectID** (`store.go:166-168`), and every app in the project is a client of that project. So a JWT
access token minted for a native app in the same project carries the same `iss` and `aud` plus the roles claim
(`apps.go:42-49` forces JWT plus the role assertion for every kind). **The generated services accept it unchanged** over
`Authorization: Bearer` (`middleware.go.tmpl:221`). Caveat: the scopes must include
`urn:zitadel:iam:org:project:id:<id>:aud` (published as `Output.Scopes`). Without that scope `aud` misses the project and every RPC 401s.

## 5. Declarative config seam

The path is KCL `IdentityApplication` → `release.ShapeIdentityApplication` (+validation `release/identity.go:590`) →
dev `devidentity.applicationsSpec` / hosted control-plane projection → `identity.Spec.Applications` →
`reconcile.go` (apps converge with drift detection, `apps.go:105-135`) → `Output.Apps[name].client_id` →
consumer config (`IdentityFields`, `schema.k:6756-6810`). The native client converges when
`kind = "native"` is added at each hop:
```
forge.IdentityApplication {name = "prod:mobile", workload = "api", kind = "native",
    redirect_uris = ["com.acme.app:/oauthredirect"], logout_uris = ["com.acme.app:/signedout"]}
```
- **Derivation rule:** the redirect comes from the Expo `scheme` / bundle id, not from an http origin. `applicationsSpec` currently
  derives `origin + "/auth/callback"` (`devidentity.go:420-426`).
- **Publication:** the client_id must reach the **mobile build's runtime config**, not a server workload. That is a new
  consumer kind; `IdentityConsumer` today is keyed by `workload`.
- `requiresKeys` (`reconcile.go:250-254`) is correctly false for native, since there is no secret.

## 6. Recommendation (minimal, durable)

1. **forge (dev + hosted shape):** add `kind = "native"` to `IdentityApplication`, the release shape, validation, and the
   `devidentity` switch to `identity.AppNative`. Redirects are required literals, or derived from the frontend's Expo scheme. Lint
   that the redirect is a private-use reverse-DNS scheme (RFC 8252 §7.1) or https. Publish `client_id`, issuer,
   `Output.Scopes` (+`offline_access`) and the discovery URL into the Expo frontend's generated runtime config.
2. **Broker:** add `POST /auth/authorize/finalize`, which completes a caller-supplied `authRequest` after the same mandatory
   credential checks and returns the callback for the native client. Option: point the native app's `LoginURI` at the web sign-in
   so the product UI is reused inside `ASWebAuthenticationSession`/Custom Tabs. The app then exchanges and refreshes **directly at the
   IdP token endpoint** as a public client, with no server proxy. Tokens go in `expo-secure-store`.
3. **Logout:** the app calls `/oauth/v2/revoke` for the refresh token, then optionally `end_session` with `id_token_hint`.
   Fix web logout in the same change. It currently leaves the IdP session alive.
4. **Dev reachability:** publish a device-reachable issuer (Android `10.0.2.2` / `adb reverse`) and test the Host-header
   instance resolution.
5. Keep the web cookie broker as it is. Its no-refresh limit (`tmpl:380-389`) is a separate web debt.

## Open decisions (owner)

- **Hosted public-client registration:** the control plane must accept and converge `kind = "native"`
  (`IdentityLogin.mode="hosted"` is unconverged). Who owns that change, and in which release?
- **Which login UI:** finalize through the product's own sign-in (the new finalize route, which is security-sensitive) or Zitadel-hosted
  pages (login V1 for the native app, which leaves a visible seam).
- **Callback identity:** private-use scheme versus https universal/app links. Universal links need forge to serve AASA/assetlinks
  and to decide on a team id or SHA-256 fingerprint. Decide also whether one client is shared across iOS/Android/dev builds or there is one per build variant.
- **Refresh policy:** token lifetimes, idle/absolute refresh expiry, and whether rotation reuse-detection is required.
- **Account isolation:** whether a second account means a second IdP session (`prompt=login`) and a per-account
  secure-store namespace. Also whether sign-out revokes all of the user's tokens or only this device's.
- **Test environment:** which named isolated instance and test accounts run credentialed F0b acceptance.

## Universal links later

Status: design note only. Nothing below is implemented; what ships now makes it a drop-in.

Already in place, so that universal/app links are additive:

- `redirect_uris` / `logout_uris` of a `kind = "native"` application are an ordered list (the shape keeps the
  order; every other kind is sorted). The converged client accepts a private-use scheme and an https URL side by
  side (proved against a real Zitadel in `internal/devidentity/hostzitadel_native_test.go`), and validation
  accepts both (`release.ValidateNativeCallbackURI`, mirrored by `IdentityApplication` in `kcl/schema.k`).
- The callback PATH is the same under every scheme: `<appid>:/oauthredirect` today, `https://<domain>/oauthredirect`
  later, and `/signedout` likewise, so the app's one route serves both.
- `EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI` is a single full URI, the FIRST redirect, so the client never branches on
  the scheme. Moving to universal links is therefore reordering the list.

What would change when we do it (and nothing else):

1. **KCL.** An optional `associated_domain: str = ""` on `IdentityApplication` (kind native only), additive. When
   set and `redirect_uris` is empty, derive `https://<domain>/oauthredirect` and `https://<domain>/signedout`
   ahead of the private-use pair (https first, private-use kept as the fallback for builds without the link).
   An explicit `redirect_uris` still wins. It is not added now: with no consumer it would be a field that does
   nothing.
2. **Served files.** The domain must serve `/.well-known/apple-app-site-association` (the iOS team id + bundle id,
   `applinks` details with `/oauthredirect` and `/signedout`) and `/.well-known/assetlinks.json` (the Android
   package + SHA-256 signing fingerprint). Both are public, static, JSON with no redirect, and must be served over
   https on that exact host. forge would render them from the declaration and publish them with the env's static
   site or gateway. Open decisions: where the Apple team id and the Android fingerprint are declared (per
   env: a debug and a release key differ), and whether dev gets a tunnel host.
3. **app.json.** `expo.ios.associatedDomains = ["applinks:<domain>"]` and an Android intent filter
   (`expo.android.intentFilters`: `autoVerify: true`, `scheme: https`, `host: <domain>`, `pathPrefix` of the two
   callback paths, categories `BROWSABLE` + `DEFAULT`). The template stays as is until a project opts in: these
   entries are written by the migration playbook, not scaffolded, because a project with no domain would
   otherwise ship an unverifiable claim.
4. **Redirect list order.** https first, private-use second. The IdP matches any registered entry, so existing
   installs that signed in with the private-use URI keep working while their stored session refreshes (the refresh
   token is not bound to the redirect), and new builds use the link. No client change: it reads the first entry.
5. **Hosted.** The control plane converges the list as declared, so the https entry needs nothing beyond what
   `kind = "native"` already requires of it (see the open decision at the top: hosted registration of native
   clients).
