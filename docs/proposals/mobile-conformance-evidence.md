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
the `migrations/v1.2.0/native-dev-loop` playbook; that path was not executed.

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
- Real native identity, secure storage/account isolation, device logs/crashes,
  store publication, signed OTA and health/recovery evidence.

No scenario above certifies physical-device compatibility or production readiness.
The integration PR remains draft until the foundation milestone's broader gates
are met; this slice must not merge independently into main.
