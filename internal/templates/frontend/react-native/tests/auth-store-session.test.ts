// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { describe, expect, it, jest } from "@jest/globals";

import { QueryClient } from "@tanstack/react-query";

import { createSession } from "../src/lib/auth/session";
import {
  StorageError,
  StorageUnavailableError,
  createTokenStore,
  storageNamespace,
  type StoredTokens,
} from "../src/lib/auth/token-store";
import { CLIENT_ID, FakeMarker, FakeSecureStore, ISSUER } from "./auth-fakes";

const tokens: StoredTokens = {
  accessToken: "A",
  refreshToken: "R",
  idToken: "I",
  expiresAt: 123456,
};

describe("storageNamespace", () => {
  it("only emits SecureStore-legal characters", () => {
    const ns = storageNamespace(
      "https://login.example.com:8443/path?q=1",
      "client id/with spaces:and\u00e9",
    );
    expect(ns).toMatch(/^[A-Za-z0-9._-]+$/);
  });
  it("separates different issuers and different clients", () => {
    const a = storageNamespace(ISSUER, CLIENT_ID);
    expect(storageNamespace("https://other.example.com", CLIENT_ID)).not.toBe(a);
    expect(storageNamespace(ISSUER, "another-client")).not.toBe(a);
    expect(storageNamespace(ISSUER, CLIENT_ID)).toBe(a);
  });
  it("does not let two pairs collapse by sanitising", () => {
    expect(storageNamespace("https://a-b.example.com", "c")).not.toBe(
      storageNamespace("https://a.b.example.com", "c"),
    );
  });
});

describe("token store", () => {
  const ns = storageNamespace(ISSUER, CLIENT_ID);
  const make = (secureStore = new FakeSecureStore(), launchMarker = new FakeMarker(true)) => ({
    secureStore,
    launchMarker,
    store: createTokenStore({ namespace: ns, secureStore, launchMarker }),
  });

  it("round-trips tokens, with valid keys and the AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY accessibility", async () => {
    const { store, secureStore } = make();
    expect(await store.load()).toBeNull();
    await store.save(tokens);
    expect(await store.load()).toEqual(tokens);
    expect(secureStore.optionsSeen.length).toBeGreaterThan(0);
    expect(new Set(secureStore.optionsSeen)).toEqual(new Set([3]));
    for (const key of secureStore.items.keys()) expect(key).toMatch(/^[A-Za-z0-9._-]+$/);
    expect([...secureStore.items.keys()].every((k) => k.includes(ns))).toBe(true);
  });

  it("stores each token under its own key and drops an absent refresh token", async () => {
    const { store, secureStore } = make();
    await store.save(tokens);
    expect(secureStore.items.size).toBe(4);
    await store.save({ ...tokens, accessToken: "A2", refreshToken: null });
    expect(secureStore.items.size).toBe(3);
    expect(await store.load()).toEqual({ ...tokens, accessToken: "A2", refreshToken: null });
  });

  it("serializes operations: a clear() issued during a save() runs after it", async () => {
    const { store, secureStore } = make();
    secureStore.latencyMs = 1;
    const save = store.save(tokens);
    const clear = store.clear(); // issued while save's keychain writes are in flight
    await Promise.all([save, clear]);
    expect([...secureStore.items.keys()].filter((k) => !k.endsWith(".relogin"))).toEqual([]);
    expect(await store.load()).toBeNull();
  });

  it("keeps working after a failed operation", async () => {
    const { store, secureStore } = make();
    secureStore.failWith = new Error("locked");
    await expect(store.save(tokens)).rejects.toBeInstanceOf(StorageError);
    secureStore.failWith = null;
    await store.save(tokens);
    expect(await store.load()).toEqual(tokens);
  });

  type Record = { access?: string; refresh?: string; id?: string; expires?: string };
  it.each<[string, Record]>([
    ["only an access token", { access: "A" }],
    ["no id token", { access: "A", refresh: "R", expires: "123456" }],
    ["no expiry", { access: "A", refresh: "R", id: "I" }],
    ["a non-numeric expiry", { access: "A", refresh: "R", id: "I", expires: "soon" }],
    ["no access token", { refresh: "R", id: "I", expires: "123456" }],
  ])("treats a stored record with %s as no session and deletes the remainder", async (_n, rec) => {
    const { store, secureStore } = make();
    for (const [name, value] of Object.entries(rec)) {
      secureStore.items.set(`forge.auth.${ns}.${name}`, value);
    }
    secureStore.items.set(`forge.auth.${ns}.relogin`, "1");
    expect(await store.load()).toBeNull();
    expect([...secureStore.items.keys()]).toEqual([`forge.auth.${ns}.relogin`]);
  });

  it("isolates namespaces", async () => {
    const secureStore = new FakeSecureStore();
    const a = createTokenStore({
      namespace: storageNamespace(ISSUER, "a"),
      secureStore,
      launchMarker: new FakeMarker(),
    });
    const b = createTokenStore({
      namespace: storageNamespace(ISSUER, "b"),
      secureStore,
      launchMarker: new FakeMarker(),
    });
    await a.save(tokens);
    expect(await b.load()).toBeNull();
  });

  it("clear() removes the session but keeps the forced-login flag", async () => {
    const { store, secureStore } = make();
    await store.save(tokens);
    await store.setForceLogin(true);
    await store.clear();
    expect(await store.load()).toBeNull();
    expect(await store.getForceLogin()).toBe(true);
    await store.setForceLogin(false);
    expect(await store.getForceLogin()).toBe(false);
    expect(secureStore.items.size).toBe(0);
  });

  it("wipes a surviving keychain on first launch (iOS keychain outlives uninstall), exactly once", async () => {
    const secureStore = new FakeSecureStore();
    const old = createTokenStore({
      namespace: ns,
      secureStore,
      launchMarker: new FakeMarker(true),
    });
    await old.save(tokens);
    await old.setForceLogin(true);

    const marker = new FakeMarker(false); // fresh install: the app-container marker is gone
    const reinstalled = createTokenStore({ namespace: ns, secureStore, launchMarker: marker });
    expect(await reinstalled.load()).toBeNull();
    expect(await reinstalled.getForceLogin()).toBe(false);
    expect(marker.value).toBe(true);

    await reinstalled.save(tokens);
    const nextLaunch = createTokenStore({ namespace: ns, secureStore, launchMarker: marker });
    expect(await nextLaunch.load()).toEqual(tokens);
  });

  it("does not set the marker if the wipe fails, so the wipe is retried", async () => {
    const secureStore = new FakeSecureStore();
    const marker = new FakeMarker(false);
    const store = createTokenStore({ namespace: ns, secureStore, launchMarker: marker });
    secureStore.failWith = new Error("keystore locked");
    await expect(store.load()).rejects.toBeInstanceOf(StorageError);
    expect(marker.value).toBe(false);
    secureStore.failWith = null;
    expect(await store.load()).toBeNull();
    expect(marker.value).toBe(true);
  });

  it("throws StorageUnavailableError when the platform has no secure storage", async () => {
    const secureStore = new FakeSecureStore();
    secureStore.available = false;
    const { store } = make(secureStore);
    await expect(store.load()).rejects.toBeInstanceOf(StorageUnavailableError);
    await expect(store.save(tokens)).rejects.toBeInstanceOf(StorageUnavailableError);
    expect(secureStore.items.size).toBe(0);
  });

  it("wraps keystore failures in StorageError without leaking token values", async () => {
    const { store, secureStore } = make();
    secureStore.failWith = new Error("User canceled the operation.");
    const err = await store.save(tokens).catch((e) => e);
    expect(err).toBeInstanceOf(StorageError);
    expect(String(err.message)).not.toContain("A");
    expect(String((err as Error).message)).toBe("secure storage write failed");
  });

  it("rejects an illegal namespace up front", () => {
    expect(() =>
      createTokenStore({ namespace: "bad ns/", secureStore: new FakeSecureStore() }),
    ).toThrow(/namespace/);
  });
});

