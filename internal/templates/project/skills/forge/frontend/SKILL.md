---
name: frontend
description: Write Next.js frontends — generated hooks, the component library, Tailwind v4, the output/base_path build shapes, visual verification, and Connect RPC clients.
---

# Frontend Development in Forge

## Project structure

Each frontend lives in `frontends/<name>/` as a Next.js App Router app (`forge scaffold frontend <name>`): `src/app/`, `src/components/`, `src/hooks/`, `src/lib/`. Generated TypeScript lives in this frontend's own `src/gen/` — import it as `@/gen/...`, never from the project-root `gen/` (that tree is Go).

`--kind` also takes `mobile` (Expo React Native: `app/` screens, the same hooks/lib/stores adapted — the event bus gains `app:background`/`app:foreground`, the UI store `drawerOpen`/`bottomSheetOpen`) and `vite-spa` (`--output`/`--base-path` are web-only).

## Build & serving shapes

`forge.yaml`'s `output:` picks the production shape — `standalone` (the default
Node sidecar), `static` (export to a CDN), or `server` (full Next.js). `static`
is **incompatible with generated CRUD pages**, whose `/<entity>/[id]` routes are
dynamic. `base_path: /admin` mounts a frontend under a URL prefix, and hand-built
URLs then have to go through `joinBasePath` from `src/lib/basepath_gen.ts`.
Both, with the footguns, are in `frontend/serving`.

## Generated TypeScript hooks

`forge generate` produces per-service React Query hooks in `src/hooks/` — read RPCs get `useQuery`, mutating RPCs `useMutation`. Import from the barrel: `import { useGetTask, useListTasks } from "@/hooks"`. Base wrappers `useApiQuery` / `useApiMutation` cover composite operations.

## The component library

**Search it before you hand-write any UI.** 74 components ship across layouts, charts, diagrams, deck, and ui:

```bash
forge component search card      # by keyword — try the noun you were about to build
forge component list             # the whole catalog, grouped by category
forge component install card_grid
```

**What is already in `src/components/ui/` is not the catalog** — it is the 31 the scaffold auto-installs. The rest arrive only via `forge component install`. Reading the directory and concluding the library is exhausted is the failure mode; search instead.

**Every component already uses this project's theme tokens** (`ink`, `surface`, `border`, `accent`, `danger`, `success`, `warning` and their `-surface`/`-border`/`-ink` variants), so installing one never means re-theming it. A raw palette class like `bg-blue-600` in a component is a forge bug to report — not a reason to hand-write your own.

Reach for these before writing markup yourself. **✓ already ships in `src/components/ui/`** — import it, do not rebuild it; the rest are one `forge component install` away:

| About to build | Use |
|---|---|
| A stat / KPI block | ✓ `stat_grid`, ✓ `metric_card` |
| "No results yet" copy | ✓ `empty_state` |
| A destructive-action prompt | ✓ `confirmation_dialog` |
| A sortable/dense table | ✓ `data_table` |
| A filter/search bar for a list page | ✓ `filter_bar` |
| A board, timeline, tile grid | `kanban_board`, `timeline`, `card_grid` |
| A conversion / stage funnel | `funnel_chart` |
| A dropdown, breadcrumb, toggle | `dropdown_menu`, `breadcrumb`, `toggle_switch` |

A search that returns nothing is a real answer: there is no drawer/sheet or quantity-stepper, so those you write yourself. (The `component_library` MCP tool is the same catalog.)

**Data viz: use Recharts, not the library.** For commodity charts (bar/line/area/donut/pie/scatter/sparkline) `npm i recharts`. The library ships only **narrative** charts (`quadrant_chart`, `concentric_circles`, `funnel_chart`) and `slide_*` deck charts. Dashboard → Recharts; slide / marketing page → check the library first.

### Base UI primitives (always available)

Every frontend ships low-level primitives under `src/components/ui/`. Compose these instead of inlining `<button>`/`<input>`/`<table>` markup:

| Primitive | What it is |
|-----------|-----------|
| `button` | `primary`/`secondary`/`outline`/`ghost`/`danger` variants, sizes, loading state |
| `input`, `label`, `form` | Text input, field label, and `Form`/`FormField`/`FormError`/`FormActions`. `<FormField>` mints an id via `useId()`, so child `<Label>`/`<Input>`/`<Select>` auto-bind without `htmlFor`/`id` |
| `card` | Generic surface primitive |
| `avatar`, `tabs`, `chip` | Avatar (image/initials/status), tab nav (underline/pills/boxed), removable filter chip. `Tabs`/`FilterBar`: pass `activeTab`/`values` to control them from the URL |
| `table` | Bare structural table — markup only. For a wired list view use `<Resource>` (see `frontend-runtime`) |
| `select` | Options array, sizes, invalid state |
| `toast_notification` | The toast STACK (default export), rendered once by the owned `providers.tsx`. Never call it directly — fire `emitToast({ message, variant })` from `@/lib/events` |

