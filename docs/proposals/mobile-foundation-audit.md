# Forge mobile foundation: audit and restructuring recommendation

**Status:** audit and proposed work; no implementation changes.
**Baseline:** `6c04734393bf58a20adfe169f1904b29009206f2`.
**Scope:** Expo scaffolding/adoption, shared frontend runtime, workspace APIs/hooks,
configuration, dev/test integration and compatibility. Releases are covered by the
[separate proposal](mobile-release-target.md). Broader lifecycle, old-client,
security, migration and device acceptance requirements are in the
[production-readiness contract](mobile-production-readiness.md).

## Verdict

Mobile is **not yet demonstrably on par with web or battle-tested**. There is a
useful shared foundation, but concrete contract mismatches prevent treating the
current scaffold as a production-ready native application. Fix the existing mobile
foundation before extending it with releases; do not wait for a wholesale
`MobileApp` migration to repair today's v1 RN frontends.

Keep Expo/React Native, Expo Router, Connect/protobuf, TanStack Query and pnpm.
There is no evidence here that replacing the stack would solve the real problems.
The primary failures are integration, dependency drift and insufficient executable
consumer coverage—not a fundamentally wrong framework choice.

**Recommended disposition: simplify specific seams first, not rewrite everything.**
The prerequisite refactors are canonical kind normalization, truly lazy transport
resolution and a portable runtime import boundary. Config and validation delivery
changes can be implemented alongside their tests. Release machinery remains a
separate concern that consumes this functioning foundation.

## Evidence and limitations

Three independently scoped investigations produced detailed evidence:

- [Dependency/runtime compatibility](mobile-foundation-dependencies-audit.md).
- [Dev/config/codegen lifecycle](mobile-foundation-lifecycle-audit.md).
- [Shared boundaries and validation](mobile-foundation-boundaries-audit.md).

Those reports contain file/line references and distinguish source-established
contracts from unexecuted runtime hypotheses. They are research reports, not device
certification. Upstream registry observations are mutable and should be captured
again when selecting the upgrade matrix; latest observed Expo was 58.0.23, not the
SDK 57 assumed in the initial release discussion.

Validation actually performed by the parent audit:

```text
task test:short -- ./internal/config/... ./internal/templates/... ./internal/generator/...
```

All selected packages passed. No registry installation, Expo Doctor, Metro export,
Xcode/Gradle build, simulator/physical-device auth or streaming session was run.
Passing Go tests therefore does not negate the findings below. Existing e2e source
does include a standalone mobile TypeScript/Metro export fixture; it was not run
here and does not cover a mixed web/mobile workspace or native execution.

## 1. Prioritized findings

