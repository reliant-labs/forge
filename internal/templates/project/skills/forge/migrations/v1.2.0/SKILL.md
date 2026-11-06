---
name: v1.2.0
description: Upgrade a project to forge v1.2.0. In mock mode every RPC of every service a frontend has generated TypeScript for — custom reads, streams, vendored contracts — now answers a deterministic, schema-valid default instead of Unimplemented; forge generate writes src/mocks/services/<service>_gen.ts per service and mock-transport_gen.ts gathers them. Hand-written scenario handlers that only filled in a response shape can become mockService(Service, { rpc: (req, d) => patch(d.rpc(req), …) }) overrides, typed for vendored services too. Use when a browser frontend has src/lib/mock-transport_gen.ts.
version: v1.2.0
detection: for f in frontends/*/src/lib/mock-transport_gen.ts; do [ -f "$f" ] && exit 0; done; exit 1
---

# Upgrading to v1.2.0

## 1. Every RPC answers in mock mode

**Use when** a Next.js or Vite frontend has `src/lib/mock-transport_gen.ts`.

### What changed

`forge generate` writes one Tier-1 module per Connect service found in the
frontend's `src/gen/**/*_pb.ts` — the project's own protos and any vendored
contract another buf template generated there:

```
src/mocks/services/<service>_gen.ts   export const <service>Defaults = serviceDefaults(<Service>);
```

`src/lib/mock-transport_gen.ts` gathers them and passes them to the engine as
`services`. The dispatch order gains one rung, after the entity fixtures:
scenario handler → hybrid passthrough → fixtures → **service defaults** →
`Unimplemented`. A default is built at runtime from the service's descriptor
(field names, enums, maps, oneofs, well-known types, protovalidate rules) and is
the same for the same request; the `frontend/mock-mode` skill lists what each
field becomes.

The new modules import `serviceDefaults` and `isServiceDefaults` from
`@reliantlabs/forge-web-runtime/mock-transport`; `forge generate` raises the
frontend's `@reliantlabs/forge-web-runtime` range to the release that has them.
Run `npm install` in the frontend after generating.

### What to check

- A mocked page that showed an error for a custom RPC now shows sample data.
  A test or a scenario that ASSERTED `Unimplemented` for an unmocked RPC must
  name that RPC now (a scenario handler that throws
  `new ConnectError("…", Code.Unimplemented)`).
- In hybrid mode (`passthrough: true`), unmatched RPCs still go to the backend:
  defaults rank below passthrough.

### Optional: shrink hand-written handlers

A scenario handler that built a whole response by hand to make a page render
can start from the default and change only what the scenario is about:

```ts
import { mockService, patch } from "@reliantlabs/forge-web-runtime/mock-transport";

handlers: {
  ...mockService(ReportService, {
    listReports: (req, d) => patch(d.listReports(req), { reports: [] }),
  }),
},
```

`mockService` is typed by the service descriptor, so a vendored service's
handlers no longer need the untyped `"pkg.v1.Svc/Method"` escape hatch.

## 2. Native frontend foundation repairs

**Use when** the project has an Expo frontend or a shared hooks workspace package.
These repairs do not upgrade the Expo SDK or add native authentication, signing,
store publication or OTA support. Existing owned code is preserved by generation;
review the following adaptations rather than deleting customized files.

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
does not consume `PORT` itself. Existing apps should add the wrapper from the
current scaffold only when absent, reviewing any custom dev entrypoint. Preserve
`start`, `ios`, `android` and `web` scripts. Do not overwrite custom package scripts.

The native `build` script is an **Expo web static export** into `dist`, not an IPA
or Android package. Add it only if that is the app's declared output; keep custom
builds and do not mistake a successful Metro/web export for native device evidence.
For SDK 52's advertised web output, declare `react-dom: "18.3.1"` directly when
absent, matching the SDK's React renderer matrix; review customized existing
versions rather than overwriting them. Reinstall the frontend lockfile.

The old runtime range `^0.2.0` cannot resolve from the observed public registry.
Generation reconciles the dependency with Forge's supported published range;
review the package.json/lockfile diff and reinstall with the project's package
manager. Keep compatible newer custom ranges. SDK 52 remains unchanged by these
repairs; SDK migration requires its own validated dependency matrix.

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

### Portable runtime imports and publication boundary

Portable error type imports now use the existing `/interceptors` entry point.
The runtime source also adds `userMessage` there. Shared templates intentionally
retain their two existing root value imports until that additive export is
published and the consumer dependency floor is raised. Do not manually switch
those value imports while using an older runtime that lacks the export. This
publication gate is not evidence that native bundle isolation is already complete.

### Acceptance for existing apps

Run app and shared-package typechecks and configured tests. Exercise import before
transport registration, subsequent transport replacement, allocated Expo port,
non-default native config, and any advertised static build. Preserve owned edits
across two generations. Actual native auth/streaming/device checks remain separate
foundation gates; these repairs alone do not establish production readiness.