Plus auto-installed: `sidebar_layout`, `page_header`, `badge`, `modal`, `skeleton_loader`, `pagination`, `search_input`, `alert_banner`, `key_value_list`, `login_form`. All `overwrite: once` — yours after install.

Two carry aliases; write new code against canonical. **`Badge`** — `error`/`success`/`warning`/`info`/`neutral`, aliases `danger→error`, `default→neutral`. **`Modal`** — canonical footer is the **`footer` slot prop**; rewrite footer-in-`children` when you next touch it. (`Button`'s destructive variant is `danger`, not `error`.)

### Owned higher-level components

Owned scaffold (not `ui/`), composing the base primitives — yours to edit or delete:

- **`status-badge.tsx`** / **`enum-select.tsx`** — proto-enum badge + select. See **Formatting** below for the enum-object rule and status colors.
- **`entity-picker.tsx`** / **`entity-name.tsx`** — the foreign-key pair: searchable select over a generated LIST hook, id→display-name over a GET hook. Never hand-roll a per-entity picker. See `frontend/pages`.
- **`src/components/session_nav.tsx`** (Next.js) — signed-in user + sign-out, on `useAuth()`. No login form or `/login` route ships; sign-in is your IdP's. See `auth`.

Typed list views are NOT owned scaffold: the generated CRUD pages import `<Resource>` from `@reliantlabs/forge-web-runtime`, which owns the loading/error/empty/data ladder and cursor pagination (`frontend-runtime`).

## Formatting: `@/lib/format-utils`

Scaffolded into every frontend. **Import these — never re-derive them per feature.**

| Need | Export |
|---|---|
| Money (`int64` cents) — exact | `formatMoneyCents(cents)` → `$272,000.00` |
| Money — headline, no cents | `formatMoneyWhole(cents)` → `$272,000` |
| Recurring price | `formatMoneyInterval(cents, "month")` → `$29.00/mo` |
| Proto `Timestamp` → `Date` | `timestampToDate(ts)` — null when unset/out of range |
| Date, date+time | `formatDate(ts)`, `formatDateTime(ts)` |
| Elapsed time | `formatAge(ts)` → `"3 days ago"` |
| Enum → label | `enumLabel(value, EnumObject)` → `"Inspection Scheduled"` |
| Enum → `<select>` options | `enumOptions(EnumObject)` |
| Any column value | `formatValue(v)` |

Unset renders `—` throughout. Pass the enum **object**, never its type name — protobuf-es enums are runtime numbers and only the object reverse-maps.

**Status colors:** `<StatusBadge value={x.status} enumType={X}>` resolves a variant from a built-in generic-lifecycle map (`active`, `paid`, `failed`, …). Declare your product's own words once at module scope — never edit the built-in map or write a per-feature color record:

```ts
registerStatusVariants({ weather_hold: "warning", emergency: "error" });
```

An unregistered status renders neutral, never a guessed color.

## Connect RPC clients + mock mode

Import the generated transport from `src/lib/connect.ts`; for direct calls `createClient(MyService, transport)`. The scaffold talks to the REAL backend out of the box (`connect.ts` reads `NEXT_PUBLIC_API_URL` or the generate-maintained dev port); start it with `forge env up dev`. Mock mode is **opt-in** via `.env.local`:

| `NEXT_PUBLIC_MOCK_API` | Behavior |
|---|---|
| unset (default) | Real backend; RPC failures surface as real errors. |
| `true` | Mock transport only — the layout renders a persistent "MOCK DATA — backend not connected" banner. |
| `hybrid` | `?scenario=` overlays on a real transport. |

**Never remove the mock banner from the layout** — it stops a working-looking UI masquerading as a working stack.

### Where the mock pipeline lives

The dispatch ENGINE is library code — `@reliantlabs/forge-web-runtime/mock-transport`, imported through that subpath and never the barrel, so a production bundle can shake it out. Your project keeps only a declarative table:

- **`src/lib/mock-transport_gen.ts`** (Tier-1) — one `MockEntityDescriptor` per entity: service name, primary-key field, fixture module, entity schema, and each CRUD RPC's response schema. Regenerated from your protos every run.
- **`src/mocks/<entity>_gen.ts`** — the deterministic fixtures, the same rows `forge db seed apply` writes.
- **`src/mocks/scenarios/*.ts`** — your scenarios; forge only regenerates the `index_gen.ts` barrel.

Dispatch order: scenario handler → hybrid passthrough → entity fixtures → `Unimplemented`. A Get miss is a real `NotFound`, never a silent wrong record. Writes round-trip within one browser session and reset on reload.

To stub an RPC, run `forge scaffold scenario <name>` and add a typed handler — do NOT edit `mock-transport_gen.ts`. Activate with `?scenario=<name>`; it is read once at module init, so client-side navigation keeps it active until a full reload.

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

Use `@theme`/scoped CSS variables for reusable tokens rather than one-off colors, and keep global CSS small. `npm run lint:styles` catches `!important` and invalid v4 at-rules. Full rules in `frontend/patterns`.

## Visual verification

**Use BOTH `take_snapshot` AND `take_screenshot` (Chrome DevTools) before declaring frontend work complete.** The a11y-tree snapshot cannot detect layout shifts, wrong colors, z-index or overflow; only screenshots catch those. Screenshot at multiple breakpoints. `frontend/design` carries the overflow probe.

## Component patterns

Functional components with hooks only. Prefer **server components**; add `"use client"` only for interactivity, browser APIs, or hooks. Handle Connect errors by code (`err instanceof ConnectError`, switch on `err.code`). Every data-fetching component must handle **loading**, **success**, and **error**. Composition and effects discipline are in `frontend/patterns`.

## Files NOT to edit

Regenerated by `forge generate` — edits overwritten: `src/gen/`, `src/lib/connect.ts`, `src/lib/basepath_gen.ts`, `src/hooks/*-hooks.ts`. Put custom code in separate files (`src/hooks/custom-hooks.ts`).

## Files that are starter code (rewrite expected)

Everything carrying the `// yours: scaffolded once` banner is written once and left alone — `src/app/page.tsx`, `src/app/auth/sign-in/page.tsx`, and every generated list/detail/create/edit page under `src/app/<entity>/`. `src/components/nav.tsx` and `src/app/dashboard.tsx` stay current until your first edit to either.

None of it is a design — it is Rails-style scaffolding built from the entity list, enough to prove hooks, transport and auth are wired. Replacing it is expected: real copy, a hierarchy built around what the product does, and chosen empty/loading/error states.

Load `frontend/design` before building the real thing; it asks for a brief first and will not proceed on taste alone.

## Scaffolded systems (yours to extend)

Created by `forge scaffold frontend`, yours to modify:

- **Auth provider** (`src/lib/auth/`) — DI'd via `AuthProvider`; implement the interface to add real auth. `useAuth()` gives user/token/login/logout.
- **Event bus** (`src/lib/events.ts` + `src/lib/event-context.tsx`) — typed pub/sub for imperative cross-cutting actions (`toast:show`, `auth:expired`, `navigate`). Extend `EventMap`; use `useEvent(name, handler)`. Not a source of truth.
- **UI store** (`src/stores/ui-store.ts`) — Zustand baseline for client state (`sidebarCollapsed`, `commandPaletteOpen`). Add domain stores in `src/stores/`. **Subscribe to slices, not the whole store.** Server data stays in the React Query hooks, never copied into Zustand.
- **`src/lib/format-utils.ts`** — presentation formatting. Import from `@/lib/format-utils`; do not re-derive these per feature (see below).
- Also scaffolded: `src/lib/admin-url.ts` (`adminUrl`/`absoluteAdminUrl` over `basepath_gen.ts` — use these or `joinBasePath` for any string handed to an external system that round-trips back).

## Dev workflow

```bash
forge env up dev   # Full stack: infra + Go (hot reload) + Next.js; reads deploy/kcl/<env>/
forge generate     # After any .proto change
```

`forge env up` dev-serves each declared frontend via `npm run dev` (`npm run build` is for `forge build` / `forge env deploy`). Each serves on its own KCL-declared port (`forge.Frontend.port`), **force-injected as `PORT`** into the child, so the browser URL matches the declaration even if a stale `PORT` bled in from the shell. There is no dev reverse proxy. To route a service under the prod Gateway, declare an `HTTPRoute` with a `host:` in `deploy/kcl/<env>/main.k`.

## File naming inside `frontends/<name>/src/`

- **Components under `src/components/ui/`** are `snake_case` (`data_table.tsx`, `toast_notification.tsx`), each default-exporting one PascalCase component.
- **Hooks, lib utilities, stores** are `kebab-case` (`use-api-query.ts`, `ui-store.ts`).

For the full Go / proto / TS / `forge.yaml` casing table, see `architecture` → **Naming conventions**.

## Sub-skills & rules

Load **state** (decision table, state vs events, ownership), **patterns** (composition, container/presentational, effects, typed boundaries), and `frontend-testing` (what to test, the `mockTransport()` seam, per-layer recipes).

- Never hand-edit generated files; run `forge generate` after every `.proto` change.
- Always `create(Schema, {...})` for protobuf messages, never `new Message()`.
- `"use client"` only when needed; verify visually with BOTH `take_snapshot` and `take_screenshot`.
- `forge component search <noun>` before hand-writing any UI — `src/components/ui/` holds only the auto-installed core, not the catalog. Recharts for dashboard charts.
- Money, dates, enum labels and status colors come from `@/lib/format-utils`. Re-deriving one per feature is how three modules end up disagreeing.
- React Query hooks for server data, Zustand for client state only (subscribe to slices), event bus for imperative actions — never as a source of truth.
- Keep forms in react-hook-form + Zod. Never remove the mock-mode banner.
