---
name: migration-v1.4.0-native-dev-loop
description: Adopt the v1.4.0 native (Expo) dev loop in an existing app — declare babel-preset-expo, stream over expo/fetch with a Jest expo/fetch mapping, bundle generated *_pb clients via a Metro .js→.ts resolver fallback, import only the portable runtime entry, stop tsconfig churn, reach the API and Metro from a device (--device-host, Metro on 8081, EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN), forward device logs to .forge/logs with sanitized receivers, track AppState/network in React Query, and never pause-and-replay mutations.
relevance: migration
---

# v1.4.0 — native dev loop

**Use when** the project has an Expo frontend. New scaffolds include everything
below; existing apps own these files, so forge never backfills them. Adapt by hand.
Do section 2 of `forge skill load migrations/v1.4.0` first (it carries the Metro
resolver and `tests/setup/expo-fetch.ts` this page also touches).

**Getting a scaffold file** (this page says "from the current scaffold" often): at
the project's pinned forge, `forge project rescaffold <path>...` writes a file that
is absent, `forge project upgrade --check <path>` prints one file's diff against the
template, and `forge project upgrade --force <path>` replaces one you never edited.

### `babel-preset-expo` is a declared dependency

Add `"babel-preset-expo": "~58.0.25"` (the version `expo@58.0.24` depends on) to
`devDependencies` and reinstall. Your owned `babel.config.js` names that preset,
and under pnpm's isolated `node_modules` it is not resolvable from the app root.
Without it, `pnpm run dev` (the command `forge env up` runs) answers HTTP 500 for
every bundle with `Cannot find module 'babel-preset-expo'`, and `pnpm run test`
cannot load the Babel config. `pnpm exec expo ...` hides the problem because the
pnpm shim sets `NODE_PATH`.

### Streaming transport (`src/lib/connect.ts`)

Import `fetch as expoFetch` from `"expo/fetch"` (a subpath of `expo`, no new
dependency) and pass `fetch: expoFetch` to `createConnectTransport`. On SDK 58 the
global fetch is already `expo/fetch` unless `EXPO_PUBLIC_USE_RN_FETCH` is set, but
React Native's own fetch buffers whole responses, so server-streaming RPCs depend
on that default; passing it explicitly removes the dependency. Optionally export
`createTransport(fetchImpl = expoFetch)` so tests can inject a fake network.
(`tests/connect-transport.test.ts` exercises the auth interceptors, so it belongs
to `migrations/v1.4.0/native-auth`, not here.)

jest-expo stubs the native fetch module, so with an explicit `expo/fetch` import a
test that stubs `globalThis.fetch` (or uses MSW) would never see the app's
requests. Add `tests/setup/expo-fetch.ts` (`forge project rescaffold
frontends/<name>/tests/setup/expo-fetch.ts`; section 2 of the main playbook adds it
first, because `jest.config.cjs` loads it) and `tests/fetch-stub.test.ts`, the
pattern for a test that stubs the network
(`jest.mock("expo/fetch", () => ({ fetch: (...a) => globalThis.fetch(...a) }))`)
and set `setupFiles: ["<rootDir>/tests/setup/expo-fetch.ts"]` in `jest.config.cjs`.
jest-expo appends project setup files to its own, so do not spread the preset's
list. A test that must exercise the real `expo/fetch` declares its own
`jest.mock("expo/fetch", ...)`.

In the workspace layout, move `import { setApiTransport } from "<hooks package>"` up
with the other imports at the top of `connect.ts` (the call stays after `transport`
is created); an import mid-file fails `eslint --max-warnings 0` (`import/first`).

### Generated clients bundle in Metro (`metro.config.js`)

Required before the first `expo export`, so section 2 of the main playbook sends
you here early; it is not optional on any SDK from 53 on.