| Priority | Finding                                                    | Evidence and consequence                                                                                                                                                                                                                                                             |
| -------- | ---------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| P0       | RN runtime manifest requests an unavailable version family | `react-native/package.json.tmpl:21` requests `^0.2.0`; observed registry has no 0.2.x. A direct registry install cannot resolve it. Generator reconciliation can later rewrite the range; that does not make the emitted manifest correct or every consumer path safe.               |
| P0       | Workspace hooks eagerly require an unset transport         | `frontend_workspaces.go:407–430,512–521` and `frontend/hooks.ts.tmpl:66–85`; importing setter from the root barrel evaluates service modules before registration. Source-established initialization-order defect, not device-reproduced here. Affects shared web/native integration. |
| P0       | Mobile dev command does not match launcher contract        | RN manifest has `start` but no `dev`; `internal/cli/up.go:3376–3392` invokes `run dev`. Default scaffold and launcher disagree.                                                                                                                                                      |
| P0       | Generic frontend build expects a missing script            | RN has no `build`; generated Taskfile invokes it for discovered frontends. CLI selection has runtime filtering, so not every env hits the failure. Native packaging must not be silently equated with static web build.                                                              |
| P0       | Typed mobile config does not receive env values            | `frontend_config_ts.go:113–134` falls back to `{}` for RN; config.js emission is skipped. Defaults/required failures are not env delivery. Transport separately reads Expo API env.                                                                                                  |
| P1       | Production native auth is not implemented                  | Explicit mock session and unwired token getter. Protected backend calls remain protected; this is an incomplete client flow, not an auth bypass.                                                                                                                                     |
| P1       | Streaming on native is unproven                            | Default global fetch selected; Connect server streaming requires response readers. Select/validate an Expo-native fetch implementation on Hermes. Do not claim every RPC streaming mode works.                                                                                       |
| P1       | Mobile validation lane is incomplete                       | No native Jest preset/tests or ESLint flat config in composed tree; Jest dependency alone is not a harness. Shared packages are outside the frontend Taskfile loop. Expected lint failure is source-derived, not executed here.                                                      |
| P1       | Dev reachability is host-oriented                          | Injected localhost API origin is not a physical device origin. Simulator/emulator/LAN/tunnel behavior needs explicit resolution and smoke checks.                                                                                                                                    |
| P1       | Shared portable helpers import browser barrel              | Query client/optimistic helper value-import root runtime despite native transport using `/interceptors`. Proven boundary leak; actual Metro bundle/crash impact remains unmeasured.                                                                                                  |
| P2       | Kind aliases disagree across consumers                     | KCL emits `rn`; dashboard exclusion checks `react-native`. Phantom browser telemetry identities are possible despite intentional native browser-SDK exclusion.                                                                                                                       |
| P2       | Adoption/discovery are narrower than Expo supports         | Marker recognizes app.json, not app.config.ts/js; adopted RN transport recovery lacks the web equivalent. Root-native exclusion is an explicit current limitation, not automatically a defect.                                                                                       |
| P2       | Logs/scenarios/mocks are incomplete native integrations    | Process stdout is not device console/crash collection. Browser tests/scenarios cannot simply be un-excluded or copied into RN.                                                                                                                                                       |

Concrete Forge defects were filed during investigation: dependency range
`339531a4-f183-40f2-96cb-3a25910044e0`, workspace initialization
`666a5273-51c8-4996-bbde-387b0a3ab36e`, lifecycle contracts
`94879943-7cdb-4dc3-b684-c39bb1f76e7e`.

## 2. Is the technology current and compatible?

### Expo matrix, not independent version bumps

The scaffold uses Expo 52, RN 0.76.9, React 18.3, Router 4 and jest-expo 52.
Upstream SDK 52 metadata largely agrees with that family. It is old relative to
observed stable registry releases, but not randomly incompatible internally.

Choose a currently supported stable Expo SDK through a clean adoption fixture;
upgrade RN, React, Router, native modules and jest-expo as its coordinated matrix.
Validate minimum Xcode/Node/Android requirements and New Architecture/plugin
compatibility at that point. Never upgrade each package independently to latest.

React 18 native versus React 19 web is not itself a bug: Expo controls native React
compatibility. Align them where the selected supported SDK allows it, but shared
packages must declare honest peers and be compiled against supported consumers.
The current checkout runtime already accepts React 18 and 19 and dual-typechecks;
its native template comment that the runtime categorically rejects React 18 is
stale. The browser/native import boundary is still worthwhile regardless of React
major equality.

### Candidate package upgrade matrix

The following candidate was checked against the published `expo@58.0.23`
`bundledNativeModules.json` and npm package metadata during this audit. It is
**not a validated Forge consumer lockfile**. Recheck availability and compatibility
when implementing; acceptance requires Expo Doctor, mixed-workspace compilation,
Metro exports and native/device tests. Existing apps need SDK migration guidance,
not a blind replacement of owned manifests.

