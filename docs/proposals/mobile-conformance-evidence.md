# Mobile consumer conformance slice: evidence

**Status:** evidence record for the mobile foundation milestone, updated at
integration head `dc5976a19` (`feat/mobile-gates`). It is
not a production-readiness claim; the acceptance checklist and the items awaiting
an owner decision are in [mobile-integration-stack.md](mobile-integration-stack.md).
**History:** the sections below are in the order the slices landed. The first
sections describe the Expo 52 baseline slice as it was validated then; the SDK 58
slice supersedes it, and fresh scaffolds are on SDK 58.

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
rerun for this slice, and no full `task test` has been run on the integrated head
(pending owner decision; see the integration checklist). The preceding foundation
attempt was not green and cannot be claimed as a pass. Independent source review found no remaining concrete
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
`expo-doctor` 20/20. Those two Expo checks are now part of the automated fixture
(see "Expo Doctor and `expo install --check` in the fixture" below). The SDK
52-to-58 path for existing apps has since been executed; see "Executed upgrade of
an existing v1.3.0 app". It was run one SDK at a time, not as a direct jump.

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
the `migrations/v1.4.0/native-dev-loop` playbook, which has been executed (see
"Executed upgrade of an existing v1.3.0 app").

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
    injects `EXPO_PUBLIC_FORGE_OIDC_*`. Under `--device-host` the run's issuer
    moves to the LAN address (see the device-reachable issuer section below and
    `deploy/managed-identity/mobile`).
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
- **Existing apps** adopt it through `migrations/v1.4.0/native-auth`, which has
  been executed on a real v1.3.0 app (see "Executed upgrade of an existing v1.3.0
  app"). That run did not include a simulator sign-in on the upgraded app.

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

That run was on the simulator only. Physical-device runs are recorded in the two
sections below. Android, hosted (control-plane) registration and universal links
were not exercised anywhere; each is pending an owner decision (see the integration
checklist).

## Device-reachable dev identity issuer

A physical device cannot reach `localhost`, and the dev IdP's issuer is the one
address the API, the login broker and the native app must all agree on. So
`forge env up --device-host <LAN IPv4>` now moves the whole run's issuer to the
LAN address, rather than only changing where the API is dialled.

- **What `--device-host` does for the IdP.** Forge restarts the dev Zitadel once
  with the LAN address added as an extra instance domain (no data is reset) and
  publishes `http://<LAN IP>:<idp port>` as the run's single issuer. The API's
  `jwt_issuer`, the broker and the native app's injected
  `EXPO_PUBLIC_FORGE_OIDC_ISSUER` all use it. A run without the flag publishes
  `localhost` again and wipes nothing.
- **When it applies.** Only for an IPv4 address that belongs to this machine, and
  only when a native frontend has a native application. A hostname, an IPv6
  address or `10.0.2.2` keeps the localhost issuer and prints a notice asking for
  the LAN IPv4.
- **Fallback.** If the IdP cannot serve the device host, forge falls back to the
  localhost issuer instead of failing `env up`.
- **Plain-HTTP userinfo, opt-in.** The API enriches claims such as email from the
  issuer's userinfo endpoint, which is plain HTTP on a dev LAN issuer. That is off
  by default (`jwt_userinfo_allow_local_http`); forge sets it only for
  identity-consuming host workloads in a `--device-host` run, so production
  behaviour is unchanged.
  - When enabled, the request goes through the new `pkg/netguard.LocalOnly`
    transport. It ignores proxies and checks at dial time that the remote IP is on
    this machine. The userinfo URL must have the issuer's origin, redirects
    included.
  - The broker's IdP client uses the same transport, behind an explicit
    `devidp.Client.LocalOnly` flag.
- **LAN notice.** The IdP's start line now says it listens on all interfaces and
  is reachable from the LAN over plain HTTP with the dev credentials.
- **Owned files.** `auth.go`, `config.proto` and `login_broker.go` change; the
  `migrations/v1.4.0/native-auth` playbook lists the edits (and, since the
  executed upgrade run, the matching `LocalIssuerHTTPClient` edit that confines
  the JWKS and discovery fetch to this machine under the same opt-in).
- **Review.** An independent review requested changes (one high, five medium,
  plus lows). All were fixed and a re-review approved. The fixes include
  connect-time confinement (`f9946a513`) and the explicit broker signal
  (`451ee050a`). A later test-only commit (`1ce0a15fc`) fixed the full-mode
  `TestNativeJestTransformGuards`, which still required `@connectrpc/connect` to
  be Jest-transformed after the native config deliberately stopped doing so.
- **Real-Zitadel coverage.** `TestConverge_DeviceHostAgainstTheRealHostZitadel`
  (six subtests) and `TestConverge_NativeApplicationAgainstTheRealHostZitadel`
  pass against a real host-run Zitadel.
- **Not exercised on the device runs:** the broker path (the device project had no
  web frontend) and the fallback to localhost (the IdP could always serve the
  device host on that machine).

## Physical device (iPad)

Hardware: an iPad Pro 12.9" 6th generation on iPadOS 27.2, over Wi-Fi to the
development Mac, built with Xcode 27.3 beta and signed with the Reliant Labs
development team (automatic signing, development certificate only). Each run used
a fresh scaffolded project (`forge project new`, then `forge scaffold frontend
mobile --kind mobile`) with scratch-only additions: a `Ping` unary and a `Ticks`
server stream, a `WhoAmI` rpc that logs the caller's claims, and a probe screen
instrumented to log (never a token). The Metro port fell back to 3000 because
another process held 8081, as in the simulator run. The evidence set (build logs,
Metro and API logs, screenshots, XCUITest result bundles) is kept with the
acceptance run, not in this repository.

### Dev loop

Run on integration head `cf809a8f9`, Debug build, `forge env up dev --device-host`
with the Mac's LAN address. All seven checks passed, one with a note and one only
with explicit configuration:

| #   | Check                                                                                                                                     | Result                                                                                                                                                                                                                                                                                                        |
| --- | ----------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Debug build installs and loads JS from Metro over Wi-Fi, no redbox                                                                        | Pass. The first launch after install redboxed (`No script URL provided`) while the Local Network prompt was pending; every launch after allowing it was clean.                                                                                                                                                |
| 2   | Generated client reaches the API over the LAN                                                                                             | Pass. The API logged the iPad's IP through `connectClient` and `expo/fetch`.                                                                                                                                                                                                                                  |
| 3   | Server stream is incremental; cancel surfaces `Canceled` on the client and `context canceled` on the server                               | Pass. Some adjacent messages coalesced within about 1 ms, consistent with Wi-Fi power-save batching.                                                                                                                                                                                                          |
| 4   | Dev logs reach `.forge/logs`: forwarder attached, multi-line errors arrive as prefixed lines, an unhandled rejection arrives with a stack | Pass. The origin came from the injected `EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN`; a plain Debug build has no Expo manifest to read it from.                                                                                                                                                                          |
| 5   | Backgrounding toggles React Query focus; Airplane Mode flips the online manager and paused queries resume                                 | Pass. Lines logged while offline reached Metro through the in-app replay buffer once back online.                                                                                                                                                                                                             |
| 6   | Release build: embedded Hermes bundle launches, carries no dev-log code, reaches the API                                                  | Pass with explicit config. The bundle had no `installNativeDevLogging`, `forge-devlog` or `__forge/log` (a positive control confirmed the grep sees string literals). The default Release dialled `localhost` and failed; with `EXPO_PUBLIC_API_URL` set to the LAN API it connected, streamed and cancelled. |
| 7   | `expo-doctor` and `expo install --check`                                                                                                  | Pass. 20/20 checks, and dependencies up to date.                                                                                                                                                                                                                                                              |

A frontend with `native_sign_in = False` also kept the mock provider on the
device, with its one warning and no sign-in screen.

### Native sign-in

Driven by an XCUITest runner on the iPad against the local Zitadel, with seeded
users alice and bob. Two runs, both on Debug builds.

**First run, `88cb950de`: 8 of 8 passed.**

- The injected issuer, client id and redirect used the LAN address and the single
  sanitised scheme, and a fresh launch showed the sign-in screen.
- Alice signed in through the system sheet and returned via
  `<scheme>:/oauthredirect`; the code exchange returned 200 and `WhoAmI` returned
  200 from the iPad's IP, with the email filled in through the userinfo opt-in.
- Cancel in the sheet, twice in a row, left a usable sign-in screen.
- Kill and relaunch restored the session from the device keychain with no IdP
  call and no browser.
- A forced expired token produced exactly one refresh, and concurrent calls
  succeeded.
- Logout revoked the refresh token (reuse got `400 invalid_request`), cleared the
  query cache, and the next sign-in sent `prompt=login`. Bob then saw none of
  alice's data.
- The scaffold's Jest suites passed (294 of 294, including `auth-config`).
- Localhost discovery kept working during the device-host run, and switching back
  to a run without the flag restored the localhost issuer and wiped nothing.

**Re-run on the fixed commit, `f9946a513`: 4 of 4 passed, plus 2
confirmations.** The binary was built from that commit's tree, because the
worktree head had moved on to a test-only commit, and the project was fresh and
carried the new owned files.

- The injected environment, the LAN issuer and the `JWT_USERINFO_ALLOW_LOCAL_HTTP`
  opt-in were present, and the sign-in screen showed.
- Alice signed in with an authenticated `WhoAmI` carrying her email, which
  Zitadel's own log shows came from a userinfo request to the LAN address.
- Kill and relaunch restored the session.
- Logout and then bob's sign-in showed no data from alice.
- Confirmed: the IdP start line carries the LAN-reachability notice.
- Confirmed: `--device-host` with a hostname kept the localhost issuer, left the
  userinfo opt-in off and asked for the LAN IPv4.

### Known limits

- **Release builds.** A Release build made outside forge bakes the dev API default
  (`http://localhost:8080`), which a device cannot reach. Pointing it at the dev
  stack needs an explicit `EXPO_PUBLIC_API_URL` at build time. Deployable builds
  take it from KCL.
- **First Debug launch.** The first launch of a Debug device build shows `No
script URL provided` until Local Network access is allowed, then works after
  relaunching. React Native's bundle-URL check fails while the prompt is pending
  and a Debug build has no embedded bundle. The prebuilt Info.plist has no
  `NSLocalNetworkUsageDescription`, so the prompt shows generic text. The current
  Metro-port hint describes this.
- **LAN domain persists.** After switching back to a run without `--device-host`,
  the dev IdP still answers at the LAN address with that issuer, because Zitadel
  offers no way to remove an instance domain. The API pins the localhost issuer
  again, so tokens from the LAN issuer are not accepted.
- **Observed wording defect.** On `f9946a513`, the per-frontend sign-in line for a
  hostname `--device-host` gave the wrong reason for the localhost issuer; the
  run-level notice was correct. It was a message defect only, fixed in `451ee050a`
  (the line now names the refused device host and asks for the LAN IPv4 address),
  with unit tests for hostname, IPv6 and `10.0.2.2`; it was not re-run on a device.
- **Harness notes.** The runner needed Local Network allowed and Settings ›
  Developer › Enable UI Automation turned on, and submitting the Zitadel login
  fields with Return was reliable where tapping Next during the keyboard animation
  was not. Every device console line also appears twice in the frontend log, once
  as Metro's own line and once as `[browser:…]`.

### What this does not prove

- **Android:** neither an emulator nor a device was run.
- **iPhone hardware:** only an iPad was used.
- **Hosted registration:** the device runs used the host-run dev IdP; native
  applications on a hosted identity are still refused.
- **Release-mode sign-in:** sign-in was run on Debug builds only.

## Expo Doctor and `expo install --check` in the fixture

Each of the five consumer cases now also runs `expo-doctor` and `expo install
--check` in the app directory, with the case's package manager. `expo-doctor` is
pinned (1.21.6, via `pnpm dlx`; it is not an app dependency and the SDK 58 CLI does
not bundle it), because a new release adds checks and would fail a green scaffold
with no change on our side. A real finding fails the case. If Expo's own service
is unreachable (a host or network outage, classified from the command output and
exit code, with one real failed check among network ones still a failure), the case
runs its remaining checks and then skips, so a host outage costs only that verdict.
On the final tree all five cases reported **20/20 checks passed** and
**dependencies up to date**. The fixture now sets `EXPO_DOCTOR_ENABLE_DIRECTORY_CHECK=0`
(expo-doctor 1.21.6's documented switch): that one check validates dependencies
against live React Native Directory metadata, so a Directory change could redden the
run with no change on our side. Doctor then runs 19 checks, and the
`standalone/registry` case reported **19/19 checks passed** with it off; every other
check still runs.

## Executed upgrade of an existing v1.3.0 app

The existing-app path was executed end to end, by an agent following the shipped
playbooks, on a real app: a v1.3.0 scaffold (Task entity, Expo 52 mobile frontend,
Next.js web frontend) that was then customized as a user would (a task-list home
screen using the generated hooks, a settings screen, a header interceptor,
`date-fns`, an edited `app.json` with a two-entry `scheme`). The project was
upgraded to this head with `forge project upgrade`, then one commit per playbook
step: section 2 (foundation), section 3 one SDK at a time (53, 54, 55, 56, 57, 58),
section 4 (dev loop), section 5 (native auth). Each SDK step was gated on
`typecheck`, `lint --max-warnings 0`, `test`, an iOS and an Android `expo export`,
`expo-doctor` and `expo install --check`.

**Result.** It ends green on SDK 58: typecheck, lint, 293 Jest tests (10 suites;
a fresh scaffold has 294 in 11, the difference being `tests/fetch-stub.test.ts`,
which the playbook did not list; it does now), iOS and Android export, the Expo web
export, `expo-doctor` 20/20, `expo install --check` up to date, and
`forge generate` at a fixed point on a second run. Per step, `expo-doctor` was clean
on 53 (18/18), 54 (18/18) and 57 (21/21); on 55 it flagged `newArchEnabled`, which
that step's commit removed; on 56 it fails by design (below). Jest was red on 53
to 57 for one case only, the `app-config` scheme assertion of section 5 (below). `forge env up dev` started the
stack, Metro, the dev IdP and the native sign-in client, and printed the
sign-in line with the injected issuer, client id and `com.taskapp.mobile` redirect.

**What was skipped in that first run.** A simulator sign-in on the upgraded app was
not run, and the web frontend's `forge lint` lane failed once on the dev-log
receiver's `no-control-regex` (a template defect, since fixed in the Next.js and
Vite templates and added to the playbook as an edit). Both are covered by the
corrected-playbook run below.

**Playbook defects the run found, fixed in this pass** (each was a step the playbook
described that did not work as written):

- Nothing said how to obtain a "current scaffold" file. The playbooks now name
  `forge project rescaffold <path>` (absent file), `forge project upgrade --check
<path>` (diff) and `forge project upgrade --force <path>` (replace).
- Section 2 added `jest.config.cjs` without `tests/setup/expo-fetch.ts`, which it
  loads, so every test suite failed until it was added; `tests/fetch-stub.test.ts`
  was never listed.
- `tests/app-config.test.ts` asserts section 5's scheme contract but was added in
  section 2, so Jest was red on every SDK step; it is now added without that case
  and completed in section 5.
- The Metro `.js` to `.ts` resolver was in section 4 but `expo export` needs it
  from SDK 53 (the run pulled it forward); section 2 now requires it first.
- SDK 56's `expo-doctor` fails on a Hermes V1 regression that only SDK 57 clears;
  the playbook now says to continue rather than look for a fix.
- SDK 58's `expo install --fix` fails with `ERR_PNPM_IGNORED_BUILDS` for Jest 30's
  `@parcel/watcher` unless `allowBuilds` names it first; the playbook says so, and
  uses `npx expo install expo@~N.0.0 --fix`.
- `connect-transport.test.ts` was in the dev-loop playbook but exercises auth
  interceptors; it moved to native auth.
- Paths read `shared/src/...`; a project's are `frontends/<name>/src/...`, in every
  frontend including web.
- A pre-v1.4.0 `deploy/kcl/dev/main.k` declares a native `port`, which defeats
  Metro's preference for 8081; the playbook says to delete it (the scaffold no
  longer writes one).
