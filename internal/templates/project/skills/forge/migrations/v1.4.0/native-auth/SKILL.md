---
name: migration-v1.4.0-native-auth
description: Adopt v1.4.0 native sign-in in an existing Expo app — OAuth public client (authorization code + PKCE) through the system browser, tokens in the OS keystore, the EXPO_PUBLIC_FORGE_OIDC_* variables forge env up injects, the new dependencies and expo-secure-store plugin, the single reverse-DNS scheme, the owned auth files to copy and the provider/context/connect/layout/jest edits, mock fallback and opt-out, what the auth context and transport now do, and what is not yet proven (Android, hosted registration) and how a physical device signs in with --device-host.
relevance: migration
---

# v1.4.0 — native auth

**Use when** the project has an Expo frontend. New scaffolds include everything
below; existing apps own these files, so forge never backfills them. Adapt by
hand. Server side (kind-native client, callbacks, env vars):
`forge skill load deploy/managed-identity/mobile`.

## What an existing app gets without doing anything

Nothing changes until `EXPO_PUBLIC_FORGE_OIDC_ISSUER` and
`EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID` are set in the bundle. `forge env up` injects
them (with `_SCOPES`, `_REDIRECT_URI`, `_POST_LOGOUT_REDIRECT_URI`) for a native
frontend on a host-run identity; Expo inlines only static `process.env.NAME`
reads, so restart Metro after they change. The five are injected as **one unit**:
if any layer (your shell, KCL `env_vars`, the frontend's `.env.<env>`) sets
`_ISSUER` or `_CLIENT_ID`, forge injects none of them and prints one line saying
so (the scopes and redirects belong to its dev client, not to the provider you
chose); otherwise each variable no layer has set is filled, so one can still be
overridden alone. Apps with their **own** `AuthProvider`
keep working: `restore`, `signIn` and `onTokenRejected` are optional members, and
the context calls them only when present. **Opt out** by leaving the
`EXPO_PUBLIC_FORGE_OIDC_*` variables unset: the app stays on the mock session
provider. An empty value does not hold forge back (it injects over an empty
one), so under `forge env up` on a host-run identity either keep the old
`context.tsx` or delete the OIDC branch of `chooseAuth`.

## Behaviour

- `src/lib/auth/provider.ts`: `AuthProvider` gains optional `restore?()`,
  `signIn?()` (null = cancelled, not an error) and
  `onTokenRejected?(rejectedToken?)` (true = a different token is ready).
- RN `AuthContextValue` (`use-auth.ts`) gains `signIn`, `requiresSignIn` and
  `configError`.
- Provider choice, once at mount: valid OIDC variables → the OIDC provider;
  absent → the mock provider, one-time warning, `requiresSignIn` false; present
  but invalid → signed out, `configError` set, `console.error`, **no** fallback.
- Mount runs `restore()` under `isLoading`. With OIDC the context wires
  `setAuthTokenGetter` and `setTokenRejectedHandler` from `connect.ts` and unwires
  both on unmount. Mock mode wires nothing, so it sends no `Authorization` header.
- `logout()` runs `endSession(queryClient)` (epoch bump, abort in flight,
  `cancelQueries`, `clear`) and then `provider.logout()` (revoke best effort,
  wipe the keystore); identity flips even if the wipe throws. The provider's
  `onSessionEnded` (refresh token refused) and a changed `sub` end the old
  session the same way.
- `connect.ts` interceptors, outermost first:
  `[authExpired, epoch, tokenRejected, ...buildRuntimeInterceptors(...)]`.
  `authExpired` emits `auth:expired` when a call ends `Unauthenticated`. `epoch`
  merges the identity signal into each request and drops a response that lands
  after the epoch moved (a `Code.Canceled` error). `tokenRejected` handles a 401
  by calling the handler once and replaying a **unary** call once, **only** when
  the proto declares the method `idempotency_level = NO_SIDE_EFFECTS` or
  `IDEMPOTENT`. forge's authn answers a bare `Unauthenticated` with no
  `x-forge-error-reason`, so a 401 cannot prove a write never ran; a
  non-idempotent call still gets its token renewed but its 401 propagates, and
  the user retries. Streams are never replayed.
- Sessions end, not linger: a refresh the IdP refuses for good (`invalid_grant`,
  or any non-retryable 4xx; Zitadel sends `400 invalid_request` for a revoked
  token) or answers for a different `sub` wipes memory and keystore. 408, 429
  and 5xx keep the session. The token store runs every operation in call order,
  and a stored record missing its id token or expiry is treated as no session.
- Routes: `app/_layout.tsx` guards `index` and `sign-in` with `Stack.Protected`
  (signed in or `!requiresSignIn` → `index`, otherwise `sign-in`); `/oauthredirect`
  and `/signedout` are unguarded and redirect to `/`.

## Migrate

1. **Dependencies.** Add `"expo-crypto": "~58.0.7"`, `"expo-secure-store":
   "~58.0.2"`, `"expo-web-browser": "~58.0.7"` and devDependency
   `"test-renderer": "1.3.0"` (the context tests render with it); reinstall.
2. **`app.json`.** Add the plugin `["expo-secure-store", { "faceIDPermission":
   false }]` to `expo.plugins`, and set `expo.scheme` to **one string**, a valid
   reverse-DNS id: lowercase letters, digits and `-`, at least one `.`
   (`com.<project>.<frontend>`, lowercase, no separators, in new scaffolds). The
   redirect is `<scheme>:/oauthredirect` and the OS only returns to the app for
   a registered scheme. **Never change an existing `ios.bundleIdentifier` or
   `android.package`**: changing either makes the OS treat the app as a
   different one, so a published app would be renamed. forge reads only
   `expo.scheme`; the bundle id and package stay as they are, whatever they
   spell. A string, not an array: a second entry makes expo-linking warn on
   every launch (it uses the first and ignores the rest). **A custom scheme you
   already have** (say `["taskapp", "mobile"]`) is not lost by this: either list
   the reverse-DNS id first and keep yours after it (forge takes the first valid
   entry; expect the expo-linking warning), or keep your scheme alone and
   declare the callback yourself, `redirect_uris = ["com.acme.mobile:/oauthredirect"]`
   on a `kind = "native"` `forge.IdentityApplication` (the callback scheme must
   still be registered in `expo.scheme`), or set `native_sign_in = False` on the
   frontend. Existing `<frontend>://` deep links keep working only while their
   scheme stays registered. `forge env up` warns when no scheme qualifies.
   Rebuild the dev client: plugins and schemes are native config, and Expo Go
   cannot register an OAuth redirect.
3. **Copy the owned files** from the current scaffold (do not overwrite edits;
   how to get them: next paragraph): `src/lib/auth/{config,oidc,token-store,oidc-provider,session,native,transport,sign-in-error}.ts`,
   `app/{sign-in,oauthredirect,signedout}.tsx` (the sign-in screen is a plain
   placeholder: replace it), and the tests `tests/auth-fakes.ts` (a shared helper, not a test), `tests/auth-{config,oidc,
   store-session,provider,sign-in-error}.test.ts`, `tests/auth-context.test.tsx`
   and `tests/connect-transport.test.ts` (it exercises the `connect.ts` edits of
   step 4, so it belongs here, not in the dev-loop playbook).
   `tests/app-config.test.ts` asserts this step's contract (one string scheme;
   the scaffold's version also expects it to equal the bundle id, which is only
   true of apps scaffolded at v1.4.0): replace it after the owned-files copy
   (if you keep a bundle id that differs, drop that assertion). With the
   project at the pinned forge, `forge project rescaffold <path>...` writes any
   file that is absent (the old `tests/app-config.test.ts`: delete it first), and
   `forge project upgrade --force <path>` replaces one that exists, untouched or
   not (`forge project upgrade --check <path>` prints its diff first).
