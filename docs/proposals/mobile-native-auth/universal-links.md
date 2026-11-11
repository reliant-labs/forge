# https universal-link / App Link sign-in callbacks

Status: design, owner-approved to build. Base: mobile integration head `c36447c89`
(Expo SDK 58: `expo ~58.0.23`, `expo-web-browser ~58.0.7`). This extends
[broker.md "Universal links later"](broker.md#universal-links-later). It does not reopen anything broker.md
settled:

- native `redirect_uris` are an ordered list, and the first entry is used;
- callbacks are matched by scheme+authority+path;
- one `app/oauthredirect.tsx` route serves both forms;
- `release.ValidateNativeCallbackURI` already accepts https.

## Why

Any app on the device can claim a private-use scheme such as `com.<project>.<frontend>:/oauthredirect`
(RFC 8252 §8.6). An https callback on a domain we own is bound to our signed app by association files that the
domain hosts. PKCE protects the code either way. Universal links also remove the risk that another app
intercepts the redirect: on iOS 17.4+, and on Android once the App Link is verified.

## 1. iOS

**API.** No new API is needed. In expo-web-browser SDK 58,
`openAuthSessionAsync(url, redirectUrl, { preferUniversalLinks: true })` already does this. The native module
([`ios/WebAuthSession.swift`](https://github.com/expo/expo/blob/main/packages/expo-web-browser/ios/WebAuthSession.swift))
builds `ASWebAuthenticationSession(url:callback: .https(host:path:))` only when all four of these hold:

- `preferUniversalLinks` is set;
- the device runs iOS 17.4+;
- the redirect scheme is `https`;
- the redirect has a non-empty host.

The host and path (`/oauthredirect`) are taken from the redirect URI. Otherwise the module falls back to
`callbackURLScheme: redirectUrl.scheme`. `preferEphemeralSession` is applied on both paths. Apple documents the
callback types at
[ASWebAuthenticationSession.Callback](https://developer.apple.com/documentation/authenticationservices/aswebauthenticationsession/callback)
and [`https(host:path:)`](https://developer.apple.com/documentation/authenticationservices/aswebauthenticationsession/callback/https(host:path:)).
The session intercepts the matching navigation itself, so the callback page never loads and no app-level URL
routing is involved.

**Entitlement.** The https callback works only when the app is associated with that host. Apple's
`Callback.https` docs require an associated domain. Implementer reports (Expo PR #33437 for
`preferUniversalLinks`, and the Apple developer forums) tie the auth-session check to the **`webcredentials:`**
service. **To verify in test row 2.** We declare both services, so either reading holds:

- `webcredentials:<host>`: the auth-session callback. It also gives password AutoFill on the IdP login page.
- `applinks:<host>`: makes `https://<host>/oauthredirect` a universal link outside the session too (a
  cold-start delivery, or a later magic link). It is the twin of an Android App Link.

**AASA.** The file must meet all of these
([Supporting associated domains](https://developer.apple.com/documentation/xcode/supporting-associated-domains)):

- served at `https://<host>/.well-known/apple-app-site-association`, with no `.json` suffix;
- over https with a valid certificate;
- with **no redirects**;
- as `Content-Type: application/json`;
- no larger than 128 KB.

Apple's CDN (`app-site-association.cdn-apple.com`) fetches the file, not the device.

```json
{
  "applinks": { "details": [ {
    "appIDs": ["23P64LQTZD.com.acme.mobile"],
    "components": [ { "/": "/oauthredirect" }, { "/": "/signedout" } ] } ] },
  "webcredentials": { "apps": ["23P64LQTZD.com.acme.mobile"] }
}
```

`appIDs` is `<TeamID>.<bundle id>`, with the team Reliant Labs `23P64LQTZD`. One file can list several bundle
ids, for example dev, staging and prod variants.

**Developer mode.** Appending `?mode=developer` to the entitlement (`applinks:<host>?mode=developer`, likewise
`webcredentials:`) makes the device fetch the AASA **directly** from the host. That bypasses Apple's CDN cache,
which can lag by hours.

- **Simulator:** works with no extra setup.
- **Device:** needs Developer Mode on, Settings → Developer → *Associated Domains Development* enabled, and a
  development-signed build.

The host still needs a publicly trusted certificate; developer mode removes only the CDN. Release builds ignore
the mode, but forge must not emit it outside dev. See "Entitlement format" on the Apple page above.

**Below iOS 17.4.** On older iOS the module falls back to `callbackURLScheme: "https"`, which
ASWebAuthenticationSession does not support as an interceptable scheme, so the session never completes. **The
client must not send an https redirect on iOS < 17.4**, and uses the private-use entry instead (§3, *Client
changes*). Expo SDK 58's iOS floor is below 17.4, so this path is real.

## 2. Android

**Mechanism.** expo-web-browser has no native auth session on Android. In
[`WebBrowser.ts`](https://github.com/expo/expo/blob/main/packages/expo-web-browser/src/WebBrowser.ts),
`_openAuthSessionPolyfillAsync` opens a Custom Tab and races two events:

- the app returning to `active` → `dismiss`;
- a `Linking` `url` event where `event.url.startsWith(returnUrl)` → `success`.

`preferUniversalLinks` is not forwarded on Android, and passing it is harmless. **An https return therefore works
unchanged if and only if Android delivers the https URL to our activity**, and a verified App Link does exactly
that. The IdP's 302 to `https://<host>/oauthredirect?code=…` lands in the Custom Tab. The redirect chain began
with a user gesture (the login submit), so Chrome resolves the verified link to the app instead of loading the
page ([Chrome: Android intents](https://developer.chrome.com/docs/android/intents),
[App Links](https://developer.android.com/training/app-links)).

If the link is **not** verified, the tab loads `https://<host>/oauthredirect`. The race resolves `dismiss` when
the user backs out, and sign-in fails. That page therefore needs a designed fallback (§3, *Serving*).

**Risk:** some non-Chrome default browsers do not hand a redirect-initiated App Link to the app. The mitigation
is the private-use fallback page (§3, *Precedence*), plus an E2E check on Chrome, the emulator's default browser.

**app.json** ([Expo: Android App Links](https://docs.expo.dev/linking/android-app-links/), `android.intentFilters`):

```json
"intentFilters": [ { "action": "VIEW", "autoVerify": true,
  "data": [ { "scheme": "https", "host": "<host>", "pathPrefix": "/oauthredirect" },
            { "scheme": "https", "host": "<host>", "pathPrefix": "/signedout" } ],
  "category": ["BROWSABLE", "DEFAULT"] } ]
```

**assetlinks.json** is served at `https://<host>/.well-known/assetlinks.json`, as `application/json`, with no
redirects ([Verify Android App Links](https://developer.android.com/training/app-links/verify-android-applinks)):

```json
[ { "relation": ["delegate_permission/common.handle_all_urls"],
    "target": { "namespace": "android_app", "package_name": "com.acme.mobile",
                "sha256_cert_fingerprints": ["AA:BB:…"] } } ]
```

`sha256_cert_fingerprints` must list **every** certificate that signs an installable build:

- **Dev and emulator:** the debug keystore. The prebuild debug build signs with `android/app/debug.keystore`,
  which is per-checkout, so read that file and not `~/.android`:
  `keytool -list -v -keystore android/app/debug.keystore -alias androiddebugkey -storepass android`.
- **Release:** the **Play app-signing key** (Play Console → App integrity), not the upload key. `eas credentials`
  shows it if EAS manages signing. Add the upload key as well if sideloaded builds are distributed.

On Android 12+, the device verifies the link at install. The commands:

- `adb shell pm verify-app-links --re-verify <pkg>` re-runs verification.
- `adb shell pm get-app-links <pkg>` shows the state; expect `verified`.
- `adb shell pm set-app-links --package <pkg> 2 <host>` force-approves (state 2) on debug builds. It is a
  **fake**, for the logic lane only.

## 3. Forge design

### Declaring it (KCL, additive, semver-safe)

Add an optional `associated_domains: [str] = []` to `forge.IdentityApplication`, valid only for
`kind = "native"`. It is enforced by the `kcl/schema.k` check and by a Go mirror next to
`ValidateNativeCallbackURI`. It is a list, not broker.md's single `associated_domain`, because per-env hosts and
an apex-plus-`app.` split are realistic.

**Derivation.**

- If `associated_domains` is set and `redirect_uris` is not: derive `https://<domains[0]>/oauthredirect` and
  `/signedout`, **ahead of** the private-use pair, which stays as the fallback.
- An explicit `redirect_uris` still wins.

**Validation.** It runs as a lint lane and in the `env up` / `env deploy` preflight:

- the host of the **first https redirect must be in `associated_domains`**, and so must the host of every other
  https redirect;
- each domain is a bare hostname: no scheme, port, path or wildcard (a wildcard cannot serve `webcredentials`).

**Platform identities.** These describe the binary, not the IdP, so they are declared on the native **Frontend**:

- `apple_team_id: str` (default empty; `23P64LQTZD` here);
- `android_cert_fingerprints: {str: [str]}`, keyed by env, because debug and release keys differ. The special
  value `"debug"` is resolved at `env up` from the keystore the build actually uses.

The bundle id and Android package come from app.json. Each per-env bundle suffix (for example `.dev`) adds one
`appIDs` entry.

### Serving the files

1. **The project's web frontend / hosted static site on that host (recommended).** The files are per-env (the
   fingerprints differ), but a promoted release is byte-identical. So they are **not** baked into the build:
   forge renders them at deploy as a platform-served `/.well-known/` overlay, beside the per-env runtime config.
   **Gap (control plane, in scope):** hosted static hosting refuses `Content-Type` in `_headers`
   (`deploy/static-site`, "Response headers"). The platform must serve the extensionless
   `/.well-known/apple-app-site-association` and `assetlinks.json` as `application/json`, with no apex↔www,
   trailing-slash or `.html` rewrite and no redirect. For a non-hosted target (your own bucket, or `env up` with
   files on disk), the same render is written to `public/.well-known/`, and the target's own header config is
   named in the lint.
2. **The API gateway** (a `/.well-known/*` handler). This couples static association files to a running service,
   and the callback host is usually the user-facing web host, not `api.`. Keep it as the escape hatch.
3. **A links-only static site** for a mobile-only project. It costs a site per env (billable beyond the free one).

**The `/oauthredirect` and `/signedout` web pages.** An unverified link loads these, so forge scaffolds them into
that frontend when `associated_domains` is set. Each is a static page that:

- never reads, stores or logs the query string;
- sends `Referrer-Policy: no-referrer` and makes no analytics call;
- shows an "Open in app" button that relaunches the private-use URI with the same query.

The code is useless without the PKCE verifier, which only the app holds.

### App config

Ship a **local Expo config plugin** in the template (`plugins/with-forge-links.js`). The alternatives rejected
were scaffold-time owned edits to app.json and a generated `app.config.ts`. At prebuild the plugin:

- reads `FORGE_ASSOCIATED_DOMAINS` and `FORGE_APP_LINK_MODE` (`developer` in dev), env vars that
  `forge env up`/`build` writes the same way `native_dev_oidc.go` writes `EXPO_PUBLIC_FORGE_OIDC_*`;
- writes `ios.associatedDomains` (`applinks:` + `webcredentials:`) and `android.intentFilters`;
- does nothing when no domain is set.

That keeps app.json the user's file and means a domain-less project ships no unverifiable claim, which was
broker.md's concern. It also stays per-env and declarative. Existing projects opt in through a
`migrations/<next>` playbook that adds the plugin to `plugins`.

### Dev loop

Both platforms need a public host with a CA-trusted certificate: localhost, LAN IPs and self-signed
certificates do not work.

- **A hosted dev site** (Reliant static hosting) gives a stable host and a real certificate, and
  `?mode=developer` removes the iOS CDN lag. Recommended.
- **A named tunnel** (cloudflared or ngrok with a reserved name) also works. The name must be stable, because it
  is compiled into the entitlement and registered at the IdP.

The IdP can stay the local dev Zitadel. It only needs the https redirect registered, and `native_dev_oidc.go`
already passes `RedirectURIs[0]` through. **Minimal real setup:** one stable public https host serving the two
files, plus a dev build carrying the entitlement and intent filter.

### Client changes (`src/lib/auth/{config,native}.ts`)

The config gains an optional `EXPO_PUBLIC_FORGE_OIDC_FALLBACK_REDIRECT_URI`, the private-use entry, written by
forge from the second redirect.

`native.ts` decides once per attempt:

1. Compute `useHttps = redirect is https && (android || iOS >= 17.4)`.
2. If `useHttps`, pass `preferUniversalLinks: true`.
3. If not, use the fallback.

The chosen URI is used for **both** the authorize `redirect_uri` and the token exchange (RFC 6749 §4.1.3), so
it is stored in the pending-auth state. Matching stays scheme+authority+path. Android needs nothing more: the
polyfill's `startsWith` already matches https. The expo-router route keeps serving both forms for cold-start
deliveries.

### Precedence and ordering

The registered list is `[https, private-use]`, and the IdP accepts either entry. Clients try https first and fall
back to the private-use entry on old iOS, on an unverified link (via the web page) and in domain-less builds.
Logout uses the same order. Refresh tokens are not bound to the redirect, so installed apps survive the switch.
Zitadel accepts https redirect URIs for native apps
([Zitadel: redirect URIs](https://zitadel.com/docs/guides/integrate/login/oidc/login-users); proved locally in
`internal/devidentity/hostzitadel_native_test.go`).

## 4. Test plan

**Must be real:**

- the public host and its CA certificate;
- both files on that exact host, with the right content type and no redirect;
- the iOS entitlement in a **signed** dev build (team `23P64LQTZD`, Associated Domains on the App ID);
- the IdP registration of the https redirect;
- the Android debug fingerprint of the build actually installed.

**Can be local or fake:**

- the IdP (local dev Zitadel) and the API;
- Android verification via `pm set-app-links … 2`, in the logic lane only. It is never used in row 5.

| # | Where | Steps | Pass |
|---|---|---|---|
| 1 | files | `curl -sI` both `.well-known` URLs; Google's [Statement List tester](https://developers.google.com/digital-asset-links/tools/generator) | 200, `application/json`, no 3xx, valid JSON |
| 2 | iOS Sim (17.4+ runtime) | dev build with `?mode=developer`, sign in | session closes; tokens stored; host access log shows no `/oauthredirect` GET. Also run once with only `applinks:` to settle the `webcredentials` question |
| 3 | iOS Sim (17.0 runtime) | same build | client picks the private-use entry; sign-in succeeds |
| 4 | iOS | sign out | `/signedout` closes the session |
| 5 | Android emulator (API 34+, Chrome) | install the debug build, `pm verify-app-links --re-verify`, `pm get-app-links` | `verified` without manual approval |
| 6 | Android | sign in | Custom Tab hands off; `Linking` reports `success`; tokens stored |
| 7 | Android | `pm set-app-links --package <pkg> 0 <host>`, sign in | the fallback page appears; "Open in app" completes sign-in over the scheme |
| 8 | both | kill the app mid-login, then complete | `app/oauthredirect.tsx` completes, or reports a clean error |
| 9 | unit | decision table (OS × version × scheme); KCL rule that the first https host is in the domains | Vitest + Go, `-short` |

Rows 2–8 are a manual or Maestro lane, not CI: they need a signed build and the public host.

## 5. Owner questions

1. **Domain:** a Reliant-hosted dev site hostname (recommended), or a custom domain we own (which one)? It is
   compiled into the entitlement, so it must stay stable.
2. **Reliant hosting for the test site:** is the control-plane change in scope (serve the extensionless AASA as
   `application/json`, never redirect it)? The dev site also takes the free site slot.
3. **Android key:** for the test, should the debug keystore be committed or stay per machine, given that its
   fingerprint goes in the file? For release, Play app signing or our own key?
4. **iOS signing:** who enables Associated Domains on the `23P64LQTZD` App ID(s) and issues the dev provisioning
   profile? Do we use per-env bundle suffixes (`.dev`)?
5. **Hosted IdP:** is hosted registration of native clients (broker.md's open decision) settled, so that the
   https redirect converges for staging and prod?
6. **iOS floor:** keep the < 17.4 private-use fallback indefinitely, or raise the deployment target to 17.4
   later and drop the scheme on iOS?
