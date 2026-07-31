---
name: frontend
description: Write Next.js frontends — generated hooks, the component library, Tailwind v4, the output/base_path build shapes, visual verification, and Connect RPC clients.
---

# Frontend Development in Forge

## Project structure

Each frontend lives in `frontends/<name>/` as a Next.js App Router app (`forge scaffold frontend <name>`): `src/app/` (pages/layouts), `src/components/`, `src/hooks/` (generated + custom), `src/lib/` (utilities + Connect client). Generated TypeScript lives in this frontend's own `src/gen/` — import it as `@/gen/...`, never from the project-root `gen/` (that tree is Go). `--kind` also takes `mobile` (an Expo React Native app: `app/` screens, the same hooks/lib/stores systems adapted — the event bus gains `app:background`/`app:foreground`, the UI store `drawerOpen`/`bottomSheetOpen`) and `vite-spa` (a plain Vite SPA; `--output`/`--base-path` are web-only).

## Production build shape (`output:`)

`forge scaffold frontend` emits a `next.config.ts` captured in `forge.yaml` as `output:`. Three values:

| `output:`    | Production shape | Use when |
| ------------ | ---------------- | -------- |
| `standalone` | Node sidecar (`output: "standalone"`) | **The default.** Pairs with the shipped Dockerfile; supports the generated dynamic CRUD routes, server components/actions, request-time `redirect()`/`cookies()`. |
| `static`     | Static export (`output: "export"`) | Pure UI shell with NO dynamic routes — drop `out/` on a CDN. |
| `server`     | Full Next.js (no `output:`) | Custom server, ISR, managed host (Vercel). |

Opt in at scaffold time: `forge scaffold frontend dashboard --output static`.

**FOOTGUN: `static` is incompatible with generated CRUD pages.** `output: "export"` requires `generateStaticParams()` on every dynamic segment, and the generated detail/edit pages (`/<entity>/[id]`) are dynamic client routes whose ids only exist at runtime — `npm run build` fails on any project with a CRUD entity. Server-runtime APIs (`redirect()` from `next/navigation`, `cookies()`, server actions) also require `standalone` or `server`; for a root redirect under static export, use a client component with `useRouter().replace()` in a `useEffect`, not `redirect()`.

**Build dirs are fenced.** In `standalone`/`server`, production builds write to `.next-prod` (a `distDir` conditional) while `next dev` keeps `.next` — so `npm run build` during a live `forge env up` dev session can't clobber the dev cache. `next start` and the Dockerfile read `.next-prod`. `static` keeps Next.js defaults, so avoid production builds during a live dev session in that mode. `output:` takes effect only at scaffold time (`next.config.ts` is yours to edit after).

## Serving under a path prefix (`base_path`)

To mount a frontend under a URL prefix, declare `base_path: /admin` in `forge.yaml` (or `--base-path /admin`; must start with `/`, no trailing `/`). What it drives:

- `next.config.ts` sets **both `basePath` AND `assetPrefix`** to the same value — `assetPrefix` is required or some RSC chunk URLs skip the prefix and React never hydrates.
- **ONE env var**: `NEXT_PUBLIC_BASE_PATH` is the only override forge reads or writes. Never invent a second variant (`ADMIN_WEB_BASE_PATH` etc.) — it is silently ignored.
- `src/lib/basepath_gen.ts` (regenerated) exports `BASE_PATH` + `joinBasePath(path)`.
- Static-export builds **fail loudly** if `NEXT_PUBLIC_BASE_PATH` is emptied while `forge.yaml` declares a prefix (a root-mounted export 404s behind the proxy).

Internal navigation (`<Link href="/tasks">`, `router.push("/tasks")`) keeps app-relative paths — Next.js prepends the basePath automatically; do NOT wrap these in `joinBasePath`. Hand-built URLs Next.js can't see — `window.location.origin`-based return URLs, OAuth `redirect_uri`, share links, raw `fetch()`/`<a>` paths — MUST go through `joinBasePath`:

```typescript
import { joinBasePath } from "@/lib/basepath_gen";
const successUrl = window.location.origin + joinBasePath("/billing/success");
```

Anti-patterns lint catches: bare `"/admin" + path` literals, bare `/route` strings in hand-built URLs, and reading any env var other than `NEXT_PUBLIC_BASE_PATH`.

## Generated TypeScript hooks

`forge generate` produces per-service React Query hooks in `src/hooks/` — read RPCs get `useQuery`, mutating RPCs get `useMutation`. Import from the barrel: `import { useGetTask, useCreateTask, useListTasks } from "@/hooks"`. Base wrappers `useApiQuery` / `useApiMutation` cover one-off or composite operations.

## The component library

Find production-ready components before building from scratch: `component_library(action="search", query="dashboard")`, `component_library(action="get", name="quadrant_chart")`. Categories: layouts, charts, diagrams, deck, ui.

