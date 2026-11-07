# Mobile foundation lifecycle audit

## Scope and confidence

Audited source HEAD **6c04734393bf58a20adfe169f1904b29009206f2**. This is a lifecycle integration audit, not a mobile release design or implementation. `mobile-release-target.md` was consulted only for context; none of its assertions are evidence here. Other proposal files are outside this audit's ownership.

`forge skill load debug` preceded investigation. Evidence below is static source verification, not a claim that a device, Metro, native compiler, or deployed environment was exercised. No whole test suite, dependency installation, generation, or native build was run. **Verified** means the cited branches/contracts establish the behavior; **conditional** means the failure requires the stated input; **unverified** explicitly names remaining runtime uncertainty.

## Executive summary

RN is already a frontend codegen participant, not an entirely separate product: disk/KCL discovery, protobuf output, React Query hooks, workspace transport injection, typed public config schemas and Node toolchain inventory reach it. But shared lifecycle consumers assume web package scripts and browser configuration delivery. The most immediate defects are missing `dev`/`build` scripts, an RN typed-config reader with no native values source, and failure to exclude KCL's `rn` spelling from browser dashboard identity generation.

Do not solve these by inventing a universal frontend/mobile lifecycle framework. Keep code ownership, codegen, dev process launch, client config delivery and release artifacts separate. Repair the narrow contracts that actually disagree. Browser-only exclusions (cookie login, DOM tests, browser telemetry and scenario transport) are mostly intentional, not evidence that all exclusions should disappear.

## Actual architecture and lifecycle map

| Stage            | Source evidence                                                                                                                          | Actual RN behavior                                                                                                                                                                                                        |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Load/discover    | `internal/config/frontend_inventory.go:169-179,347-375,466-483`                                                                          | Conventional `frontends/<name>` directories participate; `app.json` identifies RN.                                                                                                                                        |
| KCL overlay      | `internal/cli/generate_frontend_inventory.go:66-87,132-141,168-180`; `internal/config/frontend_inventory.go:492-526,608-615`             | Generate renders environments best-effort, admits owned existing directories, normalizes `rn` to `react-native`, merges into inventory.                                                                                   |
| Ownership        | `internal/config/frontend_inventory.go:82-94,123-143`; `internal/config/config.go:705-716`                                               | Cross-repo pins are not local codegen targets. Toolchain inventory excludes unresolved pins, not RN. Preserve this distinction.                                                                                           |
| Scaffold         | `internal/cli/scaffold/scaffold.go:1900-1957,1972-1988`; `internal/templates/frontend_tree.go:51-59`                                     | Config proto/module and KCL declaration are emitted; RN composes shared + native templates, deliberately not shared-web.                                                                                                  |
| APIs             | `internal/cli/generate_frontend_hooks.go:250-266,274-281`; `internal/cli/generate_buf.go:335-340`                                        | RN is explicitly accepted for hooks; service allowlists remain relevant. Buf selection includes RN.                                                                                                                       |
| Native transport | `internal/templates/frontend/react-native/src/lib/connect.ts.tmpl:1-5,19-29,43-62`                                                       | Connect web transport with DOM-free runtime interceptors; Expo API origin; optional workspace hook transport injection. Real token getter is an owned seam.                                                               |
| Build/lint       | `internal/config/config.go:680-716`; `internal/cli/build.go:1400-1415,1441-1460`; `internal/cli/lint/lint_frontend_typecheck.go:131-145` | Both consume toolchain inventory, which includes RN. Host-bound frontend builds are skipped; this is not native packaging.                                                                                                |
| Dev/env          | `internal/cli/up.go:942,3268-3271,3376-3427`; `internal/cli/up_env_compose.go:137-162`                                                   | Env up merges disk inventory with rendered frontends and launches package-manager `run dev`, layering config/env/parent values.                                                                                           |
| Legacy dev files | `internal/cli/dev.go:1-21,30-57`                                                                                                         | These implement `forge cluster`, not a second mobile app runner. Cluster status/URLs are not device reachability proofs.                                                                                                  |
| KCL runtime      | `kcl/schema.k:3485,3812-3846,3880-3899,3930-3965`                                                                                        | `FrontendRuntime` is host/hosted/bucket/Firebase/build-only; RN is `type = "rn"`. Public dirs, browser runtime config and browser telemetry restrictions describe a web/static lifecycle, not APK/IPA/store distribution. |

