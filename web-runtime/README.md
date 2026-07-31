# @reliant-labs/web-runtime

The forge frontend runtime — the web twin of `forge/pkg`. Framework-agnostic
React/TypeScript mechanism shared by every frontend forge scaffolds (Next.js
App Router and Vite SPA alike).

```ts
import {
  buildRuntimeInterceptors, // Connect transport stack
  ConnectClientError,       // typed client errors
  RuntimeShell,             // session + error boundary + toast host
  useSession,               // the signed-in user
  RouteGuard,               // signed-in render gate
  Resource,                 // the list tristate + cursor pagination
  initClientTelemetry,      // opt-in client RUM
} from "@reliant-labs/web-runtime";
```

## What is in here, and what is deliberately not

**In:** mechanism. The interceptor stack (auth, brand headers, W3C
traceparent, error normalization, idempotency-gated retry), the session
derived from JWT claims, the error boundary, the toast queue, the
`<Resource>` tristate/pagination container, trace-context propagation, and
client RUM.

**Out:** presentation a project is meant to own. The component library under
`src/components/ui`, `globals.css`, the nav and the app layout all stay in the
scaffold, because forking them is the point.

The dependency runs one way. Nothing here imports application code — auth
state, event subscriptions and toast markup all arrive as **props**, so an app
can rename, restyle or delete its own files without breaking the runtime.

## What this package publishes

`dist/` — compiled ESM plus `.d.ts` — and nothing else. `src/` is not in the
tarball.

That is a contract, not a packaging detail. This package used to publish its
TypeScript sources: `types` pointed at `src/index.ts`, so **every consumer
typechecked this package's internals against its own React**. A React 18 app
(Expo SDK 52 pins 18.3) compiling sources written against `@types/react` 19
failed on files it never rendered —

```
providers.tsx: 'RuntimeErrorBoundary' cannot be used as a JSX component
resource.tsx:  Type '0n' is not assignable to type 'ReactNode'
```

— and no amount of version pinning fixes the shape of that: a package that
ships sources makes its own dependency versions its consumers' problem,
forever, for every consumer. Publishing declarations ends it. A consumer
typechecks against the `.d.ts`, which `skipLibCheck` does not re-check, and
whose `react` references resolve in the consumer's own program.

`src/published-surface.test.ts` is the gate: it fails if any entry point
points back into `src/`, or if `src` reappears in `files`.

### One `.d.ts` for React 18 and 19

`peerDependencies` declares `react: "^18.3.0 || ^19.0.0"`, and both halves are
real: forge's Next.js and Vite scaffolds are on 19, its Expo scaffold on 18.3.

The two `@types/react` majors disagree about where `JSX.Element` lives — 19
declares it inside the `react` module, 18.3 declares it globally and
re-exports it from `react/jsx-runtime`. An **inferred** component return type
is therefore emitted as `import("react").JSX.Element` when compiled against 19
and `import("react/jsx-runtime").JSX.Element` when compiled against 18, so the
published `.d.ts` would silently depend on whichever major happened to be
installed at build time.

So every exported component **declares `: ReactElement`**, a name both majors
export from `react`. With that, the emitted `.d.ts` is byte-for-byte identical
under either — the peer range is a fact about the artifact rather than a hope.
`src/published-surface.test.ts` fails on any `JSX.Element` reaching `dist/`,
and `npm run typecheck` compiles the package against **both** majors
(`tsconfig.react18.json` redirects `react` at an aliased `@types/react-18`).

### Peer dependencies

`react`, `@connectrpc/connect`, `@bufbuild/protobuf` and `@opentelemetry/api`
are peers, resolved from the consuming app. `next` deliberately is **not** — a
Vite SPA consumes this package identically.

