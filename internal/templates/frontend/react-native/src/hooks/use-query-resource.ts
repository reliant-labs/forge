import type { UseQueryResult } from "@tanstack/react-query";

/**
 * useQueryResource — tristate adapter over React Query's UseQueryResult.
 *
 * Why this exists
 * ---------------
 * React Query returns a result where `isLoading`, `isError`, and `data` can
 * co-exist in surprising ways during refetches and stale states. The most
 * destructive bug class is gates like:
 *
 *   const ready = !isLoading && data;       // FALSE during the load window
 *   if (ready) doSomething();               // … which can wipe real state
 *
 * Those boolean gates collapse the loading state into "the negative of
 * success", which is wrong. This helper normalizes the result into a
 * discriminated union the type checker can exhaustively switch on:
 *
 *   const res = useQueryResource(useGetWidget({ id }));
 *   if (res.status === "loading") return <ActivityIndicator />;
 *   if (res.status === "error")   return <ErrorBanner message={res.error.message} />;
 *   return <WidgetView widget={res.data} />;
 *
 * Now there is no path where "loading" silently means "complete". The
 * compiler enforces it.
 *
 * Note: this is a derivation, not a replacement. Pass the result of an
 * existing query hook — the original cache key, refetch behavior, etc.
 * are all preserved.
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
  if (query.isPending) {
    return { status: "loading" };
  }
  if (query.isError && query.data === undefined) {
    return {
      status: "error",
      error: query.error,
      refetch: () => void query.refetch(),
    };
  }
  return {
    status: "success",
    data: query.data as TData,
    isStale: query.isStale,
    refetch: () => void query.refetch(),
  };
}
