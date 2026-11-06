# Proposal: mobile apps as a forge release target

**Status:** proposal, not implemented. Written for owner review; nothing here
has landed in forge, control-plane or reliant.
**Date:** 2026-11-06
**Scope:** forge (React Native scaffold, dev loop, KCL schema, CLI,
`pkg/release`, a new `pkg/push`), with adoption in reliant (`frontends/mobile`)
and control-plane (the channel envs).
**Builds on:** `docs/proposals/desktop-release-target.md` (builders, channel =
env, one OCI release artifact, secret names). That proposal lists "Mobile
(iOS/Android) and store submission" as a non-goal and says "the builder
protocol and the channel model carry over, but the packager and publications
are different work". This is that work.
**Forcing case:** the Reliant native app (reliant
`dev-docs/design/native-mobile-app.md`), iOS first, Android after.

## Summary

forge can scaffold an Expo app (`forge scaffold frontend <n> --kind mobile`)
and generate Connect hooks for it, but stops there. A React Native frontend
cannot be released, deployed, health-checked or even observed in dev by
forge. Every team that ships one would hand-write an EAS pipeline beside
forge, which is the two-authority shape the desktop proposal exists to remove.

The recommended design:

1. **Fix the dev loop first (phase F0).** The RN scaffold pins Expo SDK 52
   and React 18 (the current SDK is 57, on React 19), ships no dev log mirror, no per-env config, no
   streaming-capable transport, and is excluded from scenarios and the test
   lanes. These block any real app, and are independent of releasing.
2. **`forge.MobileApp`** is a new declared entity, sibling of
   `forge.DesktopApp`. It names its source, its packager (Expo first), its
   platforms, and for each platform the **builder** and **signing identity**.
   It replaces a `type = "react-native"` `forge.Frontend` for anything beyond
   `OnHost`.
3. **A release is one OCI artifact, `mobile.v1`**: per platform, the signed
   store binary (`.ipa` / `.aab`), its **runtime fingerprint**, the over-the-air
   (OTA) JS update built for that fingerprint, and the debug symbols
   (dSYMs, source maps). It goes in the ledger as an ordinary shared `oci`
   artifact, exactly as `desktop.v1` and `static.v1` do. No closed-enum change.
4. **A channel is an environment.** `mobile-beta` and `mobile-prod` are envs
   whose app binding is a `forge.MobileChannel` with up to two publications:
   **store** (TestFlight / App Store / Play tracks) and **updates** (an OTA
   channel). Promoting TestFlight's build to the App Store is
   `forge env deploy mobile-prod --from mobile-beta`, the same uploaded build,
   which is literally promote-by-digest.
5. **The plan learns about mobile releases**: identity changes, build numbers
   that are not ahead, OTA updates that no installed binary can run, and new
   permissions or entitlements that need App Review.
6. **`forge obs check` covers mobile**: crash-free sessions and update
   adoption from the app's OTel signals, so a mobile deploy can be proven good
   or reported "not proven good" like every other deploy.
7. **Native-app plumbing that every forge mobile app needs** becomes forge's:
   `forge/pkg/push` (APNs, then FCM, including Live Activity pushes),
   generated `apple-app-site-association` / `assetlinks.json` on the bound web
   frontend, native sign-in for RN, and a Maestro runner for scenarios.

Phases: F0 dev loop, F1 the shared app-release core (with desktop), F2 mobile
build and cut, F3 publication, F4 health and native plumbing, F5 Android. Each
phase is useful alone. F0 is required before any real app; F0–F3's iOS slice
is required before a forge-built TestFlight.

---

## 1. Where things stand

Read at forge `6e107397` (main, 2026-11-06).

### What forge does for a mobile frontend today

| Area | State | Evidence |
| --- | --- | --- |
| Scaffold | `--kind mobile` writes an Expo Router app: `app.json`, `app/_layout.tsx`, `app/index.tsx`, `metro.config.js`, `src/lib/connect.ts`, `src/lib/apiurl_gen.ts` | `internal/templates/frontend/react-native/` |
| Versions | Expo `~52.0.0`, React Native `0.76.9`, React `^18.3.0`, `jest-expo ~52` | `react-native/package.json.tmpl` |
| Generated hooks | Generated for RN frontends, and into the shared hooks package under frontend-workspaces | `internal/cli/generate_frontend_hooks.go`, `frontend_workspaces.go` |
| Transport | `@connectrpc/connect-web` `createConnectTransport` over global `fetch` | `react-native/src/lib/connect.ts.tmpl` |
| Auth | Mock session provider only; `setAuthTokenGetter` seam, wired to nothing | `react-native/app/_layout.tsx.tmpl`, skill `auth/frontend` |
| Per-env config | `forge.Frontend.env_vars` are documented as build-time `EXPO_PUBLIC_*`, but the generated runtime config loader returns `{}` on RN | `internal/codegen/frontend_config_ts.go:133` |
| Dev logs | No mirror. Next.js has `/__forge/log`, Vite has `devlog-plugin.ts`; RN has neither, so `[browser:*]` lines never reach `.forge/logs/<env>/` | `internal/templates/frontend/{nextjs,vite-spa}` |
| Dev API URL | `DEV_API_URL` is the host's localhost port; the scaffold tells the user to set `EXPO_PUBLIC_API_URL` to a LAN IP or tunnel by hand | `connect.ts.tmpl` comment |
| Scenarios / e2e | RN frontends are excluded | `internal/cli/scaffold/scenario.go:201` |
| App identity | Static `app.json`, `bundleIdentifier = com.<project>.<frontend>`; nothing in KCL | `app.json.tmpl` |
| Build | `forge build` has no RN target; root-frontend discovery skips RN | `internal/config/frontend_inventory.go:207` |
| Release / deploy | `forge.Frontend.runtime` is `OnHost`, `OnHosted`, `OnBucket`, `OnFirebase` or `BuildOnly`. None publishes a mobile app; `deploy/release` and `deploy/frontends` never mention one | `kcl/schema.k:3404` |
| Observability | `@opentelemetry/api` is a dependency; no exporter, no crash capture, nothing `obs check` reads | `package.json.tmpl` |
| Push, deep links | Nothing | — |