## Findings

### F1 — Dev entrypoint contract is broken for the scaffold (high, verified)

`internal/templates/frontend/react-native/package.json.tmpl:7-15` ships `start`, `android`, `ios`, `web`, lint, typecheck and test. It has **no `dev` script**. `internal/cli/up.go:3376-3392` unconditionally builds `<manager> run dev` for frontends. RN is neither removed from inventory nor given a native launch branch. Consequently an unchanged scaffold cannot satisfy that command's package-script contract.

The same launcher forces `PORT` (`up.go:3425-3427`), while port allocation/summary assumes the selected listener (`up_env_compose.go:200-222`). Whether Expo SDK 52 honors that exact environment variable is **unverified** here; adding a script alone does not prove port/readiness correctness.

**Targeted repair:** add a scaffolded `dev` alias to the existing Expo start command, preserving `start/android/ios/web`. Factor a small pure launch-argv builder only if Expo needs explicit `--port`; keep package-manager argument forwarding in the existing launcher. Test rendered scripts against launch argv and resolved ports without starting Expo. Existing owned manifests must not be overwritten: use the established owned-file migration/notice seam.

### F2 — Generic build reaches a missing script, not a mobile artifact (high, verified conditional)

The same RN manifest has **no `build` script**. Toolchain inventory includes it (`config.go:705-716`), and `internal/cli/build.go:2384-2386` executes `run build`. A generic selected RN build that survives runtime filtering therefore fails at the script boundary. **Not every env build fails:** `build.go:1451-1460` excludes host-bound frontends, and env up explicitly skips production frontend builds (`1441-1448`).

**Targeted repair:** decide and test the existing RN frontend build contract explicitly: if its artifact is Expo web export, scaffold a `build` script for that export and assert its output directory against KCL. Do not silently redefine `forge build` to mean signed native packaging. If a selected target has no supported artifact, report that precise unsupported capability before invoking a missing script. Preserve existing custom scripts and existing static RN frontend behavior. Native artifacts need their own later design, not a boolean on the static builder.

### F3 — Typed RN config has no native environment delivery path (high, verified conditional)

`internal/codegen/frontend_config_ts.go:113-134` first reads `window.__FORGE_CONFIG__`; for RN the fallback is simply `{}`. `internal/cli/generate_frontend_config.go:95-126` emits typed config and schema but deliberately skips RN's config.js. Refresh has the same exclusion (`internal/cli/frontend_config_refresh.go:154`). Thus absent app-authored global injection, an RN `loadConfig()` sees schema defaults (or required-field validation failure), **not KCL per-env values or Expo public variables**. Its error guidance still tells a native consumer to load static config.js (`frontend_config_ts.go:193-201`).

Meanwhile transport independently reads `EXPO_PUBLIC_API_URL` (`react-native/src/lib/connect.ts.tmpl:19`). Shared auth's typed-config branch wins before the Expo branch (`shared/src/lib/auth/session-provider.ts.tmpl:51-75`). This creates two configuration truths: a valid dev transport override need not be visible to typed consumers; a configured mock flag need not reach auth.

**Targeted repair:** keep `FrontendConfig`, field flattening, sensitive-field rejection and validation shared (`internal/codegen/frontend_config.go:28-73,89-109`). Split only raw-value acquisition and delivery diagnostics by platform. Generate explicit, statically addressable Expo public reads per declared field, or a generated bundled public-config module supplied by the build; choose one documented precedence, rather than another dynamic `process.env[key]` reader that Expo may not inline. Keep browser config.js/runtime promotion unchanged. Native embedded config changes require a new bundle; never promise web-style runtime promotion. Pass the chosen source consistently to auth and transport while retaining existing `EXPO_PUBLIC_API_URL` override behavior.

### F4 — KCL RN leaks into browser dashboard identities (medium, verified)