- Section 5 told apps to rewrite `ios.bundleIdentifier` and `android.package` to
  match `expo.scheme`, which renames a published app; it now says to set
  `expo.scheme` alone and never to touch either, and offers ways to keep a custom
  scheme.
- A "generated-hook test" was required but the scaffold ships none; the
  requirement is dropped.
- Four shared hook files' `ConnectClientError` type import moves to
  `/interceptors`; they were not listed. Also newly listed: the staging and prod
  `runtime_config = f.runtime_config | _api_config` line, the web `next.config.ts`
  request-log rule and `route.ts` query-string rejection, the web `provider.ts`
  optional members, and the `internal/app/auth.go` JWKS and discovery
  confinement edit.

Upgrade tooling findings from the same run were reported to engineering
separately: the web lint failure, the KCL native `port` defeating 8081, and
`forge project upgrade` with a `+dev` build.

### Executed v1.3.0 → v1.4.0 upgrade with corrected playbooks

A second agent upgraded a v1.3.0 app by following the corrected playbooks
literally, from an installed binary and without help. It ended fully green:
typecheck, lint, Jest 294 (equal to a fresh scaffold), `expo-doctor` 20/20,
`expo install --check`, iOS and Android export with no dev-log code or fakes in the
bundle, `forge lint` 28/28, Metro on 8081, and the native client derived from the
first `expo.scheme` entry, with customizations (including a kept custom scheme)
preserved. An iOS simulator run on the upgraded app then passed sign-in, an
authenticated `ListTasks`, restore after relaunch, and logout followed by
`prompt=login`. Verdict: an agent can do this upgrade unaided.

