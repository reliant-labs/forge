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

## Component Patterns

- Use **functional components** with hooks exclusively — no class components.
- Prefer **server components** by default. Add `"use client"` only when you need interactivity, browser APIs, or hooks like `useState`/`useEffect`.
- Keep components small and focused. Extract reusable logic into custom hooks in `lib/`.

## Styling with Tailwind CSS

Use Tailwind utility classes directly in JSX. Follow responsive-first design (`sm:`, `md:`, `lg:` prefixes). Ensure accessible contrast ratios and use semantic HTML elements. Avoid custom CSS files unless absolutely necessary.

## Form Handling

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

## Loading and Error States

Every data-fetching component must handle three states: **loading**, **success**, and **error**. Use skeleton placeholders or spinners for loading. Display actionable error messages — never show raw error objects to users.

## TypeScript

Enable strict mode in `tsconfig.json`. Avoid `any` — use generated types from `gen/` for all RPC request/response shapes. Let the generated code be the single source of truth for API types.

## Dev Workflow

Run the full stack locally with a single command:

```bash
forge run
```

This hot-reloads the Go backend and all frontends simultaneously. Changes to frontend code reflect instantly in the browser.

## Common Pitfalls

- **Wrong import paths**: Always import generated clients from the project-root `gen/` directory, not from a local copy.
- **Stale TypeScript clients**: Run `forge generate` after every `.proto` change. The compiler won't catch schema drift — your UI will just break at runtime.
- **Ignoring Connect error codes**: Catch `ConnectError` and switch on `err.code`. Generic catch-all messages frustrate users.
- **Missing `"use client"` directive**: Hooks and event handlers require client components. Next.js will error if you forget the directive.
