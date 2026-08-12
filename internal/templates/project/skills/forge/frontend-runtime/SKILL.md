---
name: frontend-runtime
description: The forge frontend runtime, @reliant-labs/web-runtime — the web twin of forge/pkg. Transport interceptor stack (auth, brand, W3C traceparent, error normalization, retry), app-shell providers (session, error boundary, toast host), the generic <Resource> data-table container, and client telemetry.
---

# Frontend Runtime

Every generated web frontend — Next.js and Vite SPA alike — is built on
**`@reliant-labs/web-runtime`**, the frontend analog of `forge/pkg`. It is a
set of pull-out-of-the-box batteries the app wires through thin, owned
composition.

## Ownership

The runtime is a **package**, not project code: there is nothing to edit and
nothing to regenerate. Fixes to it (e.g. tracing behavior) arrive with a
version bump.

You compose it from **your** owned files:

- `src/lib/connect.ts` — builds the transport interceptors from the runtime.
- `src/app/providers.tsx` — mounts `RuntimeShell` and inits telemetry.

Import everything from the barrel:
`import { … } from "@reliant-labs/web-runtime"`.

Three subpaths are deliberately OUTSIDE the barrel, each for its own reason:

| Subpath | Why it is not in the barrel |
|---|---|
| `/interceptors` | DOM-free transport layer, for React Native / non-react-dom renderers. |
| `/mock-transport` | Dev-only. The fixture dispatch engine must be tree-shakeable out of a production bundle. |
| `/otel` | Pulls the eight `@opentelemetry/*` SDK packages, which only the Next.js scaffold installs — they are OPTIONAL peer deps. A Vite-SPA or React-Native frontend declares `@opentelemetry/api` alone and imports the barrel. |

Nothing you write imports `/mock-transport` or `/otel` directly: forge generates
the thin project files (`src/lib/mock-transport_gen.ts`, `src/lib/otel_gen.ts`)
that do, carrying the per-project table and the `NEXT_PUBLIC_*` env reads
respectively.

Presentation stays yours and is deliberately NOT in the package: the component
library under `src/components/ui`, `globals.css`, the nav and the layout are
all scaffolded once for you to fork.

Tailwind v4 does not scan `node_modules`, so your stylesheet carries an
`@source` directive pointing at the package. Keep it — without it every
utility only the runtime renders vanishes from the built CSS.

## 1. Transport interceptor stack

`connect.ts` builds its Connect interceptors from the runtime:

```ts
import { buildRuntimeInterceptors } from "@reliant-labs/web-runtime";

const interceptors = buildRuntimeInterceptors({
  getToken: () => _getToken(),      // bearer token (wired by AuthTokenBridge)
  getBrand: () => currentBrand(),   // optional: X-Brand header
  onError: (e) => report(e),        // optional: typed-error sink
});
```

The chain (outermost → innermost): **retry → error-normalize → auth →
brand headers → traceparent**. Every attempt re-runs the whole chain
(fresh token, fresh traceparent).

### Full-stack tracing (always on)

`traceInterceptor` attaches a valid **W3C `traceparent`** to every RPC — with
**no OTLP collector required**. The forge backend runs `otelconnect` +
`otelhttp` with a `TraceContext` propagator, so browser→backend calls **join
the same distributed trace**. If you additionally configure OpenTelemetry
export (`NEXT_PUBLIC_OTEL_ENDPOINT`, see `src/lib/otel_gen.ts`), the active browser
span's context is propagated instead, stitching the browser and server spans
into one trace.

### Typed errors

Failures are normalized to `ConnectClientError`:

| field | what it is |
| --- | --- |
| `.reason` | **stable machine code** the backend stamps on every error (`"not_found"`, `"duplicate"`, `"reference_in_use"`, …), or `null` |
| `.code` | canonical Connect code name (`"not_found"`, `"unavailable"`, …) |
| `.status` | approximate HTTP status, for telemetry and generic UI |
| `.retryable` | true for transient failures worth a transparent retry |

