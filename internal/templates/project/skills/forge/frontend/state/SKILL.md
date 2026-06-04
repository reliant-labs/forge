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
| Cross-page selection (survives reload) | Zustand `persist` store (`src/stores/`) | `currentOrgId`, `currentTenantId`, `currentWorkspaceId` |
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

## Cross-page selection state

A specific shape of shared client state that comes up in almost every
admin-style frontend: **"the user picked an org / tenant / workspace on
one page, and every other page needs to know which one is selected."**

This is NOT server data (the org list is, but the *current selection*
isn't). It's NOT URL state (it persists across navigations and reloads,
not part of the URL). It's NOT auth (the user might have access to many
orgs and switch between them). It's shared client state with one extra
requirement: **it must survive a hard reload** so the page the user
lands on after refresh is the page they were looking at.

Use a small Zustand store with `persist` middleware:

```typescript
// src/stores/org-store.ts
import { create } from "zustand";
import { persist } from "zustand/middleware";

interface OrgState {
  currentOrgId: string | null;
  currentOrgName: string | null;
  setCurrentOrg: (id: string, name: string) => void;
  clearCurrentOrg: () => void;
}

export const useOrgStore = create<OrgState>()(
  persist(
    (set) => ({
      currentOrgId: null,
      currentOrgName: null,
      setCurrentOrg: (id, name) =>
        set({ currentOrgId: id, currentOrgName: name }),
      clearCurrentOrg: () =>
        set({ currentOrgId: null, currentOrgName: null }),
    }),
    { name: "<project>-org" }, // localStorage key — scope per project
  ),
);
```

Use it from any page as a slice subscription:

```tsx
const orgId = useOrgStore((s) => s.currentOrgId);
const setCurrentOrg = useOrgStore((s) => s.setCurrentOrg);
```

Conventions:

- **`name`** the localStorage key. Persistence is scoped to the
  origin/basePath; multiple forge apps on the same domain need
  different `name`s.
- **Persist only IDs and display labels**, never full server objects.
  Re-fetch the rich record via a React Query hook (`useGetOrg(orgId)`)
  so the cache stays the single source of truth.
- **Reset on logout.** Wire `clearCurrentOrg()` into your auth
  `logout()` path so the next user doesn't inherit the previous user's
  selection.
- **Don't use this for URL-shaped selections.** Selected row in a list,
  active tab, filters — those belong in `useSearchParams`. The persisted
  store is for selections that travel with the user across the entire
  app.

The same pattern fits `currentTenantId`, `currentWorkspaceId`,
`currentProjectId`, etc. One store per orthogonal selection axis; don't
pile them into the base `ui-store` (the UI store should stay
non-persisted client UI state).

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