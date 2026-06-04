---
name: frontend
description: Write Next.js frontends — generated hooks, component library, Tailwind v4, visual verification, and Connect RPC clients.
---

# Frontend Development in Forge

## Project Structure

Each frontend lives in `frontends/<name>/` as a standalone Next.js app with App Router. Create one with:

```bash
forge add frontend <name>
```

Key directories inside `frontends/<name>/`:
- `src/app/` — Next.js App Router pages and layouts
- `src/components/` — Reusable React components
- `src/hooks/` — Generated and custom hooks
- `src/lib/` — Utilities and Connect RPC client setup

Generated TypeScript clients live in `gen/` at the project root, shared across all frontends.

For React Native mobile frontends, `forge add frontend <name> --kind mobile` creates an Expo app with the same systems:
- `app/` — Expo Router screens and layouts
- `src/hooks/` — Generated Connect RPC hooks (shared template)
- `src/lib/` — Connect client, event bus, auth provider
- `src/stores/` — Zustand stores (mobile-adapted: drawer, bottom sheet)

## Generated TypeScript Hooks

`forge generate` produces per-service React Query hooks in `src/hooks/`. Read RPCs get `useQuery` hooks, mutating RPCs get `useMutation` hooks. Import from the barrel:

```typescript
import { useGetTask, useCreateTask, useListTasks } from "@/hooks";
```

Do not hand-edit `*-hooks.ts` files — they are overwritten on `forge generate`. For custom hooks, create separate files in `src/hooks/` (e.g., `src/hooks/custom-hooks.ts`).

Base wrappers `useApiQuery` and `useApiMutation` in `src/hooks/` are available for one-off or composite operations that don't map to generated hooks.

## Using the Component Library

Use the `component_library` tool to find production-ready components before building from scratch:

```
component_library(action="search", query="dashboard")
component_library(action="search", tag="chart")
component_library(action="get", name="quadrant_chart")
```

Categories: layouts, charts, diagrams, deck, ui.

### Data viz: use Recharts, not the library

For commodity data viz — bar, line, area, donut, pie, scatter, sparkline — install Recharts:

```bash
npm i recharts
```

Hand-rolling SVG paths for these loses to a real library on every dimension that matters: hover tooltips, click handlers, brush selection, datetime/log axes, locale formatting, dual axes, canvas rendering past ~1k points, accessibility. Don't compete with hundreds of person-years of investment.

The component library only ships **narrative** charts — `quadrant_chart` (competitive matrix), `concentric_circles` (TAM/SAM/SOM), `funnel_chart` (sales conversion) — and the `slide_*` deck charts. These are presentation-grade where heavy customization matters more than interactivity, and where libraries are weakest.

Rule of thumb: if it's going on a dashboard, use Recharts. If it's going on a slide or a marketing page, check the component library first.

### Base UI primitives (always available)

Every forge frontend ships a small set of low-level primitives at scaffold time, under `src/components/ui/`. Pages and frontend packs MUST compose these instead of inlining their own `<button>` / `<input>` / `<table>` markup. The full set:

| Primitive | Import | What it is |
|-----------|--------|-----------|
| `button` | `import Button from "@/components/ui/button"` | Generic button — `primary` / `secondary` / `outline` / `ghost` / `danger` variants, sizes, loading state. |
| `input` | `import Input from "@/components/ui/input"` | Generic text input — sizes, invalid state, forwarded ref. Pair with `<Label>`. |
| `label` | `import Label from "@/components/ui/label"` | Form field label with optional required-asterisk. |
| `form` | `import Form, { FormField, FormError, FormActions } from "@/components/ui/form"` | Form structural primitives — root `<form>` plus field/error/actions wrappers. |
| `card` | `import Card, { CardHeader, CardBody, CardFooter } from "@/components/ui/card"` | Generic surface primitive. Distinct from `MetricCard`/`StatCards` (domain components). |
| `avatar` | `import Avatar from "@/components/ui/avatar"` | User avatar with image, initials fallback, status indicator. |
| `tabs` | `import Tabs from "@/components/ui/tabs"` | Tab navigation with underline/pills/boxed variants. |
| `table` | `import Table, { TableHeader, TableBody, TableRow, TableHead, TableCell } from "@/components/ui/table"` | Bare structural table — pair with `@tanstack/react-table` for headless sort/filter. |
| `select` | `import Select from "@/components/ui/select"` | Generic select — options array, sizes, invalid state. |
| `chip` | `import Chip from "@/components/ui/chip"` | Removable filter chip / tag. Distinct from `Badge` (status-shaped). |
| `toast_notification` | `import { ToastProvider, useToast } from "@/components/ui/toast_notification"` | Toast/notification system — success/error/warning/info, auto-dismiss. |

