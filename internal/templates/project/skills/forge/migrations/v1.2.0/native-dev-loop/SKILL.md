---
name: migration-v1.2.0-native-dev-loop
description: Adopt the v1.2.0 native (Expo) dev loop in an existing app — declare babel-preset-expo, stream over expo/fetch with a Jest expo/fetch mapping, bundle generated *_pb clients via a Metro .js→.ts resolver fallback, import only the portable runtime entry, stop tsconfig churn, reach the API and Metro from a device (--device-host, Metro on 8081, EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN), forward device logs to .forge/logs with sanitized receivers, track AppState/network in React Query, and never pause-and-replay mutations.
relevance: migration
---

# v1.2.0 — native dev loop

**Use when** the project has an Expo frontend. New scaffolds include everything
below; existing apps own these files, so forge never backfills them. Adapt by hand.

### `babel-preset-expo` is a declared dependency

Add `"babel-preset-expo": "~58.0.24"` (the version `expo@58.0.23` depends on) to
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
`createTransport(fetchImpl = expoFetch)` so tests can inject a fake network, and
copy `tests/connect-transport.test.ts`.

jest-expo stubs the native fetch module, so with an explicit `expo/fetch` import a
test that stubs `globalThis.fetch` (or uses MSW) would never see the app's
requests. Add `tests/setup/expo-fetch.ts` from the current scaffold
(`jest.mock("expo/fetch", () => ({ fetch: (...a) => globalThis.fetch(...a) }))`)
and set `setupFiles: ["<rootDir>/tests/setup/expo-fetch.ts"]` in `jest.config.cjs`.
jest-expo appends project setup files to its own, so do not spread the preset's
list. A test that must exercise the real `expo/fetch` declares its own
`jest.mock("expo/fetch", ...)`.

In the workspace layout, move `import { setApiTransport } from "<hooks package>"` up
with the other imports at the top of `connect.ts` (the call stays after `transport`
is created); an import mid-file fails `eslint --max-warnings 0` (`import/first`).

### Generated clients bundle in Metro (`metro.config.js`)

Generated `*_pb.ts` files import siblings with a `.js` extension
(`import_extension=.js`, which the web toolchains need), and Metro does not map
`.js` to `.ts`: any screen that imports a generated client failed with
`Unable to resolve "…forge_pb.js"`. Copy the `resolver.resolveRequest` block from
the new scaffold's `metro.config.js`. It resolves exactly as before first and,
only after a "could not resolve" error, retries the TypeScript source extension
(`.js` → `.ts`/`.tsx`, `.mjs` → `.mts`, `.cjs` → `.cts`); it never rewrites a
successful resolution or an import made from inside `node_modules`, and chains any
`resolveRequest` you already set.

### Shared helpers import only the portable runtime entry

`shared/src/lib/query-client.ts` and `shared/src/hooks/use-optimistic-mutation.ts`
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
over the injected localhost value (previously it was ignored); a value written in
the frontend's own `.env.<env>` file still wins over the shell.
`FORGE_DEV_DEVICE_HOST` is only read when a native frontend is targeted, so a
stale global export does not fail web-only runs. Web frontends are unaffected.

### Metro prefers port 8081

`forge env up` now gives a native frontend with no declared `port` Expo's
conventional 8081 when it is free, so a Debug build (`expo run:ios` or prebuild)
finds Metro without extra flags. If 8081 is busy, forge keeps an ephemeral port,
never touches the holder, and prints how to point a build at it: iOS
`xcrun simctl launch <udid> <bundle-id> -RCT_jsLocation localhost:<port>` (or rebuild
with `RCT_METRO_PORT=<port>`); Android `adb reverse tcp:<port> tcp:<port>` then Dev
Menu > Change Bundle Location; dev-client users open the URL Metro prints. A `port`
declared in KCL still wins. Only the first targeted native frontend claims 8081.
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
native frontends from the Metro port and `--device-host`; a value you set in the
shell, KCL or `.env.<env>` wins over the injected one). A plain Debug build without a
dev client has none of the `expo-constants` fields, so outside `forge env up` set
the variable yourself. If nothing resolves it logs one `console.warn` naming the fix
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

`shared/src/lib/query-client.ts` now sets `mutations.networkMode: "always"` beside
`retry: 0`. An offline write runs and fails visibly instead of pausing and being
silently replayed after reconnect, which could apply a stale or duplicate write.
This changes newly scaffolded web clients too (React Query pauses offline mutations
on web via `navigator.onLine`). Existing apps own this file: add the setting by hand
if you want the same policy, and opt individual mutations into
`useMutation({ networkMode: "online" })` only where replay is safe.

These are dev-loop and JS-level changes. Simulator, device and native-auth
evidence remain separate gates.
