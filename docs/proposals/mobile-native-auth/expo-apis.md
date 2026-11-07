# Expo APIs for native OAuth (SDK 58) — research notes

Status: research only. Verified 2026 against the npm registry and `expo/expo` branch `sdk-58`
(raw.githubusercontent.com/expo/expo/sdk-58/packages/...). Template baseline:
`internal/templates/frontend/react-native/` (expo ~58.0.23, RN 0.88.1, jest-expo ~58.0.14).

## 1. Versions, peers, config plugins

Source of truth for `npx expo install`: `packages/expo/bundledNativeModules.json` on sdk-58.
All five resolve through `expo install`; these match npm `latest` today.

| Package | SDK 58 pin | npm deps / peers (latest) |
|---|---|---|
| expo-auth-session | `~58.0.17` | deps: expo-crypto ~58.0.7, expo-linking ~58.0.16, expo-constants ~58.0.15, expo-application ~58.1.2, expo-web-browser ~58.0.7, invariant; peers react, react-native |
| expo-web-browser | `~58.0.7` | peers expo, react-native |
| expo-secure-store | `~58.0.2` | peer expo |
| expo-crypto | `~58.0.7` | peer expo |
| expo-linking | `~58.0.16` (already in template) | deps expo-constants, invariant |
| (transitive) expo-application | `~58.1.2` | pulled by auth-session only |

Note: expo-auth-session pulls expo-application + expo-constants as hard deps — extra native
module surface even if we use none of its UI.

`app.json` (template `app.json.tmpl`) already has `"scheme": "{{.FrontendName}}"` — required for
the redirect URI. Plugins:
- `expo-secure-store`: has a config plugin (`plugin/src/withSecureStore.ts`) — sets
  `NSFaceIDUsageDescription` (prop `faceIDPermission`, `false` to omit) and, by default
  (`configureAndroidBackup: true`), adds Android backup/data-extraction rules excluding the
  SecureStore prefs (restored-but-undecryptable entries otherwise crash reads after restore).
  Add `"expo-secure-store"` to `plugins`; pass `{ "faceIDPermission": false }` if we never use
  `requireAuthentication`.
- expo-web-browser / expo-crypto / expo-auth-session / expo-linking: no plugin needed for this flow
  (Prebuild autolinking only). Android Custom Tabs need no manifest changes; the scheme intent
  filter comes from `scheme`.

## 2. expo-auth-session

Source: `packages/expo-auth-session/src/{AuthRequest,AuthSession,PKCE}.ts`.

- **Hooks vs imperative.** `useAuthRequest(config, discovery)` → `[request, response, promptAsync]`
  is a thin React wrapper over `new AuthRequest(config)` + `request.promptAsync(discovery)`.
  Token ops are plain async functions, usable outside React: `exchangeCodeAsync`,
  `refreshAsync`, `revokeAsync`, `fetchUserInfoAsync` (all form-POST to discovery endpoints,
  return `TokenResponse`).
- **PKCE defaults** (AuthRequest ctor): `usePKCE ?? true`, `codeChallengeMethod ?? S256`,
  verifier = 128 chars from `expo-crypto.getRandomValues` mapped onto `[A-Za-z0-9]`
  (`PKCE.generateRandom`; modulo-62 bias, harmless), challenge =
  `digestStringAsync(SHA256, verifier, BASE64)` → url-safe, unpadded. Verifier is on
  `request.codeVerifier`; you must pass it to `exchangeCodeAsync({ extraParams:{code_verifier} })`
  yourself.
- **State.** `state ?? generateRandom(10)` (only ~59 bits); `parseReturnUrl` compares returned
  `state` to `this.state` and yields `{type:'error', error:'state_mismatch'}`. Pass our own
  32-byte state if we care.
