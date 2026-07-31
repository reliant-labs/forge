// Where credentials live, asserted against every write the code performs.
//
// The token-storage decision documented at the top of oidc-storage.ts is only
// real if it is enforced, so these tests do not read the source and do not
// scan a store's contents afterwards (an inert or namespaced store would make
// such a scan pass vacuously). Instead every persistent store is replaced with
// a RECORDING one, and the assertions derive from the writes that were
// actually attempted. A future edit that "just adds a localStorage.setItem for
// convenience" turns this file red with the offending key in the failure.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { createState, createVerifier } from "./oidc";
import {
  clearSession,
  getAccessToken,
  getIdToken,
  getRefreshToken,
  onSessionChange,
  setSession,
  storePendingLogin,
  takePendingLogin,
} from "./oidc-storage";

/**
 * A Storage that records every write. Substituted for the real
 * localStorage/sessionStorage so "was this persisted?" is answered by
 * observation rather than by inspecting a store whose behaviour varies with
 * the host (Node's experimental Web Storage shadows jsdom's localStorage in
 * some versions, leaving an object with no setItem at all).
 */
class RecordingStorage implements Storage {
  private readonly store = new Map<string, string>();
  readonly writes: { key: string; value: string }[] = [];

  get length(): number {
    return this.store.size;
  }
  key(index: number): string | null {
    return [...this.store.keys()][index] ?? null;
  }
  getItem(key: string): string | null {
    return this.store.get(key) ?? null;
  }
  setItem(key: string, value: string): void {
    this.writes.push({ key, value });
    this.store.set(key, value);
  }
  removeItem(key: string): void {
    this.store.delete(key);
  }
  clear(): void {
    this.store.clear();
  }
  /** Every value ever written, for "does this contain the token?" checks. */
  everyWrittenValue(): string {
    return this.writes.map((w) => `${w.key}=${w.value}`).join("\u0000");
  }
}

let local: RecordingStorage;
let session: RecordingStorage;

beforeEach(() => {
  local = new RecordingStorage();
  session = new RecordingStorage();
  vi.stubGlobal("localStorage", local);
  vi.stubGlobal("sessionStorage", session);
  clearSession();
});

afterEach(() => {
  clearSession();
  vi.unstubAllGlobals();
});

describe("the token-storage decision: in-memory only", () => {
  it("writes the access, refresh and id tokens to NO persistent store", () => {
    const accessToken = "access-token-must-not-persist-a1b2c3";
    const refreshToken = "refresh-token-must-not-persist-d4e5f6";
    const idToken = "id-token-must-not-persist-g7h8i9";

    setSession({ accessToken, refreshToken, idToken, expiresIn: 3600 });

    // In memory: available to the transport.
    expect(getAccessToken()).toBe(accessToken);
    expect(getRefreshToken()).toBe(refreshToken);
    expect(getIdToken()).toBe(idToken);

    // Persisted: nothing at all. This is the decision, mechanically enforced.
    expect(local.writes).toEqual([]);
    expect(session.writes).toEqual([]);
    expect(document.cookie).toBe("");
  });

  it("still persists nothing when a whole login is replayed end to end", () => {
    // The storage a login DOES need (verifier/state) must not become a
    // smuggling route for the token itself.
    const accessToken = "at-end-to-end-x9y8z7";
    storePendingLogin({
      verifier: createVerifier(),
      state: createState(),
      returnTo: "/items",
    });
    takePendingLogin();
    setSession({ accessToken, idToken: "it-end-to-end", expiresIn: 60 });

    expect(local.everyWrittenValue()).not.toContain(accessToken);
    expect(session.everyWrittenValue()).not.toContain(accessToken);
    expect(session.everyWrittenValue()).not.toContain("it-end-to-end");
  });

  it("stops offering an expired token instead of handing back a 401", () => {
    setSession({ accessToken: "at", expiresIn: -1 });
    expect(getAccessToken()).toBeNull();
  });

  it("keeps offering a token when the provider sent no expiry", () => {
    // There is nothing to compare against, and inventing a default lifetime
    // would sign users out on a schedule forge made up.
    setSession({ accessToken: "at" });
    expect(getAccessToken()).toBe("at");
  });

  it("reports null for everything once cleared", () => {
    setSession({ accessToken: "at", refreshToken: "rt", idToken: "it" });
    clearSession();
    expect(getAccessToken()).toBeNull();
    expect(getRefreshToken()).toBeNull();
    expect(getIdToken()).toBeNull();
  });

  it("notifies subscribers when the session appears and disappears", () => {
    const seen: (string | null)[] = [];
    const off = onSessionChange(() => seen.push(getAccessToken()));
    setSession({ accessToken: "at-1" });
    clearSession();
    off();
    setSession({ accessToken: "at-2" });
    expect(seen).toEqual(["at-1", null]);
  });
});