The run found five playbook defects, fixed in this pass: the staging and prod
`runtime_config = f.runtime_config | _api_config` edit sat behind section 1's
replay-only "Use when" (it now applies to every project, and a note covers adopting
replay in an env that never had it); the `tests/app-config.test.ts` assertions did
not say how to adapt to each keep-a-scheme option; the `metro.config.js` bullet left
a stale header comment and a misplaced `nodeModulesPaths` unless the top of the file
was replaced with the template's; section 2 did not name `expo.web.output: "static"`
as the declared output of the native `build` script, and its SDK 52 install advice
read as optional although section 2 runs before the SDK upgrade; and the
`expo-asset` and `expo-status-bar` plugins that `expo install --fix` adds are
optional (a fresh scaffold has neither), not "keep".

## https universal links / App Links (pending device acceptance)

Design: [mobile-native-auth/universal-links.md](mobile-native-auth/universal-links.md).
Owner-confirmed defaults, built as final: the test domain is a Reliant-hosted dev
static site hostname; Android dev uses the per-machine debug keystore declared per
env in KCL and release uses Play App Signing; iOS keeps the private-use fallback
below 17.4; Associated Domains come from Xcode automatic signing on team
`23P64LQTZD`; one bundle id per app.

**Status: implemented and unit-tested; device acceptance has not been run.** Nothing
below claims a signed build signed in through an https link.

