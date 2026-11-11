// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/session.ts — the identity epoch.
//
// One account is signed in at a time. Every identity change (sign-out, a
// refresh token the IdP no longer accepts, a different account signing in)
// bumps the epoch. Anything started under an older epoch is stale:
//
//   - an in-flight request is aborted through `currentSignal()`;
//   - a response that lands after the bump is dropped by whoever checks
//     `isCurrent(epochTheyStartedUnder)` — it must not repopulate the cache
//     or the token store.
//
// `endSession(queryClient)` is the cache half of sign-out: it makes sure no
// query or mutation from the previous account survives into the next one
// (generated hook keys carry no identity, so a shared cache would leak).
//
// Wire `currentSignal()` into the transport (e.g. an interceptor that merges
// it into each request's signal) so in-flight WRITES abort too — React Query
// cannot cancel mutations.

/** The slice of TanStack's QueryClient that sign-out needs. */
export interface QueryClientLike {
  cancelQueries(): Promise<unknown>;
  clear(): void;
}

export interface IdentitySession {
  /** The current identity epoch. Starts at 0. */
  epoch(): number;
  /** Aborted (and replaced) the next time the epoch bumps. */
  currentSignal(): AbortSignal;
  /** True while `epoch` is still the live one. */
  isCurrent(epoch: number): boolean;
  /** Abort everything in flight and start a new epoch; returns it. */
  bumpEpoch(): number;
  /** bump epoch → abort → cancelQueries() → clear(). Never throws. */
  endSession(queryClient: QueryClientLike): Promise<void>;
}

export function createSession(): IdentitySession {
  let epoch = 0;
  let controller = new AbortController();

  const bumpEpoch = (): number => {
    epoch += 1;
    const previous = controller;
    controller = new AbortController();
    previous.abort();
    return epoch;
  };

  return {
    epoch: () => epoch,
    currentSignal: () => controller.signal,
    isCurrent: (e) => e === epoch,
    bumpEpoch,
    async endSession(queryClient) {
      bumpEpoch();
      try {
        await queryClient.cancelQueries();
      } catch {
        // A query that refuses to cancel must not keep the old account's data.
      }
      queryClient.clear();
    },
  };
}

/** The app-wide session. Tests should build their own with createSession(). */
export const session: IdentitySession = createSession();

export const currentEpoch = (): number => session.epoch();
export const currentSignal = (): AbortSignal => session.currentSignal();
export const isCurrent = (epoch: number): boolean => session.isCurrent(epoch);
export const endSession = (queryClient: QueryClientLike): Promise<void> =>
  session.endSession(queryClient);
