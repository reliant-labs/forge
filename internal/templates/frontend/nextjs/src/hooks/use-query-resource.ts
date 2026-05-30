import type { UseQueryResult } from "@tanstack/react-query";

/**
 * useQueryResource — tristate adapter over React Query's UseQueryResult.
 *
 * Why this exists
 * ---------------
 * React Query returns a result where `isLoading`, `isError`, and `data` can
 * co-exist in surprising ways during refetches and stale states. The most
 * destructive bug class in our codebase has been gates like:
 *
 *   const ready = !isLoading && data;       // FALSE during the load window
 *   if (ready) doSomething();               // … which can wipe real state
 *
 * Those boolean gates collapse the loading state into "the negative of
 * success", which is wrong. This helper normalizes the result into a
 * discriminated union the type checker can exhaustively switch on:
 *
 *   const res = useQueryResource(useGetWidget({ id }));
 *   if (res.status === "loading") return <SkeletonLoader />;
 *   if (res.status === "error")   return <AlertBanner message={res.error.message} />;
 *   return <WidgetView widget={res.data} />;
 *
 * Now there is no path where "loading" silently means "complete". The
 * compiler enforces it.
 *
 * Note: this is a derivation, not a replacement. Pass the result of an
 * existing query hook (useApiQuery, generated useGetX, etc.) — the original
 * cache key, refetch behavior, etc. are all preserved.
 *
 * See `frontend/state` ("Loading is not the negative of success").
 */

export type QueryResource<TData, TError = Error> =
  | { status: "loading" }
  | { status: "error"; error: TError; refetch: () => void }
  | { status: "success"; data: TData; isStale: boolean; refetch: () => void };

export function useQueryResource<TData, TError = Error>(
  query: UseQueryResult<TData, TError>,
): QueryResource<TData, TError> {
  // First load — no data yet, no settled error.
  if (query.isPending) {
    return { status: "loading" };
  }
  // Settled with an error AND no usable cached data.
  if (query.isError && query.data === undefined) {
    return {
      status: "error",
      error: query.error,
      refetch: () => void query.refetch(),
    };
  }
  // Settled with data (possibly stale, possibly with a background error
  // surfaced via query.error — callers can re-check via `isStale`).
  return {
    status: "success",
    data: query.data as TData,
    isStale: query.isStale,
    refetch: () => void query.refetch(),
  };
}