### What is built, and the unit evidence

| Part | Evidence |
|---|---|
| KCL `associated_domains` (kind native), `apple_team_id`, per-env `android_cert_fingerprints`, `associated_site`; the first https host must be in the domains | `kcl/tests/{positive,negative}_*associated_domain*`, `negative_frontend_{apple_team_id,android_fingerprint}_*`, `TestKCLModule_*` |
| Shape and validation; one https-prepend helper (`release.PrependAppLinkRedirect`) | `pkg/release/applinks_test.go`; the field is part of `shape-identity-native-app`, which is not advertised, so no control plane is sent it |
| Rendered `apple-app-site-association` and `assetlinks.json` (exact contents, sorted, byte-stable, `debug` via `keytool`) | `internal/applinks/applinks_test.go` |
| Per-env resolution, site choice, refusals | `internal/cli/app_links_test.go` |
| Serving: staging overlay, OnBucket upload as `application/json`, OnFirebase `headers`, hosted spec `wellKnown` | `internal/deploytarget/wellknown_test.go`, `pkg/deploy/v1alpha1/wellknown_test.go`, `internal/cli/frontend_custom_domains_test.go` |
| Dev env: https first redirect, `EXPO_PUBLIC_FORGE_OIDC_FALLBACK_*` inside the one OIDC unit, plugin variables, developer mode for a local env only | `internal/cli/native_dev_oidc_test.go`, `internal/cli/app_links_env_test.go` |
| Expo plugin (`ios.associatedDomains` with both services, `android.intentFilters` with `autoVerify`, inert without a domain) | `tests/with-forge-links.test.js` (Jest) |
| Client: https on Android and iOS 17.4+, fallback below; one URI for authorize, match and token; `preferUniversalLinks`; logout pair | `tests/auth-redirect.test.ts`, `tests/auth-config.test.ts`, `tests/auth-provider.test.ts` (Jest, inside `TestE2EMobileConsumerConformance`) |

