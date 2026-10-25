// Fixture: a Zustand store that builds a Connect client itself through
// forge's seam, with the service descriptor arriving via a re-export (no
// `/gen/` path in sight). It fetches server data all the same.

import { create } from "zustand";
import { connectClient } from "@/lib/connect";
import { OrderService } from "@/services";

const client = connectClient(OrderService);

type OrdersState = {
  orders: string[];
  load: () => Promise<void>;
};

export const useOrdersStore = create<OrdersState>((set) => ({
  orders: [],
  load: async () => {
    const res = await client.listOrders({});
    set({ orders: res.orders.map((o) => o.id) });
  },
}));