Plus the higher-level domain components scaffolded out of the box: `sidebar_layout`, `page_header`, `badge`, `modal`, `skeleton_loader`, `pagination`, `search_input`, `alert_banner`, `key_value_list`, `login_form`.

These primitives are written as `overwrite: once` from the scaffolder — once installed, they are yours to edit. If you find yourself re-inlining a button or input shape in a page or pack, stop and use the primitive instead.

### Variant naming conventions

- `Badge` accepts `success | warning | error | info | neutral` as canonical variants, plus `danger` (alias for `error`) and `default` (alias for `neutral`) so ports from codebases using either naming work without an adapter table.
- `Button` ships `primary | secondary | outline | ghost | danger` — destructive action is `danger`, not `error`, because Button is action-shaped and Badge is status-shaped.
- `Modal` accepts a `footer` slot prop AND inline footer markup in `children`. Both shapes are first-class — pick whichever the source codebase already uses to minimise port churn.

## Connect RPC Clients

Import the generated Connect transport from `src/lib/connect.ts` (generated — do not edit):

```typescript
import { useGetTask } from "@/hooks";
// or for direct calls:
import { createClient } from "@connectrpc/connect";
import { MyService } from "@/../../gen/my/service/v1/service_connect";
import { transport } from "@/lib/connect";

export const myServiceClient = createClient(MyService, transport);
```

## Protobuf-ES v2 Patterns

Forge uses protobuf-es v2. Create message instances with `create()`, not constructors:

```typescript
import { create } from "@bufbuild/protobuf";
import { CreateTaskRequestSchema } from "@/../../gen/my/service/v1/service_pb";

// CORRECT — protobuf-es v2
const req = create(CreateTaskRequestSchema, {
  name: "My Task",
  description: "Details here",
});

// WRONG — this is protobuf-es v1 syntax
// const req = new CreateTaskRequest({ name: "My Task" });
```

## Styling with Tailwind CSS v4

This project uses **Tailwind CSS v4**. Key differences from v3:

- **No `tailwind.config.js`** — configuration is done in CSS with `@theme`
- **Import with** `@import "tailwindcss"` in your global CSS (not `@tailwind base/components/utilities`)
- **PostCSS plugin** is `@tailwindcss/postcss` (not `tailwindcss`)
- Use `@theme` blocks in CSS for custom design tokens instead of a config file

```css
/* src/app/globals.css */
@import "tailwindcss";

@theme {
  --color-brand: #3b82f6;
  --font-sans: "Inter", sans-serif;
}
```

Use Tailwind utility classes directly in JSX. Follow responsive-first design (`sm:`, `md:`, `lg:` prefixes).

## CSS Health

Treat CSS as architecture, not a dumping ground. Keep styling predictable and easy to refactor:

- Prefer Tailwind utilities and component variants for layout, spacing, color, and state styling.
- Use CSS variables in `@theme` or scoped CSS for reusable design tokens; do not hard-code one-off colors across components.
- Avoid `!important`. If specificity fights you, simplify selectors, move styles closer to the component boundary, or introduce a variant API.
- Avoid DOM `style={{...}}` props except for truly dynamic runtime values (measured dimensions, CSS custom properties, chart coordinates). Prefer `className` and CSS variables.
- Keep global CSS small: Tailwind import, theme tokens, base element defaults, and app-wide variables only.
- Run `npm run lint:styles` when changing CSS-heavy files; it catches `!important` and invalid Tailwind v4 at-rules.

## Visual Verification

**ALWAYS use BOTH `take_snapshot` AND `take_screenshot` via Chrome DevTools before declaring frontend work complete.**

Snapshots (a11y tree) alone cannot detect CSS/visual issues — layout shifts, misaligned elements, wrong colors, broken responsive layouts, z-index problems, and overflow issues are invisible to snapshots. Screenshots catch what snapshots miss.

```
# After making changes, verify with BOTH:
take_snapshot()         # Check element structure, text content, accessibility
take_screenshot()       # Check visual layout, styling, spacing, colors
```

For responsive testing, resize the page and screenshot at multiple breakpoints.

## Component Patterns

- Use **functional components** with hooks exclusively — no class components.
- Prefer **server components** by default. Add `"use client"` only when you need interactivity, browser APIs, or hooks like `useState`/`useEffect`.
- Keep components small and focused. Extract reusable logic into custom hooks.

## Error Handling

Handle Connect RPC errors by code:

