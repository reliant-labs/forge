# Mobile foundation boundaries audit

## Scope and verdict

Audited Forge **6c04734393bf58a20adfe169f1904b29009206f2** by source inspection. No tests, dependency installs, native builds, or code generation were run. `forge` is available; `reliant-backend` is not. Only this document is owned by this audit. Release proposals are desired future behavior, not evidence of implemented behavior; this audit does not use them as proof.

**Verdict:** the broad boundaries are sensible (shared API/hooks, separate browser/native UI, app-owned transport), but workspace initialization violates that model, shared helpers bypass the native-safe runtime entry, and validation does not exercise the combined workspace/mobile product. Do not interpret existing mobile e2e coverage as workspace conformance.

Severity: **High** blocks a supported integration; **Medium** weakens validation or creates plausible platform failure; **Low** is avoidable maintenance complexity. “Proven” below means established from source, not reproduced in a running app. Risks explicitly require execution to establish impact.

## Actual architecture and paths

There is **no `internal/templates/frontend-workspaces/` directory** at this HEAD. Workspace manifests, tsconfigs, transport and base hooks are embedded strings in `internal/generator/frontend_workspaces.go:166-554`; UI emitters occupy `:573-755` and `:804-925`. The named skill is `internal/templates/project/skills/forge/frontend-workspaces/SKILL.md`, not a workspace template implementation.

`FrontendWorkspaceLayout` derives scope and package names (`internal/generator/frontend_workspaces.go:36-76`). API and hooks always accompany workspace generation; ui-web is also emitted unconditionally (`:120-144`), while the native writer relies on caller gates (`:795-803`). Source packages export TypeScript directly, leaving compilation to the consuming app (`:220-240`, `:330-358`, `:811-835`).

Frontend composition is already centralized: web gets `shared + shared-web + kind`, native gets `shared + kind` (`internal/templates/frontend_tree.go:44-59`). Preserve that distinction rather than creating another parallel mobile framework. Native primitives come from `pkg/components/native` through `internal/generator/frontend_workspaces.go:865-890`. Native and web UI are deliberately separate, scaffold-once packages, not cross-platform interchangeable components (`:565-572`, `:790-803`, `:894-917`).

## Findings

### F1 — High, proven integration defect: workspace bootstrap eagerly consumes an unset transport

- The hooks package singleton begins null and throws if read before registration (`internal/generator/frontend_workspaces.go:407-420`); `connectClient` reads it immediately (`:429-430`).
- Its root barrel re-exports transport **and all generated service modules** (`:512-521`).
- A generated service imports that transport and constructs its client at module scope (`internal/templates/frontend/hooks.ts.tmpl:66-85`).
- Native connect imports `setApiTransport` from that root barrel and only registers in its module body (`internal/templates/frontend/react-native/src/lib/connect.ts.tmpl:55-62`). Static dependencies evaluate before that body. With at least one generated service, the bootstrap therefore reaches the unset transport first.

The emitter's claim that clients resolve lazily when hooks fire (`internal/generator/frontend_workspaces.go:396-405`) is inconsistent with emitted code. The same root-barrel contract is asserted for all three frontend kinds by `internal/templates/frontend_workspaces_test.go:174-199`; that assertion cannot detect evaluation order.

**Recommendation:** make client resolution genuinely call-time, or inject a client/transport getter into hook factories. A transport-only subpath removes the bootstrap barrel cycle but is not by itself a durable solution for apps importing hooks before bootstrap. Preserve existing exports in v1. Pin import-before-registration and subsequent transport replacement behavior with executable module tests. Reported as Forge defect `666a5273-51c8-4996-bbde-387b0a3ab36e`.

### F2 — Medium, proven boundary leakage; native runtime impact is a risk

Native connect correctly imports `/interceptors`, explicitly avoiding the React/component barrel (`internal/templates/frontend/react-native/src/lib/connect.ts.tmpl:3,40-42`). But its layout imports the shared query client (`internal/templates/frontend/react-native/app/_layout.tsx.tmpl:3-6`), which value-imports `userMessage` from the **root** runtime (`internal/templates/frontend/shared/src/lib/query-client.ts:1-5`). The shared optimistic mutation helper does the same (`internal/templates/frontend/shared/src/hooks/use-optimistic-mutation.ts:1`). The root re-exports session, browser UI shell, error boundary, toast host, route guard and Resource (`web-runtime/src/index.ts:92-142`). These are unnecessary module dependencies for mobile error formatting.