4. **Edit** `src/lib/auth/provider.ts` (optional members), `use-auth.ts` (three
   new members), `context.tsx` (`chooseAuth`, restore effect, layout effect
   wiring the transport, ended-listener effect, `logout`, `signIn`) and
   `src/lib/connect.ts` (`setTokenRejectedHandler`, the three interceptors and
   the epoch helpers). `session-provider.ts` is unchanged. Merge by hand if any of
   these were customized; the scaffold's comments in each file explain the seams.
   `forge project upgrade --check <path>` prints each file's diff against the
   template; one you never edited is `forge project upgrade --force <path>`.
   Web frontends carry the same `src/lib/auth/provider.ts` (the optional
   members are in the shared template): adopt it there too, or the next
   `forge generate` leaves web and native typed differently. The edits are
   additive and break nothing.
5. **`app/_layout.tsx`.** Replace the bare `<Stack>` with the `AuthStack` guard
   from the scaffold (restoring spinner while `isLoading && requiresSignIn`).
   Put your own signed-in screens inside the first `<Stack.Protected>`.
6. **`jest.config.cjs`.** Remove `@connectrpc/connect` from `esmPackages` (keep
   `@connectrpc/connect-web` and `@bufbuild/protobuf`). It ships CJS; Babel
   transforming it makes `ConnectError` a class the runtime's `ConnectClientError`
   cannot extend, so every Connect error-path test became `Code.Unknown`. If you
   customized the pattern, keep your changes and drop only that package.