Next.js consumers keep `transpilePackages: ["@reliant-labs/web-runtime"]`
(forge's generated `next.config.ts` sets it). The reason changed with the
build — it is no longer "these are `.tsx` files" but "these modules carry
`"use client"` and Next has to process them rather than treat the package as
an opaque external."

## The DOM-free interceptors entry point

```ts
import {
  buildRuntimeInterceptors, // the Connect transport stack
  ConnectClientError,       // what the stack throws
  setTraceSampled,          // traceInterceptor's knob
} from "@reliant-labs/web-runtime/interceptors";
```

The `./interceptors` subpath is the transport layer on its own: `src/errors.ts`
and `src/trace.ts` are the only modules it reaches. It imports **no React and
no `@types/react`**, so a consumer never typechecks components it does not
render.

That is what keeps the package honest on React Native. A mobile app renders
none of the components here — they emit `<table>`, `<input>`, `<div>` — so
reaching them would drag a bundle's worth of dead code and a React binding
onto a platform whose renderer is not react-dom. The subpath skips them
entirely. `src/interceptors.ts` is guarded by
`src/interceptors-entry.test.ts`, which walks the entry point's real import
graph and fails if React ever appears in it.

(This subpath was *also* the workaround for React 18 vs 19 type skew, back
when the package published its sources. That is no longer why it exists — see
"One `.d.ts` for React 18 and 19" above; the barrel typechecks fine from Expo
now. It stays because dead components in a mobile bundle is a separate and
still-real cost.)

React Native resolvers predate `exports`: Expo SDK 52 / React Native 0.76 runs
Metro with `unstable_enablePackageExports` off, and `expo/tsconfig.base` sets
`moduleResolution: node10`. Neither reads the `exports` map. The
`interceptors/package.json` directory shim is what they resolve instead — same
build output, reached the old way. Delete it and mobile breaks while web keeps
working.

## The `--color-*` contract

Several components render semantic Tailwind utilities. They are part of this
package's public contract: the consuming app's stylesheet MUST declare these
custom properties in its `@theme` block, and Tailwind v4 must be told to scan
this package — **it does not scan `node_modules`**:

```css
/* frontends/<name>/src/app/globals.css (Next.js) */
@source "../../node_modules/@reliant-labs/web-runtime";

/* frontends/<name>/src/index.css (Vite SPA) */
@source "../node_modules/@reliant-labs/web-runtime";
```

Without that directive every utility only this package uses is dropped from
the built CSS and the components render unstyled.

Required tokens:

| Token | Used for |
| --- | --- |
| `--color-surface` | table / panel background |
| `--color-surface-muted` | table head, row hover |
| `--color-border` | table and panel rules |
| `--color-ink` | primary text |
| `--color-ink-muted` | secondary text, empty states |
| `--color-ink-subtle` | placeholders, disabled controls |
| `--color-accent` | focus ring, active filter border |
| `--color-danger` | error text, destructive fill |
| `--color-danger-hover` | destructive fill hover |
| `--color-danger-surface` | error panel background |
| `--color-danger-border` | error panel border |
| `--color-danger-ink` | error text on a danger surface |
| `--color-on-danger` | text on a danger fill |

## Local development

A dev forge build writes an npm `file:` dependency on this directory into a
generated frontend's `package.json`, so npm creates and maintains the link
itself and edits here are picked up with nothing published. The path is
written relative or home-anchored, never absolute. A release build writes an
ordinary version range instead.

`forge project libraries` prints where the package resolved to and what every
module exports — read it instead of searching the disk.

### The live-edit loop

The bridge resolves `dist/`, so **run `npm run dev` here while you edit** —
`tsc --watch`, a few hundred milliseconds per incremental build. The linked
app's bundler is already watching this directory (Metro via `watchFolders`,
Next via `transpilePackages`, Vite because a linked dep is treated as source),
so a save still lands in the running app; it just arrives one compile later.

You do not have to remember to build before the *first* install. `prepare`
(`scripts/build.mjs`) runs when npm materialises the `file:` link and again
before `npm publish` — the two moments a consumable `dist/` has to exist. On a
checkout that has never been developed it installs this package's own
devDependencies once so `tsc` is there at all, because npm does not install a
link target's devDependencies and forge's CI never runs `npm install` here.
The script never runs for a registry install: the tarball already has `dist/`.

`npm test` builds first (`pretest`), so `src/published-surface.test.ts` always
asserts against a current artifact rather than whatever was last on disk.