| Package                        | Current native scaffold | Candidate target |
| ------------------------------ | ----------------------- | ---------------- |
| expo                           | ~52.0.0                 | ~58.0.23         |
| react                          | ^18.3.0                 | 19.3.0           |
| react-native                   | 0.76.9                  | 0.88.1           |
| expo-router                    | ~4.0.0                  | ~58.0.30         |
| expo-asset                     | ~11.0.5                 | ~58.0.20         |
| expo-constants                 | ~17.0.0                 | ~58.0.15         |
| expo-linking                   | ~7.0.0                  | ~58.0.16         |
| expo-status-bar                | ~2.0.0                  | ~58.0.4          |
| react-native-safe-area-context | 4.12.0                  | ~5.11.1          |
| react-native-screens           | ~4.4.0                  | ~4.30.1          |
| react-native-web               | ~0.19.0                 | ~0.21.4          |
| jest-expo                      | ~52.0.0                 | ~58.0.14         |
| @reliantlabs/forge-web-runtime | ^0.2.0                  | ^0.13.5          |
| @connectrpc/connect            | ^2.0.0                  | 2.3.0            |
| @connectrpc/connect-web        | ^2.0.0                  | 2.3.0            |
| @bufbuild/protobuf             | ^2.5.0                  | 2.17.0           |
| @bufbuild/protoc-gen-es        | ^2.5.0                  | 2.17.0           |

Add `react-dom: 19.3.0` if Expo web remains supported; align `@types/react`
with React 19.3. Select Jest, Babel and TypeScript against the chosen Expo template
and tooling rather than independently upgrading to npm latest. Expo 58's declared
Node engine is `^22.13.0 || ^24.3.0 || ^26.0.0 || >=27.0.0`.

Capability additions, only when their integration is implemented:

| Capability               | Matching Expo package      |
| ------------------------ | -------------------------- |
| Development builds       | expo-dev-client ~58.0.18   |
| Native browser PKCE flow | expo-auth-session ~58.0.17 |
| System browser           | expo-web-browser ~58.0.7   |
| Secure token storage     | expo-secure-store ~58.0.2  |

Keep Connect versions aligned, and protobuf runtime/generator versions aligned.
Apply shared RPC/runtime policy across web/native/workspace templates and generator
reconciliation, not only one package.json. Match Prettier to Forge's centralized
pin and configure native ESLint/Jest. Defer Zod 3-to-4 and TypeScript major upgrades
to separate compatibility work. Query/Zustand/OTel ranges already admit compatible
updates; inspect resolved lockfiles before prescribing a version bump.