```typescript
import { ConnectError, Code } from "@connectrpc/connect";

try {
  const res = await myServiceClient.createItem({ name: "example" });
} catch (err) {
  if (err instanceof ConnectError) {
    if (err.code === Code.InvalidArgument) {
      setFieldErrors(err.message);
    } else if (err.code === Code.PermissionDenied) {
      setError("You don't have permission to do this.");
    } else {
      setError("Something went wrong. Please try again.");
    }
  }
}
```

Every data-fetching component must handle three states: **loading**, **success**, and **error**.

## Files NOT to Edit

These files are regenerated by `forge generate` — changes will be overwritten:

- `src/gen/` — Generated TypeScript stubs and clients
- `src/lib/connect.ts` — Connect transport setup
- `src/hooks/*-hooks.ts` — Generated React Query hooks

Put custom code in separate files alongside them (e.g., `src/hooks/custom-hooks.ts`, `src/lib/utils.ts`).

## Scaffolded Infrastructure (yours to extend)

These files are created by `forge add frontend` and are yours to modify:

- `src/lib/auth/` — Auth provider interface, stub provider, context. Implement `AuthProvider` to add real auth.
- `src/lib/events.ts` — Typed event bus. Extend the `EventMap` interface to add custom events.
- `src/lib/event-context.tsx` — Event bus React context and hooks.
- `src/stores/ui-store.ts` — Zustand base UI store. Extend or create domain stores in `src/stores/`.
- `src/lib/format-utils.ts` — Shared formatting utilities used by generated pages.

## Dev Workflow

```bash
forge run    # Full stack: infra + Go (hot reload) + Next.js
```

Changes to frontend code reflect instantly in the browser. After changing `.proto` files, always regenerate:

```bash
forge generate
```

## Scaffolded Systems

The frontend scaffold includes three extensible systems:

- **Auth provider** (`src/lib/auth/`) — DI'd via `AuthProvider`. Swap in Auth0, Clerk, or custom JWT by implementing the `AuthProvider` interface. `useAuth()` gives you user, token, login, logout.
- **Event bus** (`src/lib/events.ts`) — Typed pub/sub for imperative cross-cutting actions (`toast:show`, `auth:expired`, `navigate`). Extend the event map with your own events. Use `useEvent(name, handler)` in components.
- **UI store** (`src/stores/ui-store.ts`) — Zustand baseline for shared client state (`sidebarCollapsed`, `commandPaletteOpen`). Extend or create domain stores in `src/stores/`.

Mobile (React Native) frontends include the same three systems adapted for mobile:
- **Auth provider** — same `AuthProvider` interface, same `useAuth()` hook
- **Event bus** — same typed pub/sub, plus mobile-specific events (`app:background`, `app:foreground`)
- **UI store** — mobile-adapted: `drawerOpen`, `bottomSheetOpen` instead of `sidebarCollapsed`, `commandPaletteOpen`

## File naming inside `frontends/<name>/src/`

Forge templates follow two TS file-naming conventions, both intentional:

- **Components under `src/components/ui/`** are `snake_case` (`data_table.tsx`, `toast_notification.tsx`, `key_value_list.tsx`). Each file default-exports a single PascalCase component (`Button`, `DataTable`).
- **Hooks, lib utilities, and stores** are `kebab-case` (`use-api-query.ts`, `format-utils.ts`, `ui-store.ts`).

For the full Go / proto / TS / `forge.yaml` casing table, see `architecture` → **Naming conventions**.

## Sub-skills

Load sub-skills for specific frontend topics:

- **state** — State management decision table, state vs events, ownership, async handling
- **patterns** — Component composition, container/presentational, effects discipline, typed boundaries

For frontend testing — what to test, what NOT to test, the `mockTransport()` seam, and recipes per layer — see the top-level `frontend-testing` skill.

## Rules

- Never hand-edit generated files (`*-hooks.ts`, `src/gen/`, `src/lib/connect.ts`).
- Always use `create(Schema, {...})` for protobuf messages, never `new Message()`.
- Run `forge generate` after every `.proto` change.
- Add `"use client"` only when needed — hooks and event handlers require client components.
- Verify frontend work visually with BOTH `take_snapshot` and `take_screenshot`.
- Use the `component_library` tool before building UI components from scratch.
- Use generated React Query hooks for server data — do not copy backend data into Zustand.
- Subscribe to Zustand slices, not the whole store.
- Use the event bus for imperative actions (toasts, navigation commands) — not as a source of truth.
- Keep forms in react-hook-form + Zod — do not hand-roll form state with scattered useState.