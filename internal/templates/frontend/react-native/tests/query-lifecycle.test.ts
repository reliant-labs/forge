// Scaffolded by forge. Yours to own — forge will not overwrite this file.
// Drives the real TanStack focusManager/onlineManager through mocked native
// AppState and expo-network signals. No device, no native module.
import { afterEach, beforeEach, describe, expect, it, jest } from "@jest/globals";
import { focusManager, onlineManager, QueryClient, QueryObserver } from "@tanstack/react-query";
import * as Network from "expo-network";
import { AppState, type AppStateStatus } from "react-native";

import { queryClient as sharedQueryClient } from "../src/lib/query-client";
import { setupQueryLifecycle as setup } from "../src/lib/query-lifecycle";

jest.mock("expo-network", () => ({
  addNetworkStateListener: jest.fn(),
  getNetworkStateAsync: jest.fn(),
}));

type NetState = Network.NetworkState;
const network = Network as unknown as {
  addNetworkStateListener: jest.Mock<(l: (s: NetState) => void) => { remove: () => void }>;
  getNetworkStateAsync: jest.Mock<() => Promise<NetState>>;
};

let emitAppState: (s: AppStateStatus) => void;
let emitNetwork: (s: NetState) => void;
let appStateRemove: jest.Mock;
let networkRemove: jest.Mock;
let addAppListener: jest.SpiedFunction<typeof AppState.addEventListener>;
let initialNetwork: NetState;

// Every client and unsubscribe is tracked so a failing assertion cannot leak a
// listener registration or a gc timer into the next test.
const stops: (() => void)[] = [];
const clients: QueryClient[] = [];

// The app's real defaults (mutations.networkMode "always", retry 0, ...), on a
// fresh client so cached state never leaks between tests.
const sharedDefaults = () => sharedQueryClient.getDefaultOptions();
function newClient(options?: ConstructorParameters<typeof QueryClient>[0]): QueryClient {
  const client = new QueryClient(options);
  clients.push(client);
  return client;
}
function setupQueryLifecycle(client: QueryClient): () => void {
  const stop = setup(client);
  stops.push(stop);
  return stop;
}

const flush = () => new Promise<void>((resolve) => setImmediate(resolve));

beforeEach(() => {
  appStateRemove = jest.fn();
  networkRemove = jest.fn();
  initialNetwork = { isConnected: true, isInternetReachable: true };
  addAppListener = jest.spyOn(AppState, "addEventListener").mockImplementation(((
    _type: string,
    listener: (s: AppStateStatus) => void,
  ) => {
    emitAppState = listener;
    return { remove: appStateRemove };
  }) as never);
  Object.defineProperty(AppState, "currentState", { value: "active", configurable: true });
  network.addNetworkStateListener.mockReset().mockImplementation((listener) => {
    emitNetwork = listener;
    return { remove: networkRemove };
  });
  network.getNetworkStateAsync.mockReset().mockImplementation(async () => initialNetwork);
  focusManager.setFocused(undefined);
  onlineManager.setOnline(true);
});

afterEach(() => {
  stops.splice(0).forEach((stop) => stop());
  clients.splice(0).forEach((client) => client.clear());
  addAppListener.mockRestore();
  focusManager.setFocused(undefined);
  onlineManager.setOnline(true);
});

