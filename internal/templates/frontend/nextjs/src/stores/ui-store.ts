import { create } from "zustand";

/**
 * Base UI store — shared client-side state used across unrelated components.
 *
 * Scope: ONLY app-wide, cross-component UI state that doesn't fit anywhere
 * else (sidebar layout, command palette open). For everything else, prefer:
 *
 *   - Modals / dialogs / confirm-deletes → `useState` in the component that
 *     owns the action. See the canonical example at
 *     `src/app/[entitySlug]/[id]/page.tsx` (detail page delete confirm).
 *     Lift to a store ONLY if two unrelated components both need to read
 *     the same modal's open state.
 *
 *   - Half-page panels (settings, workflow editor, detail view) → URL route.
 *     If the browser back button should dismiss it, it is a route, not a
 *     Zustand field. See the `frontend/state` skill.
 *
 *   - Server data (the current user, fetched lists) → generated React Query
 *     hooks in `src/hooks/*-hooks.ts`. Never copy backend data into Zustand.
 *
 * Extend by creating domain stores in src/stores/:
 *   src/stores/editor-store.ts
 *   src/stores/workflow-ui-store.ts
 *
 * Always subscribe to slices, not the whole store:
 *   const collapsed = useUiStore(s => s.sidebarCollapsed);  // GOOD
 *   const store = useUiStore();  // BAD — subscribes to everything
 */

interface UiState {
  // Sidebar
  sidebarCollapsed: boolean;
  toggleSidebar: () => void;
  setSidebarCollapsed: (collapsed: boolean) => void;

  // Command palette
  commandPaletteOpen: boolean;
  setCommandPaletteOpen: (open: boolean) => void;
  toggleCommandPalette: () => void;
}

export const useUiStore = create<UiState>((set) => ({
  // Sidebar
  sidebarCollapsed: false,
  toggleSidebar: () => set((s) => ({ sidebarCollapsed: !s.sidebarCollapsed })),
  setSidebarCollapsed: (collapsed) => set({ sidebarCollapsed: collapsed }),

  // Command palette
  commandPaletteOpen: false,
  setCommandPaletteOpen: (open) => set({ commandPaletteOpen: open }),
  toggleCommandPalette: () =>
    set((s) => ({ commandPaletteOpen: !s.commandPaletteOpen })),
}));