describe("the redirect round-trip values", () => {
  it("writes the verifier and state to sessionStorage and never localStorage", () => {
    // These MUST survive the redirect — the page is destroyed and rebuilt by
    // the provider — so they are the one thing deliberately persisted.
    // sessionStorage is per-tab and dies with the tab.
    const verifier = createVerifier();
    const state = createState();
    storePendingLogin({ verifier, state, returnTo: "/items" });

    expect(local.writes).toEqual([]);
    expect(session.writes.map((w) => w.key)).toEqual([
      "forge.oidc.verifier",
      "forge.oidc.state",
      "forge.oidc.returnTo",
    ]);
    expect(session.getItem("forge.oidc.verifier")).toBe(verifier.secret());
    expect(session.getItem("forge.oidc.state")).toBe(state);
  });

  it("round-trips the verifier, state and destination", () => {
    const verifier = createVerifier();
    const state = createState();
    storePendingLogin({ verifier, state, returnTo: "/items/42" });

    const pending = takePendingLogin();
    expect(pending).not.toBeNull();
    expect(pending?.verifier.secret()).toBe(verifier.secret());
    expect(pending?.state).toBe(state);
    expect(pending?.returnTo).toBe("/items/42");
  });

  it("DELETES the pending login on first read, so a replay fails closed", () => {
    storePendingLogin({
      verifier: createVerifier(),
      state: createState(),
      returnTo: "/",
    });

    expect(takePendingLogin()).not.toBeNull();
    // A reloaded, shared or bookmarked callback URL finds nothing.
    expect(takePendingLogin()).toBeNull();
    expect(session.length).toBe(0);
  });

  it("returns null when nothing is pending", () => {
    expect(takePendingLogin()).toBeNull();
  });

  it("refuses a tampered verifier rather than sending it to the IdP", () => {
    storePendingLogin({
      verifier: createVerifier(),
      state: createState(),
      returnTo: "/",
    });
    // Overwrite with something outside RFC 7636's alphabet, as a hostile or
    // buggy writer would.
    session.setItem("forge.oidc.verifier", "!!!not-a-legal-verifier!!!");
    expect(takePendingLogin()).toBeNull();
  });

  it("fails closed when storage cannot be read at all", () => {
    vi.stubGlobal("sessionStorage", {
      get length(): number {
        throw new Error("storage blocked");
      },
      key: () => null,
      getItem: () => {
        throw new Error("storage blocked");
      },
      setItem: () => {
        throw new Error("storage blocked");
      },
      removeItem: () => undefined,
      clear: () => undefined,
    });
    // Unreadable storage is indistinguishable from empty for this decision:
    // either way there is no verifier, so the flow must not continue.
    expect(takePendingLogin()).toBeNull();
  });

  it("fails loudly at login when the verifier cannot be stored", () => {
    // The alternative is a login that fails LATER, at the callback, where the
    // cause is no longer visible.
    vi.stubGlobal("sessionStorage", {
      length: 0,
      key: () => null,
      getItem: () => null,
      setItem: () => {
        throw new Error("storage blocked");
      },
      removeItem: () => undefined,
      clear: () => undefined,
    });
    expect(() =>
      storePendingLogin({
        verifier: createVerifier(),
        state: createState(),
        returnTo: "/",
      }),
    ).toThrow(/sessionStorage is unavailable/);
  });

  it("defaults the destination to / when none was recorded", () => {
    storePendingLogin({
      verifier: createVerifier(),
      state: createState(),
      returnTo: "",
    });
    expect(takePendingLogin()?.returnTo).toBe("/");
  });
});