describe("setupQueryLifecycle", () => {
  it("maps AppState to focusManager (only 'active' is focused)", () => {
    const stop = setupQueryLifecycle(newClient());
    const seen: boolean[] = [];
    const unsub = focusManager.subscribe((focused) => seen.push(focused));

    emitAppState("background");
    expect(focusManager.isFocused()).toBe(false);
    emitAppState("inactive");
    expect(focusManager.isFocused()).toBe(false);
    emitAppState("active");
    expect(focusManager.isFocused()).toBe(true);
    expect(seen).toEqual([false, true]);

    unsub();
    stop();
  });

  it("seeds focus from AppState.currentState", () => {
    Object.defineProperty(AppState, "currentState", { value: "background", configurable: true });
    const stop = setupQueryLifecycle(newClient());
    expect(focusManager.isFocused()).toBe(false);
    stop();
  });

  it("maps expo-network to onlineManager, preferring isInternetReachable", async () => {
    const stop = setupQueryLifecycle(newClient());
    await flush();
    expect(onlineManager.isOnline()).toBe(true);

    emitNetwork({ isConnected: true, isInternetReachable: false });
    expect(onlineManager.isOnline()).toBe(false);
    emitNetwork({ isConnected: true, isInternetReachable: true });
    expect(onlineManager.isOnline()).toBe(true);
    emitNetwork({ isConnected: false });
    expect(onlineManager.isOnline()).toBe(false);
    emitNetwork({ isConnected: true });
    expect(onlineManager.isOnline()).toBe(true);
    emitNetwork({});
    expect(onlineManager.isOnline()).toBe(true);
    stop();
  });

  it("applies the initial network state when the app starts offline", async () => {
    initialNetwork = { isConnected: false, isInternetReachable: false };
    const stop = setupQueryLifecycle(newClient());
    await flush();
    expect(onlineManager.isOnline()).toBe(false);
    stop();
  });

  it("lets a network event that arrives first beat the slower initial read", async () => {
    initialNetwork = { isConnected: false, isInternetReachable: false };
    const stop = setupQueryLifecycle(newClient());
    emitNetwork({ isConnected: true, isInternetReachable: true });
    await flush();
    expect(onlineManager.isOnline()).toBe(true);
    stop();
  });

  it("refetches stale queries on focus and reconnect", async () => {
    const client = newClient({
      defaultOptions: { queries: { staleTime: 0, refetchOnWindowFocus: true } },
    });
    client.mount();
    const stop = setupQueryLifecycle(client);
    const queryFn = jest.fn(async () => "v");
    const observer = new QueryObserver(client, {
      queryKey: ["k"],
      queryFn,
      staleTime: 0,
      refetchOnWindowFocus: true,
    });
    const off = observer.subscribe(() => {});
    await flush();
    const baseline = queryFn.mock.calls.length;

    emitAppState("background");
    emitAppState("active");
    await flush();
    expect(queryFn.mock.calls.length).toBeGreaterThan(baseline);

    const afterFocus = queryFn.mock.calls.length;
    emitNetwork({ isConnected: false, isInternetReachable: false });
    emitNetwork({ isConnected: true, isInternetReachable: true });
    await flush();
    expect(queryFn.mock.calls.length).toBeGreaterThan(afterFocus);

    off();
    stop();
    client.unmount();
  });

  it("does not queue mutations offline under the shared client's defaults: they run and fail, never replay", async () => {
    const client = newClient({
      defaultOptions: {
        ...sharedDefaults(),
        mutations: { ...sharedDefaults().mutations, gcTime: 0 },
      },
    });
    client.mount();
    const stop = setupQueryLifecycle(client);
    emitNetwork({ isConnected: false, isInternetReachable: false });
    expect(onlineManager.isOnline()).toBe(false);

    const mutationFn = jest.fn(async () => {
      throw new TypeError("network request failed");
    });
    const mutation = client.getMutationCache().build(client, { mutationFn });
    await expect(mutation.execute(undefined)).rejects.toThrow("network request failed");
    expect(mutationFn).toHaveBeenCalledTimes(1);
    expect(mutation.state.isPaused).toBe(false);

    emitNetwork({ isConnected: true, isInternetReachable: true });
    await flush();
    expect(mutationFn).toHaveBeenCalledTimes(1);

    stop();
    client.unmount();
  });

  it("pauses queries while offline (fetchStatus 'paused') and runs them on reconnect", async () => {
    const client = newClient({ defaultOptions: sharedDefaults() });
    client.mount();
    const stop = setupQueryLifecycle(client);
    emitNetwork({ isConnected: false, isInternetReachable: false });

    const queryFn = jest.fn(async () => "v");
    const observer = new QueryObserver(client, { queryKey: ["offline"], queryFn });
    const off = observer.subscribe(() => {});
    await flush();
    expect(queryFn).not.toHaveBeenCalled();
    expect(observer.getCurrentResult().fetchStatus).toBe("paused");

    emitNetwork({ isConnected: true, isInternetReachable: true });
    await flush();
    expect(queryFn).toHaveBeenCalledTimes(1);
    expect(observer.getCurrentResult().data).toBe("v");

    off();
    stop();
    client.unmount();
  });

  it("keeps an explicit per-mutation networkMode opt-in working (paused, then resumed)", async () => {
    const client = newClient({
      defaultOptions: {
        ...sharedDefaults(),
        mutations: { ...sharedDefaults().mutations, gcTime: 0 },
      },
    });
    client.mount();
    const stop = setupQueryLifecycle(client);
    emitNetwork({ isConnected: false, isInternetReachable: false });

    const mutationFn = jest.fn(async () => "ok");
    const mutation = client.getMutationCache().build(client, { mutationFn, networkMode: "online" });
    const done = mutation.execute(undefined);
    await flush();
    expect(mutationFn).not.toHaveBeenCalled();
    expect(mutation.state.isPaused).toBe(true);

    emitNetwork({ isConnected: true, isInternetReachable: true });
    await expect(done).resolves.toBe("ok");

    stop();
    client.unmount();
  });

  it("the shared client owns the mutation default, not the adapter", () => {
    expect(sharedDefaults().mutations?.networkMode).toBe("always");
    expect(sharedDefaults().mutations?.retry).toBe(0);
  });

  it("is idempotent: repeated setup shares one listener of each kind", () => {
    const client = newClient();
    const stopA = setupQueryLifecycle(client);
    const stopB = setupQueryLifecycle(client);
    expect(addAppListener).toHaveBeenCalledTimes(1);
    expect(network.addNetworkStateListener).toHaveBeenCalledTimes(1);

    stopA();
    expect(appStateRemove).not.toHaveBeenCalled();
    expect(networkRemove).not.toHaveBeenCalled();
    stopB();
    expect(appStateRemove).toHaveBeenCalledTimes(1);
    expect(networkRemove).toHaveBeenCalledTimes(1);
  });

  it("joins existing listeners when the module is re-evaluated (Fast Refresh)", () => {
    const client = newClient();
    const stopOld = setupQueryLifecycle(client);
    let stopNew: () => void = () => {};
    jest.isolateModules(() => {
      // isolateModules needs a synchronous load, hence require().
      type Lifecycle = typeof import("../src/lib/query-lifecycle");
      // eslint-disable-next-line @typescript-eslint/no-require-imports
      const fresh: Lifecycle = require("../src/lib/query-lifecycle");
      stopNew = fresh.setupQueryLifecycle(client);
      stops.push(stopNew);
    });
    expect(addAppListener).toHaveBeenCalledTimes(1);
    expect(network.addNetworkStateListener).toHaveBeenCalledTimes(1);
    stopOld();
    expect(appStateRemove).not.toHaveBeenCalled();
    stopNew();
    expect(appStateRemove).toHaveBeenCalledTimes(1);
    expect(networkRemove).toHaveBeenCalledTimes(1);
  });

  it("unsubscribe removes listeners, restores defaults, and is safe to call twice", async () => {
    const stop = setupQueryLifecycle(newClient());
    emitAppState("background");
    emitNetwork({ isConnected: false, isInternetReachable: false });
    expect(focusManager.isFocused()).toBe(false);
    expect(onlineManager.isOnline()).toBe(false);

    stop();
    stop();
    expect(appStateRemove).toHaveBeenCalledTimes(1);
    expect(networkRemove).toHaveBeenCalledTimes(1);
    expect(focusManager.isFocused()).toBe(true);
    expect(onlineManager.isOnline()).toBe(true);

    // A pending initial network read must not resurrect state after teardown.
    initialNetwork = { isConnected: false, isInternetReachable: false };
    const stop2 = setupQueryLifecycle(newClient());
    stop2();
    await flush();
    expect(onlineManager.isOnline()).toBe(true);

    // Setup works again after a full teardown.
    const stop3 = setupQueryLifecycle(newClient());
    expect(addAppListener).toHaveBeenCalledTimes(3);
    stop3();
  });

  it("preserves the client's other default options", () => {
    const client = newClient({
      defaultOptions: { queries: { staleTime: 123 }, mutations: { retry: 0 } },
    });
    const stop = setupQueryLifecycle(client);
    expect(client.getDefaultOptions().queries?.staleTime).toBe(123);
    expect(client.getDefaultOptions().mutations?.retry).toBe(0);
    // The adapter feeds the managers; it never rewrites the client's defaults.
    expect(client.getDefaultOptions().mutations?.networkMode).toBeUndefined();
    stop();
  });
});
