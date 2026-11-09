# Mobile consumer conformance slice: evidence

**Status:** draft stacked slice, not production readiness or main integration.
**Base:** `feat/mobile-foundation-integration` (PR #1887).
**SDK:** retained Expo 52 baseline. The coordinated SDK upgrade is the next
stacked slice; this baseline is not the intended final mobile foundation version.

## What the fixture exercises

`TestE2EMobileConsumerConformance` builds the checkout Forge and executes five
isolated consumers:

- Standalone mobile with published registry runtime.
- Standalone mobile with a packed checkout runtime artifact.
- Mixed pnpm web/mobile workspace with registry runtime.
- Mixed pnpm web/mobile workspace with packed checkout runtime.
- Customized scaffold with owned configuration edits, using registry runtime.

Registry installs explicitly avoid the corpus's local JS bridge and registry
overlay. Packed installs use an actual built tarball. Lockfile tarball locations
and installed module realpaths/versions are inspected. Go API development linkage
is separate and does not silently supply a JS runtime source checkout.

Each case typechecks and lints native code, executes Jest configuration and a real
generated unary-query bootstrap test, bundles iOS/Android JS through Metro, and
checks regeneration convergence. The request probe uses the actual generated
client and asserts service path, POST, content type and serialized int64 value;
its fetch response is synthetic, not a live backend interoperability test.
Workspaces also compile API/hooks/ui-web/ui-native and the browser consumer.
Customized fixtures preserve edits to Metro/lint/Jest/test files across generation.

## Repairs directly established by consumers

- Native TypeScript needs export-map-aware `moduleResolution: bundler` on SDK 52.
- Jest needs narrow ESM transforms for runtime/Connect/protobuf and pnpm nesting,
  retaining Expo's native transforms rather than transforming every dependency.
- UI workspace barrels must distinguish default components from named helpers.
- Scenario RPC handler types need shared API package imports in workspaces, not
  app-local `@/gen` paths. Entity-backed mock coverage is a separate remaining gate.
- Native code emitted by Babel needs a direct `@babel/runtime` dependency under
  isolated pnpm; a transitive hoisted copy only masked its absence in standalone.

Native lint/Jest configuration is owned starter code with executable smoke tests
outside the Expo route tree. React/React DOM are aligned to SDK 52's 18.3.1 matrix.
Migration guidance preserves custom scripts/config and existing owned barrels.

## Validation record

Initial consumer passes were followed by stronger generated-service fixtures;
failures were retained as failures and repaired at their actual source. An early
config-only success was not treated as proof of generated hook compatibility.

On the final repair set, the workspace registry and packed cases both passed
actual installs, all typechecks, lint, Jest, iOS/Android Metro and convergence in
one focused run (463.208 seconds total). The final combined five-case run passed
in 360.176 seconds: standalone registry, standalone packed, workspace registry,
workspace packed and customized registry. Each ran the complete checks described
above on the final source. Earlier full matrices correctly failed their then-broken
workspace bundles; those failed attempts are not represented as passing.

Scoped codegen/generator/template short suites, focused full/race regressions and
`go build ./...` are recorded with the PR. The full repository test gate was not
rerun for this slice; the preceding foundation attempt was not green and cannot
be claimed as a pass. Independent source review found no remaining concrete
blockers after its request-assertion, export-alias and packed-workspace findings
were addressed.

## SDK 58 upgrade slice

The stacked SDK slice moves fresh native scaffolds to Expo 58.0.23's bundled
matrix (React/React DOM 19.3.0, React Native 0.88.1, Router ~58.0.30), with
TypeScript ~6.0.3, Jest 30 and `@react-native/jest-preset` per Expo's SDK 58
template and peers. Workarounds were removed only with evidence: Metro package
exports and workspace-root resolution are SDK defaults; `query-string`,
`newArchEnabled`, `baseUrl`/explicit resolver and hoisted linking are obsolete.
`@babel/runtime`, the runtime devlink Metro block and direct `react-dom` remain.
The ui-native Input ref is derived from `TextInput`, verified to typecheck against
both React Native 0.76.9 and 0.88.1 types, so new shared packages do not break
apps still on SDK 52.

On final SDK 58 source all five consumer cases passed (146.246 seconds), with the
scoped config/codegen/generator/template/component suites and `go build ./...`.
A one-off scaffolded standalone app reported `expo install --check` up to date and
`expo-doctor` 20/20; those Expo checks are not yet part of the automated fixture.
The SDK 52-to-58 migration path for existing apps was not executed: guidance
requires one SDK at a time and does not claim a validated direct jump.

## Native dev-loop slice

The stacked dev-loop slice makes a fresh SDK 58 app usable against a running
forge stack:

- **Transport.** The native Connect transport fetches through `expo/fetch`, so
  server streams arrive incrementally on Hermes and abort cancels them (Jest
  maps `expo/fetch` to the global fetch).
- **Device origin.** `forge env up --device-host` (or `FORGE_DEV_DEVICE_HOST`)
  sets the host a device or simulator uses to reach the API. Metro prefers 8081
  and falls back to an ephemeral port when another process holds it; `env up`
  injects `EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN` with the port it chose.
- **Dev logs.** `src/lib/devlog.ts` forwards `console.*`, uncaught errors and
  unhandled rejections to Metro's `/__forge/log`, which writes them into
  `.forge/logs/<env>/` as `[browser:<level>]` lines, the same shape as the web
  dev servers. All three receivers (Vite, Next.js, Metro) sanitize input, and the
  forwarder masks obvious credentials before sending. Production bundles contain
  none of it.
- **Lifecycle.** `query-lifecycle.ts` drives React Query's focus and online
  managers from `AppState` and `expo-network`. Mutations use
  `networkMode: "always"` in the shared query client.
- **Generated clients.** A Metro `resolveRequest` fallback (`.js` → `.ts`) lets
  generated `_pb` clients bundle with buf's `import_extension=.js` unchanged.

All five consumer cases passed on the final tree, along with the scoped
template, generator, codegen, config and component suites and focused
`internal/cli` tests. An iOS simulator acceptance run (Xcode 27.3 beta,
iPhone 18 Pro, Expo 58.0.23 / RN 0.88.1, Debug build) passed:

- a screen calling a generated client bundled with no redbox;
- a server stream delivered messages about 700 ms apart with 0–11 ms lag, and
  cancel surfaced `Canceled` on the client and `context canceled` on the server;
- focus toggled through inactive, background and active;
- the forwarder attached, multi-line errors arrived one prefixed line each, and
  an unhandled rejection arrived with its stack while the app stayed alive;
- without the origin variable the app warned once and kept running;
- the production export contained no dev-log code;
- `tsconfig.json` stayed byte-identical through generate, prebuild, dev and
  export.

The 8081 path itself was substituted: a non-forge process held 8081, so the
build pinned `RCT_METRO_PORT` to the fallback port. Stack frames from rejections
are bundle URLs, not symbolicated source. Existing apps adopt the slice through
the `migrations/v1.4.0/native-dev-loop` playbook; that path was not executed.

## Native auth slice

The stacked native-auth slice gives a fresh SDK 58 app real sign-in against the
env's identity provider. Research and design notes are in
`docs/proposals/mobile-native-auth/`.

- **Client.** The app is an OAuth public client using authorization code +
  PKCE (S256):
  - Sign-in runs in the system browser (`expo-web-browser`, ephemeral session on
    iOS), and tokens come from the issuer's own endpoints. There is no broker and
    no secret.
  - Tokens live in `expo-secure-store` (`AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY`).
    They are namespaced by issuer and client, and wiped on the first launch after
    a reinstall. Store operations are serialized, so a sign-out racing a refresh
    cannot leave a token behind, and an incomplete stored record loads as signed
    out.
  - Refresh is single-flight. A dead refresh grant ends the session once, with no
    loop. That covers `invalid_grant` and any non-retryable 4xx, including
    Zitadel's `400 invalid_request`. An offline or 5xx failure keeps the session.
  - Sign-out, and any change of `sub`, bumps an identity epoch. The epoch aborts
    in-flight requests, drops late responses and clears the query cache.
- **Server half.**
  - Every `rn` frontend on a host-run identity gets a derived `kind = "native"`
    application (public PKCE, IdP-hosted login). `Frontend.native_sign_in = False`
    opts a frontend out.
  - `forge env up` reads the callback scheme from `app.json`'s `expo.scheme` and
    injects `EXPO_PUBLIC_FORGE_OIDC_*`. Under `--device-host` the dev IdP also serves that address and the run's issuer moves there (`deploy/managed-identity/mobile`).
  - KCL and `pkg/release` accept private-use or https callbacks, so adding
    universal links later is an additive change.
  - On a hosted identity, native applications are refused for now.
  - If the IdP refuses a native application, it is skipped with a warning, and
    the web app's sign-in still publishes.
  - A project created without a frontend gains the dev identity when its first
    frontend is scaffolded.
  - The dev IdP's instance name is now a valid DNS label, so a project named
    `My_App` boots.
- **Scaffold.**
  - New owned files under `src/lib/auth/`, three routes (`sign-in`,
    `oauthredirect`, `signedout`), and a `Stack.Protected` guard in `_layout.tsx`.
  - The transport stack is `[authExpired, epoch, tokenRejected, ...runtime]`. A
    401 renews the token once and replays a unary call once, and only for methods
    declared `NO_SIDE_EFFECTS` or `IDEMPOTENT`.
  - `expo.scheme` is one sanitised reverse-DNS id, also used as the bundle id and
    Android package.
  - Absent variables keep the mock provider. Invalid ones leave the app signed
    out with a visible `configError`.
  - `jest.config.cjs` no longer Babel-transforms `@connectrpc/connect`, which had
    turned every Connect error-path test into `Code.Unknown`.
- **Existing apps** adopt it through `migrations/v1.4.0/native-auth`. That path
  was not executed.

Automated validation on the final tree:

- `TestE2EMobileConsumerConformance`: all five cases passed. Each case ran Jest
  (294 tests, including every auth suite), typecheck, lint with
  `--max-warnings 0`, iOS and Android `expo export` and the dev-server bundle.
  - The fixture fails if a native-auth file or passing auth suite is missing, or
    if `app.json` lacks the secure-store plugin or the reverse-DNS scheme.
  - The customized case shows `generate` preserving owned edits.
  - Both production exports are free of test fakes.
- Short suites pass for `internal/cli`, `internal/devidentity`, `internal/templates`,
  `internal/generator`, `internal/codegen`, `internal/config`, `internal/deploytarget`,
  `internal/bundle`, `pkg/release`, `pkg/identity` and `pkg/components`. The
  full-mode KCL corpus and identity tests also pass.
  - `go vet` and `gofmt` are clean.
  - The `pkg/` API gate reports 10 changes, all compatible.
  - A real-Zitadel test registers private-use and https callbacks and converges
    idempotently.
- An independent review requested changes (one high, four medium, seven low).
  All of them were fixed, with regression tests that fail on the pre-fix code.

**iOS simulator acceptance** (Xcode 27.3 beta, iPhone 18 Pro, local Zitadel from
`forge env up dev`, driven by XCUITest). Two runs; the second was on the final
tree, with a mobile-only project. All checks passed with no substitutions:

- **Guard and config:** a fresh launch showed the sign-in screen, with the
  injected variables using the single sanitised id.
- **Sign-in:** user A signed in through the system browser and came back via
  `com.<project>.<frontend>:/oauthredirect`. An authenticated RPC returned 200,
  so the server accepts the native JWT and its project audience.
- **Cancel:** cancelling in the browser returned to a usable sign-in screen.
- **Restore:** kill and relaunch restored the session with no IdP call.
- **Refresh:** expired and rejected tokens each produced exactly one refresh, and
  the read call was replayed.
- **Logout:** the token was revoked (reuse was refused), the cache cleared, and
  the next sign-in sent `prompt=login`.
- **Isolation:** user B saw none of user A's data.
- **Revocation at the IdP:** one refresh, after which the session ended and stayed
  ended across relaunch.
- **Mock and production:** with no OIDC variables the app ran on the mock
  provider with one warning, and the production export contains no dev-log code.

Physical devices, Android, hosted (control-plane) registration and universal
links were not exercised.

## Explicit remaining gates

- True adopted-app ingress: a customized scaffold is preservation coverage, not
  proof of adopting an independently authored Expo application.
- Entity-backed workspace mocks, pagination/mutation renderer scenarios and
  browser hook execution (browser compilation alone is covered here).
- Backend-only `project new --frontend-workspaces` does not persist a complete
  workspace before a frontend exists; this fixture initializes the canonical
  workspace emitter explicitly. It does not certify that CLI onboarding path.
- Executed existing-app SDK and dev-loop migrations, automated Expo Doctor,
  Android/Gradle builds, and Hermes, streaming/cancellation and network/lifecycle
  behavior on a physical device (the iOS simulator is covered above).
- Real native identity is partly covered: the client, token storage and account
  isolation are tested against fakes, and the iOS simulator run is pending (above).
  Still open: registering native clients with a hosted control plane, Android
  (the emulator needs `adb reverse` for the issuer), a physical device, and
  universal links.
- Device logs/crashes, store publication, signed OTA and health/recovery evidence.

No scenario above certifies physical-device compatibility or production readiness.
The integration PR remains draft until the foundation milestone's broader gates
are met; this slice must not merge independently into main.