**Data viz: use Recharts, not the library.** For commodity charts (bar/line/area/donut/pie/scatter/sparkline) `npm i recharts` — hand-rolled SVG loses on every dimension that matters (tooltips, brush, datetime/log axes, locale formatting, canvas past ~1k points, a11y). The component library only ships **narrative** charts (`quadrant_chart`, `concentric_circles`, `funnel_chart`) and `slide_*` deck charts. Rule: dashboard → Recharts; slide / marketing page → check the library first.

### Base UI primitives (always available)

Every frontend ships low-level primitives under `src/components/ui/`. Pages and higher-level components MUST compose these instead of inlining `<button>`/`<input>`/`<table>` markup:

| Primitive | What it is |
|-----------|-----------|
| `button` | `primary`/`secondary`/`outline`/`ghost`/`danger` variants, sizes, loading state |
| `input`, `label`, `form` | Text input, field label, and `Form`/`FormField`/`FormError`/`FormActions` — `<FormField>` mints an id via `useId()` and exposes it via `FormFieldContext` so child `<Label>`/`<Input>`/`<Select>` auto-bind without `htmlFor`/`id` boilerplate |
| `card` | Generic surface primitive |
| `avatar`, `tabs`, `chip` | Avatar (image/initials/status), tab nav (underline/pills/boxed), removable filter chip. `Tabs`/`FilterBar`: pass `activeTab`/`values` to control them from the URL |
| `table` | Bare structural table — markup only. For a wired list view use `<Resource>` (see `frontend-runtime`) |
| `select` | Options array, sizes, invalid state |
| `toast_notification` | The toast STACK (default export), rendered once by the owned `providers.tsx`. Never call it directly — fire `emitToast({ message, variant })` from `@/lib/events` |

Plus higher-level scaffolded components: `sidebar_layout`, `page_header`, `badge`, `modal`, `skeleton_loader`, `pagination`, `search_input`, `alert_banner`, `key_value_list`, `login_form`. All are `overwrite: once` — yours to edit after install.