describe("session (identity epoch)", () => {
  it("starts at epoch 0 and bump aborts the previous signal and issues a new live one", () => {
    const s = createSession();
    const first = s.currentSignal();
    expect(s.epoch()).toBe(0);
    expect(s.isCurrent(0)).toBe(true);
    expect(first.aborted).toBe(false);
    expect(s.bumpEpoch()).toBe(1);
    expect(first.aborted).toBe(true);
    expect(s.currentSignal().aborted).toBe(false);
    expect(s.currentSignal()).not.toBe(first);
    expect(s.isCurrent(0)).toBe(false);
    expect(s.isCurrent(1)).toBe(true);
  });

  it("endSession bumps → aborts → cancelQueries → clear, in that order", async () => {
    const s = createSession();
    const signal = s.currentSignal();
    const order: string[] = [];
    signal.addEventListener("abort", () => order.push("abort"));
    const qc = {
      cancelQueries: jest.fn(async () => {
        order.push(`cancel@epoch${s.epoch()}`);
      }),
      clear: jest.fn(() => {
        order.push("clear");
      }),
    };
    await s.endSession(qc);
    expect(order).toEqual(["abort", "cancel@epoch1", "clear"]);
  });

  it("still clears the cache when cancelQueries rejects", async () => {
    const s = createSession();
    const qc = {
      cancelQueries: jest.fn(async () => {
        throw new Error("boom");
      }),
      clear: jest.fn(),
    };
    await expect(s.endSession(qc)).resolves.toBeUndefined();
    expect(qc.clear).toHaveBeenCalledTimes(1);
  });

  it("works against a real TanStack QueryClient", async () => {
    const s = createSession();
    const client = new QueryClient();
    client.setQueryData(["x"], { account: "old" });
    await s.endSession(client);
    expect(client.getQueryData(["x"])).toBeUndefined();
    client.clear();
  });
});