### Control-plane requirement (separate work; forge does not touch the control plane)

For a **hosted** site, forge publishes the files as the StaticSite spec's `wellKnown`
map, a sibling of `runtimeConfig`: lowerCamelCase field, `map[string]string`, keyed by
file name (`apple-app-site-association`, `assetlinks.json`), each value the file's
exact contents as a raw JSON string (not base64), at most 8 entries and 128 KiB each.
The release artifact does not carry them: they are per-environment, because the
Android fingerprints differ between environments. After every sync (a sync prunes
files the release lacks) the platform must serve:

- `GET https://<host>/.well-known/apple-app-site-association` and
  `GET https://<host>/.well-known/assetlinks.json` at the **host root**, not under
  `basePath`;
- **200**, **`Content-Type: application/json`** (the extensionless file cannot be
  inferred, and `_headers` refuses `Content-Type` today), body byte-for-byte the
  spec's value;
- **no redirect of any kind** (no apex/www, trailing-slash or `.html` rewrite) and
  **no SPA fallback**; a missing file is a 404, never `index.html`;
- over https with a certificate valid for the host, with no cookies.

A hosted control plane must advertise `GetCapabilities.resourceFeatures` containing
`static-site-well-known` before forge publishes a nonempty `spec.wellKnown`.
An absent field, an old RPC, or a missing feature fails closed with upgrade guidance;
forge never silently drops required verification files. This check runs before hosted
records enter a bundle, at snapshot sealing and promotion, and at the hosted provider
boundary. The platform strict-decodes CR specs and older CRDs prune unknown fields,
so decode/serve support must land together. Fetching the files still remains acceptance
row 1.

