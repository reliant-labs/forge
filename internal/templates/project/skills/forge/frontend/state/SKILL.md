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
| Shared client UI state | Zustand store (`src/stores/`) | `sidebarCollapsed`, `commandPaletteOpen`, `activeModal` |
| Server/backend data | Generated React Query hooks | workflows, users, runs — from `src/hooks/*-hooks.ts` |
| Durable navigation | URL (route params, search params) | current project, selected run, active tab, filters |
| Imperative cross-cutting actions | Event bus (`src/lib/events.ts`) | `toast:show`, `navigate`, `auth:expired` |
| Persistent preferences | `localStorage` | sidebar width, theme preference |

Do not jump to global state because props feel annoying. A few props is fine.

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

## Sub-skills

Load the parent `frontend` skill for project structure, hooks, and dev workflow.