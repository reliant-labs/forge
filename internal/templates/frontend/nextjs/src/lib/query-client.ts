import { QueryClient } from "@tanstack/react-query";

/**
 * Shared QueryClient for the app.
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