Type-only imports also use the wide root: shared query resource (`internal/templates/frontend/shared/src/hooks/use-query-resource.ts:3`) and workspace base wrappers (`internal/generator/frontend_workspaces.go:439,478`). Those imports are erased at runtime: **do not count them as proven bundle imports**. They still enlarge the declaration-resolution boundary.

`/interceptors` already exposes error utilities (`web-runtime/src/interceptors.ts:217-226`) and has a React-free graph guard (`web-runtime/src/interceptors-entry.test.ts:105-130`). Prefer that existing entry for portable error values/types, or add an explicitly portable errors entry if a narrower contract is justified. Do not move all hooks into a new package merely because the published package is named web-runtime.

**Not proven:** that HyperDX or react-dom installs in every mobile app, or that the root import crashes Hermes. Most runtime peers are optional (`web-runtime/package.json:76-101`), `sideEffects` is false (`:27`), and bundlers may prune exports. Measure the resolved Metro graph rather than asserting bundle size from a manifest.

### F3 — High validation gap, proven: mobile scripts are not a complete runnable lane

Native declares `lint: eslint .`, `typecheck: tsc --noEmit`, and `test: jest`, but no `build` script (`internal/templates/frontend/react-native/package.json.tmpl:7-15`). It declares ESLint 9 and jest-expo (`:43-51`), but the native/shared template inventory contains no native Jest configuration/preset, test suite, or ESLint flat configuration. Installing jest-expo is not configuring Jest.

Generated Taskfile `build:frontend` executes `$PM run build` for **every discovered frontend** (`internal/templates/project/Taskfile.yml.tmpl:133-137,212-249`): native therefore has a proven missing-script mismatch. Its test lane explicitly allows no tests (`:197-210`), so an empty mobile test tree can pass without exercising native code. Treat ESLint's expected missing-config failure as an unexecuted risk here, not a reproduced CLI result.

**Recommendation:** declare frontend validation capabilities once (bundle/typecheck/lint/test, with explicit platform commands), and let Taskfile and CI consume that inventory. Give native a meaningful non-device validation command rather than pretending web `build` and Expo release builds are equivalent. Add a configured native test harness with at least one real test; retain an explicit no-tests policy only for genuinely empty projects.

### F4 — Medium, proven coverage omission: shared workspace packages fall outside the frontend loop

API, hooks, ui-web and ui-native each get `typecheck` scripts (`internal/generator/frontend_workspaces.go:230-232,341-343,601-603,822-824`). The generated Taskfile iterates discovered **frontends**, not `packages/*` (`internal/templates/project/Taskfile.yml.tmpl:212-249`). App compilation may visit imported sources, but is not a replacement for compiling every package or checking unused exports. The “no DOM” tsconfig assertions do not prove the package compiles without ambient DOM types.

**Recommendation:** add a shared-package validation inventory/lane, not a second ad hoc shell glob. Assert each declared package's typecheck executes and a deliberate error in an otherwise unimported native primitive fails it. Validate React 18 native and React 19 browser consumers separately.

### F5 — Medium risk: workspace resolution and export conformance are incompletely checked

Metro's custom workspace search recognizes only a parent `package.json.workspaces` (`internal/templates/frontend/react-native/metro.config.js:50-75`), while the workspace emitter declares membership in `pnpm-workspace.yaml` (`internal/generator/frontend_workspaces.go:172-206`). Expo defaults may compensate; this is **not proof that pnpm Metro bundling fails**. It is a second workspace-recognition algorithm with a different input contract and a five-parent limit.

Only `/interceptors` has a physical legacy resolver shim (`web-runtime/package.json:28-65`; `web-runtime/src/interceptors-entry.test.ts:133-161`). Generated hooks import `/service-hooks` (`internal/templates/frontend/hooks.ts.tmpl:48-65`); Metro explicitly enables package exports (`internal/templates/frontend/react-native/metro.config.js:42`), but the app tsconfig inherits Expo's resolver without an explicit override (`internal/templates/frontend/react-native/tsconfig.json:1-10`). Whether that SDK resolves the new subpath must be established with its actual compiler, not inferred from web exports tests.

