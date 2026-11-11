// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { describe, expect, it } from "@jest/globals";

import {
  AbortedError,
  AuthorizationError,
  DiscoveryError,
  IdTokenError,
  MissingCodeError,
  OfflineError,
  ProtocolError,
  StateMismatchError,
  TokenEndpointError,
  authorizeUrl,
  base64ToBase64Url,
  base64UrlDecode,
  base64UrlEncode,
  checkIdTokenClaims,
  createPkce,
  decodeJwtClaims,
  discover,
  endSessionUrl,
  exchangeCode,
  matchesRedirect,
  parseCallback,
  randomToken,
  refreshTokens,
  revokeToken,
  type CryptoLike,
} from "../src/lib/auth/oidc";
import {
  CLIENT_ID,
  FakeIdp,
  ISSUER,
  POST_LOGOUT,
  REDIRECT,
  config,
  jwt,
  nodeCrypto,
  sha256,
  utf8,
} from "./auth-fakes";

// RFC 7636 Appendix B.
const RFC_VERIFIER = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
const RFC_CHALLENGE = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";

describe("base64url", () => {
  it("round-trips arbitrary bytes at every length mod 3", () => {
    for (const n of [0, 1, 2, 3, 4, 31, 32, 33, 255]) {
      const bytes = Uint8Array.from({ length: n }, (_, i) => (i * 37 + 11) & 0xff);
      const enc = base64UrlEncode(bytes);
      expect(enc).toMatch(/^[A-Za-z0-9_-]*$/);
      expect(Array.from(base64UrlDecode(enc))).toEqual(Array.from(bytes));
    }
  });
  it("converts standard base64 to url-safe unpadded", () => {
    expect(base64ToBase64Url("a+b/c==")).toBe("a-b_c");
  });
});

describe("sha256 test fake", () => {
  it("matches the FIPS 180-4 vectors", () => {
    const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
    expect(hex(sha256(utf8("abc")))).toBe(
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    );
    expect(hex(sha256(utf8("")))).toBe(
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    );
    expect(hex(sha256(utf8("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq")))).toBe(
      "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
    );
  });
});