- **`makeRedirectUri({ native, scheme, path })`**: on native in `Standalone`/`Bare`
  execution environment *and* `native` given → returns `native` verbatim. Otherwise
  `Linking.createURL(path, {scheme})` → dev client / production: `<scheme>://<path>`;
  **Expo Go: `exp://<lan-ip>:8081/--/<path>`** (unregisterable with a real broker; treat Expo
  Go as unsupported for auth, require a dev client). Web: `https://origin/<path>`.
- **Browser** (expo-web-browser `openAuthSessionAsync(url, redirectUrl, opts)`):
  iOS → `ASWebAuthenticationSession`; `preferEphemeralSession: true` = no shared Safari cookies
  (good for account isolation / "switch account"; costs SSO). Android → Custom Tabs polyfill:
  races `_openBrowserAndWaitAndroidAsync` against a `Linking` redirect listener; only one
  session at a time (throws "WebBrowser is already open"). Options other than these are
  ignored on Android for auth.
- **Result types** (`WebBrowserResultType`): `cancel` (iOS user cancel), `dismiss` (Android tab
  closed / programmatic `dismissAuthSession`), `opened`, `locked` (another session in flight);
  success is `{ type:'success', url }`. AuthSession maps to `success|error|cancel|dismiss|opened|locked`.
- **`WebBrowser.maybeCompleteAuthSession()`**: web-only (closes the popup and posts the URL to
  the opener); a no-op on native. Call it at module scope of the callback route only if the
  Expo web target does auth.
- **expo-router pitfall.** On Android (and on iOS if the OS also delivers it) the redirect
  `myapp://oauth/callback?code=…` is *also* dispatched as a deep link; expo-router will try to
  navigate to `app/oauth/callback` → "Unmatched route" screen flashes. Fixes: add a real
  `app/oauth/callback.tsx` that renders nothing/redirects home (and never consumes the code
  itself), or rewrite it in `app/+native-intent.tsx` (`redirectSystemPath`) to `/`. The
  in-flight `openAuthSessionAsync` promise is what consumes the code.
  Also: on Android a cold-started process (app killed while in Custom Tab) loses the pending
  promise and in-memory verifier — the deep link then arrives with nothing to finish it; persist
  `{state, verifier}` briefly (SecureStore) if we want to survive that, else show "try again".

## 3. expo-secure-store

Source: `packages/expo-secure-store/src/SecureStore.ts`.
- **Size**: SDK 58 `isValidValue` only checks `typeof value === 'string'` — the historical
  >2048-byte warning is gone. iOS Keychain has no practical small cap; Android stores AES-GCM
  ciphertext in SharedPreferences. Still: keep JWTs per key, don't blob the whole session.
- **Keys**: `[A-Za-z0-9._-]` only (`ensureValidKey`) — encode account ids before using in keys.
- **`keychainAccessible`** (iOS): `AFTER_FIRST_UNLOCK`, `AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY`,
  `WHEN_UNLOCKED` (**default**), `WHEN_UNLOCKED_THIS_DEVICE_ONLY`,
  `WHEN_PASSCODE_SET_THIS_DEVICE_ONLY`; `ALWAYS*` deprecated. Recommend
  `AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY` (background refresh works; not restored to new device).
  Reads must pass the same `keychainService`/accessibility options used on write.
- **`requireAuthentication`**: biometric gate; prompt failures reject with native message strings
  ("User canceled the authentication" / "User canceled the operation."); invalidated on
  biometric enrolment change. `canUseBiometricAuthentication()` checks it. Don't use for the
  refresh token (breaks silent refresh).
- **Reinstall**: iOS Keychain items **survive uninstall** → on first launch, detect via a
  non-keychain marker (e.g. AsyncStorage/file "installed" flag) and wipe. Android: keys live
  in Keystore + app prefs → cleared on uninstall; restore from backup is excluded by the plugin.
- **`isAvailableAsync()`**: `!!native.getValueWithKeyAsync` → true on iOS/Android only.
- **Web**: `ExpoSecureStore.web.ts` is `export default {}` — every call throws
  `UnavailabilityError`. No fallback; token storage needs a platform split (web → cookie/
  broker session, not localStorage).
