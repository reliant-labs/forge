import { create } from "zustand";

/**
 * Base UI store — shared client-side state used across unrelated components.
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

  // Active modal (at most one app-level modal at a time)
  activeModal: string | null;
  openModal: (id: string) => void;
  closeModal: () => void;
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

  // Active modal
  activeModal: null,
  openModal: (id) => set({ activeModal: id }),
  closeModal: () => set({ activeModal: null }),
}));