Retryable transient failures (unavailable, deadline-exceeded, aborted,
resource-exhausted) are retried with exponential backoff + jitter (unary only
— streams are never retried).

```ts
import {
  ConnectClientError,
  FORGE_ERROR_REASON_HEADER, // "x-forge-error-reason" — the metadata key .reason is read from
  normalizeError,
  userMessage,
} from "@reliant-labs/web-runtime";
```

**The rule: branch on `.reason` (or `.code`), render with `userMessage(err)`,
never match on message text.** Message prose is display-only — it is written
for humans, it gets reworded, and it is localized eventually; a `switch` on it
is a bug that ships green.

```tsx
const create = useCreateItem();               // hooks type their error as ConnectClientError
// …
if (create.error) {
  switch (create.error.reason) {
    case "duplicate":         return <FieldError name="sku">That SKU is taken.</FieldError>;
    case "reference_missing": return <Banner>Pick a category that still exists.</Banner>;
    default:                  return <Banner>{userMessage(create.error)}</Banner>;
  }
}
```

`.reason` is genuinely end-to-end and does not depend on a handler
remembering to set it: `forge/pkg/crud` — which produces most of a generated
app's errors — routes every failure through one exit that cannot construct an
error without a reason, and `CORSMiddleware` exposes the header
cross-origin. The vocabulary is **total** (`not_found`, `duplicate`,
`reference_missing`, `reference_in_use`, `required_field_missing`,
`constraint_violated`, `invalid_format`, `unknown_field`,
`invalid_page_token`, `invalid_order_by`, `page_token_order_conflict`,
`internal`), so a `switch` never falls through to `null` and back to sniffing
prose. Your own handlers join it with
`svcerr.Wrap(svcerr.WithReason(err, "no_active_subscription"))`.

`userMessage(err)` is the only correct way to SHOW an error. It strips the
transport framing that must never reach a user — the `[code]` prefix
connect-es adds — and falls back to generic copy when there is nothing
presentable. What remains is the message the backend wrote, shown verbatim.
Never render `err.message`.

Generated hooks (`use<Rpc>` in `src/hooks/`) declare their error as
`ConnectClientError`, so `.reason` is reachable at the call site with no cast.
Outside a hook — a raw transport call, an error boundary, a `catch` — run the
value through `normalizeError(err)` first.

## 2. App-shell providers, error boundary, toast host

The dependency points **app → runtime, one way**. The runtime never imports
`@/lib/auth/*`, `@/lib/event-context`, or `@/components/ui/*` — those are yours
to rename, restyle, or delete, and a forge-owned file may not depend on them.
Everything app-shaped is a **prop**, wired once in your `providers.tsx`:

```tsx
import { RuntimeShell } from "@reliant-labs/web-runtime";
import ToastNotification from "@/components/ui/toast_notification";
import { useAuth } from "@/lib/auth/context";
import { useEventBus } from "@/lib/event-context";

function RuntimeLayer({ children }: { children: React.ReactNode }) {
  const auth = useAuth();
  const bus = useEventBus();

  const subscribe = useCallback((sink: RuntimeToastSink) => {
    const offShow = bus.on("toast:show", (p) => sink.onShow(p));
    const offDismiss = bus.on("toast:dismiss", (p) => sink.onDismiss(p?.id));
    return () => { offShow(); offDismiss(); };
  }, [bus]);

  const render = useCallback<RuntimeToastHostProps["render"]>(
    ({ toasts, onDismiss }) => <ToastNotification toasts={toasts} onDismiss={onDismiss} />,
    [],
  );

  return (
    <RuntimeShell auth={auth} toast={{ subscribe, render }}>
      {children}
    </RuntimeShell>
  );
}
```

`RuntimeShellProps`: `auth` (required — any value shaped like `SessionAuth`:
`{ user, isAuthenticated, isLoading, getToken }`), `toast` (optional — omit and
no host is mounted), `onError` (optional — forwarded to the error boundary).