**Recommendation:** test a real pnpm workspace plus installed packed runtime under both the app resolver and Metro. Simplify custom workspace discovery only after measuring what Expo already provides; do not emulate pnpm's resolution rules inside Forge.

### F6 — Low, proven duplication: version/tool ownership is only partly centralized

There is already a canonical pnpm constant and consumer contract (`internal/templates/toolversions.go:44-55`), while native uses `{{pnpmVersion}}` (`internal/templates/frontend/react-native/package.json.tmpl:5`). Do not create a second version registry.

Remaining copies include protobuf/Connect/TypeScript versions across API/hooks/UI emitter strings (`internal/generator/frontend_workspaces.go:234-239,346-355,615-617,831-832`) and native manifest (`internal/templates/frontend/react-native/package.json.tmpl:17-22,45-51`). Native hardcodes Prettier `3.5.3` (`:50`) beside the canonical `PrettierVersion` (`internal/templates/toolversions.go:17-34`). Native runtime template starts at `^0.2.0` (`:21`), while reconciliation uses `^0.13.3` (`internal/generator/frontend_webruntime.go:57-68`): this is duplicated fallback policy, **not proof generated apps retain the stale range**, because the reconciler exists.

Workspace pnpm settings are embedded Go (`internal/generator/frontend_workspaces.go:188-205`) alongside standalone settings templates (`internal/templates/frontend/shared-web/pnpm-workspace.yaml.tmpl`, `internal/templates/frontend/react-native/pnpm-workspace.yaml.tmpl`). Extract common policy data while retaining platform-specific allow-build entries. Move large workspace literals into an actual template category; keep naming/layout in Go. Avoid a generic package-description DSL that merely relocates all current conditionals.

### F7 — Medium, proven: mock/hook testing remains browser-shaped

Local hook generation explicitly skips native starter tests because their DOM test utilities do not apply (`internal/cli/generate_frontend_hooks.go:331-341`). Workspace generation emits modules/barrel but no starter test (`:473-527`). Browser starters import `@testing-library/react`, Vitest and app-local `setTransport`/test-utils (`internal/templates/frontend/hooks.test.tsx.tmpl:14-20`); transplanting them into shared hooks or native is incorrect.

The rich generated scenario/mock surface is explicitly browser-oriented (`internal/generator/frontend_mocks.go:20-23,41-63`). Native transport is constructed directly without that scenario seam (`internal/templates/frontend/react-native/src/lib/connect.ts.tmpl:43-54`). This is a capability gap, not proof native cannot use the portable published mock transport (`web-runtime/package.json:49-55`).

**Recommendation:** keep proto fixture/dispatch generation independent of DOM runner and app alias paths. Test the portable transport and hook factories once; layer a native renderer harness and browser renderer harness over the same behavior cases. Test success, typed failure, mutation invalidation, pagination, and transport swapping, not just that imports mention the right names.

## What existing validation actually proves