7. **Verify.** `pnpm run typecheck`, `pnpm run lint --max-warnings 0`,
   `pnpm run test`, and `expo export` for both platforms. The auth suites run
   under the existing `tests/**` match.

## What is not proven

Evidence is JS-level (Jest with fakes, typecheck, lint, export) plus a local dev
identity on the iOS simulator. **Not proven here:** Android
(emulator included), registering native clients with a hosted control plane
(forge refuses `kind = "native"` on a hosted identity until it does), and universal links
(the callback path is identical, so adding one is a redirect-URI change).
A physical device signs in with `forge env up --device-host <LAN address>`: the dev IdP
then also serves that address, the run's one issuer becomes `http://<LAN
address>:<idp port>` and the API's `jwt_issuer` follows (details, limits and the
security notice: `forge skill load deploy/managed-identity/mobile`). **Existing
apps own `src/lib/auth/config.ts`**: without the change below the app refuses that
issuer ("must be an https:// URL"), because the scaffold of the first v1.4.0
builds only allowed `http` to loopback. Add the exported `isDevNetworkHost`
(loopback, RFC 1918, RFC 6598, link-local, IPv6 ULA/link-local, `.local`) and make
`isSecureEndpoint` call it instead of `isLoopbackHost`; leave the redirect check
on `isLoopbackHost` (an `http` callback stays loopback-only). Both stay gated on
`__DEV__`. **Owned-file edits for the API half**: add `bool
jwt_userinfo_allow_local_http` (any free field number; 51 in new scaffolds; env
`JWT_USERINFO_ALLOW_LOCAL_HTTP`, default `false`) to your config proto and
regenerate. Then, in `internal/app/auth.go`:

1. Build the userinfo lookup as `userInfo := auth.NewUserInfo(...)`, then
   `if cfg.JwtUserinfoAllowLocalHttp { userInfo.AllowLocalPlainHTTP() }`, then
   `authenticator.UseUserInfo(userInfo)`.
2. Confine the JWKS and discovery fetches to this machine too (the same
   threat: a proxy variable or a changed network must not put someone else's
   keys in front of the validator). Import `"net/http"` and add, before
   `SetupAuth`:

   ```go
   func issuerClient(cfg *config.Config) *http.Client {
       if !cfg.JwtUserinfoAllowLocalHttp { return nil }
       return auth.LocalIssuerHTTPClient(cfg.JwtIssuer)
   }

   func discoveryDoer(c *http.Client) oauth2.HTTPDoer {
       if c == nil { return nil } // a nil *http.Client must not become a non-nil interface
       return c
   }
   ```

   Put `httpClient := issuerClient(cfg)` at the top of `SetupAuth`, pass
   `discoveryDoer(httpClient)` instead of `nil` as the second argument of both
   `oauth2.Discover(...)` calls (the JWKS `discover` closure and the userinfo
   endpoint closure), and add `HTTPClient: httpClient,` to the `auth.JWTConfig{...}`
   literal. `config.Config` is your config type (the scaffold uses the name its
   config package declares). `LocalIssuerHTTPClient` returns nil unless the
   issuer is plain HTTP at this machine's IPv4 address, so every other run is
   unchanged.

`internal/app/auth.go` is not on `forge project upgrade --check`'s list. To see
the scaffold's version, move yours aside and run `forge project rescaffold
internal/app/auth.go` at the pinned forge (it writes the helpers above once the
config declares the field), then diff the two; if you never edited it, keep the
new one. And in `internal/app/login_broker.go` add `LocalOnly:
cfg.JwtUserinfoAllowLocalHttp,` to the `devidp.Client{...}` literal (`Base:
cfg.IdpBase`). `forge env up --device-host` sets
that variable for the identity's host workloads unless the workload declares it itself
(a declared value, true or false, is left as declared); it is off everywhere else, and
without these edits a device-host run signs in but `Claims.Email` stays empty and the
broker's and the validator's IdP traffic is not confined to this machine. The device host must be
this machine's LAN **IPv4** address (not a name or IPv6). An Android emulator without `--device-host` still needs
`adb reverse tcp:<idp port> tcp:<idp port>`. Production exports must carry
no test fakes: `tests/auth-fakes.ts` is imported only by tests.
