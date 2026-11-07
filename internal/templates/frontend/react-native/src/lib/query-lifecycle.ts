// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/query-lifecycle.ts — drives TanStack Query's focus and online state
// from native signals.
//
// The shared QueryClient is DOM-agnostic: by default React Query learns about
// focus from `document.visibilitychange` and about connectivity from
// `window` online/offline events, and neither exists on a device. So without
// this file a backgrounded app never "refocuses" and the client believes it is
// online forever. Here:
//
//   focusManager   <- react-native AppState ("active" == focused)
//   onlineManager  <- expo-network (isInternetReachable, else isConnected)
//
// DEFAULT SEMANTICS (what you get, and what you do not)
//
//   - Queries refetch on focus and on reconnect, per React Query defaults —
//     but only when stale (shared staleTime is 30s) and only for queries that
//     do not set `refetchOnWindowFocus` / `refetchOnReconnect` to false. The
//     shared client sets `refetchOnWindowFocus: false` globally (browser
//     frontends do not want a refetch per tab switch); on a phone, returning
//     from the background is the main moment data goes stale, so opt in per
//     query (`refetchOnWindowFocus: true`) or change the default in your own
//     query-client.ts. `refetchOnReconnect` keeps React Query's default (true).
//   - Offline queries PAUSE. With the default "online" networkMode, a query
//     that would fetch while onlineManager says offline does not run: it
//     reports `fetchStatus: "paused"` (not "idle", and not an error) and
//     resumes by itself on reconnect. Render `isPaused` / `fetchStatus` if you
//     want an "offline" affordance.
//   - Mutations are NOT auto-replayed. That is the SHARED query-client's
//     choice, not this adapter's: it sets mutations `networkMode: "always"`
//     (write safety — no silent pause-and-replay of a stale write), so a
//     mutation fired offline runs now and fails with the network error. This
//     adapter never touches the client's defaults; it only feeds the
//     onlineManager. If a specific mutation is safe to queue, opt in with
//     `useMutation({ networkMode: "online", ... })` and persist/replay it
//     deliberately.
//   - Server streams are NOT resumed. A Connect server-stream is a plain
//     fetch; backgrounding or losing the network ends it. React Query has no
//     notion of it. Re-subscribe from the caller when focus or connectivity
//     returns, e.g. `focusManager.subscribe` / `onlineManager.subscribe`, or
//     an effect keyed on `useIsFocused`-style state of your own.
//
// SAFETY
//
//   - Idempotent and reference counted: any number of calls share ONE AppState
//     listener and ONE network listener. Each call returns its own unsubscribe
//     (safe to call twice); the listeners go away when the last one is called.
//   - Fast Refresh safe: the registry lives on globalThis, not module scope, so
//     a re-evaluated copy of this file joins the existing listeners instead of
//     adding a second set. React StrictMode's mount/unmount/mount is the same
//     case.
//   - On teardown the focus override is cleared (React Query's default focus
//     check applies again) and the online flag is reset to its default (true).
import { focusManager, onlineManager, type QueryClient } from "@tanstack/react-query";
import * as Network from "expo-network";
import { AppState, type AppStateStatus } from "react-native";

interface Installation {
  refs: number;
  teardown: () => void;
}

const REGISTRY_KEY = Symbol.for("forge.queryLifecycle");

type Registry = { [REGISTRY_KEY]?: Installation };

/** isOnline maps an expo-network state to React Query's boolean. */
function isOnline(state: Network.NetworkState): boolean {
  // iOS reports isInternetReachable === isConnected; Android refines it with
  // validated-internet. A missing field means "unknown": stay online and let
  // the request itself fail, rather than pausing every query.
  return state.isInternetReachable ?? state.isConnected ?? true;
}

function install(): () => void {
  let disposed = false;
  let sawNetworkEvent = false;

  const onAppState = (status: AppStateStatus) => {
    focusManager.setFocused(status === "active");
  };
  // "unknown" (RN before the first native event) keeps React Query's default.
  const initial = AppState.currentState;
  if (initial === "active" || initial === "background" || initial === "inactive") {
    onAppState(initial);
  }
  const appStateSub = AppState.addEventListener("change", onAppState);

  const networkSub = Network.addNetworkStateListener((state) => {
    sawNetworkEvent = true;
    onlineManager.setOnline(isOnline(state));
  });
  // The listener only reports changes; ask once for the state at startup.
  // An event that lands first is newer than this answer, so it wins.
  Network.getNetworkStateAsync().then(
    (state) => {
      if (!disposed && !sawNetworkEvent) onlineManager.setOnline(isOnline(state));
    },
    (error: unknown) => {
      if (__DEV__) console.warn("[query-lifecycle] network state unavailable:", error);
    },
  );

  return () => {
    disposed = true;
    appStateSub.remove();
    networkSub.remove();
    focusManager.setFocused(undefined);
    onlineManager.setOnline(true);
  };
}

/**
 * setupQueryLifecycle wires AppState and network state into React Query and
 * returns an unsubscribe. Call it once from the root layout inside an effect:
 *
 *   useEffect(() => setupQueryLifecycle(queryClient), []);
 *
 * focusManager and onlineManager are process-global, so the client argument is
 * accepted for call-site clarity and future per-client wiring but not read; the
 * adapter never mutates the client's defaults.
 */
export function setupQueryLifecycle(_queryClient?: QueryClient): () => void {
  const registry = globalThis as Registry;
  const existing = registry[REGISTRY_KEY];
  const installation = existing ?? { refs: 0, teardown: install() };
  registry[REGISTRY_KEY] = installation;
  installation.refs += 1;

  let released = false;
  return () => {
    if (released) return;
    released = true;
    installation.refs -= 1;
    if (installation.refs === 0) {
      installation.teardown();
      if (registry[REGISTRY_KEY] === installation) delete registry[REGISTRY_KEY];
    }
  };
}