Two have canonical APIs plus source-port aliases (write new code against canonical): **`Badge`** — canonical `error`/`success`/`warning`/`info`/`neutral`; aliases `danger→error`, `default→neutral`. **`Modal`** — canonical footer is the **`footer` slot prop**; footer-in-`children` is accepted source-port shorthand, rewrite to the slot when you next touch it. (`Button`'s destructive variant is `danger`, not `error` — Button is action-shaped, Badge is status-shaped.)

### Owned higher-level components

Owned scaffold (not `ui/`), composing the base primitives — yours to edit or delete:

- **`status-badge.tsx`** / **`enum-select.tsx`** — proto-enum badge + select. Pass the enum OBJECT (`<StatusBadge value={order.status} enumType={OrderStatus} />`), never its type NAME: the field is a runtime NUMBER and only the object reverse-maps it.
- **`entity-picker.tsx`** / **`entity-name.tsx`** — the foreign-key pair: searchable single-select over a generated LIST hook, id→display-name over a generated GET hook. Never hand-roll a per-entity picker. See `frontend/pages`.
- **`src/components/session_nav.tsx`** (Next.js) — signed-in user + sign-out, on `useAuth()`. No login form or `/login` route ships; sign-in is your IdP's. See `auth`.

Typed list views are NOT owned scaffold: the generated CRUD pages import `<Resource>` from `@reliant-labs/web-runtime`, which owns the loading/error/empty/data ladder and cursor pagination. See `frontend-runtime`.

## Connect RPC clients + mock mode

Import the generated transport from `src/lib/connect.ts`; for direct calls `createClient(MyService, transport)`. The scaffold talks to the REAL backend out of the box (`src/lib/connect.ts` reads `NEXT_PUBLIC_API_URL` or the `forge generate`-maintained dev port); start it with `forge env up dev`. Mock mode is **opt-in** via `.env.local`:

| `NEXT_PUBLIC_MOCK_API` | Behavior |
|---|---|
| unset (default) | Real backend; RPC failures surface as real errors. |
| `true` | Mock transport only — the layout renders a persistent "MOCK DATA — backend not connected" banner. |
| `hybrid` | `?scenario=` overlays on a real transport. |

**Never remove the mock banner from the layout** — it prevents a working-looking UI masquerading as a working stack.

## Protobuf-ES v2

Forge uses protobuf-es v2 — create message instances with `create()`, never constructors:

```typescript
import { create } from "@bufbuild/protobuf";
import { CreateTaskRequestSchema } from "@/gen/services/tasks/v1/tasks_pb";
const req = create(CreateTaskRequestSchema, { name: "My Task" }); // NOT new CreateTaskRequest({...}) — that's v1
```

## Styling: Tailwind v4

**Tailwind CSS v4** — no `tailwind.config.js`; configure in CSS with `@theme`; import with `@import "tailwindcss"` (not `@tailwind base/...`); the PostCSS plugin is `@tailwindcss/postcss`.

```css
/* src/app/globals.css */
@import "tailwindcss";
@theme { --color-brand: #3b82f6; --font-sans: "Inter", sans-serif; }
```

Treat CSS as architecture: prefer Tailwind utilities + component variants; `@theme`/scoped CSS variables for reusable tokens (don't hard-code one-off colors); avoid `!important` (simplify selectors or add a variant API); avoid DOM `style={{...}}` except for truly dynamic runtime values (measured dimensions, chart coordinates); keep global CSS small. `npm run lint:styles` catches `!important` and invalid v4 at-rules.

## Visual verification

**ALWAYS use BOTH `take_snapshot` AND `take_screenshot` (Chrome DevTools) before declaring frontend work complete.** Snapshots (a11y tree) cannot detect CSS/visual issues — layout shifts, wrong colors, z-index, overflow; only screenshots catch those. For responsive testing, resize the page and screenshot at multiple breakpoints.

## Component patterns

Functional components with hooks only — no class components. Prefer **server components**; add `"use client"` only for interactivity, browser APIs, or hooks (`useState`/`useEffect`). Keep components small; extract reusable logic into custom hooks. Handle Connect RPC errors by code (`err instanceof ConnectError`, switch on `err.code` — `Code.InvalidArgument`, `Code.PermissionDenied`, else generic). Every data-fetching component must handle **loading**, **success**, and **error**.

## Files NOT to edit

Regenerated by `forge generate` — changes overwritten: `src/gen/`, `src/lib/connect.ts`, `src/lib/basepath_gen.ts`, `src/hooks/*-hooks.ts`. Put custom code in separate files (`src/hooks/custom-hooks.ts`, `src/lib/utils.ts`).

## Scaffolded systems (yours to extend)

Created by `forge scaffold frontend`, yours to modify:

- **Auth provider** (`src/lib/auth/`) — DI'd via `AuthProvider`; implement the interface to add real auth (Auth0/Clerk/custom JWT). `useAuth()` gives user/token/login/logout.
- **Event bus** (`src/lib/events.ts` + `src/lib/event-context.tsx`) — typed pub/sub for imperative cross-cutting actions (`toast:show`, `auth:expired`, `navigate`). Extend the `EventMap`; use `useEvent(name, handler)`. Not a source of truth.
- **UI store** (`src/stores/ui-store.ts`) — Zustand baseline for shared client state (`sidebarCollapsed`, `commandPaletteOpen`). Extend or create domain stores in `src/stores/`. **Subscribe to slices, not the whole store.** Use generated React Query hooks for server data — do NOT copy backend data into Zustand.
- Also scaffolded: `src/lib/format-utils.ts`, and `src/lib/admin-url.ts` (`adminUrl`/`absoluteAdminUrl` over `basepath_gen.ts` — use these or `joinBasePath` for any string handed to an external system that round-trips back).

## Dev workflow

```bash
forge env up dev   # Full stack: infra + Go (hot reload) + Next.js; reads deploy/kcl/<env>/
forge generate     # After any .proto change
```

`forge env up` dev-serves each declared frontend via `npm run dev` (NOT `npm run build` — that is for `forge build` / `forge env deploy`). Each serves on its own KCL-declared port (`forge.Frontend.port`), **force-injected as `PORT`** into the Next.js child, so the browser URL matches the declaration even if a stale `PORT` bled in from the shell. There is no dev reverse proxy. To route a service under the prod Gateway too, declare an `HTTPRoute` with a `host:` in `deploy/kcl/<env>/main.k`.

## File naming inside `frontends/<name>/src/`

- **Components under `src/components/ui/`** are `snake_case` (`data_table.tsx`, `toast_notification.tsx`), each default-exporting one PascalCase component.
- **Hooks, lib utilities, stores** are `kebab-case` (`use-api-query.ts`, `ui-store.ts`).

For the full Go / proto / TS / `forge.yaml` casing table, see `architecture` → **Naming conventions**.

## Sub-skills & rules

Load **state** (decision table, state vs events, ownership), **patterns** (composition, container/presentational, effects, typed boundaries), and `frontend-testing` (what to test, the `mockTransport()` seam, per-layer recipes).

- Never hand-edit generated files; run `forge generate` after every `.proto` change.
- Always `create(Schema, {...})` for protobuf messages, never `new Message()`.
- `"use client"` only when needed; verify visually with BOTH `take_snapshot` and `take_screenshot`.
- `component_library` before building UI from scratch; Recharts for dashboard charts.
- React Query hooks for server data, Zustand for client state only (subscribe to slices), event bus for imperative actions — never as a source of truth.
- Keep forms in react-hook-form + Zod. Never remove the mock-mode banner.
