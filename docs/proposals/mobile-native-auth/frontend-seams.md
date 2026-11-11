# Native auth — frontend seams (research)

Scope: what exists today for React Native auth, what the next slice (system-browser
PKCE via the login broker, expo-secure-store, single-flight refresh, logout, account
isolation) must touch. Paths are relative to `internal/templates/frontend/` unless noted.

## 0. Correction to the brief

There is **no** `react-native/src/lib/auth/` directory. All four auth files live in
`shared/src/lib/auth/` and branch on `{{.Platform}}`; the RN tree composes `shared/`
(`internal/templates/frontend_tree.go:46-55`). Native-only files today:
`react-native/src/lib/{connect.ts.tmpl,query-lifecycle.ts,devlog.ts,apiurl_gen.ts.tmpl}`,
`react-native/app/{_layout,index}.tsx.tmpl`.

## 1. The auth abstraction

- `shared/src/lib/auth/provider.ts:38-57` — `AuthUser {id,email?,name?}`;
  `AuthProvider { getToken(): Promise<string|null>; getUser(): AuthUser|null; logout(): Promise<void> }`.
  Header (`:16-36`) documents that `login()`, `onAuthStateChange()`, `isAuthenticated/isLoading`,
  `claims` were deliberately removed as no-ops. **No login, no subscribe, no restore** — the
  next slice must add at least an async restore + sign-in entry point.
- `shared/src/lib/auth/session-provider.ts.tmpl` — the only shipped implementation (mock).
  - mode: RN reads `process.env.EXPO_PUBLIC_MOCK_API` (`:70-76`); RN has no hybrid (`:71-73`).
    `isPureMock` `:88`, `isMockMode` `:95-97`.
  - fixture `MOCK_USER`/`MOCK_TOKEN` `:102-107`; `getToken` returns token only in pure mock `:136-139`.
  - `warnUnwired` `:118-130`: lazy, once, only when not mock; message points at `context.tsx`.
  - `logout` just nulls in-memory `current` `:144-146`. No persistence (`:20-22`).
- `shared/src/lib/auth/use-auth.ts.tmpl` — owned. RN defines its own `Identity`
  (`:17-25`), web imports it from `native-login` (`:13-16`). `AuthContextValue`
  `{identity,isAuthenticated,isLoading,refresh,logout}` `:28-37`. **No `signIn`** in the value.
- `shared/src/lib/auth/context.tsx.tmpl` — owned. RN branch: provider built once via
  `useState(createSessionAuthProvider)` `:105`; `refresh` = synchronous `getUser()` → identity `:116-124`;
  mount effect `:156-160`; `auth:expired` listener with single-flight `expiryCheck` ref `:169-178`
  (shared by both platforms); `logout` = `provider.logout()` + signed-out identity `:181-184`.
  **Logout touches neither QueryClient nor the transport token getter.**
- Web: `shared-web/src/lib/auth/native-login.ts.tmpl` (cookie broker client: `signIn :145`,
  `signOut :170`, `fetchIdentity :181` → `GET /auth/session`), `route-guard.tsx.tmpl:92-101`
  (router-agnostic, injected `navigate`/`pathname`, `useAuth()`), `return-to.ts`,
  `use-auth-options.ts`; nextjs pages `nextjs/src/app/auth/{sign-in,forgot,reset,layout}`.
  Runtime also has `web-runtime/src/{route-guard.tsx:22,session.tsx:27-43}`.
- Token → transport: `react-native/src/lib/connect.ts.tmpl:30-34` module-level
  `_getToken` default `async () => null`, `setAuthTokenGetter(fn)`; passed into
  `buildRuntimeInterceptors({ getToken: () => _getToken() })` `:47-49`. **Nothing calls
  `setAuthTokenGetter`** in the scaffold — only a comment in `app/_layout.tsx.tmpl:17-31`.
- **RN transport has no `onError` → `emitAuthExpired()`**; web does
  (`vite-spa/src/lib/connect.ts.tmpl:62-70`, nextjs `connect.ts.tmpl:66`). So on native the
  context's `auth:expired` listener (`context.tsx.tmpl:169-178`) is dead code today, and the
  `query-client.ts:184` toast suppression comment ("the transport emitted auth:expired") is false on native.