| Evidence                                                                          | Classification and limit                                                                                                                                                                                        |
| --------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/generator/frontend_workspaces_test.go:79-154,160-180,192-254`           | Go filesystem emission/JSON/string assertions, plus preservation tests. No pnpm install, TS compiler or Metro execution.                                                                                        |
| `internal/generator/frontend_workspaces_ui_native_test.go:29-114,117-143`         | Emitted manifest/tokens/barrel content and idempotence. Checking absence of `DOM` is not compiling native primitives.                                                                                           |
| `internal/templates/frontend_workspaces_test.go:42-96,121-144,174-199`            | Rendered strings for deps/imports/registration; cannot prove workspace linking or module evaluation order.                                                                                                      |
| `internal/templates/react_native_test.go:11-49,52-126`                            | Composed-tree presence, nonempty rendering, selected strings and absence of `use client`; no TS/native execution.                                                                                               |
| `internal/cli/generate_frontend_hooks_test.go:383-411`                            | Workspace output placement assertions with web/mobile config, not a combined consumer build.                                                                                                                    |
| `internal/cli/scaffold_frontend_noun_e2e_test.go:44-66,88-107`                    | **Real integration:** scaffold all three kinds, install, run each app's tsc, build web, run Metro iOS export. No workspaces opt-in. This is a JS bundle, not an Xcode/native application build or device test.  |
| Same test `:109-112`                                                              | Regenerates, then rechecks web/SPA only; mobile is not rechecked after regeneration.                                                                                                                            |
| `internal/cli/scaffold_frontend_error_contract_e2e_test.go:55-59,125-128,169-195` | **Real compile integration:** generated web hooks, positive and negative error-type probes. No native/workspace matrix.                                                                                         |
| `web-runtime/src/interceptors-entry.test.ts:38-103,105-161`                       | Source-graph walk and export/shim metadata assertions. Valuable boundary guard, not a Metro consumer integration.                                                                                               |
| `internal/generator/webruntime_export_drift_test.go:29-52,80-81`                  | **Real registry artifact access** with parsed-symbol comparison; skipped under short/offline. Scanner matches root imports only, so does not certify `/service-hooks` or `/mock-transport` consumer resolution. |

CI executes the e2e corpus using planned shards and a coverage ledger (`.github/workflows/e2e-suite.yml:490-510,619-693`). That proves scheduling/accounting of selected tests, not coverage of an absent workspace fixture. Generated consumer CI delegates frontend tests to the Taskfile (`internal/templates/ci/github/ci.yml.tmpl:240-241`); it inherits the no-tests/shared-package limitations above. Root CI's npm license checks (`.github/workflows/ci.yml:1166-1176`) are not native conformance checks. Do not say “no mobile CI exists”; say **the current real mobile integration covers standalone compilation/export, not shared workspace behavior, runner configuration, or native device behavior**.

## Recommended sequence and conformance tests

1. **Fix transport lifecycle first** at `frontend_workspaces.go` and `hooks.ts.tmpl`. Add an executable test importing the generated hooks barrel before registration, then register a recording transport and invoke query/mutation calls. Require no import-time throw, correct service routing, explicit uninitialized-call error, and documented behavior when transport changes. A string snapshot cannot substitute.
2. **Use narrow portable runtime entries** in shared helpers. Add an AST/bundler-based forbidden-dependency assertion for native entry graphs: no browser components, react-dom or HyperDX through error formatting. Count type-only imports separately. Preserve public root exports for compatibility.
3. **Create one mixed fixture:** pnpm workspaces on, browser + Expo + one real query/mutation/paged service. Compile API/hooks/UI packages and each app; execute shared hook behavior under both renderers; export Expo JS for iOS and Android in an appropriately slow lane. This needs no simulator or store credentials. Assert transport bootstrap is actually imported by the app.
4. **Packed-artifact matrix:** test dev bridge and an `npm pack` artifact separately, with no source-checkout fallback in the packed case. Resolve `/interceptors`, `/service-hooks`, `/mock-transport` using the actual Expo TS settings and Metro. Positive and negative type probes must reject nonexistent error fields and preserve bigint/proto request semantics.
5. **Validation inventory conformance:** generated Taskfile and generated CI run the same commands for each supported kind and shared package. Deliberately break an unimported primitive, hook test, ESLint config, and native test to prove each lane turns red. Require at least one executed mobile test; do not call `--passWithNoTests` a mobile regression gate.
6. **Ownership/convergence:** edit scaffold-owned package manifests, tokens and barrels; regenerate twice; verify edits survive and generated service files converge. Current write-if-missing behavior (`frontend_workspaces.go:771-778,865-890`) is valuable and must survive extraction into templates.

### Design trade-off

Prefer a small shared lifecycle/validation contract plus platform adapters over either extreme: copying web scaffolds into mobile hides DOM assumptions; building a universal UI/runtime framework expands scope without proving current boundaries. The counterargument to more shared machinery is coupling—especially version changes across React 18/19. Address it with independently compiled consumer fixtures and narrow inputs, not a large capability enum or platform switch spread across Taskfile, templates and generator. Native device/store validation remains a separate future lane; neither source inspection nor Metro export certifies it.
