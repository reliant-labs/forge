// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/transport.ts — the three Connect interceptors that tie the
// transport to the signed-in identity. src/lib/connect.ts composes them:
//
//   authExpired → epoch → tokenRejected → (runtime stack: errors, bearer, trace)
//
// epochInterceptor      Every request runs under the identity epoch it started
//                       in (src/lib/auth/session). The epoch's AbortSignal is
//                       merged into the request's own, so sign-out aborts
//                       in-flight calls — WRITES included, which React Query
//                       cannot cancel. A response that lands after the epoch
//                       moved is dropped (a Canceled error), so nothing from
//                       the previous account can reach the cache.
//
// tokenRejected         The API said Unauthenticated for the token we sent. Ask
//                       the provider to renew it once and, if it produced a
//                       different token, replay the request ONCE. Unary calls
//                       only (a stream that already started cannot be
//                       replayed), never when the epoch moved meanwhile, and
//                       never twice: a second 401 is final.
//                       Only a method the proto declares
//                       `idempotency_level = NO_SIDE_EFFECTS` or `IDEMPOTENT`
//                       is replayed. forge's authn rejection carries no
//                       distinguishing reason header (it is a bare
//                       Unauthenticated), so a 401 cannot prove the handler
//                       never ran — a proxy or a handler can answer 401 after
//                       acting — and replaying a write could apply it twice.
//                       A non-idempotent call still gets the token renewed (the
//                       NEXT call works), but its 401 propagates.
//
// authExpired           When a call ENDS Unauthenticated (after the replay, if
//                       any), emit `auth:expired` — what the browser
//                       transports do. The auth context re-checks the session
//                       on it. A call cancelled by a sign-out does not emit.
//
// All dependencies are injected, so tests drive these with plain fakes.

import { MethodOptions_IdempotencyLevel } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type Interceptor, type StreamResponse } from "@connectrpc/connect";

export interface EpochSession {
  currentEpoch(): number;
  currentSignal(): AbortSignal;
  isCurrent(epoch: number): boolean;
}

const identityChanged = () =>
  new ConnectError("the signed-in identity changed during the request", Code.Canceled);

/** An AbortSignal that aborts when any of `signals` does, and a way to stop listening. */
function mergeSignals(...signals: AbortSignal[]): { signal: AbortSignal; dispose: () => void } {
  const controller = new AbortController();
  const listeners: (() => void)[] = [];
  const abort = (from: AbortSignal) => () => controller.abort(from.reason);
  for (const s of signals) {
    if (s.aborted) {
      controller.abort(s.reason);
      break;
    }
    const onAbort = abort(s);
    s.addEventListener("abort", onAbort, { once: true });
    listeners.push(() => s.removeEventListener("abort", onAbort));
  }
  return {
    signal: controller.signal,
    dispose: () => listeners.forEach((off) => off()),
  };
}

export function epochInterceptor(session: EpochSession): Interceptor {
  return (next) => async (req) => {
    const epoch = session.currentEpoch();
    const { signal, dispose } = mergeSignals(req.signal, session.currentSignal());
    const scoped = { ...req, signal };

    let res;
    try {
      res = await next(scoped);
    } catch (e) {
      dispose();
      // Whatever it failed with, it belonged to an identity that is gone.
      if (!session.isCurrent(epoch)) throw identityChanged();
      throw e;
    }

    if (!res.stream) {
      dispose();
      if (!session.isCurrent(epoch)) throw identityChanged();
      return res;
    }

    // A stream outlives this call: drop its messages once the epoch moves,
    // and stop listening when it ends.
    const source = res.message;
    const guarded: StreamResponse = {
      ...res,
      message: {
        async *[Symbol.asyncIterator]() {
          try {
            for await (const message of source) {
              if (!session.isCurrent(epoch)) throw identityChanged();
              yield message;
            }
          } catch (e) {
            if (!session.isCurrent(epoch)) throw identityChanged();
            throw e;
          } finally {
            dispose();
          }
        },
      },
    };
    return guarded;
  };
}

export interface TokenRejectedDeps {
  session: Pick<EpochSession, "currentEpoch" | "isCurrent">;
  /** The token the refused request carried is passed along; true = a new token is ready. */
  handler: () => ((rejectedToken: string | null) => Promise<boolean>) | null;
}

const BEARER = /^Bearer\s+/i;

/** True when running the method twice is declared harmless in the proto. */
function isReplaySafe(method: { idempotency: MethodOptions_IdempotencyLevel }): boolean {
  return (
    method.idempotency === MethodOptions_IdempotencyLevel.NO_SIDE_EFFECTS ||
    method.idempotency === MethodOptions_IdempotencyLevel.IDEMPOTENT
  );
}

export function tokenRejectedInterceptor({ session, handler }: TokenRejectedDeps): Interceptor {
  return (next) => async (req) => {
    if (req.stream) return next(req);
    const epoch = session.currentEpoch();
    try {
      return await next(req);
    } catch (e) {
      const onRejected = handler();
      if (
        !onRejected ||
        !(e instanceof ConnectError) ||
        e.code !== Code.Unauthenticated ||
        !session.isCurrent(epoch)
      ) {
        throw e;
      }
      // The runtime's bearer interceptor wrote the token we sent onto this
      // same Headers object.
      const sent = req.header.get("Authorization")?.replace(BEARER, "") || null;
      let renewed = false;
      try {
        renewed = await onRejected(sent);
      } catch {
        renewed = false; // offline or refused: the original 401 stands
      }
      if (!renewed || !session.isCurrent(epoch) || !isReplaySafe(req.method)) throw e;
      // Cleared so a provider with no token now sends none, rather than the stale one.
      req.header.delete("Authorization");
      return next(req); // once: a second Unauthenticated propagates
    }
  };
}

export function authExpiredInterceptor(emit: () => void): Interceptor {
  return (next) => async (req) => {
    try {
      return await next(req);
    } catch (e) {
      if (e instanceof ConnectError && e.code === Code.Unauthenticated) emit();
      throw e;
    }
  };
}
