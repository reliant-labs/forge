// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { describe, expect, it, jest } from "@jest/globals";

import {
  AuthorizationError,
  MissingCodeError,
  OfflineError,
  StateMismatchError,
  base64UrlEncode,
  type TokenEndpointError,
} from "../src/lib/auth/oidc";
import {
  BrowserBusyError,
  createOidcAuthProvider,
  type OidcProviderDeps,
  type SessionEndReason,
} from "../src/lib/auth/oidc-provider";
import { createSession } from "../src/lib/auth/session";
import { createTokenStore, storageNamespace, type StoredTokens } from "../src/lib/auth/token-store";
import {
  CLIENT_ID,
  FakeBrowser,
  FakeIdp,
  FakeMarker,
  FakeSecureStore,
  REDIRECT,
  config,
  nodeCrypto,
  sha256,
  successScript,
  utf8,
  type BrowserScript,
} from "./auth-fakes";

function setup(overrides: Partial<OidcProviderDeps> = {}, cfg = config) {
  const idp = new FakeIdp();
  const secureStore = new FakeSecureStore();
  const clock = { t: 1_700_000_000_000 };
  const store = createTokenStore({
    namespace: storageNamespace(cfg.issuer, cfg.clientId),
    secureStore,
    launchMarker: new FakeMarker(true),
  });
  const session = createSession();
  const browser = new FakeBrowser(idp);
  const ended: SessionEndReason[] = [];
  const provider = createOidcAuthProvider(cfg, {
    fetch: idp.fetch,
    crypto: nodeCrypto,
    browser,
    store,
    session,
    now: () => clock.t,
    discoveryCache: new Map(),
    onSessionEnded: (r) => ended.push(r),
    ...overrides,
  });
  return { idp, secureStore, store, session, browser, provider, clock, ended };
}

const stored = (store: ReturnType<typeof setup>["store"]) => store.load();