### Validation record (2026-11-11)

- `go build ./...`, `go vet ./...`, changed Go files' `gofmt`, and `git diff --check`: passed.
- Requested `TMPDIR=/private/tmp/forge-devloop-tmp task test:short -- ./pkg/... ./internal/cli/... ./internal/templates/... ./internal/deploytarget/... ./internal/applinks/... ./internal/devidentity/...`: passed.
- Full-mode `go test -count=1 ./internal/templates/`: passed (99s).
- `TestE2EMobileConsumerConformance`: all five cases passed (650s), including the
  new Jest suites, Doctor 19/19, typecheck, zero-warning lint, native exports and
  Metro. The final rerun after the app-owned intent-filter preservation regression
  test also passed all five cases (225s) on clean, stable HEAD `ab76860f7`.
  The later factory-codegen fix was cherry-picked separately and validated with
  focused codegen/generator checks; it does not alter the client templates.
- `go run ./internal/apigate -base origin/main`: fails because main advanced
  beyond this mobile branch (51 identifiers added on main are absent here, in
  accesstoken, dashspec, obsquery, releasehealth and release). This is branch
  drift, not this slice deleting APIs. Against the actual slice base
  `00a561e04`, apigate passes with eight compatible additions.

The SDK patch cohort moved during validation: Expo 58.0.24 was published on
2026-11-11. Doctor reads current SDK metadata, while pnpm 12 prefers aged versions
within a compatible tilde range. Fresh fixtures (no lockfile) therefore resolved
58.0.23 while Doctor required 58.0.24. The scaffold floors now match the official
`expo-58.0.24.tgz` bundledNativeModules.json and Babel dependency: Expo .24, asset
.21, constants .16, crypto .8, linking .17, network .5, router .31, secure-store .3,
status-bar .5, web-browser .8, babel-preset-expo .25. No Doctor assertion or
supply-chain policy was bypassed. External SDK metadata is a moving gate input,
not a test flake.

