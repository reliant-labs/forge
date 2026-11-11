---
name: v1.4.0
description: Upgrade a project to forge v1.4.0. (1) Session replay is a per-frontend, per-environment switch, forge.BrowserTelemetry {replay = True}, which renders the frontend's OTEL_REPLAY and opts the env in to replay ingestion; the env-wide Observability.signals.replay is deprecated (it still opts in) and an explicit runtime_config OTEL_REPLAY outranks the new switch. (2) Native (Expo) apps: foundation repairs, the Expo SDK 52 to 58 upgrade, the native dev loop and native sign-in (system-browser PKCE, keystore tokens), each an owned-file adaptation. Use when a deploy/kcl env declares ObservabilitySignals {replay = …} or a frontend's runtime_config OTEL_REPLAY, as environments scaffolded with _session_replay before v1.4.0 do, or when a frontend's package.json declares expo.
version: v1.4.0
detection: grep -rqsE 'ObservabilitySignals *\{[^}]*replay *=|OTEL_REPLAY *=' --include='*.k' deploy/kcl && exit 0; grep -qsE '"expo"[[:space:]]*:' frontends/*/package.json && exit 0; exit 1
---

# Upgrading to v1.4.0

## 1. Session replay moves onto the frontend

**Use when** an env's KCL (`deploy/kcl/<env>/main.k`) declares
`observability = forge.Observability {signals = forge.ObservabilitySignals {replay = …}}`
or a frontend's `runtime_config = {OTEL_REPLAY = …}`. Environments scaffolded
with the `_session_replay` switch before v1.4.0 declare both.

### Every project, replay or not: merge the frontend's `runtime_config`

In each **hosted** env (`deploy/kcl/staging/main.k`, `prod/main.k`), change
`_hosted_frontend`'s `runtime_config = _api_config` to
`runtime_config = f.runtime_config | _api_config`. The old line replaces the
frontend's whole `runtime_config` with the API URL, so any entry the frontend
declares (an `OTEL_REPLAY` you kept, a feature flag) never reaches the hosted
site. New scaffolds merge; this is a one-line owned-file edit with no effect on
a frontend that declares none.

### What changed

Replay is a choice about one frontend in one environment, so it is declared
there:

```kcl
_web_frontend = forge.Frontend {
    name = "web"
    # …
    telemetry = forge.BrowserTelemetry {replay = _session_replay}
}
```

`telemetry.replay = True` renders that frontend's `OTEL_REPLAY = "true"` into
its runtime config (a hosted site's StaticSite spec too, so the platform reads
it live) and opts the env's observability policy in to replay, so the platform
keeps the events. Off renders no key: unset is off. A hosted frontend with it on
needs the env to declare `observability`. With observability or browser
telemetry off, nothing records, whatever is declared.

Nothing breaks: `signals.replay` still opts the env in (it is deprecated, and
switches no frontend on) and a `runtime_config` `OTEL_REPLAY` still reaches the
browser. Upgrading changes no env's behavior and rewrites none of your KCL.

### What to do

In each env that uses `_session_replay` (or replay at all):

1. On each web frontend, set `telemetry = forge.BrowserTelemetry {replay = _session_replay}`
   (in the dev env scaffold: `replay = _observability and _session_replay`).
   An existing `telemetry` block takes the `replay` field instead.
2. Delete the frontend's `runtime_config` `OTEL_REPLAY` entry. **It outranks
   `telemetry.replay`**: an explicit `"false"` left behind keeps that frontend
   off with the switch on.
3. Drop `signals = forge.ObservabilitySignals {replay = _session_replay}` from
   `observability` (leave `observability = forge.Observability {}` if it was
   the only field). The frontend's switch now opts the env in.

**Adopting replay in an env that never had it** (a staging or prod scaffolded
before `_session_replay`): add `_session_replay = True` (or `False`) above the
frontend, `telemetry = forge.BrowserTelemetry {replay = _session_replay}` on the
web frontend, and `observability = forge.Observability {}` to the env's
`forge.Bundle` (a hosted frontend with replay on needs the env to declare it).

Then `forge env render <env>` and check the frontend's `runtime_config` (or
`runtime_config_spec` when hosted) carries `OTEL_REPLAY = "true"` exactly when
`_session_replay` is True, and `observability.policy.opt_in` lists `replay`.

## 2. Native frontend foundation repairs

**Use when** the project has an Expo frontend or a shared hooks workspace package.
These repairs are independent of the Expo SDK version (section 3 covers the SDK
upgrade) and do not add native authentication, signing,
store publication or OTA support. Existing owned code is preserved by generation;
review the following adaptations rather than deleting customized files. Do this
section **before** the SDK upgrade, and in order: the lint and test harness below
needs `tests/setup/expo-fetch.ts` and the Metro resolver before the first
`expo export` or Jest run passes on any SDK.

### Shared workspace transport

`packages/hooks/src/transport.ts` (or the project's named hooks package) is owned
starter code. If its `connectClient` calls `getApiTransport()` while constructing a
client, generated services can fail during import before frontend registration.
Adapt it to use a stable forwarding transport whose methods resolve the registered
transport at RPC invocation:

```ts
const forwardingTransport: Transport = {
  unary: (...args) => getApiTransport().unary(...args),
  stream: (...args) => getApiTransport().stream(...args),
};

export function connectClient<S extends DescService>(service: S) {
  return createClient(service, forwardingTransport);
}
```

Keep existing registration, custom instrumentation and exported APIs. Calls made
before registration still fail explicitly; import alone does not. Swapping the
registered transport affects subsequent calls, not operations already started.
Verify both unary and server-streaming behavior in your consumer before shipping.

### Expo scripts and runtime dependency

New native scaffolds include an owned `expo-dev.cjs` wrapper and `dev` script. The
wrapper forwards Forge's allocated `PORT` as Expo's `--port`; Expo's default CLI
does not consume `PORT` itself. Existing apps should add the wrapper only when absent
(`forge project rescaffold frontends/<name>/expo-dev.cjs`, then set the `dev`
script to `node expo-dev.cjs`), reviewing any custom dev entrypoint. Preserve
`start`, `ios`, `android` and `web` scripts. Do not overwrite custom package scripts.

The native `build` script is an **Expo web static export** into `dist`, not an IPA
or Android package. Add it only if that is the app's declared output (`app.json`
`expo.web.output: "static"`); keep custom
builds and do not mistake a successful Metro/web export for native device evidence.
For the advertised web output, declare `react-dom` directly when absent at the
version your SDK pins (18.3.1 on SDK 52, 19.3.0 on SDK 58; expo lists it only as an
optional peer); review customized existing
versions rather than overwriting them. Reinstall the frontend lockfile.

The old runtime range `^0.2.0` cannot resolve from the observed public registry.
Generation reconciles the dependency with Forge's supported published range;
review the package.json/lockfile diff and reinstall with the project's package
manager. Keep compatible newer custom ranges. These repairs do not change the SDK.

### Native typed public configuration

Regenerate native `src/lib/config_gen.ts`. It now reads explicit statically
addressed `EXPO_PUBLIC_<ENV_VAR>` values so Expo can inline them into the bundle,
using the existing schema defaults and validation. An explicit
`globalThis.__FORGE_CONFIG__` document remains the highest-precedence override.
These values are public, not secret storage; changing them requires a new bundle.
Set them before starting/exporting Expo and restart the bundler when changing the
build environment. Existing browser config.js delivery is unchanged.

Check non-default API/auth/feature values in the same native build. Required-field
errors should name the native build configuration, not a missing browser config.js.
Discovery also recognizes Expo `app.config.ts`/`app.config.js` with package evidence;
existing app.json discovery remains supported.

### Portable runtime imports

The shared helpers import only the runtime's portable `/interceptors` entry, never
the root barrel: the barrel also re-exports the browser dev-log forwarder and DOM
telemetry, and Metro does not tree-shake them out of a native production bundle.
Two edits, in **every** frontend that has these files (web included: they exist in
every frontend, and a project's paths are `frontends/<name>/src/...`, not
`shared/src/...`):

- `src/lib/query-client.ts` and `src/hooks/use-optimistic-mutation.ts` imported
  `userMessage` from the root. Replace it with
  `import { normalizeError } from "@reliantlabs/forge-web-runtime/interceptors"`
  and use `normalizeError(err).message` (the server text without its `[code] `
  prefix) through a local `userFacingMessage` that maps an empty message or
  `"Unknown error"` to "Something went wrong. Please try again.", matching the old
  copy. These two files also carry the mutation `networkMode` default of the
  dev-loop playbook; `forge project upgrade --check <path>` prints each diff.
- `src/hooks/use-api-query.ts`, `use-api-mutation.ts`, `use-join.ts` and
  `use-query-resource.ts` import the `ConnectClientError` **type** from the root;
  move that one import to `@reliantlabs/forge-web-runtime/interceptors`.

`normalizeError` and `ConnectClientError` are exported from `/interceptors` by the
published runtime (checked against 0.13.1 and 0.13.9), so neither edit needs a
runtime upgrade. The runtime source also exports `userMessage` there, but no
published release does and no template imports it.

### Metro must resolve generated clients before the first export

Do this in this section, not at section 4: any screen that imports a generated
client (`*_pb.ts` files import siblings as `./x_pb.js`, from buf's
`import_extension=.js`) fails `expo export` with `Unable to resolve "…_pb.js"`
because Metro does not map `.js` to `.ts`, and that happens on SDK 53 already.
Add the `resolver.resolveRequest` block described in
`forge skill load migrations/v1.4.0/native-dev-loop` (heading "Generated clients
bundle in Metro") to `metro.config.js` first.

### Acceptance for existing apps

Run app and shared-package typechecks and configured tests. Exercise import before
transport registration, subsequent transport replacement, allocated Expo port,
non-default native config, and any advertised static build. Preserve owned edits
across two generations. Actual native auth/streaming/device checks remain separate
foundation gates; these repairs alone do not establish production readiness.

### Native lint and unit-test harness

New Expo scaffolds include owned `eslint.config.cjs`, `jest.config.cjs`,
`tests/setup/expo-fetch.ts`, `tests/fetch-stub.test.ts` and
`tests/app-config.test.ts`. Existing apps may add missing files from the current
scaffold, but must preserve customized lint/test configurations and app config
formats.

**How to get a scaffold file** (this applies everywhere a playbook says "from the
current scaffold"; run it at the project's pinned forge): `forge project rescaffold
<path>...` writes a file that is absent, as forge would scaffold it today (it
refuses a path that exists); `forge project upgrade --check <path>` prints one
existing file's diff against the template; `forge project upgrade --force <path>`
replaces one that exists (use it for a file you never edited). `forge project
upgrade --check --all` lists every file with how far behind it is.

The set belongs together: `jest.config.cjs` lists `tests/setup/expo-fetch.ts` in
`setupFiles` (the `expo/fetch` to `globalThis.fetch` mapping the tests rely on),
so adding the config without that file fails every test suite with `Module
<rootDir>/tests/setup/expo-fetch.ts in the setupFiles option was not found`. Add
both, and `tests/fetch-stub.test.ts` (the pattern for an app test that stubs the
network; it passes on any SDK). `tests/app-config.test.ts` is a Router/identity
configuration smoke test, not native UI, secure-storage or device coverage. Its
second case, "declares native identifiers and a valid deep-link scheme", asserts
the section 5 scheme contract (one string equal to the bundle id), so an app that
has not done section 5 fails it: add the file now **without that case**, and
section 5 adds it back. Adapt the file if moving to dynamic Expo config.

This section runs before the SDK upgrade, so install these now (on SDK 52):
React/React DOM 18.3.1, Jest/jest-expo 29/52, `@jest/globals` 29 and the tested
ESLint 9 flat configuration dependency `eslint-config-expo` 9.2.0. Do not assume that package's major implies an Expo SDK
upgrade. Reinstall with the declared package manager and verify `lint`, `typecheck`
and at least one executed test. Native config bounds Jest workers and disables
watching; CLI worker flags may override the defaults for local diagnosis.

Native apps also declare `@babel/runtime: "^7.25.0"` as a direct runtime dependency.
Babel-generated helper imports cannot rely on a transitive/hoisted copy in isolated
pnpm workspaces. Add it when missing and reinstall the workspace lockfile.

These harness files are scaffold-once owned code; generation must not replace
application edits.

On SDK 52, add `"moduleResolution": "bundler"` to the owned native tsconfig's
`compilerOptions` when absent. Its inherited legacy resolver cannot resolve the
runtime's export-map subpaths used by generated hooks (`/service-hooks` and
`/mock-transport`). Review an existing explicit resolver choice rather than
replacing customized settings blindly. Typecheck a real generated service and
bundle with Metro; a no-service starter can miss this failure entirely.

If an existing owned `packages/ui-web/src/index.ts` aliases `ButtonClasses` or
`FormFieldContext` from a default export, replace those two lines with named
re-exports (`export * from "./components/ui/button_classes"` and
`export * from "./components/ui/form_field_context"`). These helpers have no default
export. Fresh workspace barrels now distinguish default components from named-only
helpers; customized existing barrels remain owned and are never overwritten.

Workspace scenario RPC type maps now import protobuf messages from the shared API
package rather than nonexistent app-local `@/gen` modules. Run `forge generate`
and browser/native/shared-package typechecks; do not hand-edit the generated
scenario-rpcs file to repair imports. Standalone import behavior is unchanged.

Jest retains Expo's native transform exceptions and additionally transforms the
runtime/Connect/protobuf ESM modules, including nested pnpm paths. Existing owned
Jest configs need an equivalent narrow allowance to exercise real generated hooks;
config-only smoke tests do not expose this module-loading requirement.

## 3. Expo SDK 52 to 58

**Use when** an Expo frontend's `package.json` declares `expo` below `~58`. New
scaffolds are generated on SDK 58; forge never rewrites an existing app's
`package.json`, `app.json`, `metro.config.js`, `tsconfig.json`, Jest or ESLint
files. Upgrade one SDK at a time with `npx expo install expo@~N.0.0 --fix`, after
sections 2 (the foundation, which must be in place first) and before 4 and 5.
The per-SDK table, the SDK 56 `expo-doctor` regression, the pnpm `allowBuilds`
entry SDK 58 needs first and the owned files to adapt are in
`forge skill load migrations/v1.4.0/native-sdk`.

## 4. Native dev loop

**Use when** the project has an Expo frontend. New scaffolds include all of it;
existing apps own the affected files and adapt them by hand: the
`babel-preset-expo` dependency, the `expo/fetch` streaming transport and Jest
mapping, the Metro `.js`→`.ts` resolver fallback for generated clients (already
required by section 2), device-reachable API and Metro origins, the Metro port
(delete the mobile `port` line from `deploy/kcl/dev/main.k`), native dev-log
forwarding, the web receivers' `no-control-regex` lint line, focus/connectivity
adapters and the mutation `networkMode` default. Load
`forge skill load migrations/v1.4.0/native-dev-loop`.

## 5. Native auth

**Use when** the project has an Expo frontend and wants system-browser sign-in
(OAuth code + PKCE, keystore tokens). Owned files, so adapt by hand: new
dependencies, the `expo-secure-store` plugin, `expo.scheme` (one reverse-DNS
string; never change `bundleIdentifier` or `package`), `src/lib/auth/*`, the
context, transport and layout edits, `tests/connect-transport.test.ts` and the
Jest config, plus the API half (`internal/app/auth.go`, `login_broker.go`, the
config proto). Existing apps keep working until `EXPO_PUBLIC_FORGE_OIDC_*` is
set. Load `forge skill load migrations/v1.4.0/native-auth`.