`RuntimeShell` supplies three batteries:

- **`SessionProvider` / `useSession()`** — the signed-in user, merged from the
  `auth` prop and the bearer token's JWT claims.
  ```ts
  const { userId, email, name, claims, isAuthenticated, isLoading } = useSession();
  ```
- **`RuntimeErrorBoundary`** — a React error boundary with a designed
  fallback. Next's `app/error.tsx` / `app/global-error.tsx` catch route
  crashes; use this to isolate a subtree so one crashing widget degrades to a
  small fallback instead of taking the route down:
  ```tsx
  import { RuntimeErrorBoundary } from "@reliant-labs/web-runtime";
  <RuntimeErrorBoundary compact><RiskyWidget /></RuntimeErrorBoundary>
  ```
- **`RuntimeToastHost`** — owns the toast QUEUE (ids, add, dismiss-one,
  dismiss-all) and nothing else. `subscribe` feeds it your bus events; `render`
  draws them with your own component. The QueryClient's mutation-error
  chokepoint already emits `toast:show`; this host is what makes those toasts
  actually appear.

### Route guarding (render-gate only)

```tsx
import { RouteGuard } from "@reliant-labs/web-runtime";

<RouteGuard loading={<Spinner />} fallback={<SignInPrompt />}>
  <AdminPanel />
</RouteGuard>
```

`RouteGuard` gates on **signed-in state only**: `loading` while the session
resolves, `fallback` when nobody is signed in, `children` otherwise. This is a
**render convenience, not a security boundary** — a client can always call the
RPC directly, so the handler's own checks are the only thing that actually
decides the outcome.

The runtime ends at identity. **Authorization is application code** — forge
does not generate it, and what a caller may do is yours to design and enforce.
`useSession().claims` hands you the decoded JWT; deciding what a claim entitles
someone to render is your call. See the `security-review` skill for the review
bar.

## 3. `<Resource>` — the data-table container

`<Resource>` encapsulates the loading / error / empty / data tristate ladder,
a debounced server-side filter, and cursor pagination in one owned-once
component. Pair it with `useQueryResource` and a generated list hook. Do not
hand-roll the tristate ladder, and do not client-side-filter a single page cap.

```tsx
import { Resource, type ResourceColumn } from "@reliant-labs/web-runtime";
import { useQueryResource } from "@/hooks/use-query-resource";

const columns: ResourceColumn<Item>[] = [
  { header: "Name", cell: (i) => i.name },
  { header: "Created", cell: (i) => fmtDate(i.createdAt) },
];

export function ItemsPage() {
  const [cursor, setCursor] = useState<string[]>([]);
  const [filter, setFilter] = useState("");
  const q = useQueryResource(useListItems({ pageToken: cursor.at(-1), filter }));
  return (
    <Resource
      title="Items"
      status={q.status}
      data={q.status === "success" ? q.data.items : undefined}
      error={q.status === "error" ? q.error : undefined}
      columns={columns}
      rowKey={(i) => i.id}
      filter={filter}
      onFilterChange={setFilter}
      onNextPage={() => setCursor((c) => [...c, nextToken])}
      onPrevPage={() => setCursor((c) => c.slice(0, -1))}
      hasNextPage={Boolean(nextToken)}
      hasPrevPage={cursor.length > 0}
    />
  );
}
```

## 4. Client telemetry (RUM)

`initClientTelemetry` captures unhandled errors + core Web Vitals (LCP/CLS/INP)
with native browser APIs — no extra dependency. Export is **opt-in**: events
go to an in-process sink (console in dev) unless you pass an `endpoint`.

```ts
import { initClientTelemetry } from "@reliant-labs/web-runtime";

initClientTelemetry({
  onEvent: (e) => myReporter(e),
  // endpoint: "https://collector.example.com/v1/events", // opt-in export
});
```

Distributed tracing (the traceparent on every RPC) is independent and always
on — it does **not** depend on this module.