- Route guards: none on native. `app/_layout.tsx.tmpl:33-50` is a single `<Stack>` with
  `index`; `app/index.tsx.tmpl` is a placeholder. `TestReactNativeShipsNoBrowserSignIn`
  (`internal/templates/frontend_auth_flow_test.go:510-545`) pins absence of route-guard/native-login on RN.

## 2. `@reliantlabs/forge-web-runtime` (`web-runtime/`)

- Entry points: `.`, `./interceptors`, `./node` (`web-runtime/package.json:36-48`);
  `web-runtime/interceptors/package.json` is a legacy-resolver shim with a `react-native` field.
- `web-runtime/src/interceptors.ts`: `RuntimeInterceptorConfig {getToken,getBrand,onError,maxRetries,retryBaseMs}` `:53-64`;
  `authInterceptor` `:69-80` (per-call `await getToken()`, sets Bearer, no 401 handling);
  `errorNormalizeInterceptor` `:113`; `retryInterceptor` `:159` (NOT in the default stack);
  `buildRuntimeInterceptors` `:191-200` = errorNormalize, auth, header, trace.
- **No refresh, no 401-retry, no single-flight, no token store** anywhere in the runtime.
  React Query is declared the single retry owner (`shared/src/lib/query-client.ts:147-152`).
- Portable to RN: everything in `./interceptors` (DOM-free, already consumed natively).
  Not portable: `session.tsx`, `session-cookie-fallback.ts` (document.cookie), `route-guard.tsx` (web router).
- Natural home for a generic piece: a `singleFlight`/`createTokenRefresher` and an optional
  `onUnauthenticated → refresh → replay once` interceptor in `./interceptors`. Public pkg
  surface ⇒ additive only (semver at v1).

## 3. React Query

- Created once at module scope: `shared/src/lib/query-client.ts:177-207` (`export const queryClient`),
  provided in `react-native/app/_layout.tsx.tmpl:39`; lifecycle wired `:36`
  (`react-native/src/lib/query-lifecycle.ts`). Mutations `networkMode:"always"` `:200-205`.
- Generated hooks' keys contain no identity, so a shared cache leaks across accounts.
- Needed on logout/account switch, in order: (1) stop new requests (swap token getter to
  null / bump an identity epoch), (2) `queryClient.cancelQueries()` (aborts via the
  `signal` React Query passes; connect-web + `expo/fetch` honour it — `connect.ts.tmpl:60-62`),
  (3) mutations cannot be cancelled by RQ: keep an app-level `AbortController` per identity
  epoch and pass `signal` via a transport interceptor (`req.signal` merge) so in-flight
  writes abort, (4) `queryClient.clear()` (drops queries + mutation cache), (5) clear secure
  store. Remounting `QueryClientProvider` under `key={subject}` is the alternative to
  `clear()` but requires a per-identity client, i.e. turning the module-level singleton into
  a factory — larger blast radius (query-client.ts is shared with web and advisory-tracked:
  `internal/generator/upgrade_advisory_test.go:22`). Prefer clear+cancel+epoch.
- A response that resolves after logout must not repopulate the cache: tag requests with the
  epoch in the interceptor and drop/throw when it no longer matches.

## 4. Workspace mode

- RN `connect.ts.tmpl:6-8,76-84` calls `setApiTransport(transport)` from the shared hooks package;
  hooks resolve transport at call time (`internal/generator/frontend_workspaces.go:396-421`).
- Hooks package is DOM-free and frontend-agnostic ⇒ **auth must stay per-frontend**
  (`frontends/<app>/src/lib/auth/`), not in a shared workspace package; tokens are a
  property of the app's transport. Only pure helpers (single-flight) could be shared, and
  those belong in web-runtime, not a workspace package.

## 5. Tests that pin auth today

- `internal/templates/react_native_test.go:26-35` — RN tree must contain `query-client.ts`,
  `auth/{context.tsx,provider.ts,session-provider.ts}.tmpl`. Adding files: extend the list.
- `internal/templates/frontend_auth_flow_test.go:510-545` (`TestReactNativeShipsNoBrowserSignIn`:
  no native-login/route-guard on RN; provider seam present) and `:566-602` (RN context must
  not import `./native-login` and must fall back to the mock provider). A real native
  provider in `context.tsx` breaks `:597-602` — rewrite that assertion, don't delete it.