Generated `*_pb.ts` files import siblings with a `.js` extension
(`import_extension=.js`, which the web toolchains need), and Metro does not map
`.js` to `.ts`: any screen that imports a generated client failed with
`Unable to resolve "…forge_pb.js"`. Copy the `resolver.resolveRequest` block from
the new scaffold's `metro.config.js` (`forge project upgrade --check
frontends/<name>/metro.config.js` shows it as added lines; it needs
`const path = require("node:path")` at the top of the file). It resolves exactly as before first and,
only after a "could not resolve" error, retries the TypeScript source extension
(`.js` → `.ts`/`.tsx`, `.mjs` → `.mts`, `.cjs` → `.cts`); it never rewrites a
successful resolution or an import made from inside `node_modules`, and chains any
`resolveRequest` you already set.

### Shared helpers import only the portable runtime entry

Already done by section 2 of the main playbook (the paths are
`frontends/<name>/src/lib/query-client.ts` and
`frontends/<name>/src/hooks/use-optimistic-mutation.ts`, in every frontend, web
included, plus the `ConnectClientError` type import in four hook files). In short:
`src/lib/query-client.ts` and `src/hooks/use-optimistic-mutation.ts`
imported `userMessage` from the runtime's root barrel. The barrel also re-exports
the browser dev-log forwarder, and Metro does not tree-shake it, so it shipped in
native production bundles. Both now use
`normalizeError(err).message` from `@reliantlabs/forge-web-runtime/interceptors`
(the server text without its `[code] ` prefix) through a local `userFacingMessage`
that maps an empty message or `"Unknown error"` to "Something went wrong. Please try
again.", matching the old copy. Apply the same change to these owned
files in existing apps; it works with every published 0.13.x runtime.

### `tsconfig.json` no longer churns on first start

With `experiments.typedRoutes` off (the scaffold default), Expo 58 removes
`.expo/types/**/*.ts` and `expo-env.d.ts` from `include` on every `expo start`.
The template no longer lists them. Remove them from an existing owned
`tsconfig.json`, or enable `typedRoutes` in `app.json` and keep them.

### Reaching the API from a device

`forge env up` still injects `EXPO_PUBLIC_API_URL=http://localhost:<port>`, which
reaches the iOS simulator and this machine only. Use
`forge env up dev --device-host 10.0.2.2` for the Android emulator and
`--device-host <LAN IP>` (or `FORGE_DEV_DEVICE_HOST`) for a physical device. forge
never picks an interface or starts a tunnel, and warns if the API answers only on
loopback. Pin a fixed URL (a tunnel) in KCL `env_vars` or `config.api_url`; to
override a pinned value from the shell, set `FORGE_ENV_OVERRIDE=EXPO_PUBLIC_API_URL`.
New for native frontends: an `EXPO_PUBLIC_API_URL` exported in your shell now wins
over the injected localhost value (previously it was ignored), and so does one
written in the frontend's own `.env.<env>` or in KCL `env_vars`: forge's own value
is the lowest layer. Between the declared layers, KCL and `.env.<env>` still
outrank the shell (a declared value is a statement about how the process runs;
`FORGE_ENV_OVERRIDE=EXPO_PUBLIC_API_URL` hands the key back to the shell).
`FORGE_DEV_DEVICE_HOST` is only read when a native frontend is targeted, so a
stale global export does not fail web-only runs. Web frontends are unaffected.

### Metro prefers port 8081