### Acceptance (pending)

Rows 1-8 of the design's test plan need a signed build and the public host and are a
manual or Maestro lane, not CI:

| # | Where | Pass |
|---|---|---|
| 1 | files | `curl -sI` both URLs: 200, `application/json`, no 3xx, valid JSON |
| 2 | iOS Sim 17.4+ | session closes, tokens stored, host log shows no `/oauthredirect` GET. Run once with only `applinks:` to settle the `webcredentials` question |
| 3 | iOS Sim 17.0 | client picks the private-use entry; sign-in succeeds |
| 4 | iOS | `/signedout` closes the session |
| 5 | Android emulator (API 34+, Chrome) | `pm get-app-links` is `verified` without manual approval |
| 6 | Android | Custom Tab hands off, `Linking` reports `success`, tokens stored |
| 7 | Android | unverified link: the fallback page appears and "Open in app" completes sign-in |
| 8 | both | kill the app mid-login, then complete |

**Known gap, not built:** the `/oauthredirect` and `/signedout` web fallback pages
(row 7) are not scaffolded. Until they are, an unverified Android link loads the
site's own page for that path and sign-in fails there; the private-use entry is the
fallback only on iOS below 17.4 and in a domain-less build.

## Explicit remaining gates

- True adopted-app ingress: a customized scaffold is preservation coverage, not
  proof of adopting an independently authored Expo application.
- Entity-backed workspace mocks, pagination/mutation renderer scenarios and
  browser hook execution (browser compilation alone is covered here).
- Backend-only `project new --frontend-workspaces` does not persist a complete
  workspace before a frontend exists; this fixture initializes the canonical
  workspace emitter explicitly. It does not certify that CLI onboarding path.
- https universal links / App Links on a signed iOS build and an Android emulator
  (acceptance rows above), and the hosted control plane serving the files.
- Android: Gradle builds, and Hermes, streaming/cancellation, network/lifecycle
  and sign-in behavior on an emulator and a physical device. The emulator needs
  `adb reverse` for the issuer.
- A hosted control plane registering the native client end to end (forge sends it
  once the control plane advertises `shape-identity-native-app`), and universal
  links (pending owner decision, as is Android above).
- Real native identity on iOS is covered on the simulator and on one iPad (above).
  iPhone hardware and Release-mode sign-in are not.
- Device crashes, store publication, signed OTA and health/recovery evidence.
- A full `task test` result on the integrated head, or an authorized scope
  statement for the known host failures (pending owner decision).

Covered since the earlier revision of this list: the iOS physical device (dev
loop and native sign-in, on one iPad Pro). No scenario above certifies Android,
iPhone or production readiness.
The integration PR remains draft until the checklist in
[mobile-integration-stack.md](mobile-integration-stack.md) is resolved or each
remaining item has a recorded owner decision. Only the integration branch merges
to main: every slice is already an ancestor of its head, so there is no separate
slice merge.