KCL allows only `nextjs|vite|rn` (`kcl/schema.k:3959-3965`), and render preserves `f.type` (`kcl/render.k:2677`). `internal/cli/generate_dashboard_processes.go:72-77` excludes only `react-native`. Therefore rendered `rn` entries acquire browser frontend names/service identities despite having no browser SDK. `observedFrontends` applies a different filter to normalized inventory (`internal/cli/generate_pipeline.go:2656-2666`).

The language-server call map confirms `frontendIdentities` feeds both `dashboardSpecForEntities` and `stepObservabilityDashboards`; the former reaches `renderedObservabilitySet`. This is not merely a dormant helper. The existing test uses `Type: "react-native"` (`generate_dashboard_processes_test.go:283`), missing the actual KCL spelling.

**Targeted repair:** normalize kind at the rendered-entity consumer boundary, or use a tiny browser-telemetry eligibility predicate accepting canonical aliases. Test both raw KCL `rn` and inventory `react-native` through dashboard generation. Do not install a browser SDK in native just to make phantom dashboard rows real.

### F5 — Discovery remains command/layout dependent (medium, verified conditional)

Disk discovery recognizes only `app.json` for RN (`frontend_inventory.go:466-483`); root discovery explicitly refuses RN (`202-208`). A custom-path native app can participate through KCL during generation (`generate_frontend_inventory.go:58-87`), while load-only toolchain consumers see the cheap disk inventory (`frontend_inventory.go:169-179`; `lint_frontend_typecheck.go:144-145`). A conventional Expo app using only `app.config.ts/js` is not admitted by these marker rules. These are proven discovery limits, not proof every such layout is advertised as supported.

**Targeted repair:** extend pure framework marker discovery for supported Expo config filenames with package evidence to avoid treating unrelated app.json files as mobile apps. Preserve root-web/no-go.mod restrictions unless root-native adoption is deliberately supported. For nonconventional KCL paths, make the limitation explicit in lint output or expose a narrow inventory-overlay operation to commands that already render. Do not put a multi-env KCL render on every config load/shell completion. Add fixture tests for conventional/dynamic Expo config and custom-path generation versus lint.

### F6 — Adopted native API seam is not supplied (medium, verified conditional)

Hook generation accepts RN (`generate_frontend_hooks.go:254-266`), but adopted-frontend seam recovery supplies only Next/Vite (`internal/cli/generate_frontend_seams.go:44-76`). An adopted RN app without the expected `@/lib/connect` therefore receives generated hook imports without a matching scaffolded seam. Native regeneration only emits the API URL floor and reconciles runtime dependency (`generate_frontend_nav.go:399-422`). This does **not** affect a normal native scaffold, which already ships connect.ts.

**Targeted repair:** add only the native transport seam to adopted-native recovery, using the existing birth ledger and deletion/disown semantics. Do not add web test-utils. Verify adoption with one service hook, owned existing transport, and intentionally deleted transport cases. An alternative is to refuse hook generation until the app declares its transport seam; this is safer than silently generating broken imports but less consistent with web adoption.

### F7 — Device API reachability is not host reachability (high operational risk; source verified, device outcome unverified)

`up_env_compose.go:237-278` derives host API URLs using `http://localhost:<port>`; `up.go:3403-3423` injects that origin into the type-specific API variable. Native transport explicitly documents that localhost means the device and requires LAN/tunnel configuration (`react-native/src/lib/connect.ts.tmpl:14-19`). Parent/KCL overrides remain available. Hence the default supplied origin is not a universal device origin; host/browser success cannot prove physical-device connectivity.

**Targeted repair:** represent a device-reachable public dev API origin as an explicit override at the native launcher boundary, preserve parent/KCL precedence, and print the effective origin with a localhost/device warning. Do not silently guess a LAN interface or start a tunnel. Keep listener allocation separate from externally advertised origin. Test Android emulator, iOS simulator and physical-device cases as configuration cases; real connectivity remains an opt-in smoke check.

## Deliberate exclusions and incomplete integrations (do not misclassify)