`forge env up` now gives a native frontend with no declared `port` Expo's
conventional 8081 when it is free, so a Debug build (`expo run:ios` or prebuild)
finds Metro without extra flags. If 8081 is busy, forge keeps an ephemeral port,
never touches the holder, and prints how to point a build at it: iOS simulator
`xcrun simctl launch <udid> <bundle-id> -RCT_jsLocation localhost:<port>` (or rebuild
with `RCT_METRO_PORT=<port>`); Android `adb reverse tcp:<port> tcp:<port>` then Dev
Menu > Change Bundle Location; dev-client users open the URL Metro prints. With
`--device-host` the simulator line is replaced by the physical-device one: rebuild
with `RCT_METRO_PORT=<port>` (the build then looks for Metro at
`<device host>:<port>`), and on the first launch allow "Local Network" on the device
and relaunch (React Native shows `No script URL provided` until the prompt is
accepted). A `port`
declared in KCL still wins, and **an app scaffolded before v1.4.0 declares one**:
its `deploy/kcl/dev/main.k` has `port = _<name>_frontend_port` on the native
frontend and a `_<name>_frontend_port = plugin.resolve_port("<project>-dev-<name>",
3000)` block above it (with its `# The <name> frontend's dev-server port…`
comment). That port wins over 8081, so Metro runs on 3000 or 3001 and a Debug build
cannot find it. Delete the `port = …` line **and** the `_<name>_frontend_port`
block (no other line of the frontend declaration changes; the `rn` frontend's
`port` is optional) unless you want Metro pinned. New scaffolds declare no port for
an `rn` frontend, and `forge scaffold frontend` stops adding one. Only the first
targeted native frontend claims 8081. An `rn` frontend without a declared `port` now
has no port at all (Undefined in KCL, `null` in the rendered JSON), so KCL that
interpolates or adds to `f.port` on such a frontend must handle it; `port = <n>`
keeps the old behaviour.
If another process takes 8081 between selection and launch, `forge env up` moves
Metro to an ephemeral port and says so, instead of failing or touching the holder.

### Native dev log forwarding

New scaffolds forward console output, uncaught errors and unhandled promise
rejections to Metro, which prints them as `[browser:<level>] ...` lines, so they
land in `.forge/logs/<env>/frontend_<name>.log` like web logs
(`grep '\[browser:error\]'`). Existing apps: copy `src/lib/devlog.ts`; append the
dev-log receiver block from the current scaffold's `metro.config.js` (after the
runtime bridge block), chaining any `server.enhanceMiddleware` you already set; add
`if (__DEV__) require("../src/lib/devlog").installNativeDevLogging();` to
`app/_layout.tsx` (with an `@typescript-eslint/no-require-imports` disable comment
above it); optionally copy `tests/devlog.test.ts`. Production bundles carry no
forwarder. The device must reach Metro's origin, not the API origin. The
forwarder takes it from `installNativeDevLogging({ origin })`, then the public
`expo-constants` fields (`expoConfig.hostUri`, `experienceUrl`, `linkingUri`, which
exist only when the device actually reached Metro, so they are the right address),
then `EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN` (an http(s) URL that `forge env up` injects for
native frontends from the Metro port: `http://<device host>:<port>` under
`--device-host`, `http://localhost:<port>` otherwise; a value you set in the shell,
KCL or `.env.<env>` wins over the injected one). A device Debug build without a dev
client has none of the `expo-constants` fields, so there the injected variable is
the origin that is used; outside `forge env up` set it yourself. If nothing resolves it logs one `console.warn` naming the fix
instead of silently doing nothing. The line `[forge-devlog] native logging attached` confirms
delivery. The forwarder redacts credential-shaped values (keys ending in token,
secret, password or credential(s); credential-ish `*key` names such as `apiKey`,
`privateKey`, `sessionKey` and a bare `key=`; quoted values; whole array/object values
of those keys; cookie headers; Bearer/Basic/JWT) and aborts a stalled request after
5 s. Identifiers such as `queryKey` are not masked, and neither is prose such as
`No credential: this bundle…` (a bare `credential:` followed by an unquoted word).
Grep with `'^\[browser:error\]'`:
receivers prefix every line, so a forged prefix inside a message is not at line
start.

### Dev-log receivers sanitize output (web and native)

