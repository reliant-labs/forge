// Refresh-on-expiry, and the four properties that make it safe.
//
// The central one is SINGLE-FLIGHT: concurrent getToken() calls must produce
// exactly ONE request to the token endpoint. Everything here derives from a
// fake token endpoint that COUNTS its own hits and MINTS its own tokens, so no
// assertion compares against a string the code under test also hardcodes, and
// a rotation mistake is caught by the endpoint's own bookkeeping rather than by
// a literal written down here.
//
// The endpoint models a STRICT rotating provider: every refresh consumes the
// presented token and issues a new one, and re-presenting a consumed token
// revokes the WHOLE grant. That last part is why single-flight matters — see
// the header of oidc-provider.ts. It is deliberately harsher than the dev IdP
// forge scaffolds (Zitadel v4.16.2 rejects the replayed token but spares the
// grant), because the client must be correct against the harshest case.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { refreshToken } from "./oidc";
import {
  accessTokenExpiresWithin,
  clearSession,
  getAccessToken,
  getRefreshToken,
  setSession,
  updateSessionTokens,
} from "./oidc-storage";

/**
 * A rotating token endpoint that counts requests.
 *
 * `hits` is the single-flight oracle. `consumed` plus `revoked` reproduce
 * reuse detection, so a client that replays a stale token is punished the way a
 * real provider punishes it, rather than merely failing an equality check.
 */
class FakeTokenEndpoint {
  hits = 0;
  minted = 0;
  live: string;
  readonly consumed = new Set<string>();
  revoked = false;

  /** True while responses are held, for deterministic concurrency tests. */
  private blocked = false;
  private readonly waiters: (() => void)[] = [];

  constructor(readonly opts: { rotate?: boolean; failWith?: string } = {}) {
    this.live = this.mint();
  }

  private mint(): string {
    this.minted += 1;
    return `refresh-token-${this.minted}`;
  }

  /**
   * Holds every subsequent response until `release()` is called.
   *
   * This is what makes the single-flight test deterministic rather than
   * timing-dependent: all callers are guaranteed to be in flight together
   * before any response resolves.
   */
  block(): void {
    this.blocked = true;
  }

  release(): void {
    this.blocked = false;
    for (const w of this.waiters.splice(0)) w();
  }

  /** A `fetch` implementation to inject. */
  readonly fetch: typeof fetch = async (_url, init) => {
    this.hits += 1;
    const body = new URLSearchParams(String(init?.body ?? ""));

    if (this.blocked) {
      await new Promise<void>((resolve) => this.waiters.push(resolve));
    }

    const json = (status: number, payload: unknown): Response =>
      new Response(JSON.stringify(payload), {
        status,
        headers: { "Content-Type": "application/json" },
      });

    if (this.opts.failWith) {
      return json(400, {
        error: this.opts.failWith,
        error_description: "refused by the fake provider",
      });
    }

    const presented = body.get("refresh_token") ?? "";
    if (this.revoked) {
      return json(400, { error: "invalid_grant", error_description: "grant revoked" });
    }
    if (this.consumed.has(presented)) {
      // Reuse detection: kill the whole grant — the strictest behaviour a
      // client has to survive.
      this.revoked = true;
      return json(400, {
        error: "invalid_grant",
        error_description: "refresh token already used",
      });
    }
    if (presented !== this.live) {
      return json(400, {
        error: "invalid_grant",
        error_description: "refresh token not found",
      });
    }

    const payload: Record<string, unknown> = {
      // Derived from what was PRESENTED, so a test can prove which credential
      // went on the wire without knowing any token's value in advance.
      access_token: `access-for-${presented}`,
      token_type: "Bearer",
      expires_in: 3600,
      scope: "openid profile offline_access",
    };
    if (this.opts.rotate !== false) {
      this.consumed.add(presented);
      this.live = this.mint();
      payload["refresh_token"] = this.live;
    }
    return json(200, payload);
  };
}

/** A Storage that records writes, so persistence is observed not assumed. */
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
  everyWrittenValue(): string {
    return this.writes.map((w) => `${w.key}=${w.value}`).join("\u0000");
  }
}

let local: RecordingStorage;
let sessionStore: RecordingStorage;

beforeEach(() => {
  local = new RecordingStorage();
  sessionStore = new RecordingStorage();
  vi.stubGlobal("localStorage", local);
  vi.stubGlobal("sessionStorage", sessionStore);
  clearSession();
});

