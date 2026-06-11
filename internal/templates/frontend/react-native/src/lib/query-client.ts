import { MutationCache, QueryClient } from "@tanstack/react-query";

import { emitToast } from "@/lib/events";
import { userMessage } from "@/lib/format-utils";

// Typed mutation meta — `meta: { silenceErrorToast: true }` is the opt-out
// for pages that render the mutation error inline (form banner) and don't
// want the global toast doubling it.
declare module "@tanstack/react-query" {
  interface Register {
    mutationMeta: {
      /** Suppress the app-wide error toast for this mutation. */
      silenceErrorToast?: boolean;
    };
  }
}

/**
 * Shared QueryClient for the app.
 *
 * Error-toast policy lives HERE and only here: the MutationCache onError
 * below is the single chokepoint that surfaces mutation failures as
 * toasts. Generated hooks do not toast; pages opt out per-mutation with
 * `meta: { silenceErrorToast: true }` when they show the error inline.
 *
 * Defaults chosen for server-backed Connect RPCs:
 * - staleTime: 30s — avoid refetching on every mount for mildly fresh data.
 * - gcTime: 5m — cached data stays around briefly after no observers.
 * - retry: 1 — one retry for transient network / 5xx, then surface the error.
 * - refetchOnWindowFocus: false — most RPC responses don't need this; enable
 *   per-query if you want it.
 *
 * Tune per-query via `useQuery({ staleTime, retry, ... })` as needed.
 */
export const queryClient = new QueryClient({
  mutationCache: new MutationCache({
    onError: (error, _variables, _context, mutation) => {
      if (mutation.meta?.silenceErrorToast) return;
      emitToast({ message: userMessage(error), variant: "error" });
    },
  }),
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      gcTime: 5 * 60_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
    mutations: {
      retry: 0,
    },
  },
});