describe("PKCE", () => {
  it("derives the RFC 7636 Appendix B challenge from the RFC verifier", async () => {
    // Feed the RFC verifier's own bytes through createPkce.
    const bytes = base64UrlDecode(RFC_VERIFIER);
    const crypto: CryptoLike = { ...nodeCrypto, getRandomBytes: () => bytes };
    const pkce = await createPkce(crypto);
    expect(pkce.verifier).toBe(RFC_VERIFIER);
    expect(pkce.challenge).toBe(RFC_CHALLENGE);
    expect(pkce.method).toBe("S256");
  });

  it("uses 32 random bytes (43 chars, RFC 7636 §4.1) and fresh values each time", async () => {
    const a = await createPkce(nodeCrypto);
    const b = await createPkce(nodeCrypto);
    expect(a.verifier).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(a.verifier).not.toBe(b.verifier);
    expect(a.challenge).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it("requests at least 32 bytes for state and nonce, and rejects a short random source", () => {
    const asked: number[] = [];
    const crypto: CryptoLike = {
      ...nodeCrypto,
      getRandomBytes: (n) => {
        asked.push(n);
        return nodeCrypto.getRandomBytes(n);
      },
    };
    expect(randomToken(crypto)).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(asked[0]).toBeGreaterThanOrEqual(32);
    expect(() =>
      randomToken({ ...nodeCrypto, getRandomBytes: () => new Uint8Array(3) }, 32),
    ).toThrow(ProtocolError);
  });
});

describe("discover", () => {
  it("caches per issuer, including concurrent callers", async () => {
    const idp = new FakeIdp();
    const cache = new Map();
    const [a, b] = await Promise.all([
      discover(ISSUER, idp.fetch, { cache }),
      discover(ISSUER, idp.fetch, { cache }),
    ]);
    await discover(ISSUER, idp.fetch, { cache });
    expect(a).toBe(b);
    expect(idp.requests).toHaveLength(1);
    expect(idp.requests[0].url).toBe(`${ISSUER}/.well-known/openid-configuration`);
    expect(a.token_endpoint).toBe(`${ISSUER}/oauth/v2/token`);
  });

  it("does not share the cache between issuers", async () => {
    const idp = new FakeIdp();
    const cache = new Map();
    await discover(ISSUER, idp.fetch, { cache });
    idp.issuerInDiscovery = "https://other.example.com";
    await expect(
      discover("https://other.example.com", idp.fetch, { cache }),
    ).resolves.toBeDefined();
    expect(idp.requests).toHaveLength(2);
  });

  it("rejects a document whose issuer differs from the configured one", async () => {
    const idp = new FakeIdp();
    idp.issuerInDiscovery = "https://evil.example.com";
    await expect(discover(ISSUER, idp.fetch, { cache: new Map() })).rejects.toBeInstanceOf(
      DiscoveryError,
    );
  });

  it("does not cache a failure", async () => {
    const idp = new FakeIdp();
    const cache = new Map();
    idp.discoveryStatus = 503;
    await expect(discover(ISSUER, idp.fetch, { cache })).rejects.toBeInstanceOf(DiscoveryError);
    idp.discoveryStatus = 200;
    await expect(discover(ISSUER, idp.fetch, { cache })).resolves.toBeDefined();
  });

  it("a caller's abort stops only that caller: the shared fetch and its cache entry survive", async () => {
    const idp = new FakeIdp();
    const cache = new Map();
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const gated: typeof idp.fetch = async (url, init) => {
      await gate;
      return idp.fetch(url, init);
    };
    const controller = new AbortController();
    const first = discover(ISSUER, gated, { cache, signal: controller.signal });
    const second = discover(ISSUER, gated, { cache });
    controller.abort();
    await expect(first).rejects.toBeInstanceOf(AbortedError);
    release();
    await expect(second).resolves.toMatchObject({ issuer: ISSUER });
    expect(cache.size).toBe(1);
    await discover(ISSUER, gated, { cache });
    expect(idp.requests).toHaveLength(1);
  });

  it("rejects at once for an already-aborted signal without poisoning the cache", async () => {
    const idp = new FakeIdp();
    const cache = new Map();
    const controller = new AbortController();
    controller.abort();
    await expect(
      discover(ISSUER, idp.fetch, { cache, signal: controller.signal }),
    ).rejects.toBeInstanceOf(AbortedError);
    await expect(discover(ISSUER, idp.fetch, { cache })).resolves.toMatchObject({ issuer: ISSUER });
  });

  it("reports an unreachable IdP as OfflineError", async () => {
    const idp = new FakeIdp();
    idp.offline = true;
    await expect(discover(ISSUER, idp.fetch, { cache: new Map() })).rejects.toBeInstanceOf(
      OfflineError,
    );
  });

  it("refuses insecure endpoints unless loopback is allowed", async () => {
    const idp = new FakeIdp();
    idp.revocationEndpoint = "http://idp.example.com/revoke";
    await expect(discover(ISSUER, idp.fetch, { cache: new Map() })).rejects.toBeInstanceOf(
      DiscoveryError,
    );
  });
});

describe("authorizeUrl", () => {
  const discovery = { authorization_endpoint: `${ISSUER}/oauth/v2/authorize` };
  const input = { discovery, config, state: "S", nonce: "N", codeChallenge: "C" };

  it("builds a code+PKCE S256 request carrying state and nonce", () => {
    const u = new URL(authorizeUrl(input));
    expect(u.origin + u.pathname).toBe(`${ISSUER}/oauth/v2/authorize`);
    expect(Object.fromEntries(u.searchParams)).toEqual({
      response_type: "code",
      client_id: CLIENT_ID,
      redirect_uri: REDIRECT,
      scope: "openid profile email offline_access",
      state: "S",
      nonce: "N",
      code_challenge: "C",
      code_challenge_method: "S256",
    });
  });

  it("adds prompt=login when asked and keeps an existing query on the endpoint", () => {
    const u = new URL(
      authorizeUrl({
        ...input,
        prompt: "login",
        discovery: { authorization_endpoint: `${ISSUER}/a?tenant=1` },
      }),
    );
    expect(u.searchParams.get("prompt")).toBe("login");
    expect(u.searchParams.get("tenant")).toBe("1");
  });

  it("refuses to let extra params override reserved ones", () => {
    for (const key of [
      "state",
      "nonce",
      "redirect_uri",
      "code_challenge",
      "client_id",
      "scope",
      "prompt",
    ]) {
      expect(() => authorizeUrl({ ...input, extraParams: { [key]: "x" } })).toThrow(ProtocolError);
    }
    expect(
      new URL(authorizeUrl({ ...input, extraParams: { login_hint: "a@b.c" } })).searchParams.get(
        "login_hint",
      ),
    ).toBe("a@b.c");
  });
});

describe("matchesRedirect — scheme-agnostic", () => {
  it("matches a private-use scheme by scheme + path, ignoring query and fragment", () => {
    expect(matchesRedirect("com.acme.app:/oauthredirect?code=1&state=2", REDIRECT)).toBe(true);
    expect(matchesRedirect("com.acme.app:/oauthredirect#x", REDIRECT)).toBe(true);
    expect(matchesRedirect("COM.ACME.APP:/oauthredirect?code=1", REDIRECT)).toBe(true);
    expect(matchesRedirect("com.acme.app:/other?code=1", REDIRECT)).toBe(false);
    expect(matchesRedirect("com.evil.app:/oauthredirect?code=1", REDIRECT)).toBe(false);
  });

  it("matches an https universal link by scheme + host + path", () => {
    const https = "https://app.example.com/oauthredirect";
    expect(matchesRedirect("https://app.example.com/oauthredirect?code=1&state=2", https)).toBe(
      true,
    );
    expect(matchesRedirect("https://APP.example.com/oauthredirect/?code=1", https)).toBe(true);
    expect(matchesRedirect("https://evil.example.com/oauthredirect?code=1", https)).toBe(false);
    expect(matchesRedirect("https://app.example.com/signedout?code=1", https)).toBe(false);
    expect(matchesRedirect("http://app.example.com/oauthredirect?code=1", https)).toBe(false);
  });

  it("treats `scheme:/path` and `scheme:///path` (empty authority) as the same place", () => {
    expect(matchesRedirect("com.acme.app:///oauthredirect?code=1", REDIRECT)).toBe(true);
    expect(matchesRedirect("com.acme.app://host/oauthredirect?code=1", REDIRECT)).toBe(false);
    expect(matchesRedirect("not a url", REDIRECT)).toBe(false);
  });
});

describe("parseCallback", () => {
  const expected = { redirectUri: REDIRECT, state: "good-state", issuer: ISSUER };

  it("extracts the code from a private-use-scheme callback", () => {
    expect(parseCallback(`${REDIRECT}?code=abc&state=good-state`, expected)).toEqual({
      code: "abc",
    });
  });

  it("extracts the code from an https callback", () => {
    const e = { ...expected, redirectUri: "https://app.example.com/oauthredirect" };
    expect(
      parseCallback("https://app.example.com/oauthredirect?code=abc&state=good-state", e),
    ).toEqual({ code: "abc" });
  });

  it("decodes percent-escapes in the code", () => {
    expect(parseCallback(`${REDIRECT}?code=a%2Bb%3D&state=good-state`, expected).code).toBe("a+b=");
  });

  it("throws StateMismatchError on a wrong, missing or forged state", () => {
    expect(() => parseCallback(`${REDIRECT}?code=abc&state=other`, expected)).toThrow(
      StateMismatchError,
    );
    expect(() => parseCallback(`${REDIRECT}?code=abc`, expected)).toThrow(StateMismatchError);
    // A forged error=… without our state must not be reported as the IdP's answer.
    expect(() => parseCallback(`${REDIRECT}?error=access_denied`, expected)).toThrow(
      StateMismatchError,
    );
  });

  it("throws StateMismatchError when the callback is for a different redirect", () => {
    expect(() =>
      parseCallback("com.evil.app:/oauthredirect?code=abc&state=good-state", expected),
    ).toThrow(StateMismatchError);
  });

  it("throws AuthorizationError carrying the IdP's code on error=", () => {
    try {
      parseCallback(
        `${REDIRECT}?error=access_denied&error_description=User+said+no&state=good-state`,
        expected,
      );
      throw new Error("expected throw");
    } catch (e) {
      expect(e).toBeInstanceOf(AuthorizationError);
      expect((e as AuthorizationError).code).toBe("access_denied");
      expect((e as AuthorizationError).description).toBe("User said no");
    }
  });

  it("throws MissingCodeError when there is no code", () => {
    expect(() => parseCallback(`${REDIRECT}?state=good-state`, expected)).toThrow(MissingCodeError);
    expect(() => parseCallback(`${REDIRECT}?code=&state=good-state`, expected)).toThrow(
      MissingCodeError,
    );
  });

  it("rejects a mismatched RFC 9207 iss", () => {
    expect(() =>
      parseCallback(`${REDIRECT}?code=abc&state=good-state&iss=https://evil.example.com`, expected),
    ).toThrow(StateMismatchError);
  });
});

describe("token endpoint", () => {
  const discovery = { token_endpoint: `${ISSUER}/oauth/v2/token` };

  it("exchanges a code as a public client: client_id + verifier, no secret", async () => {
    const idp = new FakeIdp();
    const tokens = await exchangeCode({
      discovery,
      config,
      code: "the-code",
      codeVerifier: "the-verifier",
      fetch: idp.fetch,
      now: () => 1_000_000,
    });
    const [req] = idp.tokenRequests();
    expect(req.form).toEqual({
      grant_type: "authorization_code",
      code: "the-code",
      redirect_uri: REDIRECT,
      client_id: CLIENT_ID,
      code_verifier: "the-verifier",
    });
    expect(req.form).not.toHaveProperty("client_secret");
    expect(tokens.accessToken).toBe("access-1");
    expect(tokens.refreshToken).toBe("refresh-1");
    expect(tokens.expiresAt).toBe(1_000_000 + 3600 * 1000);
  });

  it("refreshes as a public client", async () => {
    const idp = new FakeIdp();
    idp.validRefresh.add("r1");
    await refreshTokens({ discovery, config, refreshToken: "r1", fetch: idp.fetch });
    expect(idp.tokenRequests()[0].form).toEqual({
      grant_type: "refresh_token",
      refresh_token: "r1",
      client_id: CLIENT_ID,
    });
  });

  it("surfaces invalid_grant as a typed error", async () => {
    const idp = new FakeIdp();
    const err = await refreshTokens({
      discovery,
      config,
      refreshToken: "dead",
      fetch: idp.fetch,
    }).catch((e) => e);
    expect(err).toBeInstanceOf(TokenEndpointError);
    expect(err.isInvalidGrant).toBe(true);
    expect(err.isRetryable).toBe(false);
    expect(err.status).toBe(400);
  });

  it("classifies 5xx and 429 as retryable, and a network failure as offline", async () => {
    const idp = new FakeIdp();
    idp.nextTokenReply = async () => ({ status: 503, body: "<html>bad gateway</html>" });
    const e1 = await refreshTokens({
      discovery,
      config,
      refreshToken: "x",
      fetch: idp.fetch,
    }).catch((e) => e);
    expect(e1).toBeInstanceOf(TokenEndpointError);
    expect(e1.isRetryable).toBe(true);
    idp.offline = true;
    await expect(
      refreshTokens({ discovery, config, refreshToken: "x", fetch: idp.fetch }),
    ).rejects.toBeInstanceOf(OfflineError);
  });

  it("rejects a token response with no access_token or a non-bearer type", async () => {
    const idp = new FakeIdp();
    idp.nextTokenReply = async () => ({ status: 200, body: { token_type: "Bearer" } });
    await expect(
      refreshTokens({ discovery, config, refreshToken: "x", fetch: idp.fetch }),
    ).rejects.toBeInstanceOf(ProtocolError);
    idp.nextTokenReply = async () => ({
      status: 200,
      body: { access_token: "a", token_type: "DPoP" },
    });
    await expect(
      refreshTokens({ discovery, config, refreshToken: "x", fetch: idp.fetch }),
    ).rejects.toBeInstanceOf(ProtocolError);
  });

  it("never puts a token in an error message", async () => {
    const idp = new FakeIdp();
    idp.nextTokenReply = async () => ({ status: 400, body: { error: "invalid_grant" } });
    const err = await refreshTokens({
      discovery,
      config,
      refreshToken: "SECRET-REFRESH",
      fetch: idp.fetch,
    }).catch((e) => e);
    expect(String(err.message)).not.toContain("SECRET-REFRESH");
  });

  it("times out a hung IdP as offline", async () => {
    const hang = (async (_url: string, init: { signal?: AbortSignal }) =>
      new Promise((_, reject) =>
        init.signal?.addEventListener("abort", () => reject(new Error("aborted"))),
      )) as never;
    await expect(
      refreshTokens({
        discovery,
        config,
        refreshToken: "x",
        fetch: hang,
        options: { timeoutMs: 20 },
      }),
    ).rejects.toBeInstanceOf(OfflineError);
  });
});

describe("revocation and end-session", () => {
  it("posts the token to the revocation endpoint, or reports there is none", async () => {
    const idp = new FakeIdp();
    const discovery = { revocation_endpoint: `${ISSUER}/oauth/v2/revoke` };
    expect(
      await revokeToken({
        discovery,
        config,
        token: "r1",
        tokenTypeHint: "refresh_token",
        fetch: idp.fetch,
      }),
    ).toBe(true);
    expect(idp.requests[0].form).toEqual({
      token: "r1",
      client_id: CLIENT_ID,
      token_type_hint: "refresh_token",
    });
    expect(
      await revokeToken({
        discovery: { revocation_endpoint: null },
        config,
        token: "r1",
        fetch: idp.fetch,
      }),
    ).toBe(false);
  });

  it("builds the end-session URL with id_token_hint and the post-logout redirect", () => {
    const u = new URL(
      endSessionUrl({
        discovery: { end_session_endpoint: `${ISSUER}/oidc/v1/end_session` },
        config,
        idTokenHint: "ID",
        state: "s",
      }) as string,
    );
    expect(u.searchParams.get("id_token_hint")).toBe("ID");
    expect(u.searchParams.get("post_logout_redirect_uri")).toBe(POST_LOGOUT);
    expect(u.searchParams.get("client_id")).toBe(CLIENT_ID);
    expect(endSessionUrl({ discovery: { end_session_endpoint: null }, config })).toBeNull();
  });
});

describe("id token (decode only)", () => {
  it("decodes UTF-8 claims without verifying the signature", () => {
    const claims = decodeJwtClaims(jwt({ sub: "u", name: "Zoë 日本" }));
    expect(claims).toEqual({ sub: "u", name: "Zoë 日本" });
  });

  it("rejects malformed tokens", () => {
    expect(() => decodeJwtClaims("nope")).toThrow(IdTokenError);
    expect(() => decodeJwtClaims("a.!!!.c")).toThrow(IdTokenError);
    expect(() => decodeJwtClaims(`a.${base64UrlEncode(utf8("[1]"))}.c`)).toThrow(IdTokenError);
  });

  it("checks issuer, audience (string or array), subject and nonce", () => {
    const ok = { iss: ISSUER, aud: CLIENT_ID, sub: "u", nonce: "n" };
    expect(() =>
      checkIdTokenClaims(ok, { issuer: ISSUER, clientId: CLIENT_ID, nonce: "n" }),
    ).not.toThrow();
    expect(() =>
      checkIdTokenClaims({ ...ok, aud: ["x", CLIENT_ID] }, { issuer: ISSUER, clientId: CLIENT_ID }),
    ).not.toThrow();
    expect(() =>
      checkIdTokenClaims({ ...ok, iss: "https://evil" }, { issuer: ISSUER, clientId: CLIENT_ID }),
    ).toThrow(IdTokenError);
    expect(() =>
      checkIdTokenClaims({ ...ok, aud: "other" }, { issuer: ISSUER, clientId: CLIENT_ID }),
    ).toThrow(IdTokenError);
    expect(() =>
      checkIdTokenClaims({ ...ok, sub: "" }, { issuer: ISSUER, clientId: CLIENT_ID }),
    ).toThrow(IdTokenError);
    expect(() =>
      checkIdTokenClaims(ok, { issuer: ISSUER, clientId: CLIENT_ID, nonce: "different" }),
    ).toThrow(IdTokenError);
    expect(() =>
      checkIdTokenClaims(
        { iss: ISSUER, aud: CLIENT_ID, sub: "u" },
        { issuer: ISSUER, clientId: CLIENT_ID, nonce: "n" },
      ),
    ).toThrow(IdTokenError);
  });
});
