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

Categories: layouts, charts, diagrams, deck, ui. Charts handle all coordinate math internally — pass data, get pixels.

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

## Dev Workflow

```bash
forge run    # Full stack: infra + Go (hot reload) + Next.js
```

Changes to frontend code reflect instantly in the browser. After changing `.proto` files, always regenerate:

```bash
forge generate
```

## Rules

- Never hand-edit generated files (`*-hooks.ts`, `src/gen/`, `src/lib/connect.ts`).
- Always use `create(Schema, {...})` for protobuf messages, never `new Message()`.
- Run `forge generate` after every `.proto` change.
- Add `"use client"` only when needed — hooks and event handlers require client components.
- Verify frontend work visually with BOTH `take_snapshot` and `take_screenshot`.
- Use the `component_library` tool before building UI components from scratch.
