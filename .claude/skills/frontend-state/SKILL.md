---
name: state
description: Frontend state management — choosing the right tool for each kind of state, ownership rules, and async handling in Forge frontends.
---

# Frontend State Management

## The decision table

Use the simplest scope that solves the problem:

| Kind of state | Tool | Example |
|---|---|---|
| Temporary UI for one component | `useState` | dropdown open, hover, form input |
| Avoiding middleman props | Component composition (slots) | pass `<Sidebar>` as a prop, not `project` through 5 layers |
| Stable app-wide dependencies | React Context | auth (`useAuth()`), event bus (`useEventBus()`), theme |
| Ephemeral overlay state (sits on top of current route) | Zustand store | `sidebarCollapsed`, `commandPaletteOpen`, `dropdownOpen`, toast queue. *A modal that is dismissed by clicking outside, not by the browser back button.* |
| Navigation-replacing surface (settings, workflow editor, detail panel) | URL route | `/settings/$section`, `/workflow/$name`. *If browser-back should dismiss it, it's a route — not a Zustand bool.* |
| Server/backend data | Generated React Query hooks | workflows, users, runs — from `src/hooks/*-hooks.ts` |
| Durable navigation | URL (route params, search params) | current project, selected run, active tab, filters |
| Imperative cross-cutting actions | Event bus (`src/lib/events.ts`) | `toast:show`, `navigate`, `auth:expired` |
| Persistent preferences | `localStorage` | sidebar width, theme preference |

Do not jump to global state because props feel annoying. A few props is fine.

## Anti-pattern: `isXMode` booleans in Zustand

If a piece of UI (a) takes over the main content area, (b) should be link-shareable, or (c) should be dismissed by the browser back button, it is a route — not a `boolean`.

Symptoms of getting this wrong:
- Stale query strings linger after the mode is exited (e.g. `?plan=...` still in the URL on `/settings`).
- OAuth callbacks land on the wrong "mode" because two effects race to consume the redirect.
- Refresh drops the user back to the default view.
- Back/forward create infinite loops because forward-state wasn't undone on back.

The fix: move the surface to a route (`/settings/$section`, `/workflow/$name`, `/project/$id`). The URL is the source of truth. Browser navigation, deep-linking, and refresh all "just work" without coordination code.

## State vs events

**State** answers "what is true right now?" — `selectedNodeId`, `currentUser`, `sidebarCollapsed`.

**Events** answer "what just happened?" — `toast:show`, `auth:expired`, `workflow:runRequested`.

Never use events as your source of truth. Update state first, then emit events for side effects:

```typescript
uiStore.setState({ selectedNodeId: node.id });
events.emit("toast:show", { message: "Node selected", variant: "success" });
```

## Ownership

For every piece of state, answer:

- **Who owns it?** (one component, a store, the server, the URL)
- **Who can update it?** (only the owner, or via actions/mutations)
- **Should it survive refresh?** (URL or localStorage if yes)

## Async states

Every async operation has at least: `loading`, `error`, `success`, `empty`. Generated hooks provide these via React Query — always handle all states:

```tsx
if (query.isLoading) return <SkeletonLoader />;
if (query.isError) return <AlertBanner variant="error" message={query.error.message} />;
if (!query.data?.length) return <EmptyState />;
return <DataList items={query.data} />;
```

### Loading is not "the negative of success"

A derived gate like `inOnboarding = !isLoading && !user.onboardingCompleted` is `false` during the loading window — same value as "fully onboarded". If that gate triggers a destructive action (redirect, reset, URL cleanup), real state can be wiped before the loaded value arrives.

Two fixes:

1. **Tristate the value.** Model loading as a distinct state:
   ```ts
   type OnboardingStatus = "loading" | "in_progress" | "complete";
   if (status === "loading") return null; // explicit
   ```
2. **Early-return in effects.** Any effect that acts on async data must explicitly bail until the data is real:
   ```ts
   useEffect(() => {
     if (query.isLoading || !query.data) return;
     // safe to act
   }, [query.isLoading, query.data]);
   ```

## What the scaffold provides

- **React Query hooks** (`src/hooks/*-hooks.ts`) — generated, handle server state
- **Auth context** (`useAuth()`) — DI'd via `AuthProvider`, stable context
- **Event bus** (`src/lib/events.ts`) — typed, extensible, for imperative actions
- **UI store** (`src/stores/ui-store.ts`) — Zustand baseline, extend for your domain
- **URL state** — App Router params and `useSearchParams` for navigation state

## Pack-driven state expansion

When packs are added to `forge.yaml`, they may expand the frontend state systems:

### Auth pack (`packs: [auth]`)
- The base `EventMap` already includes `auth:expired`, `auth:login`, `auth:logout`
- The `AuthProvider` interface in `src/lib/auth/` is the integration point — implement it for your auth provider (Auth0, Clerk, Supabase Auth, etc.)
- For session state beyond what `useAuth()` provides, create `src/stores/auth-store.ts`:
  ```typescript
  import { create } from "zustand";
  
  interface AuthUiState {
    showLoginModal: boolean;
    setShowLoginModal: (show: boolean) => void;
    lastAuthError: string | null;
    setLastAuthError: (error: string | null) => void;
  }
  
  export const useAuthUiStore = create<AuthUiState>((set) => ({
    showLoginModal: false,
    setShowLoginModal: (show) => set({ showLoginModal: show }),
    lastAuthError: null,
    setLastAuthError: (error) => set({ lastAuthError: error }),
  }));
  ```

### Network events
- `network:error` and `network:unauthorized` are emitted by the Connect interceptors
- Listen for `network:unauthorized` to trigger auth refresh flows:
  ```typescript
  useEvent("network:unauthorized", () => {
    // Trigger token refresh or redirect to login
    events.emit("auth:expired");
  });
  ```

## Rules

- Use generated hooks for server data — do not copy backend data into Zustand.
- Subscribe to Zustand slices, not the whole store: `useUiStore(s => s.sidebarCollapsed)`.
- Derive values during render — do not `useEffect` to set derived state.
- Extend the base UI store or create domain stores in `src/stores/` — do not create one giant global store.
- Never sync one store into another via useEffect. The pattern `useEffect(() => storeA.set(storeB.value), [storeB.value])` is a dual-write that will desync on race conditions — one of the two stores should not exist.
- Search params are typed boundaries. Define a Zod schema per route, parse once at the top of the component, pass typed values down. Manual `encodeURIComponent(JSON.stringify(...))` round-trips is the trap that motivates the schema.
- Zod is forge's standard validator. Use it at every boundary: forms (already via react-hook-form), route params, search params, and any API response that isn't covered by generated Connect types.

## Sub-skills

Load the parent `frontend` skill for project structure, hooks, and dev workflow.