describe("signIn", () => {
  it("runs code + PKCE in the system browser and persists the session", async () => {
    const { provider, idp, browser, store } = setup();
    const user = await provider.signIn();
    expect(user).toEqual({ id: "user-1", email: "user-1@example.com", name: "Name user-1" });
    expect(provider.getUser()).toEqual(user);
    expect(await provider.getToken()).toBe("access-1");

    expect(browser.opened).toHaveLength(1);
    const { url, redirectUri, ephemeral } = browser.opened[0];
    expect(ephemeral).toBe(true);
    expect(redirectUri).toBe(REDIRECT); // passed through unchanged
    expect(url.searchParams.get("code_challenge_method")).toBe("S256");
    expect(url.searchParams.get("prompt")).toBeNull();

    // The verifier sent to the token endpoint hashes to the challenge sent to the browser.
    const exchange = idp.tokenRequests()[0].form;
    expect(exchange.code).toBe("the-code");
    expect(base64UrlEncode(sha256(utf8(exchange.code_verifier)))).toBe(
      url.searchParams.get("code_challenge"),
    );
    expect(exchange.code_verifier).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(await stored(store)).toMatchObject({
      accessToken: "access-1",
      refreshToken: "refresh-1",
    });
  });

  it("uses a fresh state and nonce per attempt, each ≥ 256 bits", async () => {
    const { provider, browser } = setup();
    await provider.signIn();
    await provider.signOut();
    await provider.signIn();
    const [a, b] = browser.opened.map((o) => o.url.searchParams);
    expect(a.get("state")).not.toBe(b.get("state"));
    expect(a.get("nonce")).not.toBe(b.get("nonce"));
    expect(a.get("state")).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(a.get("nonce")).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it.each(["cancel", "dismiss", "opened"])(
    "resolves null (not an error) when the browser reports %s",
    async (type) => {
      const { provider, browser, idp, store } = setup();
      browser.script = () => ({ type });
      await expect(provider.signIn()).resolves.toBeNull();
      expect(provider.getUser()).toBeNull();
      expect(idp.tokenRequests()).toHaveLength(0);
      expect(await stored(store)).toBeNull();
    },
  );

  it("throws BrowserBusyError when another auth session holds the browser", async () => {
    const { provider, browser } = setup();
    browser.script = () => ({ type: "locked" });
    await expect(provider.signIn()).rejects.toBeInstanceOf(BrowserBusyError);
  });

  it("a state mismatch is a typed error and never reaches the token endpoint", async () => {
    const { provider, browser, idp } = setup();
    browser.script = (u) => ({
      type: "success",
      url: `${u.searchParams.get("redirect_uri")}?code=c&state=forged`,
    });
    await expect(provider.signIn()).rejects.toBeInstanceOf(StateMismatchError);
    expect(idp.tokenRequests()).toHaveLength(0);
    expect(provider.getUser()).toBeNull();
  });

  it("a callback without a code throws MissingCodeError", async () => {
    const { provider, browser } = setup();
    browser.script = (u) => ({
      type: "success",
      url: `${REDIRECT}?state=${u.searchParams.get("state")}`,
    });
    await expect(provider.signIn()).rejects.toBeInstanceOf(MissingCodeError);
  });

  it("error= in the callback throws AuthorizationError with the IdP's code", async () => {
    const { provider, browser } = setup();
    browser.script = (u) => ({
      type: "success",
      url: `${REDIRECT}?error=access_denied&state=${u.searchParams.get("state")}`,
    });
    const err = await provider.signIn().catch((e) => e);
    expect(err).toBeInstanceOf(AuthorizationError);
    expect(err.code).toBe("access_denied");
  });

  it("rejects an id token whose nonce does not match", async () => {
    const { provider, idp, store } = setup();
    idp.idTokenClaims = { nonce: "replayed" };
    await expect(provider.signIn()).rejects.toThrow(/nonce/);
    expect(provider.getUser()).toBeNull();
    expect(await stored(store)).toBeNull();
  });

  it("a second concurrent signIn joins the first: one browser, one exchange", async () => {
    const { provider, browser, idp } = setup();
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const script: BrowserScript = async (u) => {
      await gate;
      return successScript(u);
    };
    browser.script = script;
    const first = provider.signIn();
    const second = provider.signIn();
    release();
    const [a, b] = await Promise.all([first, second]);
    expect(a).toEqual(b);
    expect(browser.opened).toHaveLength(1);
    expect(idp.tokenRequests()).toHaveLength(1);
    // And a later signIn is a new attempt.
    await provider.signIn();
    expect(browser.opened).toHaveLength(2);
  });

  it("accepts an https universal-link redirect end to end", async () => {
    const https = { ...config, redirectUri: "https://app.example.com/oauthredirect" };
    const { provider, browser } = setup({}, https);
    await expect(provider.signIn()).resolves.toMatchObject({ id: "user-1" });
    expect(browser.opened[0].redirectUri).toBe("https://app.example.com/oauthredirect");
  });

  it("signing in as a different account ends the previous identity first", async () => {
    const { provider, idp, ended, session } = setup();
    await provider.signIn();
    const epoch = session.epoch();
    idp.sub = "user-2";
    await provider.signIn();
    expect(ended).toEqual(["account_changed"]);
    expect(session.epoch()).toBeGreaterThan(epoch);
    expect(provider.getUser()?.id).toBe("user-2");
  });

  it("drops a sign-in whose exchange lands after a sign-out", async () => {
    const { provider, idp, store, session } = setup();
    idp.tokenDelayMs = 20;
    const pending = provider.signIn();
    await new Promise((r) => setTimeout(r, 5));
    session.bumpEpoch();
    await expect(pending).resolves.toBeNull();
    expect(provider.getUser()).toBeNull();
    expect(await stored(store)).toBeNull();
  });
});

describe("restore", () => {
  const seed = async (s: ReturnType<typeof setup>, over: Partial<StoredTokens> = {}) => {
    const first = await (async () => {
      await s.provider.signIn();
      return s.store.load();
    })();
    await s.store.save({ ...(first as StoredTokens), ...over });
  };

  it("returns the persisted user without any network call when the token is fresh", async () => {
    const a = setup();
    await a.provider.signIn();
    const b = setup({ store: a.store, fetch: a.idp.fetch });
    const calls = a.idp.requests.length;
    const user = await b.provider.restore();
    expect(user?.id).toBe("user-1");
    expect(await b.provider.getToken()).toBe("access-1");
    expect(a.idp.requests).toHaveLength(calls);
  });

  it("returns null when nothing is stored", async () => {
    const { provider } = setup();
    expect(await provider.restore()).toBeNull();
    expect(await provider.getToken()).toBeNull();
  });

  it("refreshes at launch when the token is already stale", async () => {
    const s = setup();
    await seed(s);
    s.clock.t += 2 * 3600 * 1000;
    const fresh = setup({ store: s.store, fetch: s.idp.fetch, now: () => s.clock.t });
    await fresh.provider.restore();
    expect(await fresh.provider.getToken()).toBe("access-2");
  });

  it("stays signed in when launched offline with an expired token; getToken reports offline", async () => {
    const s = setup();
    await seed(s);
    s.clock.t += 2 * 3600 * 1000;
    s.idp.offline = true;
    const offline = setup({ store: s.store, fetch: s.idp.fetch, now: () => s.clock.t });
    expect((await offline.provider.restore())?.id).toBe("user-1");
    await expect(offline.provider.getToken()).rejects.toBeInstanceOf(OfflineError);
    expect(offline.provider.getUser()?.id).toBe("user-1");
    expect(await s.store.load()).not.toBeNull();
  });
});

describe("getToken / refresh", () => {
  it("returns the cached token with no network while it is outside the 60s skew", async () => {
    const { provider, idp, clock } = setup();
    await provider.signIn();
    const calls = idp.requests.length;
    clock.t += (3600 - 61) * 1000;
    expect(await provider.getToken()).toBe("access-1");
    expect(idp.requests).toHaveLength(calls);
  });

  it("refreshes inside the skew window and persists the rotated tokens", async () => {
    const { provider, idp, clock, store } = setup();
    await provider.signIn();
    clock.t += (3600 - 30) * 1000;
    expect(await provider.getToken()).toBe("access-2");
    const forms = idp.tokenRequests().map((r) => r.form);
    expect(forms[1]).toEqual({
      grant_type: "refresh_token",
      refresh_token: "refresh-1",
      client_id: CLIENT_ID,
    });
    expect(await store.load()).toMatchObject({
      accessToken: "access-2",
      refreshToken: "refresh-2",
    });
    expect(provider.getUser()?.id).toBe("user-1");
  });

  it("is single-flight: many concurrent callers share one refresh request", async () => {
    const { provider, idp, clock } = setup();
    await provider.signIn();
    clock.t += 3600 * 1000;
    idp.tokenDelayMs = 15;
    const tokens = await Promise.all(Array.from({ length: 25 }, () => provider.getToken()));
    expect(new Set(tokens)).toEqual(new Set(["access-2"]));
    expect(idp.tokenRequests().filter((r) => r.form.grant_type === "refresh_token")).toHaveLength(
      1,
    );
  });

  it("keeps the old refresh token when the IdP does not rotate it", async () => {
    const { provider, idp, clock, store } = setup();
    await provider.signIn();
    clock.t += 3600 * 1000;
    idp.nextTokenReply = async () => ({
      status: 200,
      body: { access_token: "norotate", token_type: "Bearer", expires_in: 3600 },
    });
    expect(await provider.getToken()).toBe("norotate");
    expect(await store.load()).toMatchObject({
      accessToken: "norotate",
      refreshToken: "refresh-1",
    });
    expect(provider.getUser()?.id).toBe("user-1");
  });

  it("invalid_grant wipes the session and signs out once, with no retry loop", async () => {
    const { provider, idp, clock, store, ended } = setup();
    await provider.signIn();
    idp.validRefresh.clear();
    clock.t += 3600 * 1000;
    expect(await provider.getToken()).toBeNull();
    expect(provider.getUser()).toBeNull();
    expect(await store.load()).toBeNull();
    expect(ended).toEqual(["invalid_grant"]);
    const refreshes = () =>
      idp.tokenRequests().filter((r) => r.form.grant_type === "refresh_token").length;
    expect(refreshes()).toBe(1);
    // Signed out: further calls do not hit the network.
    expect(await provider.getToken()).toBeNull();
    expect(await provider.getToken()).toBeNull();
    expect(refreshes()).toBe(1);
  });

  it("concurrent callers on a dead grant all get null from one refresh", async () => {
    const { provider, idp, clock, ended } = setup();
    await provider.signIn();
    idp.validRefresh.clear();
    clock.t += 3600 * 1000;
    const results = await Promise.all([
      provider.getToken(),
      provider.getToken(),
      provider.getToken(),
    ]);
    expect(results).toEqual([null, null, null]);
    expect(ended).toEqual(["invalid_grant"]);
    expect(idp.tokenRequests().filter((r) => r.form.grant_type === "refresh_token")).toHaveLength(
      1,
    );
  });

  it("offline: still-valid token is served (session kept); expired token throws OfflineError (session kept)", async () => {
    const { provider, idp, clock, store } = setup();
    await provider.signIn();
    idp.offline = true;
    clock.t += (3600 - 30) * 1000; // inside skew, not expired
    expect(await provider.getToken()).toBe("access-1");
    clock.t += 60 * 1000; // now expired
    await expect(provider.getToken()).rejects.toBeInstanceOf(OfflineError);
    expect(provider.getUser()?.id).toBe("user-1");
    expect(await store.load()).toMatchObject({ refreshToken: "refresh-1" });
    // Back online: recovers on the next call.
    idp.offline = false;
    expect(await provider.getToken()).toBe("access-2");
  });

  it("an IdP outage (5xx) behaves like offline: keep the session", async () => {
    const { provider, idp, clock } = setup();
    await provider.signIn();
    clock.t += 3600 * 1000;
    idp.nextTokenReply = async () => ({ status: 503, body: {} });
    const err = (await provider.getToken().catch((e) => e)) as TokenEndpointError;
    expect(err.status).toBe(503);
    expect(provider.getUser()?.id).toBe("user-1");
  });

  it("a session with no refresh token ends when its access token expires", async () => {
    const { provider, idp, clock, ended } = setup();
    idp.nextTokenReply = async () => ({
      status: 200,
      body: { ...idp.mint(true), refresh_token: undefined },
    });
    await provider.signIn();
    expect(await provider.getToken()).toBe("access-1");
    clock.t += 3600 * 1000;
    expect(await provider.getToken()).toBeNull();
    expect(ended).toEqual(["expired"]);
  });

  it("never logs tokens", async () => {
    const spies = (["log", "info", "warn", "error", "debug"] as const).map((m) =>
      jest.spyOn(console, m).mockImplementation(() => {}),
    );
    try {
      const { provider, idp, clock } = setup();
      await provider.signIn();
      clock.t += 3600 * 1000;
      await provider.getToken();
      idp.validRefresh.clear();
      clock.t += 3600 * 1000;
      await provider.getToken();
      for (const spy of spies) expect(spy).not.toHaveBeenCalled();
    } finally {
      spies.forEach((s) => s.mockRestore());
    }
  });
});

describe("sign-out racing a refresh's keychain write", () => {
  it("leaves nothing of the old account: the store is empty and getToken() is null", async () => {
    const { provider, idp, clock, store, secureStore } = setup();
    await provider.signIn();
    secureStore.latencyMs = 1; // every keychain call is a real round trip
    clock.t += 3600 * 1000;
    let signOut: Promise<unknown> | null = null;
    secureStore.beforeWrite = (op, key) => {
      // The refresh has passed its epoch check and is writing: sign out now.
      if (op === "set" && key.endsWith(".refresh") && !signOut) signOut = provider.signOut();
    };
    const pending = provider.getToken();
    await pending.catch(() => undefined);
    expect(signOut).not.toBeNull();
    await signOut;
    secureStore.beforeWrite = null;
    expect(await store.load()).toBeNull();
    // Nothing but the forced-login flag may remain in the keychain.
    expect([...secureStore.items.keys()].filter((k) => !k.endsWith(".relogin"))).toEqual([]);
    expect(await provider.getToken()).toBeNull();
    expect(provider.getUser()).toBeNull();
    // A relaunch is signed out too.
    const relaunch = setup({ store, fetch: idp.fetch });
    expect(await relaunch.provider.restore()).toBeNull();
    expect(await relaunch.provider.getToken()).toBeNull();
  });
});

describe("a refresh the IdP refuses for good", () => {
  // Zitadel answers a revoked or rotated-away refresh token with 400
  // invalid_request, not RFC 6749's invalid_grant.
  const zitadelDead = async () => ({
    status: 400,
    body: { error: "invalid_request", error_description: "Errors.OIDCSession.RefreshTokenInvalid" },
  });

  it("ends the session once (no loop) and a relaunch is signed out", async () => {
    const { provider, idp, clock, store, ended } = setup();
    await provider.signIn();
    clock.t += 3600 * 1000;
    idp.nextTokenReply = zitadelDead;
    expect(await provider.getToken()).toBeNull();
    expect(await provider.getToken()).toBeNull();
    expect(ended).toEqual(["invalid_grant"]);
    expect(idp.tokenRequests().filter((r) => r.form.grant_type === "refresh_token")).toHaveLength(
      1,
    );
    expect(provider.getUser()).toBeNull();
    expect(await store.load()).toBeNull();
    const relaunch = setup({ store, fetch: idp.fetch, now: () => clock.t });
    expect(await relaunch.provider.restore()).toBeNull();
  });

  it.each([408, 429, 500, 503])("keeps the session on a retryable %i", async (status) => {
    const { provider, idp, clock, store, ended } = setup();
    await provider.signIn();
    clock.t += 3600 * 1000;
    idp.nextTokenReply = async () => ({ status, body: { error: "temporarily_unavailable" } });
    await expect(provider.getToken()).rejects.toMatchObject({ status });
    expect(ended).toEqual([]);
    expect(await store.load()).not.toBeNull();
  });

  it("does not treat a refused CODE EXCHANGE as a dead grant", async () => {
    const { provider, idp, ended } = setup();
    idp.nextTokenReply = zitadelDead;
    await expect(provider.signIn()).rejects.toMatchObject({ status: 400 });
    expect(ended).toEqual([]);
  });
});

describe("refresh edge cases", () => {
  it("ends the session (account_changed) when a refresh answers for a different subject", async () => {
    const { provider, idp, clock, store, ended } = setup();
    await provider.signIn();
    clock.t += 3600 * 1000;
    idp.sub = "user-2";
    expect(await provider.getToken()).toBeNull();
    expect(ended).toEqual(["account_changed"]);
    expect(provider.getUser()).toBeNull();
    expect(await store.load()).toBeNull();
  });

  it("assumes a short lifetime when the IdP omits expires_in, and still persists a complete record", async () => {
    const { provider, idp, clock, store } = setup();
    idp.nextTokenReply = async () => ({
      status: 200,
      body: { ...idp.mint(true), expires_in: undefined },
    });
    await provider.signIn();
    expect((await store.load())?.expiresAt).toBe(clock.t + 5 * 60_000);
    expect(await provider.getToken()).toBe("access-1");
  });
});

describe("restore, failure and races", () => {
  it("ends the local session when restore fails for a non-transient reason", async () => {
    const s = setup();
    await s.provider.signIn();
    s.clock.t += 2 * 3600 * 1000;
    // The IdP refreshes with an id token minted for another audience.
    s.idp.idTokenClaims = { aud: ["someone-else"] };
    const relaunch = setup({ store: s.store, fetch: s.idp.fetch, now: () => s.clock.t });
    await expect(relaunch.provider.restore()).rejects.toBeDefined();
    expect(relaunch.provider.getUser()).toBeNull();
    expect(await relaunch.provider.getToken()).toBeNull();
    expect(await s.store.load()).toBeNull();
  });

  it("a slow restore cannot overwrite a sign-in that finished first", async () => {
    const old = setup();
    old.idp.sub = "old-user";
    await old.provider.signIn();

    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const slowStore = {
      ...old.store,
      load: async () => {
        const record = await old.store.load(); // the OLD account's record
        await gate;
        return record;
      },
      save: old.store.save,
      clear: old.store.clear,
      getForceLogin: old.store.getForceLogin,
      setForceLogin: old.store.setForceLogin,
    };
    const b = setup({
      store: slowStore,
      fetch: old.idp.fetch,
      browser: new FakeBrowser(old.idp),
    });
    old.idp.sub = "new-user";
    const restoring = b.provider.restore();
    const user = await b.provider.signIn();
    expect(user?.id).toBe("new-user");
    release();
    await restoring;
    expect(b.provider.getUser()?.id).toBe("new-user");
    expect(await b.provider.getToken()).toBe(`access-${old.idp.accessCounter}`);
  });
});

describe("onTokenRejected", () => {
  it("forces one refresh and reports a new token", async () => {
    const { provider, idp } = setup();
    await provider.signIn();
    expect(await provider.onTokenRejected("access-1")).toBe(true);
    expect(await provider.getToken()).toBe("access-2");
    expect(idp.tokenRequests().filter((r) => r.form.grant_type === "refresh_token")).toHaveLength(
      1,
    );
  });

  it("coalesces concurrent rejections and stale rejections into one refresh", async () => {
    const { provider, idp } = setup();
    await provider.signIn();
    idp.tokenDelayMs = 10;
    const results = await Promise.all([
      provider.onTokenRejected("access-1"),
      provider.onTokenRejected("access-1"),
      provider.onTokenRejected("access-1"),
    ]);
    expect(results).toEqual([true, true, true]);
    // A late 401 for the token already replaced needs no further refresh.
    expect(await provider.onTokenRejected("access-1")).toBe(true);
    expect(idp.tokenRequests().filter((r) => r.form.grant_type === "refresh_token")).toHaveLength(
      1,
    );
  });

  it("a request that carried no token needs no refresh when one exists now", async () => {
    const { provider, idp } = setup();
    expect(await provider.onTokenRejected(null)).toBe(false); // signed out
    await provider.signIn();
    const calls = idp.tokenRequests().length;
    expect(await provider.onTokenRejected(null)).toBe(true);
    expect(idp.tokenRequests()).toHaveLength(calls);
  });

  it("is false and signs out when the grant is dead, and false when signed out", async () => {
    const { provider, idp, ended } = setup();
    expect(await provider.onTokenRejected("x")).toBe(false);
    await provider.signIn();
    idp.validRefresh.clear();
    expect(await provider.onTokenRejected("access-1")).toBe(false);
    expect(ended).toEqual(["invalid_grant"]);
  });
});

describe("logout / signOut", () => {
  it("bumps the epoch, wipes the store, revokes the refresh token and returns the end-session URL", async () => {
    const { provider, idp, store, session } = setup();
    await provider.signIn();
    const signal = session.currentSignal();
    const epoch = session.epoch();
    const result = await provider.signOut();
    expect(session.epoch()).toBeGreaterThan(epoch);
    expect(signal.aborted).toBe(true);
    expect(provider.getUser()).toBeNull();
    expect(await provider.getToken()).toBeNull();
    expect(await store.load()).toBeNull();
    expect(idp.revoked).toEqual(["refresh-1"]);
    const url = new URL(result.endSessionUrl as string);
    expect(url.searchParams.get("post_logout_redirect_uri")).toBe("com.acme.app:/signedout");
    expect(url.searchParams.get("id_token_hint")).toBeTruthy();
  });

  it("logout() satisfies AuthProvider and signs out", async () => {
    const { provider, idp } = setup();
    await provider.signIn();
    await expect(provider.logout()).resolves.toBeUndefined();
    expect(provider.getUser()).toBeNull();
    expect(idp.revoked).toHaveLength(1);
  });

  it("the next signIn after a logout sends prompt=login, and the one after that does not", async () => {
    const { provider, browser } = setup();
    await provider.signIn();
    await provider.signOut();
    await provider.signIn();
    expect(browser.opened[1].url.searchParams.get("prompt")).toBe("login");
    await provider.signOut();
    await provider.signIn();
    expect(browser.opened[2].url.searchParams.get("prompt")).toBe("login");
    await provider.signIn();
    expect(browser.opened[3].url.searchParams.get("prompt")).toBeNull();
  });

  it("prompt=login survives an app restart between sign-out and sign-in", async () => {
    const a = setup();
    await a.provider.signIn();
    await a.provider.signOut();
    const b = setup({ store: a.store, fetch: a.idp.fetch, browser: a.browser });
    await b.provider.signIn();
    expect(a.browser.opened[1].url.searchParams.get("prompt")).toBe("login");
    expect(await a.store.getForceLogin()).toBe(false);
  });

  it("completes when the IdP is offline: local state is wiped, revocation is best-effort", async () => {
    const s = setup();
    await s.provider.signIn();
    // A cold start: discovery is not cached and the IdP is unreachable.
    const cold = setup({ store: s.store, fetch: s.idp.fetch, discoveryCache: new Map() });
    await cold.provider.restore();
    s.idp.offline = true;
    const result = await cold.provider.signOut();
    expect(cold.provider.getUser()).toBeNull();
    expect(await s.store.load()).toBeNull();
    expect(result.endSessionUrl).toBeNull();
    const idp = s.idp;
    expect(idp.revoked).toEqual([]);
  });

  it("does not hang on an IdP that never answers revocation (bounded by timeout)", async () => {
    const s = setup({ revokeTimeoutMs: 25 });
    await s.provider.signIn();
    const inner = s.idp.fetch;
    const hang: typeof inner = (url, init) =>
      url === s.idp.revocationEndpoint
        ? new Promise((_, reject) =>
            init.signal?.addEventListener("abort", () => reject(new Error("aborted"))),
          )
        : inner(url, init);
    const slow = setup({
      revokeTimeoutMs: 25,
      store: s.store,
      fetch: hang,
      discoveryCache: new Map(),
    });
    await slow.provider.restore();
    const started = Date.now();
    await slow.provider.signOut();
    expect(Date.now() - started).toBeLessThan(2000);
    expect(await s.store.load()).toBeNull();
  });

  it("wipes the keystore before the network round-trip", async () => {
    const { provider, idp, store } = setup();
    await provider.signIn();
    let storedAtRevoke: StoredTokens | null | undefined;
    const inner = idp.fetch;
    const probe: typeof inner = async (url, init) => {
      if (url === idp.revocationEndpoint) storedAtRevoke = await store.load();
      return inner(url, init);
    };
    const p2 = setup({ store, fetch: probe, discoveryCache: new Map() });
    await p2.provider.restore();
    await p2.provider.signOut();
    expect(storedAtRevoke).toBeNull();
  });

  it("aborts an in-flight refresh and drops its late response", async () => {
    const { provider, idp, clock, store } = setup();
    await provider.signIn();
    clock.t += 3600 * 1000;
    idp.tokenDelayMs = 20;
    const pending = provider.getToken();
    await new Promise((r) => setTimeout(r, 5));
    await provider.signOut();
    await expect(pending).resolves.toBeNull();
    expect(provider.getUser()).toBeNull();
    expect(await store.load()).toBeNull();
  });
});

describe("getUser", () => {
  it("derives the user from id-token claims, falling back to preferred_username", async () => {
    const { provider, idp } = setup();
    idp.idTokenClaims = { name: undefined, preferred_username: "alice", email: undefined };
    const user = await provider.signIn();
    expect(user).toEqual({ id: "user-1", name: "alice" });
  });

  it("is null before sign-in", () => {
    expect(setup().provider.getUser()).toBeNull();
  });
});

describe("https universal link redirect", () => {
  const HTTPS = "https://app.acme.example/oauthredirect";
  const FALLBACK = "com.acme.app:/oauthredirect";
  const linked = {
    ...config,
    redirectUri: HTTPS,
    postLogoutRedirectUri: "https://app.acme.example/signedout",
    fallbackRedirectUri: FALLBACK,
    fallbackPostLogoutRedirectUri: "com.acme.app:/signedout",
  };

  it("android: the https redirect is used for authorize, callback and token, with universal links preferred", async () => {
    const { provider, idp, browser } = setup({ platform: { os: "android", version: 34 } }, linked);
    await provider.signIn();
    const [opened] = browser.opened;
    expect(opened.redirectUri).toBe(HTTPS);
    expect(opened.universal).toBe(true);
    expect(opened.url.searchParams.get("redirect_uri")).toBe(HTTPS);
    // RFC 6749 §4.1.3: the token request carries the SAME redirect_uri.
    expect(idp.tokenRequests()[0].form.redirect_uri).toBe(HTTPS);
  });

  it("ios 17.4: https with preferUniversalLinks", async () => {
    const { provider, browser } = setup({ platform: { os: "ios", version: "17.4" } }, linked);
    await provider.signIn();
    expect(browser.opened[0].redirectUri).toBe(HTTPS);
    expect(browser.opened[0].universal).toBe(true);
  });

  it("ios 17.3: the private-use fallback for authorize AND token, no universal links", async () => {
    const { provider, idp, browser } = setup({ platform: { os: "ios", version: "17.3.1" } }, linked);
    await provider.signIn();
    const [opened] = browser.opened;
    expect(opened.redirectUri).toBe(FALLBACK);
    expect(opened.universal).toBe(false);
    expect(opened.url.searchParams.get("redirect_uri")).toBe(FALLBACK);
    expect(idp.tokenRequests()[0].form.redirect_uri).toBe(FALLBACK);
  });

  it("rejects a callback on the other form: one attempt, one redirect", async () => {
    const { provider, browser } = setup({ platform: { os: "android", version: 34 } }, linked);
    browser.script = (u) => ({
      type: "success",
      url: `${FALLBACK}?code=the-code&state=${encodeURIComponent(u.searchParams.get("state") ?? "")}`,
    });
    await expect(provider.signIn()).rejects.toThrow(/does not match/);
  });

  it("no platform injected: the configured redirect, never universal links (unchanged behaviour)", async () => {
    const { provider, browser } = setup({}, linked);
    await provider.signIn();
    expect(browser.opened[0].redirectUri).toBe(HTTPS);
    expect(browser.opened[0].universal).toBe(false);
  });

  it("a private-use-only config is unaffected by the platform", async () => {
    const { provider, browser } = setup({ platform: { os: "ios", version: "16.0" } });
    await provider.signIn();
    expect(browser.opened[0].redirectUri).toBe(REDIRECT);
    expect(browser.opened[0].universal).toBe(false);
  });

  it("sign-out asks the IdP to return to the post-logout URI that matches the platform", async () => {
    const droid = setup({ platform: { os: "android", version: 34 } }, linked);
    await droid.provider.signIn();
    const a = await droid.provider.signOut();
    expect(new URL(a.endSessionUrl ?? "").searchParams.get("post_logout_redirect_uri")).toBe("https://app.acme.example/signedout");

    const old = setup({ platform: { os: "ios", version: "16.0" } }, linked);
    await old.provider.signIn();
    const b = await old.provider.signOut();
    expect(new URL(b.endSessionUrl ?? "").searchParams.get("post_logout_redirect_uri")).toBe("com.acme.app:/signedout");
  });
});