Sources: [Expo version metadata](https://registry.npmjs.org/expo/58.0.23)
and the `package/bundledNativeModules.json` inside its published tarball;
individual package metadata from the [npm registry](https://registry.npmjs.org/).
These observations establish a proposed dependency set, not tested Hermes or
native plugin compatibility.

### Preserve the sound pieces

- Connect/protobuf and React Query major families are shared across web/native.
- `/interceptors` is an existing portable transport seam, with graph tests.
- Browser SDK peers are optional; do not claim native necessarily installs or
  executes every browser module merely from the root package's name.
- Expo Router remains appropriate. Drawer/reanimated/gesture dependencies depend
  on the chosen features and SDK; they are not universal missing dependencies.
- Separate DOM and RN primitive libraries are correct. A universal UI framework
  is not required for API, configuration and lifecycle parity.

Advertised Expo web support currently omits direct react-dom. Either support and
validate that output with the matching SDK React/DOM pair or explicitly narrow the
advertised capability through a compatible transition; do not depend on a sibling
web app to accidentally provide it.

## 3. Refactors I would implement

### R1 — Normalize framework identity once

**Locations:** `internal/config/frontend_inventory.go`, rendered frontend consumer
boundaries, `generate_frontend_config.go`, `up_env_compose.go`,
`generate_dashboard_processes.go` and scenario eligibility.

Provide a pure canonical-kind normalization function. Normalize KCL `rn`, inventory
`react-native` and any deliberately supported aliases at input boundaries. Keep
small capability predicates with consumers: browser telemetry eligibility is not
identical to native bundling capability. Do not build a global plugin registry
whose callbacks own every dev/build/auth concern.

**Why first:** the demonstrated dashboard mismatch is the same category of drift
that adding MobileApp would otherwise multiply. Preserve existing spellings in v1.

### R2 — Make transport resolution genuinely call-time

**Locations:** `internal/generator/frontend_workspaces.go`,
`internal/templates/frontend/hooks.ts.tmpl`, runtime hook/client factories.

Keep API descriptors and query-key/request logic shared. Resolve the current
transport when an operation executes, not when a generated service module imports.
A lazy forwarding Transport or a small injected getter can preserve existing
exports; select the simpler implementation after testing unary and streaming
semantics. Preserve public setters/exports and define replacement behavior.

A transport-only subpath can remove a barrel cycle but does not alone solve early
hook imports or clients capturing a stale transport. Add executable import-before-
registration and swap-after-registration tests before changing the emitter.

### R3 — Use the existing portable runtime boundary consistently

**Locations:** `web-runtime/src/interceptors.ts`, shared query/error helpers,
workspace base wrappers and generated imports.

Use existing portable exports for error values/types and interceptors; add a narrow
errors subpath only if it materially improves the contract. Browser session,
DOM telemetry, route guards and UI stay browser-owned. Native fetch/storage/auth
adapters stay native-owned. Retain root exports for compatibility.

Prefer additive subpaths plus dependency-graph tests over immediately splitting
into three separately published runtimes. A package split is justified only if
measured peer/bundle/release isolation cannot be enforced through exports.

### R4 — Separate config schema from value delivery

**Locations:** `internal/codegen/frontend_config*.go`,
`generate_frontend_config.go`, native app config seam and transport config reader.

One schema/projection/validation path; browser raw values come from config.js,
native values from resolved public build config (bundled generated module or Expo
extra). Pick one precedence and make auth/transport/features consume it. Avoid a
dynamic process.env lookup that Expo will not statically inline. Keep identity
provider secret rejection and required-field validation shared.

Owned app.config.ts composes generated data; it is not overwritten with application
logic. Extend discovery for supported Expo config names as part of this work.

### R5 — Define runnable validation tasks, not web-shaped assumptions

**Locations:** native manifest/config templates, existing frontend inventory,
`internal/templates/project/Taskfile.yml.tmpl`, generated CI and affected-test plan.

Describe concrete commands for dev, typecheck, lint, unit tests and bundle checking
in a small task plan consumed by Taskfile/CI. Reuse existing package-manager and
inventory logic. Keep an explicit unsupported native release-build outcome until
release support exists; do not invent a signed-build meaning for web `build`.

Include API/hooks/ui packages in validation, not only whatever sources an app
happens to import. Native gets configured Jest/native renderer tooling and at
least one executed test. Shared behavior cases can drive browser and native
renderers without sharing DOM test imports. Preserve custom owned scripts.

### R6 — Reduce emitter and dependency-policy fragmentation

**Locations:** `internal/generator/frontend_workspaces.go`,
`internal/templates/frontend/*/package.json.tmpl`, `internal/templates/toolversions.go`.

Move large embedded workspace TS/JSON literals into the existing template system;
keep naming/layout and ownership decisions in Go. Preserve write-if-missing owned
files and generator convergence. Reuse existing pnpm/Prettier/runtime version
policy and add an explicit SDK dependency matrix; do not create a second registry
for already centralized constants.

Separate the local runtime dev-link Metro exception from ordinary workspace
resolution. Prefer Expo's supported monorepo configuration after actual pnpm
fixture validation. Do not emulate pnpm resolution or keep historical hoisting
workarounds without a failing test that needs them.

### R7 — Native adapters, not a second shared platform framework

Implement real native PKCE/secure-storage/token lifecycle, Expo-compatible fetch,
device-facing dev origin resolution and dev log forwarding as small platform
adapters consuming shared contracts. Browser cookie auth must remain HttpOnly;
parity does not mean exposing browser tokens to JS. Keep listener allocation
separate from advertised device API origin. Do not silently choose an arbitrary
network interface/tunnel or weaken production ATS/cleartext settings.

Keep native navigation, deep-link activation and app foreground/offline lifecycle
platform-owned. Add React Query native focus/online integration where needed and
test reconnect/refetch semantics, rather than assuming DOM focus listeners apply.
This last integration is a validation requirement, not a reproduced defect here.

## 4. Conformance matrix that prevents another divergence

| Layer                | Required executable evidence                                                                               |
| -------------------- | ---------------------------------------------------------------------------------------------------------- |
| Released consumer    | Registry-only install; no checkout bridge; all runtime subpaths resolve                                    |
| Development consumer | Packed checkout runtime and dev-link paths both work                                                       |
| Scaffold shapes      | Standalone Expo; mixed pnpm web/native workspace; adopted Expo app                                         |
| Dependencies         | Expo Doctor/install check; exact selected SDK matrix; correct React peers                                  |
| Shared packages      | Compile every package, including unused exports; native graph avoids browser values                        |
| Generated hooks      | Import before bootstrap, explicit uninitialized-call error, transport swap, pagination/invalidation/errors |
| Lifecycle            | Declared scripts match actual launch/build/test argv; explicit unsupported outcomes                        |
| Config               | Non-default values agree across transport/auth/features; missing/sensitive values fail correctly           |
| Bundling             | iOS and Android Metro exports; advertised Expo web path if retained                                        |
| Native runtime       | Hermes unary JSON/binary, incremental server stream, UTF-8 fragmentation, abort, offline/reconnect         |
| Identity             | System-browser PKCE, restore, concurrent refresh, invalid/replayed callbacks, logout/cache isolation       |
| Observability        | Dev console/rejection forwarding and separately named native crash/log coverage                            |
| Ownership            | User edits survive generation; two unchanged generations converge                                          |

A Metro export is not an Xcode/Gradle build; a native build is not a device scenario;
a successful simulator RPC does not establish physical-device reachability. Each
capability claim names the evidence level it actually passed. Missing native
capacity is unavailable/explicitly waived, not a passing mobile gate.

Run cheap contract tests in the fast lane. Install/bundle/compile consumer fixtures
belong in the slow lane with declared inputs. Device smoke tests run on named
capacity with isolated fixtures. Shared runtime/generator changes must select both
web and mobile fixtures; mobile-only changes should not require every unrelated
web e2e test.

## 5. Recommended order and release proposal impact

1. **Repair blocking contracts:** runtime manifest, workspace lazy transport,
   native dev/build task semantics and typed config. Add regressions first.
2. **Upgrade one tested Expo matrix:** validate standalone, adopted and mixed
   workspace consumers, removing only demonstrably obsolete Metro workarounds.
3. **Close operational foundation:** native fetch/streaming, auth, device origin,
   dev logs and configured native tests. Complete the conformance matrix.
4. **Build the release vertical slice:** consume the working discovery/config/
   validation contracts, preserving existing RN Frontend behavior.
5. **Add native breadth:** scenarios/links/push/crash adapters as independently
   verified capabilities, not a prerequisite to repairing basic scaffolding.

Update the release proposal's F0 scope to include the actual blockers and these
acceptance gates. Do not claim F0 is finished based only on a version bump or Go
template tests. The release proposal should reference this foundation audit rather
than duplicate its file-level implementation inventory.

## 6. What I would not change

No Flutter rewrite, mandatory universal UI kit, blanket React-major unification,
second mobile hooks engine, custom package-manager resolver, generic plugin system
or wholesale repository reorganization. None is supported by this evidence.

The durable simplification is **one shared API/config/lifecycle contract with
small platform-owned adapters and real consumer tests**. The code already has
several of those seams; repair and consistently use them before adding more.