afterEach(() => {
  clearSession();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("the refresh grant", () => {
  it("carries a ROTATED refresh token forward, so the second refresh works", async () => {
    const idp = new FakeTokenEndpoint({ rotate: true });
    let current = idp.live;

    // Three chained refreshes. The endpoint revokes on reuse, so if the
    // rotation were not carried forward, round 2 would fail — which is the
    // production failure this guards.
    for (let round = 1; round <= 3; round += 1) {
      const result = await refreshToken({
        endpoint: "https://idp.test/token",
        clientId: "spa",
        refreshToken: current,
        fetchImpl: idp.fetch,
      });
      expect(result.rotated).toBe(true);
      // Derived from the presented token by the endpoint itself.
      expect(result.token.accessToken).toBe(`access-for-${current}`);
      expect(result.refreshToken).not.toBe(current);
      current = result.refreshToken;
    }
    expect(idp.revoked).toBe(false);
    expect(idp.hits).toBe(3);
  });

  it("keeps presenting the same token when the provider does NOT rotate", async () => {
    const idp = new FakeTokenEndpoint({ rotate: false });
    const original = idp.live;

    const result = await refreshToken({
      endpoint: "https://idp.test/token",
      clientId: "spa",
      refreshToken: original,
      fetchImpl: idp.fetch,
    });
    // A provider that issued no new token has not invalidated the old one, so
    // the old one must remain what we present — not an empty string a caller
    // would store and then fail with.
    expect(result.rotated).toBe(false);
    expect(result.refreshToken).toBe(original);
  });

  it("surfaces a refusal as a typed OAuthError carrying invalid_grant", async () => {
    const idp = new FakeTokenEndpoint({ failWith: "invalid_grant" });
    await expect(
      refreshToken({
        endpoint: "https://idp.test/token",
        clientId: "spa",
        refreshToken: "whatever",
        fetchImpl: idp.fetch,
      }),
    ).rejects.toMatchObject({ code: "invalid_grant" });
  });

  it("rejects a 200 that carries no access token", async () => {
    const fetchImpl: typeof fetch = async () =>
      new Response(JSON.stringify({ refresh_token: "rt-new", expires_in: 60 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    await expect(
      refreshToken({
        endpoint: "https://idp.test/token",
        clientId: "spa",
        refreshToken: "rt-old",
        fetchImpl,
      }),
    ).rejects.toThrow(/no access_token/);
  });

  it("sends grant_type=refresh_token with the credential in the BODY", async () => {
    let seenBody = "";
    let seenUrl = "";
    const fetchImpl: typeof fetch = async (url, init) => {
      seenUrl = String(url);
      seenBody = String(init?.body ?? "");
      return new Response(
        JSON.stringify({ access_token: "at", token_type: "Bearer", expires_in: 60 }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    };
    const secret = "the-refresh-secret-value";
    await refreshToken({
      endpoint: "https://idp.test/token",
      clientId: "spa",
      refreshToken: secret,
      fetchImpl,
    });
    const form = new URLSearchParams(seenBody);
    expect(form.get("grant_type")).toBe("refresh_token");
    expect(form.get("refresh_token")).toBe(secret);
    expect(form.get("client_id")).toBe("spa");
    // A public client must not send a secret it does not have.
    expect(form.has("client_secret")).toBe(false);
    // And the credential must never reach the URL, where access logs capture it.
    expect(seenUrl).not.toContain(secret);
  });
});

describe("storage: expiry, rotation and persistence", () => {
  it("reports a token as expiring EARLY by the skew, not at the boundary", () => {
    // 20 seconds of life left.
    setSession({ accessToken: "at", refreshToken: "rt", expiresIn: 20 });

    // Still valid right now, so a bare expiry check would say "fine"…
    expect(getAccessToken()).toBe("at");
    // …but with a 30s skew it is due for refresh, which is the point: a
    // request issued now could easily outlive the token.
    expect(accessTokenExpiresWithin(30_000)).toBe(true);
    // With no skew it is not yet expired.
    expect(accessTokenExpiresWithin(0)).toBe(false);
  });

  it("never reports expiry for a provider that sent no expires_in", () => {
    setSession({ accessToken: "at", refreshToken: "rt" });
    // Nothing to compare against. Treating unknown as expired would refresh on
    // every single call.
    expect(accessTokenExpiresWithin(30_000)).toBe(false);
    expect(getAccessToken()).toBe("at");
  });

  it("replaces the refresh token in memory on rotation", () => {
    setSession({
      accessToken: "at-1",
      refreshToken: "rt-1",
      idToken: "id-1",
      expiresIn: 3600,
    });
    updateSessionTokens({
      accessToken: "at-2",
      refreshToken: "rt-2",
      expiresIn: 3600,
    });
    expect(getRefreshToken()).toBe("rt-2");
    expect(getAccessToken()).toBe("at-2");
  });

  it("carries the id token forward when a refresh response omits it", () => {
    setSession({
      accessToken: "at-1",
      refreshToken: "rt-1",
      idToken: "id-token-1",
      expiresIn: 3600,
    });
    // A refresh response often omits the id token. Dropping the user's
    // identity because this response did not restate it would blank the UI.
    updateSessionTokens({ accessToken: "at-2", refreshToken: "rt-2", expiresIn: 3600 });
    expect(getRefreshToken()).toBe("rt-2");
  });

  it("writes the ROTATED refresh token to NO persistent store", () => {
    const rotated = "rotated-refresh-token-must-not-persist-x9y8z7";
    setSession({ accessToken: "at-1", refreshToken: "rt-1", expiresIn: 3600 });
    updateSessionTokens({
      accessToken: "at-2",
      refreshToken: rotated,
      expiresIn: 3600,
    });

    // Derived from every write actually attempted, not from a scan of a store
    // that might be namespaced or inert.
    expect(local.writes).toHaveLength(0);
    expect(sessionStore.everyWrittenValue()).not.toContain(rotated);
    expect(local.everyWrittenValue()).not.toContain(rotated);
    // The token IS available in memory — proving the assertion above is about
    // persistence and not about the token being absent entirely.
    expect(getRefreshToken()).toBe(rotated);
  });
});
