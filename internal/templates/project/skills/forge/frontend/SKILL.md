---
name: frontend
description: Write Next.js frontends — Connect RPC clients, Tailwind styling, forms, and error handling.
---

# Frontend Development in Forge

## Project Structure

Each frontend lives in `frontends/<name>/` as a standalone Next.js app. Create one with:

```bash
forge add frontend <name>
```

Key directories inside `frontends/<name>/`:
- `app/` — Next.js App Router pages and layouts
- `components/` — Reusable React components
- `lib/` — Utilities, hooks, and Connect RPC client setup

Generated TypeScript clients live in `gen/` at the project root, shared across all frontends.

## Connect RPC Clients

Import generated clients from the top-level `gen/` directory. Set up a transport once in `lib/` and reuse it:

```typescript
import { createConnectTransport } from "@connectrpc/connect-web";
import { createClient } from "@connectrpc/connect";
import { MyService } from "@/../../gen/my/service/v1/service_connect";

const transport = createConnectTransport({ baseUrl: "/api" });
export const myServiceClient = createClient(MyService, transport);
```

After changing `.proto` files, always regenerate clients:

```bash
forge generate
```

## Generated TypeScript Hooks

`forge generate` produces per-service React Query hooks in `frontends/<name>/src/hooks/`. Read RPCs get `useQuery` hooks, mutating RPCs get `useMutation` hooks. Import from the barrel:

```typescript
import { useGetTask, useCreateTask, useListTasks } from "@/hooks";
```

Do not hand-edit the generated `*-hooks.ts` files — they are overwritten on `forge generate`. For custom hooks, create separate files in `src/hooks/` (e.g., `src/hooks/custom-hooks.ts`).

Base wrappers `useApiQuery` and `useApiMutation` in `src/hooks/` are available for one-off or composite operations that don't map to generated hooks.

## Component Patterns

- Use **functional components** with hooks exclusively — no class components.
- Prefer **server components** by default. Add `"use client"` only when you need interactivity, browser APIs, or hooks like `useState`/`useEffect`.
- Keep components small and focused. Extract reusable logic into custom hooks in `lib/`.

## Styling with Tailwind CSS

Use Tailwind utility classes directly in JSX. Follow responsive-first design (`sm:`, `md:`, `lg:` prefixes). Ensure accessible contrast ratios and use semantic HTML elements.

## Form Handling and Error States

Call Connect RPC mutations in event handlers or server actions. Always handle errors by code:

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

Every data-fetching component must handle three states: **loading**, **success**, and **error**. Use skeleton placeholders or spinners for loading. Display actionable error messages.

## Dev Workflow

```bash
forge run    # Full stack: infra + Go (hot reload) + Next.js
```

Changes to frontend code reflect instantly in the browser.

## Common Pitfalls

- **Wrong import paths**: Always import generated clients from the project-root `gen/` directory.
- **Stale TypeScript clients**: Run `forge generate` after every `.proto` change.
- **Missing `"use client"` directive**: Hooks and event handlers require client components.
- **Hand-editing generated hooks**: The `*-hooks.ts` files are overwritten on `forge generate`. Put custom hooks in separate files.