### What that costs

- **Version skew with web.** A frontend-workspaces project shares packages
  between web (React 19) and mobile (React 18). The scaffold already carries a
  workaround for it: `connect.ts` imports `forge-web-runtime/interceptors`
  because "the barrel also carries React components typed against React 19,
  which Expo's React 18 types reject". Every shared package with a React
  component hits the same wall.
- **Streaming RPCs.** React Native's global `fetch` does not expose a
  streaming response body, so `connect-web` server-streaming RPCs (the
  Reliant chat stream) cannot work on it. `expo/fetch` exposes one; the
  Reliant Phase 0 spike verifies it end to end.
- **Debugging is blind.** The `debug` skill's first move, `grep
  '\[browser:error\]' .forge/logs/dev/*.log`, finds nothing for a mobile app.
- **Releasing is out of forge.** A team runs `eas build`, `eas submit` and
  `eas update` by hand or in CI. There is no ledger entry, no plan, no
  approval, no health gate, and no record of which JS a store binary is
  running. That is exactly the desktop proposal's §1 list of costs, moved to
  phones, plus a hazard desktop does not have: an OTA update reaches users in
  minutes with no store review.

## 2. Goals and non-goals

**Goals**

- A forge mobile app is a first-class project shape: scaffold, dev loop,
  logs, tests and scenarios work as they do for web.
- A mobile app is declared in KCL. `forge env build <env> --release <v>` cuts
  it into the ledger; `forge env deploy <env>` publishes it to a store track
  and/or an OTA channel with a plan, approval and ledger entry.
- OTA updates and store binaries are tied together by the runtime
  fingerprint, so forge can say which installs an update reaches.
- Signing and store credentials go through the env's secret provider, as in
  the desktop proposal §5.4.
- `forge obs check <mobile env>` proves a mobile release good or says it
  could not.
- The release core (builders, OCI app artifact, channel envs) is **shared
  with `DesktopApp`**, built once.

**Non-goals**

- Replacing Expo, EAS's native toolchain, `xcodebuild`, Gradle or
  `expo-updates`. forge orchestrates them; it does not reimplement them.
- Writing App Store metadata (screenshots, descriptions, privacy nutrition
  labels) from KCL in the first cut. Those stay in App Store Connect; §11 asks
  whether they should move.
- Hosting the expo-updates protocol on the control plane in the first cut.
  It is phase F4 (`HostedUpdates`); EAS Update is the first adapter.
- Flutter, native Swift/Kotlin apps. The schema leaves room (`packager` is a
  union), but only Expo is built.

## 3. The mapping

| Mobile concept | forge concept |
| --- | --- |
| One version's signed `.ipa`/`.aab`, its OTA bundle, its symbols | A **release** artifact `mobile.v1`: immutable, content-addressed |
| A TestFlight group, the App Store, a Play track, an OTA channel | An **environment** whose app binding publishes there |
| Moving a build to the App Store, or an update to the production channel | A **promotion**: append-only, CAS, plan-gated |
| "Ship what beta has to production" | `forge env deploy mobile-prod --from mobile-beta` |
| The Mac that archives and signs; the Linux box that runs Gradle | A **builder** (desktop §5.3, unchanged) |
| Distribution certificate, App Store Connect key, upload keystore | Secret **references** resolved by `Bundle.secret_provider` |
| `runtimeVersion` / native fingerprint | A field of the artifact, and the compatibility key the plan checks |
| `EXPO_PUBLIC_API_URL`, Supabase URL, bundle id per env | `build_env`, part of the artifact's **variant** key |
| Crash-free rate on beta before production | A **gate** recorded against the beta promotion |

## 4. Alternatives considered

### A. Who owns the pipeline

| Option | Verdict |
| --- | --- |
| **A1. EAS owns it** (EAS Build, Submit, Update, driven from CI) | Rejected as the model. It is the two-authority shape: no ledger, no plan, credentials held by a third party, and forge cannot say what a channel serves. Kept as **adapters** (A4). |
| **A2. forge generates an EAS workflow** (`.eas/workflows/*.yml` or a GitHub workflow calling `eas`) | Rejected, for the reason desktop rejects A2: build logic only one vendor can run, with release identity bolted on after. |
| **A3. `ShellBuild` around `eas build --local`** | Rejected: opaque to forge, with no artifact identity, signing model or publication. |
| **A4. First-class `forge.MobileApp` on the shared builder primitive; EAS services as optional transports** (recommended) | forge owns the build graph, release identity, publication and the plan. Expo's toolchain does what only it can do. EAS Build and EAS Update are adapters a team may choose, not the system of record. |

### B. Where iOS builds run

| Option | Verdict |
| --- | --- |
| **B1. EAS Build cloud** | Allowed as `forge.EASBuilder`, for teams without macOS capacity. Cost: signing credentials live in Expo's credential store, outside `secret_provider`. forge records the EAS build id and verifies the returned `.ipa` signature, but cannot attest to how it was built. |
| **B2. A forge builder on macOS** (`LocalBuilder` on a Mac, `GitHubActionsBuilder` `macos-15`, `SSHBuilder`) running `expo prebuild` + `xcodebuild archive` / `-exportArchive` (recommended default) | The desktop builder protocol unchanged; credentials come from the secret provider, and the build is reproducible from the job spec. Same runner economics as desktop (open question 2 there). |
| **B3. Cross-build iOS on Linux** | Not possible: `xcodebuild` requires macOS. |

### C. How channels map

**C1, channel = environment**, adopted from desktop §4.C unchanged, with one
refinement: a mobile channel env has **two** publications, because a phone
receives software two ways.

| Option | Verdict |
| --- | --- |
| **C1a. One env per store track AND one per OTA channel** | Rejected. The OTA channel and the store track are one audience (TestFlight beta testers run the beta binary and pull the beta update channel). Splitting them makes "what does a beta tester run" a join across two envs. |
| **C1b. One env = one audience = `MobileChannel {store?, updates?}`** (recommended) | One env answers "which binary and which JS does this audience run". A deploy can ship a new binary, a new OTA update, or both, and the plan shows which. |

### D. How the OTA update relates to the binary

| Option | Verdict |
| --- | --- |
| **D1. Separate releases for binaries and updates** | Rejected. A JS bundle is only valid for one native fingerprint. Separating them loses the one fact that makes OTA safe. |
| **D2. One release carries both; an update-only release reuses the previous binary by digest** (recommended) | Every release has a fingerprint per platform. The cut builds a store binary **only when the fingerprint changed** since the env's live release; otherwise the artifact's binary layer is a blob mount of the previous one, and only the OTA bundle is new. "Is this a store release or an OTA release" is computed, not chosen. |

### E. How the runtime version is chosen

| Option | Verdict |
| --- | --- |
| **E1. `runtimeVersion` = app version** (Expo's `appVersion` policy) | Rejected. A forgotten version bump lets an update reach a binary whose native modules it does not match: a crash on launch, delivered OTA. |
| **E2. `runtimeVersion` = Expo fingerprint** (`@expo/fingerprint`, the `fingerprint` policy) (recommended) | Computed from native inputs, so a native change always produces a new runtime and the plan can prove compatibility rather than trust a human. |

## 5. Recommended design

### 5.1 Phase F0: the dev loop (independent of releasing)

| # | Change | Why |
| --- | --- | --- |
| F0.1 | Bump the RN scaffold to the current Expo SDK, the React Native it pins, and React 19; drop the `/interceptors` React-18 workaround; a `migrations/<version>` playbook for existing apps | Shared packages with web must type-check on one React major |
| F0.2 | Transport: `createConnectTransport({ fetch: expoFetch })` from `expo/fetch`, with a scaffolded streaming smoke test | Server-streaming RPCs on Hermes |
| F0.3 | `app.config.ts` replaces `app.json`; it reads a generated `forge.app.json` (name, bundle id, scheme, associated domains, `runtimeVersion: {policy: "fingerprint"}`, `updates.url`, per-variant `extra`) | Identity and per-env config come from KCL, not a hand-edited file |
| F0.4 | Runtime config on RN: the generated loader reads `Constants.expoConfig.extra.forge` (baked per variant at build time) instead of returning `{}` | Typed per-env config works on mobile |
| F0.5 | RN dev log mirror: a dev-only module that patches `console.*`, `ErrorUtils.setGlobalHandler` and unhandled promise rejections, and POSTs batches to the dev API's log endpoint, landing in `.forge/logs/dev/frontend_<name>.log` as `[browser:<level>]` lines (same prefix, so every grep and the `debug` skill work unchanged) | Debugging a mobile app the way the skill says |
| F0.6 | `forge env up dev` runs `expo start --dev-client` for an `OnHost` mobile app and injects `EXPO_PUBLIC_API_URL` as the host's LAN address (or the env's tunnel when one is declared), reusing v0.1.46's dev API proxy reachability work | No hand-set LAN IP |
| F0.7 | Test lanes: the RN frontend's `jest` runs in `task test`, its typecheck and lint in `forge lint`, and it appears in the affected-tests plan | A mobile change is gated like a web change |
| F0.8 | Native sign-in for RN (`--auth-mode native` on `--kind mobile`): OIDC authorization-code + PKCE through the login broker via `expo-auth-session`, refresh token in `expo-secure-store`, `getToken` wired to the transport. `--auth-mode none` keeps today's honest mock | The skill says "RN is different, and ships a mock"; every real app writes this |
| F0.9 | Scenarios: un-exclude RN; `forge scaffold scenario` emits a Maestro flow beside the Playwright one from the same scenario definition; `task test:e2e` runs Maestro against a simulator when one is available, and skips with a named reason otherwise | One scenario catalog, two runners |

None of F0 touches `pkg/release` or the control plane.

### 5.2 Pipeline shape (F1–F3)

```
forge env build mobile-beta --release mobile-1.2.0
───────────────────────────────────────────────────────────────────────────
ORCHESTRATOR (any OS)
  1. render KCL; resolve source pin; read app version + build number
  2. compute fingerprint per platform (@expo/fingerprint, at the pin)
  3. look up the env's live release: same fingerprint → reuse its binary
     by digest; different → schedule a store build for that platform
  4. export the OTA update per platform (expo export, Hermes bytecode),
     with build_env baked in → staging @sha256:U_<os>
  5. dispatch a store build only where scheduled ─────────┐
                                                          ▼
                          BUILDER (macOS for ios, any for android)
                            forge builder run <job>
                              expo prebuild --clean
                              xcodebuild archive + exportArchive
                                (or gradle bundleRelease)
                              sign with the declared identity
                              verify: codesign, embedded profile,
                                bundle id, entitlements, fingerprint
                              push .ipa + dSYMs → staging @sha256:B_<os>
  6. collect, verify digests and fingerprints ◄───────────┘
  7. assemble mobile.v1 (blob mounts) → @sha256:R; cut the release
───────────────────────────────────────────────────────────────────────────
forge env deploy mobile-beta mobile-1.2.0
  plan → approve → promotion entry → PUBLISH:
    store:   upload binary if the store lacks this build number →
             wait for processing → assign to TestFlight group(s)
    updates: publish the OTA bundle to the channel for this fingerprint
    → health gate (obs check) → done
```

### 5.3 Builders

The desktop proposal's builder protocol (§5.3 there) is reused unchanged:
content-addressed, secret-free job spec; `forge builder run`; registry
transport; `Local`/`GitHubActions`/`SSH` adapters. Mobile adds:

- **A job schema** `forge.dev/mobile-job/v1`: packager `expo`, platform
  `ios`/`android`, the expected fingerprint (the builder recomputes it and
  refuses a mismatch), the export method (`app-store` / `ad-hoc` /
  `development`), and the signing block.
- **`forge.EASBuilder`**: dispatches `eas build --platform <os> --profile
  <generated>` with the job's inputs, waits, downloads the artifact, verifies
  signature and fingerprint, pushes it to staging. Its credentials are
  `EXPO_TOKEN` (a secret name) plus whatever EAS's credential store holds.

### 5.4 Secrets

Same convention as desktop §5.4: every credential field is a secret **name**.

| Credential | Consumer | Field |
| --- | --- | --- |
| App Store Connect API key (.p8) | iOS builder (automatic signing) and the store publisher | `AppleAppStoreSigning.asc_key`, `AppStoreConnect.asc_key` |
| Distribution certificate (.p12) + password | iOS builder, when signing manually | `AppleAppStoreSigning.certificate`, `.certificate_password` |
| Android upload keystore + passwords | Android builder | `AndroidSigning.keystore`, `.store_password`, `.key_password` |
| Play service account JSON | Play publisher | `GooglePlay.service_account` |
| `EXPO_TOKEN` | `EASBuilder`, `EASUpdate` | `token` |
| Expo update code-signing private key | OTA publisher | `MobileApp.updates_signing_key` |

The ASC API key is preferred over an Apple ID for the same reason as desktop
§6's `AppleNotaryKey`: it is a team credential and survives an
account-holder change. **OTA updates are code-signed** (`expo-updates` code
signing): the public certificate is baked into the binary at build time, the
private key is a secret only the publisher holds. An update not signed by
that key is refused by the app, so a compromised update server cannot ship
code.

### 5.5 The release artifact

`mobile.v1` is one OCI image manifest:

- **config** (`application/vnd.forge.mobile.v1+json`): app name, app version,
  per-platform build number, bundle id / application id, Apple team id,
  Android signing-certificate SHA-256, source pin, variant key, and per
  platform: fingerprint, binary record (file, size, sha256, signing
  verdicts, entitlements, declared permissions / `Info.plist` usage strings,
  minimum OS), OTA update record (manifest, asset list, launch-asset hash,
  signature), symbols record, builder run URL, and, when reused, the release
  the binary layer came from.
- **layers**: binaries, OTA bundles and assets, dSYMs, source maps. Blob
  mounts from staging, so assembly uploads nothing.

Ledger entry, as for desktop: `{"kind": "oci", "mode": "shared",
"platforms": ["ios", "android"], ...}`.

**Invariants at the cut** (refusals):

1. **Completeness**: every declared platform has a binary (built or reused)
   and an OTA bundle.
2. **One build number, one byte set**: within a project ledger, (app,
   platform, build number) maps to exactly one binary digest. Stores refuse a
   re-upload of a build number anyway; forge refuses before an hour of
   building.
3. **Version and build number are read, never invented**, from the packager's
   `version_file` at the pin. A build number not ahead of the newest in the
   ledger for that app and platform is refused, naming the file to bump.
   (`auto_build_number = True` lets forge derive it from the ledger and inject
   it, for teams that want that; §11.)
4. **Fingerprint agreement**: the fingerprint the orchestrator computed, the
   builder recomputed and the OTA bundle was exported for are one value.
5. **Variant key** = sha256 of canonical `build_env`, as desktop. Beta
   (staging API) and prod (prod API) are different variants and different
   binaries.

### 5.6 Publication

**Store (`forge.AppStoreConnect`, `forge.GooglePlay`):**

1. Upload the binary unless the store already has this build number with this
   digest (if it has the build number with a different digest: refuse).
2. Wait for processing (App Store Connect API build state), with a timeout.
3. Apply the track:
   - `testflight` with `groups`: add the build to those beta groups; external
     groups trigger Beta App Review, and the deploy finishes at "submitted"
     with `env status` tracking the review;
   - `app-store`: attach the build to the version, submit for review, and
     set the release type (`manual`, `after-approval`, or
     `phased = True` for the 7-day phased release). Review is asynchronous:
     the deploy records "submitted"; `forge env status` shows review state;
     `forge env deploy mobile-prod --release-approved` (or the App Store's
     automatic release) completes it.
   - Play `internal` / `closed` / `production` with `rollout` fraction.
4. Store metadata is not written in the first cut (non-goal).

**Updates (`forge.EASUpdate`, later `forge.HostedUpdates`):**

1. Publish the release's OTA bundle for each platform to the channel, under
   the bundle's fingerprint as runtime version, with `rollout` percentage if
   declared.
2. Because expo-updates only applies an update whose runtime version matches
   the binary, safety is enforced on the device. forge's job is to make the
   reach visible: the plan computes which live binaries share the
   fingerprint (§5.7).
3. **Roll forward, never back**, as everywhere in forge: re-deploying the
   previous release to the channel republishes its update group. A
   "rollback to embedded" (ship nothing OTA, run what the binary carries) is
   `forge env deploy <env> --updates=embedded`, recorded as a promotion like
   any other.

**Health gate:** after publication, `forge obs check <env> --wait` reads the
app's signals (§5.8). Exit codes keep their meaning.

### 5.7 Ledger, plan and approval

Nothing in the promotion machinery is new; mobile reaches it through a shape
summary (`ShapeMobile`: per platform, bundle id, team / signing cert,
fingerprint, build number, app version, permissions, entitlements, minimum OS,
store track, update channel) and a `mobile` plan section.

Findings added to the closed sets in `pkg/release` (lands with a
control-plane `pkg/release` bump, as desktop 3.3):

| Code | Class | Fires when |
| --- | --- | --- |
| `mobile_identity_change` | stop | Bundle id, Apple team id or Android signing certificate differs from the live release. Installs cannot update to it; on Android it is unrecoverable. |
| `mobile_build_number_not_ahead` | stop | The store binary's build number is not ahead of the track's live one. The store would refuse it after the upload. |
| `mobile_update_unreachable` | stop | An OTA update is published to a channel where no live binary has its fingerprint, and the deploy ships no binary. Users would get nothing while the author believes a fix shipped. `--acknowledge-destructive` for the rare intended case (pre-seeding a channel ahead of a binary). |
| `mobile_native_change_ota_only` | stop | The fingerprint changed but the env's binding has no `store` publication, so the new native code can never ship there. |
| `mobile_permission_added` | warn | A new `Info.plist` usage string, entitlement (push, App Groups, associated domains) or Android permission. Expect App Review questions; update privacy labels. |
| `mobile_min_os_raised` | warn | The minimum OS version rose. Users below it stop receiving updates. |
| `mobile_prerelease_on_store` | stop | A semver prerelease is promoted to a binding declared `prerelease = False`. |

`forge env status mobile-prod` shows per platform: ledger binding, the build
the store reports live (and review state), the update group the channel
serves, and drift (a build submitted or an update published outside forge).

Gates: `forge gate record mobile-beta --from crash-rate.json` and `--gate` on
the prod promotion, as desktop §5.7.

### 5.8 Observability

- The RN scaffold gains an OTel setup (OTLP/HTTP exporter, resource attrs:
  app version, build number, fingerprint, update id, channel) and crash
  capture (JS errors via the global handler; native crashes via the platform
  crash reporter, forwarded on next launch).
- Traces already propagate: `forge-web-runtime` interceptors send
  `traceparent`, so a mobile request's backend spans join its trace.
- `forge obs check` gains a mobile evaluator: crash-free sessions and error
  rate for the new update id or build versus the previous one, and update
  adoption. Below a minimum sample it returns exit 8 (not enough data), which
  is common for mobile, so the deploy step waits or reports honestly.
- Symbols: dSYMs and source maps are in the artifact; `pkg/release` source map
  handling (already present for `static.v1`) extends to Hermes source maps so
  stack traces symbolicate in `forge obs issue`.

### 5.9 Native plumbing forge should own

| Item | Design | Why forge, not each app |
| --- | --- | --- |
| **`forge/pkg/push`** | A `Sender` with APNs (HTTP/2, token auth, `alert` / `background` / `liveactivity` push types, priority, collapse id, 410 → `ErrTokenRevoked`) and later FCM drivers; a device-token table recipe in the `db` skill; typed config for key id, team id, topic, environment | Every mobile app needs it; it is the kind of utility forge's libraries exist for. A forge app wires it as an adapter dep |
| **Universal / App Links files** | `MobileApp.links {domains, paths}` → `forge generate` writes `.well-known/apple-app-site-association` (served `application/json`, no redirect) and `.well-known/assetlinks.json` into the bound web frontend's build, from the same bundle id / team id / signing cert the release records | The values must agree with the binary; forge already holds them |
| **Deep-link route manifest** | Generated from Expo Router's `app/` tree, so the AASA `paths` and the app's routes cannot drift | Same reason |
| **Live Activity / widget targets** | Document the supported path (`expo-apple-targets` config plugin, or Expo's widget support) in a new `frontend/mobile` skill; the builder verifies the extension is signed with the same team and an App Group entitlement | No schema needed at first; a verified convention is enough |
| **Min supported version** | A `forge.MobileApp.min_supported` recipe: the API advertises the minimum app build per platform (from KCL), and the scaffold's root layout shows a blocking "update required" screen below it | Keeps old installs honest under the v1 compatibility rule |

### 5.10 Env kind

Desktop §5.7's fix applies: an env whose only bindings publish to stores the
author operates is `self_managed`, so store credentials are write-only with a
control plane declared. With per-workload placement
(`docs/design/per-workload-placement.md`) this becomes a per-binding fact,
not an env kind; the mobile binding declares "author-operated store" the way
`OnBucket` does.

## 6. KCL schema

New `kcl/mobile.k`, re-exported as `forge.*`. `Bundle` gains
`mobiles: [MobileApp] = []`. `LocalBuilder`, `GitHubActionsBuilder`,
`SSHBuilder` and `GitSource` are shared with `kcl/desktop.k`.

```python
schema MobileApp:
    """A store-distributed mobile app: per-platform signed binaries plus OTA
    updates, built from one source, released as ONE OCI artifact, and
    published to a channel's store track and update channel."""
    name: str
    image: str                      # OCI repository for the release artifact
    source?: GitSource              # exactly one of source / path
    path?: str
    packager: ExpoPackager
    platforms: [MobilePlatform]
    # PUBLIC build-time config (API URL, publishable keys, bundle id suffix
    # per variant). Its digest is the VARIANT key.
    build_env: {str: str} = {}
    build_secrets: [str] = []       # e.g. a crash-reporter DSN
    # OTA code signing: the certificate is public and baked in; the key is a
    # secret NAME held only by the publisher.
    updates_certificate?: str
    updates_signing_key: str = "EXPO_UPDATES_SIGNING_KEY"
    links?: MobileLinks
    min_supported?: {str: int}      # platform → minimum supported build number
    runtime?: OnHost | MobileChannel | BuildOnly

    check:
        bool(source) != bool(path), "MobileApp '${name}': set exactly one of source or path"
        len(platforms) > 0, "MobileApp '${name}' declares no platforms"

schema ExpoPackager:
    type: "expo" = "expo"
    dir: str = "."                  # holds package.json and app.config.ts
    version_file: str = "package.json"
    # Read from version_file unless auto_build_number (open question 3).
    auto_build_number: bool = False

schema MobilePlatform:
    os: "ios" | "android"
    bundle_id: str                  # iOS bundle id / Android application id; public
    builder: LocalBuilder | GitHubActionsBuilder | SSHBuilder | EASBuilder
    signing?: AppleAppStoreSigning | AndroidSigning
    unsigned: bool = False          # simulator / dev builds only; never published
    min_os?: str

    check:
        signing or unsigned, "MobilePlatform '${os}': declare signing, or unsigned = True for a dev build"

schema EASBuilder:
    type: "eas" = "eas"
    token: str = "EXPO_TOKEN"       # secret NAME

schema AppleAppStoreSigning:
    type: "apple-app-store" = "apple-app-store"
    team_id: str                    # public
    asc_key: str = "ASC_API_KEY"    # secret NAME (.p8)
    asc_key_id: str                 # identifier
    asc_issuer: str                 # identifier
    # Optional manual signing; absent = automatic signing via the ASC key.
    certificate?: str               # secret NAME (.p12)
    certificate_password?: str      # secret NAME

schema AndroidSigning:
    type: "android" = "android"
    keystore: str = "ANDROID_UPLOAD_KEYSTORE"     # secret NAME
    store_password: str = "ANDROID_STORE_PASSWORD"
    key_alias: str
    key_password: str = "ANDROID_KEY_PASSWORD"
    cert_sha256: str                # public; the identity the plan compares

schema MobileLinks:
    domains: [str]                  # applinks:<domain>
    paths: [str] = []               # empty = derived from the app/ route tree
    web_frontend: str               # the forge.Frontend that serves .well-known

schema MobileChannel:
    """One audience: the binary it installs and the updates it pulls."""
    store?: AppStoreConnect | GooglePlay | AdHoc
    updates?: EASUpdate | HostedUpdates
    prerelease: bool = False

    check:
        store or updates, "MobileChannel publishes nothing — declare store, updates, or both"

schema AppStoreConnect:
    type: "app-store-connect" = "app-store-connect"
    app_id: str                     # App Store Connect numeric app id; public
    track: "testflight" | "app-store"
    groups: [str] = []              # TestFlight beta groups
    release: "manual" | "after-approval" = "manual"
    phased: bool = False
    asc_key: str = "ASC_API_KEY"

schema GooglePlay:
    type: "google-play" = "google-play"
    package: str
    track: "internal" | "closed" | "open" | "production"
    rollout: float = 1.0
    service_account: str = "PLAY_SERVICE_ACCOUNT_JSON"

schema AdHoc:
    """Internal distribution without a store: a download page for registered
    devices (iOS ad-hoc) or a direct .apk."""
    type: "ad-hoc" = "ad-hoc"

schema EASUpdate:
    type: "eas-update" = "eas-update"
    project_id: str                 # public
    channel: str
    rollout_percent: int = 100
    token: str = "EXPO_TOKEN"

schema HostedUpdates:
    """Phase F4: the control plane serves the expo-updates protocol from the
    release artifact. No third party."""
    type: "hosted-updates" = "hosted-updates"
    channel: str
    rollout_percent: int = 100
```

### Example: Reliant

`deploy/kcl/mobile.k` (in whichever repo holds the declaration, open question 1):

```python
reliant_mobile = lambda env: str -> forge.MobileApp {
    forge.MobileApp {
        name = "reliant-mobile"
        image = "<registry>/reliant/mobile"
        source = forge.GitSource {repo = "github.com/reliant-labs/reliant", ref = RELIANT_REF}
        packager = forge.ExpoPackager {dir = "frontends/mobile"}
        platforms = [forge.MobilePlatform {
            os = "ios"
            bundle_id = "<bundle-id>"           # open in the reliant plan
            builder = forge.GitHubActionsBuilder {repo = "github.com/reliant-labs/reliant", runs_on = "macos-15", environment = "mobile-ios"}
            signing = forge.AppleAppStoreSigning {team_id = "<team>", asc_key_id = "<id>", asc_issuer = "<issuer>"}
        }]
        build_env = envlib.reliant_mobile_build_env(env)   # API URL, Supabase URL + anon key
        updates_certificate = "certs/expo-updates.pem"
        links = forge.MobileLinks {domains = ["app.reliantlabs.io"], web_frontend = "web"}
    }
}
```

`deploy/kcl/mobile-beta/main.k`:

```python
output = forge.render(forge.Bundle {
    project = "control-plane"
    secret_provider = forge.ExternalSecrets {}
    mobiles = [m.reliant_mobile("staging") | {runtime = forge.MobileChannel {
        prerelease = True
        store = forge.AppStoreConnect {app_id = "<id>", track = "testflight", groups = ["Reliant team"]}
        updates = forge.EASUpdate {project_id = "<id>", channel = "beta"}
    }}]
})
```

`mobile-prod` binds `track = "app-store"`, `phased = True`, `channel =
"production"`, and the prod variant. `dev` binds `forge.OnHost {}`.

## 7. CLI

No new top-level noun.

```text
forge env up dev                                # expo start --dev-client, logs mirrored, LAN API URL
forge build mobile-dev --platform ios           # simulator build on this Mac (unsigned)

forge env build mobile-beta --release mobile-1.2.0 --plan
                                                # pin, version, build number, fingerprints,
                                                # which platforms need a store build, secrets present
forge env build mobile-beta --release mobile-1.2.0
forge env deploy mobile-beta mobile-1.2.0       # plan → approve → upload / TestFlight / OTA → obs check
forge env deploy mobile-prod --from mobile-beta # same build to App Store review; same update to production
forge env deploy mobile-prod --updates=embedded # roll forward to "no OTA update"
forge env status mobile-prod                    # live build + review state, served update, adoption, drift

forge release verify mobile-1.2.0
forge builder doctor mobile-beta                # shared with desktop
```

## 8. Phases

Sizes: S ≈ days, M ≈ a week, L ≈ several weeks, for one agent-assisted
engineer.

### F0: dev loop

| # | Task | Size |
| --- | --- | --- |
| F0.1 | Scaffold version bump (Expo SDK, RN, React 19) + upgrade playbook | M |
| F0.2 | `expo/fetch` transport + streaming smoke test | S |
| F0.3 | `app.config.ts` + generated `forge.app.json` from KCL | M |
| F0.4 | RN runtime config from `expoConfig.extra` | S |
| F0.5 | RN dev log mirror to `.forge/logs/<env>/` | S |
| F0.6 | `forge env up` for mobile: dev client, LAN/tunnel API URL | M |
| F0.7 | Test, typecheck, lint and affected lanes include RN | S |
| F0.8 | RN native sign-in (`--auth-mode native`) | M |
| F0.9 | Maestro scenario runner | M |

**Exit:** a fresh `forge project new --service x --frontend web` plus `forge
scaffold frontend mobile --kind mobile` streams an RPC on an iOS simulator,
signs in, mirrors its logs, and passes `task test`.

### F1: the shared app-release core (with desktop phases 1–2)

The builder protocol, `forge builder run/doctor/workflow`, the
`GitHubActionsBuilder` / `SSHBuilder` / `LocalBuilder` adapters, OCI app
artifact assembly by blob mount, and channel envs. **Owned once** for desktop
and mobile; whichever proposal is scheduled first builds it with the other's
job schema in mind. **Size:** as desktop phase 1–2 (L), plus S for the
mobile job schema.

### F2: mobile build and cut

| # | Task | Size |
| --- | --- | --- |
| F2.1 | `kcl/mobile.k`, render of `output.mobiles[]`, validation | M |
| F2.2 | Expo packager on the builder: prebuild, archive, export, verify (iOS) | M |
| F2.3 | Fingerprint compute + reuse-by-digest when unchanged | M |
| F2.4 | OTA export per platform, code-signed | S |
| F2.5 | `mobile.v1` assembly + cut invariants | M |
| F2.6 | `EASBuilder` adapter | S |

**Exit:** `forge env build mobile-beta --release …` on a Mac produces a
signed `.ipa` whose fingerprint, bundle id and entitlements are recorded, and
a second cut with JS-only changes builds no binary.

### F3: publication

| # | Task | Size |
| --- | --- | --- |
| F3.1 | `AppStoreConnect` publisher: upload, processing wait, TestFlight groups | M |
| F3.2 | App Store submission, review-state tracking, phased release | M |
| F3.3 | `EASUpdate` publisher, rollout, `--updates=embedded` | S |
| F3.4 | `pkg/release`: `ShapeMobile`, `mobile` section, findings, table tests; control-plane bump | M |
| F3.5 | `forge env status` mobile view, drift | S |
| F3.6 | Self-managed classification for store-only bindings (desktop 3.4) | S |

**Exit:** `forge env deploy mobile-beta` puts a build in TestFlight and an
update on the beta channel, with plan findings shown and an approval digest.

### F4: health and native plumbing

| # | Task | Size |
| --- | --- | --- |
| F4.1 | RN OTel + crash capture in the scaffold | M |
| F4.2 | `obs check` mobile evaluator; Hermes source maps / dSYMs in `obs issue` | M |
| F4.3 | `forge/pkg/push` with the APNs driver (incl. `liveactivity`) | M |
| F4.4 | AASA / assetlinks generation from `MobileLinks` | S |
| F4.5 | `frontend/mobile` skill: Live Activities, widgets, push, deep links, store review checklist | S |
| F4.6 | `HostedUpdates`: control plane serves the expo-updates protocol | L |

### F5: Android

Gradle on a Linux builder, `AndroidSigning`, `GooglePlay` publisher, FCM in
`pkg/push`, `assetlinks.json`. **Size:** M–L.

## 9. Sequencing against the Reliant app

| Reliant milestone | Needs from forge |
| --- | --- |
| Phase 0 spike | Nothing; the spike may use `eas build` directly and is thrown away. It should **exercise** F0.1–F0.6 by hand and report what broke |
| M1 internal TestFlight | F0 (minus F0.9), F1's `LocalBuilder` + `GitHubActionsBuilder`, F2 iOS, F3.1 + F3.4 |
| M2 parity, push, deep links | F3.3, F4.1–F4.4 |
| M3 App Store 1.0 | F3.2, F3.5, F4.2 (so `obs check` can prove a release good) |
| M4 Android | F5 |

The critical path is F1, because it is shared with desktop and not started.
Starting F0 and F1 in parallel with Reliant's Phase 0 is the main schedule
decision (§11, question 5).

## 10. Risks

| Risk | Mitigation |
| --- | --- |
| **An OTA update crashes every user in minutes.** | Fingerprint runtime policy (§4.E), code-signed updates, `rollout_percent`, `obs check` on the update id, `--updates=embedded` roll-forward. expo-updates also falls back to the embedded bundle after a launch crash. |
| **Apple processing and review latency** (minutes to days) makes a deploy long-running. | Deploy records "submitted" and returns; `env status` tracks review; completion is a separate, recorded step. |
| **macOS runner cost and availability.** | Same as desktop: builders declared per platform; JS-only releases build no binary (§4.D), which is most releases. |
| **Expo SDK churn** (yearly majors, native module breaks). | F0.1's upgrade playbook is per SDK; the fingerprint makes a native change visible in the plan. |
| **EAS as a third party.** | Adapters only; `HostedUpdates` and macOS builders remove the dependency when wanted. |
| **`pkg/release` closed-set changes** need the hosted ledger in lockstep. | One forge release plus one control-plane bump, as desktop 3.3. |
| **Store metadata outside forge drifts** (privacy labels vs new permissions). | `mobile_permission_added` warns at the plan; §11 question 4. |
| **Scope** is roughly desktop-sized on top of a shared core. | F0 alone is valuable to every forge mobile user; each phase stands alone. |

## 11. Open questions for the owner

1. **Where does the declaration live?** Desktop open question 1 applies
   verbatim: control-plane (owns the ledger and endpoints, pins reliant by
   `GitSource`) or reliant as its own forge project. Recommend answering both
   together.
2. **EAS Build or forge macOS builders as Reliant's default?** Recommend
   macOS builders (credentials stay in `secret_provider`), with `EASBuilder`
   available.
3. **Build numbers: read from the source, or derived from the ledger**
   (`auto_build_number`)? Recommend read, matching desktop's "the version is
   read, never invented"; the derived mode exists for teams that dislike
   committing build bumps.
4. **Should store metadata (description, screenshots, privacy labels) move
   into KCL** in a later phase, so a permission change and its label update
   are one reviewable diff?
5. **Schedule:** start F0 and the shared F1 core now, in parallel with
   Reliant's Phase 0, so M1 TestFlight ships through forge instead of a
   one-off EAS pipeline that would later be retired.
6. **Retire `type = "react-native"` on `forge.Frontend`** in favour of
   `MobileApp` bound `OnHost` for dev? Recommend deprecating it with a
   migration playbook, and keeping it working until the owner decides on
   removal.