- **Auth:** RN sends no token until the application wires `setAuthTokenGetter`; no real native provider ships (`react-native/src/lib/connect.ts.tmpl:21-29`; `shared/src/lib/auth/session-provider.ts.tmpl:23-34`). Browser cookie login is intentionally different (`session-provider.ts.tmpl:35-48`). This is an explicit integration gap, not an authentication bypass: protected backend calls will still reject an unauthenticated app. Recommend a narrow app-owned provider/token-store seam and explicit scaffold status, not reuse of cookie routes or a shared six-method identity service. Token storage, refresh, deep links and logout require a separate security design; no security exploit was established here.
- **Scenarios/mock transport:** `internal/cli/scaffold/scenario.go:196-214` excludes native. `internal/generator/frontend_gen.go:75-88` distinguishes mock support; native connect.ts always constructs a real transport (`47-54`). Shared session comments claiming pure mock answers every RPC are misleading for RN (`session-provider.ts.tmpl:70-88`). A fixture user is not a mocked network. Keep scenario eligibility explicit; either clearly reject native mock settings or implement a DOM-free scenario adapter later. Do not enable the browser template wholesale.
- **Generated APIs/tests:** native hooks are real support, but hook test emission deliberately excludes DOM-oriented native tests (`generate_frontend_hooks.go:331-335`). This avoids the false claim that missing native `setTransport`/web test-utils currently breaks scaffolded hook tests. Workspace transport injection already exists (`react-native/connect.ts.tmpl:55-62`); reuse it rather than creating another hooks runtime.
- **Logs/telemetry:** process stdout/stderr is persisted by up (`internal/cli/up.go:3485-3526,3552-3559`), but this does not imply native console/crash forwarding. RN omits shared-web (`frontend_tree.go:51-59`) and browser otel generation (`generate_frontend_nav.go:399-422`; `generate_pipeline.go:2656-2666`); KCL refuses RN browser telemetry (`schema.k:3964-3965`). No native uncaught-error sink was demonstrated. Recommend documenting process-log versus device-log coverage first; any native forwarding should be a dedicated platform adapter with opt-in endpoint/redaction, not the browser installer imported into RN.

## Targeted implementation sequence and boundaries

1. **Pin the current contracts with cheap rendered-fixture tests:** native scripts vs launch/build argv; raw RN config reads with a non-default field; raw `rn` dashboard filtering. No native build is necessary to catch F1–F4.
2. **Fix launch and artifact semantics locally:** script aliases first; a small argv function if required. Keep native packaging outside static build. Preserve existing script names, overrides and env filtering.
3. **Separate public config schema from source:** shared validation/projection, platform-specific value delivery only. Test config/auth/transport agreement and keep sensitive-field refusal on every client platform.
4. **Close inventory/adoption gaps:** marker detection and native owned transport birth; preserve containment, source pins, deletion ledger, package allowlists and workspace behavior.
5. **Make operational uncertainty visible:** effective device API origin and log coverage. Add opt-in device smoke checks later rather than claiming port-open equals app-ready.

A small canonical kind helper is justified by demonstrated alias disagreement: inventory normalization (`frontend_inventory.go:608-615`), config platform/static-dir switches (`generate_frontend_config.go:269-290`), env prefix (`up_env_compose.go:291-299`), dashboard filtering, and scenario eligibility each branch differently. Keep it a pure value normalization function with caller-local predicates, not a registry of callbacks owning build/dev/auth/release. Consumers should ask the one capability they need; producer structs must not become a grab bag of lifecycle methods.

**Alternatives rejected:** (a) normalize everything to browser behavior—breaks native origin, config timing, auth and DOM boundaries; (b) a universal target/plugin framework—too much new indirection for a few concrete contract mismatches; (c) wholesale MobileApp migration before repairs—leaves existing v1 RN frontends broken. Repair the existing seams first. Any later mobile release surface must coexist with v1 `Frontend(type="rn")`, custom scripts and static export behavior; new lint enforcement must follow warning-before-gating rollout.

## Validation still required

No runtime or test PASS is claimed. Follow-up should run only scoped short tests for touched config/codegen/generator/CLI helpers, with rendered manifests and fake package-manager executables. Expo port handling, platform HTTP restrictions, physical-device routing, native Connect fetch compatibility, secure token persistence and crash forwarding remain **unverified runtime questions**, not asserted defects. Full native release/distribution conclusions are outside this source audit.

Reported the concrete script/config lifecycle mismatch through `forge report-bug`: **94879943-7cdb-4dc3-b684-c39bb1f76e7e**. No implementation changes were made.