All three receivers (Vite `src/lib/devlog-plugin.ts`, Next.js
`src/app/%5F_forge/log/route.ts`, native `metro.config.js`) now strip ANSI escapes and
control characters other than newline and tab, and print every line of a message
with its own `[browser:<level>] ` prefix. A client can no longer forge `[up]` or
`[browser:error]` lines with an embedded newline, and stack traces become greppable
per line. These files are owned: copy the new `sanitize()`/`formatEntry()` (or the
receiver block) into existing apps. Wire protocol and clients are unchanged; tools
that grep for unprefixed continuation lines will no longer match them.

**Web receivers, three more edits** (they were found by running this on a real
v1.3.0 app; `forge project upgrade --check <path>` shows each as template lines
you lack):

- **`// eslint-disable-next-line no-control-regex`** goes on its own line directly
  above the `return s.replace(/\x1b…/g, "")…` statement inside `sanitize()`, in
  the Next.js `src/app/%5F_forge/log/route.ts` and the Vite
  `src/lib/devlog-plugin.ts` (the scaffold adds two comment lines above it saying
  why). Without it the sanitizer's control-character regexes fail the frontend's
  `eslint .` (`no-control-regex`, in `js.configs.recommended`), and `forge lint`
  with it. Add it when you copy `sanitize()`. The Metro receiver's copy passed the
  native `eslint .` unchanged in the executed run.
- **Next.js `src/app/%5F_forge/log/route.ts` rejects a query string.** After the
  production guard in `POST`, `if (new URL(request.url).search) { return new
  Response(null, { status: 400, headers: { "X-Forge-Devlog": "2" } }); }`: browser
  logs are POST bodies, never URL parameters, so a query is a malformed or
  hostile caller.
- **Next.js `next.config.ts`** needs the matching rule, because Next's own access
  logger prints the raw URL before the handler runs: add
  `logging: { incomingRequests: { ignore: [/(?:^|\/)__forge\/log(?:\?|$)/] } },`
  at the top of the `nextConfig` object (all three of its output-mode variants
  carry it in the template). An unrelated `logging` block you already have: merge
  the `ignore` entry into it.

**Native receiver (`metro.config.js`) accepts only `application/json`.** A request
with any other content type gets `415` and is not logged. The native forwarder
always sends JSON, and a page open in the developer's browser cannot (its simple
cross-site content types are form-encoded, multipart or `text/plain`), so it cannot
write lines into the log. Copy `isJsonRequest` and the early return from the
scaffold's `devLogRequest`. The web receivers are unchanged: the browser's unload
flush is `navigator.sendBeacon`, which can only send `text/plain` without a
preflight, so they accept it.

### Focus and connectivity (`src/lib/query-lifecycle.ts`)

Add `"expo-network": "~58.0.4"` to `dependencies`, copy `src/lib/query-lifecycle.ts`
(and optionally `tests/query-lifecycle.test.ts`), and call
`useEffect(() => setupQueryLifecycle(queryClient), [])` in `app/_layout.tsx`. React Query then
tracks `AppState` and network reachability instead of DOM events. The adapter sets
no client defaults. Stale queries refetch on reconnect; focus refetch stays opt-in
per query; queries in the default `"online"` network mode report
`fetchStatus: "paused"` while offline, so render that state rather than an endless
spinner. Server streams are not resumed automatically: re-subscribe on focus or
reconnect.

### Mutations never pause and replay (web and native)

`frontends/<name>/src/lib/query-client.ts` now sets `mutations.networkMode: "always"` beside
`retry: 0`. An offline write runs and fails visibly instead of pausing and being
silently replayed after reconnect, which could apply a stale or duplicate write.
This changes newly scaffolded web clients too (React Query pauses offline mutations
on web via `navigator.onLine`). Existing apps own this file: add the setting by hand
if you want the same policy, and opt individual mutations into
`useMutation({ networkMode: "online" })` only where replay is safe.

These are dev-loop and JS-level changes. Simulator, device and native-auth
evidence remain separate gates.
