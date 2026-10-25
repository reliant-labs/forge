// Fixture: a Zustand store that reaches the server through the generated
// React Query layer's module (its key factory and client), mirroring the
// cache into Zustand — two sources of truth again.

import { create } from "zustand";
import { cartServiceKeys } from "../hooks/cart-service-hooks_gen";

type CartState = {
  key: readonly unknown[];
};

export const useCartStore = create<CartState>(() => ({
  key: cartServiceKeys.all,
}));
