import { create } from "zustand";

/**
 * Base UI store for React Native — shared client-side state.
 *
 * Extend by creating domain stores in src/stores/:
 *   src/stores/editor-store.ts
 *
 * Always subscribe to slices, not the whole store:
 *   const open = useUiStore(s => s.drawerOpen);  // GOOD
 *   const store = useUiStore();  // BAD
 */

interface UiState {
  // Navigation drawer
  drawerOpen: boolean;
  toggleDrawer: () => void;
  setDrawerOpen: (open: boolean) => void;

  // Bottom sheet
  bottomSheetOpen: boolean;
  bottomSheetContent: string | null;
  openBottomSheet: (content: string) => void;
  closeBottomSheet: () => void;

  // Active modal (at most one app-level modal at a time)
  activeModal: string | null;
  openModal: (id: string) => void;
  closeModal: () => void;
}

export const useUiStore = create<UiState>((set) => ({
  // Navigation drawer
  drawerOpen: false,
  toggleDrawer: () => set((s) => ({ drawerOpen: !s.drawerOpen })),
  setDrawerOpen: (open) => set({ drawerOpen: open }),

  // Bottom sheet
  bottomSheetOpen: false,
  bottomSheetContent: null,
  openBottomSheet: (content) => set({ bottomSheetOpen: true, bottomSheetContent: content }),
  closeBottomSheet: () => set({ bottomSheetOpen: false, bottomSheetContent: null }),

  // Active modal
  activeModal: null,
  openModal: (id) => set({ activeModal: id }),
  closeModal: () => set({ activeModal: null }),
}));