- `internal/generator/service_gen_test.go:365,442-446` — expected generated file sets incl. auth files.
- `react-native/tests/connect-transport.test.ts:6,149-170` — `setAuthTokenGetter` + Bearer header.
  Keep `setAuthTokenGetter` exported (owned-file API; existing apps call it).
- `internal/templates/native_shared_runtime_boundary_test.go:63` — shared query-client must stay DOM-free.
- `internal/templates/native_devlog_origin_test.go:31` — query-client scanned for devlog origin.
- `internal/cli/mobile_consumer_conformance_e2e_test.go` — no auth references today; the new
  slice should add a conformance step (typecheck + jest) for the auth module.
- No Jest test under `react-native/tests/` covers the auth context (only devlog/app-config/
  connect-transport mention auth). `shared-web/src/lib/auth/context.test.tsx.tmpl` is web-only.

## 6. Server side gap (blocks the frontend)

`internal/templates/project/app-login-broker.go.tmpl:74-81,149-193` exposes only cookie
routes (`/auth/login|register|logout|session|password-policy|options|forgot|reset`). There is
**no** authorize/callback/token/refresh endpoint and no `code_verifier`/refresh-token handling.
System-browser PKCE needs a broker addition (e.g. `GET /auth/native/authorize`, redirect to
the app's scheme, `POST /auth/native/token`, `POST /auth/native/refresh`, `POST /auth/native/revoke`)
returning bearer + refresh tokens — or PKCE direct against the issuer with the broker
out of the loop. That decision precedes the frontend layout below.

## 7. Recommendation

Smallest public seam (extend `provider.ts`, additively, with optional members so the mock
and existing user implementations still typecheck):

```ts
interface AuthProvider {
  getToken(): Promise<string | null>; // existing; must do single-flight refresh internally
  getUser(): AuthUser | null; // existing
  logout(): Promise<void>; // existing; revoke + wipe store
  restore?(): Promise<AuthUser | null>; // new: cold-start from secure store
  signIn?(): Promise<AuthUser | null>; // new: interactive (system browser)
  onTokenRejected?(): Promise<boolean>; // new: 401 → refresh; true = replay once
}
```

Swapping IdPs = implementing this one interface; the context, transport and cache
isolation never see the IdP.

Layout (RN only unless noted; "owned" = written once, needs a v1.4.0 migration playbook
under `internal/templates/project/skills/forge/migrations/v1.4.0/` for existing apps):
| File | Status | Role |
|---|---|---|
| `shared/src/lib/auth/provider.ts` | edit (owned) | optional members above |
| `react-native/src/lib/auth/broker-provider.ts` | **new, owned** | PKCE via `expo-auth-session`/`expo-web-browser` + broker; implements AuthProvider |
| `react-native/src/lib/auth/token-store.ts` | **new, owned** | `expo-secure-store` wrapper, keyed per issuer/account |
| `web-runtime/src/single-flight.ts` (+ export from `./interceptors`) | **new, runtime** | generic single-flight; optional `unauthenticatedRetryInterceptor` |
| `react-native/src/lib/auth/session.ts` | **new, owned** | identity epoch + AbortController; `endSession(queryClient)` = cancel → abort → clear |
| `shared/src/lib/auth/context.tsx.tmpl` | edit (owned) | RN branch: pick broker vs mock provider by config, call `restore`, `signIn`, wire `setAuthTokenGetter`, run `endSession` on logout and on identity change |
| `shared/src/lib/auth/use-auth.ts.tmpl` | edit (owned) | add optional `signIn` to `AuthContextValue` (RN only) |
| `react-native/src/lib/connect.ts.tmpl` | edit (owned) | add `onError → emitAuthExpired()` (parity bug today), epoch/abort interceptor |
| `react-native/app/_layout.tsx.tmpl` | edit (owned) | `(auth)`/`(app)` groups with `<Redirect>` / `Stack.Protected` guard; `app/sign-in.tsx` new |
| `shared/src/lib/auth/session-provider.ts.tmpl` | keep | mock stays the `EXPO_PUBLIC_MOCK_API=true` provider; adjust `warnUnwired` text |
Keep the auth implementation per-frontend (not in the workspace hooks package); keep
`query-client.ts` a singleton and shared with web.