- Errors: reads/writes reject (Keystore corruption after OS restore, locked device with
  `WHEN_UNLOCKED`); docs say treat as an auth outcome (re-login), not data corruption.
  Sync variants (`getItem`/`setItem`) exist but block the JS thread.

## 4. Hermes and hand-rolled PKCE

- Hermes (RN 0.88) has no WebCrypto: `crypto.subtle` absent; `globalThis.crypto.getRandomValues`
  not guaranteed — don't rely on it. Use `expo-crypto`:
  `getRandomBytes(32)` / `getRandomValues(Uint8Array)` (sync, CSPRNG), `digest(SHA256, bytes)`
  → `ArrayBuffer` (WebCrypto-shaped), `digestStringAsync(SHA256, s, {encoding: BASE64})`.
- Verifier: 32 random bytes → base64url (43 chars). Challenge: `digestStringAsync(...BASE64)`
  then `+→- /→_ strip =` (exactly what `PKCE.deriveChallengeAsync` does). Verifier is ASCII, so
  no TextEncoder needed (Hermes has `TextEncoder` since RN 0.74 anyway).
- base64url of bytes: `btoa` exists in Hermes (RN ≥0.74); encode via
  `btoa(String.fromCharCode(...bytes))` then url-safe replace. ~5 lines; unit-testable vs RFC 7636
  Appendix B vector (`dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk` →
  `E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM`).

## 5. Jest under jest-expo 58

- jest-expo's `preset/setup.js` auto-mocks expo modules: every function on a native module
  becomes `jest.fn(async () => {})` (or a mock from its `mocks/` dir if one exists). So
  `SecureStore.getItemAsync` resolves `undefined`, `digestStringAsync` resolves `undefined`,
  `openAuthSessionAsync` resolves `undefined` — tests pass vacuously. Don't rely on them.
- Recommended: in `tests/setup/` add an in-memory SecureStore fake
  (`jest.mock('expo-secure-store', () => fake)` backed by a `Map`, with a switch to make calls
  reject), and inject the browser + crypto + fetch behind a small interface (seam) so the flow
  is tested with plain fakes; use Node's `crypto` (`createHash('sha256')`, `randomBytes`) in the
  crypto fake to get real S256 vectors. Existing `tests/setup/expo-fetch.ts` already routes
  `expo/fetch` → `globalThis.fetch` for network stubbing.
- `expo-web-browser` result ordering (cancel/dismiss/locked/success) is easiest tested by a fake
  `openAuthSession(url, redirect) => Promise<Result>`.

## 6. Recommendation

**Thin hand-written flow over `expo-web-browser` + `expo-crypto` + `fetch`; don't depend on
expo-auth-session.** Reasons:
- Our needs are a single known IdP (forge login broker), not discovery/provider presets. The
  library's value (`AuthRequest` URL builder, state check, PKCE) is ~60 lines we can own and
  test against RFC vectors; its hooks tie the flow to a component lifecycle.
- Refresh, single-flight, revocation and account isolation must run outside React (transport
  interceptor in `@reliantlabs/forge-web-runtime`); `refreshAsync`/`revokeAsync` are just
  form-POSTs and give no single-flight, retry or error typing.
- Fewer native deps (drops expo-application, expo-constants coupling) and fewer
  silent-pass auto-mocks in Jest.
- We control security parameters: 32-byte state, 32-byte verifier, ephemeral sessions,
  persisted pending-login for Android cold starts.
- Trade-off: we lose web-popup support (`maybeCompleteAuthSession`) and must maintain the
  redirect parsing — acceptable; web auth should use the existing web frontend's cookie flow.
Mirror `PKCE.ts` and `AuthRequest.parseReturnUrl` semantics as the reference implementation.
Use `makeRedirectUri`-equivalent `Linking.createURL('oauth/callback')` (expo-linking is already
a dependency) and refuse to start login when it yields `exp://` (Expo Go).
