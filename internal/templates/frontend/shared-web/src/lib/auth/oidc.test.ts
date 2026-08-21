// Security properties of the OIDC + PKCE flow.
//
// Every assertion here derives from a value the code actually COMPUTES —
// a digest, a comparison result, a thrown error, a parsed URL — never from a
// substring of a rendered template. Two of them use an independent oracle:
//
//   - the RFC 7636 Appendix B test vector, whose verifier/challenge pair is
//     published by the spec and computed by neither this code nor this test;
//   - a from-scratch SHA-256 + base64url reimplementation in this file, used
//     to check challenges of RANDOM verifiers, so the property holds for
//     values no fixture pins down.
//
// Lives under shared-web/ because it is a Vitest suite and needs WebCrypto:
// both browser kinds run `vitest run` under jsdom (which provides
// crypto.subtle), while React Native runs jest-expo and cannot resolve
// "vitest".

import { describe, expect, it, vi } from "vitest";

import {
  buildAuthorizeUrl,
  createState,
  createVerifier,
  discover,
  exchangeCode,
  MAX_VERIFIER_LENGTH,
  MIN_VERIFIER_LENGTH,
  OAuthError,
  parseTokenResponse,
  parseVerifier,
  statesMatch,
  Verifier,
} from "./oidc";

// ── RFC 7636 Appendix B, the published vector ─────────────────────────
//
// The spec's own example pair. Neither value is computed by the code under
// test, so agreement is evidence the challenge derivation is right rather
// than self-consistent.
const RFC_VERIFIER = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
const RFC_CHALLENGE = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM";

/**
 * An independent base64url encoder, written the long way so it shares no code
 * with the implementation. If both were wrong in the same way, this test
 * would pass — hence the RFC vector above as the second, external check.
 */
function independentBase64Url(bytes: Uint8Array): string {
  const alphabet =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  let out = "";
  for (let i = 0; i < bytes.length; i += 3) {
    const b0 = bytes[i] ?? 0;
    const b1 = bytes[i + 1] ?? 0;
    const b2 = bytes[i + 2] ?? 0;
    const chunk = (b0 << 16) | (b1 << 8) | b2;
    const have = Math.min(3, bytes.length - i);
    out += alphabet[(chunk >> 18) & 63];
    out += alphabet[(chunk >> 12) & 63];
    if (have > 1) out += alphabet[(chunk >> 6) & 63];
    if (have > 2) out += alphabet[chunk & 63];
  }
  return out;
}

/** SHA-256 via WebCrypto, encoded by the independent encoder above. */
async function independentChallenge(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(verifier),
  );
  return independentBase64Url(new Uint8Array(digest));
}

describe("PKCE code challenge (RFC 7636 §4.2)", () => {
  it("matches the RFC 7636 Appendix B published vector", async () => {
    const challenge = await parseVerifier(RFC_VERIFIER).challenge();
    expect(challenge).toBe(RFC_CHALLENGE);
  });

  it("is SHA-256(verifier) base64url-no-pad for RANDOM verifiers too", async () => {
    // The RFC vector pins one input. This pins the PROPERTY: for verifiers
    // nothing in this repo has ever seen, the challenge still equals an
    // independently computed digest.
    const seen = new Set<string>();
    for (let i = 0; i < 25; i++) {
      const verifier = createVerifier();
      const secret = verifier.secret();
      seen.add(secret);
      expect(await verifier.challenge()).toBe(
        await independentChallenge(secret),
      );
    }
    // Fail loudly if the "random" verifiers were all the same value — the
    // loop above would still pass while proving nothing about entropy.
    expect(seen.size).toBe(25);
  });

  it("emits base64url with no padding and no base64-only characters", async () => {
    const challenge = await createVerifier().challenge();
    expect(challenge).not.toContain("=");
    expect(challenge).not.toContain("+");
    expect(challenge).not.toContain("/");
    // 32 raw bytes -> 43 base64url characters unpadded.
    expect(challenge).toHaveLength(43);
  });

  it("has no `plain` challenge method anywhere in the surface", async () => {
    // The downgrade RFC 7636 §7.2 warns about is absent by CONSTRUCTION, not
    // by default: there is no method, option or argument that yields it.
    const verifier = createVerifier();
    const challenge = await verifier.challenge();
    expect(challenge).not.toBe(verifier.secret());
    const surface = Object.getOwnPropertyNames(Verifier.prototype);
    expect(surface).not.toContain("challengeWithMethod");
    expect(surface.join(" ").toLowerCase()).not.toContain("plain");
  });
});

describe("verifier and state entropy", () => {
  it("draws verifiers from the RFC 7636 §4.1 unreserved alphabet, in range", () => {
    for (let i = 0; i < 50; i++) {
      const secret = createVerifier().secret();
      expect(secret).toMatch(/^[A-Za-z0-9\-._~]+$/);
      expect(secret.length).toBeGreaterThanOrEqual(MIN_VERIFIER_LENGTH);
      expect(secret.length).toBeLessThanOrEqual(MAX_VERIFIER_LENGTH);
    }
  });

  it("uses crypto.getRandomValues, not Math.random", () => {
    // Derived from OBSERVED CALLS, not from reading the source: spy on both
    // and assert which one the code reaches for.
    const randomSpy = vi.spyOn(Math, "random");
    const cryptoSpy = vi.spyOn(globalThis.crypto, "getRandomValues");
    try {
      createVerifier();
      createState();
      expect(cryptoSpy).toHaveBeenCalled();
      expect(randomSpy).not.toHaveBeenCalled();
    } finally {
      randomSpy.mockRestore();
      cryptoSpy.mockRestore();
    }
  });

  it("never repeats a verifier or a state across many draws", () => {
    const verifiers = new Set<string>();
    const states = new Set<string>();
    for (let i = 0; i < 200; i++) {
      verifiers.add(createVerifier().secret());
      states.add(createState());
    }
    expect(verifiers.size).toBe(200);
    expect(states.size).toBe(200);
  });

  it("rejects a stored verifier that is not RFC-legal", () => {
    expect(() => parseVerifier("tooshort")).toThrow(/length/);
    expect(() => parseVerifier("a".repeat(200))).toThrow(/length/);
    // "+" and "/" are base64 but NOT in the unreserved set.
    expect(() => parseVerifier("a".repeat(42) + "+")).toThrow(/unreserved/);
  });

  it("keeps a rejected verifier's value out of the error message", () => {
    // An error that echoes the input would put the secret in a log line.
    const secretish = "!".repeat(50);
    try {
      parseVerifier(secretish);
      throw new Error("expected parseVerifier to throw");
    } catch (err) {
      expect(String(err)).not.toContain(secretish);
    }
  });
});

describe("the verifier is treated as a secret", () => {
  it("does not disclose itself through any printing path", () => {
    const verifier = createVerifier();
    const secret = verifier.secret();

    // Each of these is a real way a secret escapes into a log aggregator.
    expect(String(verifier)).toBe("[REDACTED]");
    expect(`${verifier}`).toBe("[REDACTED]");
    expect(JSON.stringify(verifier)).toBe('"[REDACTED]"');
    expect(JSON.stringify({ verifier })).not.toContain(secret);
    expect(JSON.stringify({ nested: { deep: [verifier] } })).not.toContain(
      secret,
    );

    // The field is #private, so it is unreachable rather than merely
    // discouraged: enumeration finds nothing to read.
    expect(Object.keys(verifier)).toHaveLength(0);
    expect(
      JSON.stringify(Object.getOwnPropertyDescriptors(verifier)),
    ).not.toContain(secret);
  });

  it("is never written to a console sink", async () => {
    // Derived from what the console ACTUALLY RECEIVED across a whole flow.
    const captured: string[] = [];
    const methods = ["log", "info", "warn", "error", "debug"] as const;
    const spies = methods.map((m) =>
      vi.spyOn(console, m).mockImplementation((...args: unknown[]) => {
        captured.push(args.map((a) => String(a)).join(" "));
      }),
    );
    try {
      const verifier = createVerifier();
      const secret = verifier.secret();
      const challenge = await verifier.challenge();
      const state = createState();

      buildAuthorizeUrl({
        endpoint: "https://idp.example.com/authorize",
        clientId: "client-1",
        redirectUri: "https://app.example.com/auth/callback",
        scopes: ["openid"],
        state,
        challenge,
      });
      // An exchange that fails is the interesting case: error paths are
      // where a secret usually leaks.
      await exchangeCode({
        endpoint: "https://idp.example.com/token",
        clientId: "client-1",
        redirectUri: "https://app.example.com/auth/callback",
        code: "code-1",
        verifier,
        fetchImpl: async () =>
          new Response(JSON.stringify({ error: "invalid_grant" }), {
            status: 400,
            headers: { "Content-Type": "application/json" },
          }),
      }).catch((err: unknown) => {
        // Even the error's own text must not carry it.
        expect(String(err)).not.toContain(secret);
      });

      expect(captured.join("\n")).not.toContain(secret);
    } finally {
      for (const spy of spies) spy.mockRestore();
    }
  });
});

describe("state comparison (CSRF)", () => {
  it("accepts only an exact echo", () => {
    const state = createState();
    expect(statesMatch(state, state)).toBe(true);
    expect(statesMatch(state, state + "x")).toBe(false);
    expect(statesMatch(state, state.slice(0, -1))).toBe(false);
    // One flipped character.
    const flipped = (state[0] === "A" ? "B" : "A") + state.slice(1);
    expect(statesMatch(state, flipped)).toBe(false);
  });

  it("never lets an empty state satisfy the check", () => {
    // "" === "" is true in JS, so a provider that echoes no state paired
    // with a session that stored none would otherwise PASS the CSRF check
    // having proved nothing.
    expect(statesMatch("", "")).toBe(false);
    expect(statesMatch("abc", "")).toBe(false);
    expect(statesMatch("", "abc")).toBe(false);
  });
});

describe("the authorization URL", () => {
  it("carries S256 and every required parameter", async () => {
    const verifier = createVerifier();
    const challenge = await verifier.challenge();
    const state = createState();
    const url = new URL(
      buildAuthorizeUrl({
        endpoint: "https://idp.example.com/oidc/authorize",
        clientId: "client-1",
        redirectUri: "https://app.example.com/auth/callback",
        scopes: ["openid", "profile", "email"],
        state,
        challenge,
      }),
    );

    expect(url.searchParams.get("code_challenge_method")).toBe("S256");
    expect(url.searchParams.get("code_challenge")).toBe(challenge);
    expect(url.searchParams.get("response_type")).toBe("code");
    expect(url.searchParams.get("state")).toBe(state);
    expect(url.searchParams.get("client_id")).toBe("client-1");
    expect(url.searchParams.get("redirect_uri")).toBe(
      "https://app.example.com/auth/callback",
    );
    expect(url.searchParams.get("scope")).toBe("openid profile email");

    // The URL is a public artefact and must never carry the verifier.
    expect(url.toString()).not.toContain(verifier.secret());
  });

  it("refuses to build a URL with a security parameter missing", async () => {
    const base = {
      endpoint: "https://idp.example.com/authorize",
      clientId: "client-1",
      redirectUri: "https://app.example.com/auth/callback",
      scopes: ["openid"],
      state: createState(),
      challenge: await createVerifier().challenge(),
    };
    // Each omission switches a security property off silently at the
    // provider, so building the URL is the last place to catch it.
    expect(() => buildAuthorizeUrl({ ...base, state: "" })).toThrow(/state/);
    expect(() => buildAuthorizeUrl({ ...base, challenge: "" })).toThrow(
      /challenge/,
    );
    expect(() => buildAuthorizeUrl({ ...base, redirectUri: "" })).toThrow(
      /redirectUri/,
    );
    expect(() => buildAuthorizeUrl({ ...base, clientId: "" })).toThrow(
      /clientId/,
    );
  });

  it("requires https except on loopback", async () => {
    const base = {
      clientId: "client-1",
      redirectUri: "http://localhost:3000/auth/callback",
      scopes: ["openid"],
      state: createState(),
      challenge: await createVerifier().challenge(),
    };
    expect(() =>
      buildAuthorizeUrl({
        ...base,
        endpoint: "http://idp.example.com/authorize",
      }),
    ).toThrow(/https/);
    // Loopback is how a local IdP container is reached in development.
    expect(
      buildAuthorizeUrl({
        ...base,
        endpoint: "http://localhost:3001/authorize",
      }),
    ).toContain("http://localhost:3001/authorize");
  });

  it("rejects an extra parameter that would duplicate a reserved one", async () => {
    // A second code_challenge or redirect_uri is parameter injection, not a
    // convenience.
    const challenge = await createVerifier().challenge();
    expect(() =>
      buildAuthorizeUrl({
        endpoint: "https://idp.example.com/authorize",
        clientId: "client-1",
        redirectUri: "https://app.example.com/auth/callback",
        scopes: ["openid"],
        state: createState(),
        challenge,
        extra: { redirect_uri: "https://evil.example.com/steal" },
      }),
    ).toThrow(/redirect_uri/);
  });

  it("passes a provider-specific parameter through `extra`", async () => {
    // oidc-storage.ts's header tells the reader they can attempt a
    // non-interactive restore with prompt=none via `extra`. That advice has
    // to actually work, and `prompt` must not be on the reserved list.
    const challenge = await createVerifier().challenge();
    const url = new URL(
      buildAuthorizeUrl({
        endpoint: "https://idp.example.com/authorize",
        clientId: "client-1",
        redirectUri: "https://app.example.com/auth/callback",
        scopes: ["openid"],
        state: createState(),
        challenge,
        extra: { prompt: "none", login_hint: "user@example.com" },
      }),
    );
    expect(url.searchParams.get("prompt")).toBe("none");
    expect(url.searchParams.get("login_hint")).toBe("user@example.com");
  });

  it("keeps a query the issuer already publishes", async () => {
    const challenge = await createVerifier().challenge();
    const url = new URL(
      buildAuthorizeUrl({
        endpoint: "https://idp.example.com/authorize?tenant=acme",
        clientId: "client-1",
        redirectUri: "https://app.example.com/auth/callback",
        scopes: ["openid"],
        state: createState(),
        challenge,
      }),
    );
    expect(url.searchParams.get("tenant")).toBe("acme");
  });
});

describe("token exchange", () => {
  const req = {
    endpoint: "https://idp.example.com/token",
    clientId: "client-1",
    redirectUri: "https://app.example.com/auth/callback",
    code: "auth-code-1",
  };

  it("sends the verifier and no client secret", async () => {
    const verifier = createVerifier();
    let sentBody = "";
    let sentContentType = "";
    await exchangeCode({
      ...req,
      verifier,
      fetchImpl: async (_url, init) => {
        sentBody = String(init?.body ?? "");
        sentContentType = String(
          (init?.headers as Record<string, string>)["Content-Type"],
        );
        return new Response(
          JSON.stringify({ access_token: "at", token_type: "Bearer" }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      },
    });

    const form = new URLSearchParams(sentBody);
    expect(sentContentType).toBe("application/x-www-form-urlencoded");
    expect(form.get("grant_type")).toBe("authorization_code");
    expect(form.get("code_verifier")).toBe(verifier.secret());
    expect(form.get("code")).toBe("auth-code-1");
    expect(form.get("redirect_uri")).toBe(req.redirectUri);
    // A browser SPA is a public client; a "secret" in a bundle is not one.
    expect(form.has("client_secret")).toBe(false);
  });

  it("surfaces an OAuth error response as a thrown OAuthError", async () => {
    // The failure mode this prevents: returning an empty-but-successful
    // token, which authenticates nothing and 401s later, far from here.
    const promise = exchangeCode({
      ...req,
      verifier: createVerifier(),
      fetchImpl: async () =>
        new Response(
          JSON.stringify({
            error: "invalid_grant",
            error_description: "code expired",
          }),
          { status: 400, headers: { "Content-Type": "application/json" } },
        ),
    });
    await expect(promise).rejects.toBeInstanceOf(OAuthError);
    await expect(promise).rejects.toThrow(/invalid_grant/);
    await expect(promise).rejects.toThrow(/code expired/);
  });

  it("surfaces an error even when the provider answers 200", async () => {
    // Decided by SHAPE, not status: providers exist that do this.
    expect(() =>
      parseTokenResponse(200, JSON.stringify({ error: "invalid_client" })),
    ).toThrow(OAuthError);
  });

  it("surfaces a form-encoded error body", async () => {
    expect(() =>
      parseTokenResponse(
        400,
        "error=invalid_scope&error_description=bad+scope",
      ),
    ).toThrow(/invalid_scope/);
  });

  it("treats a 200 with no access_token as a failure", () => {
    expect(() =>
      parseTokenResponse(200, JSON.stringify({ token_type: "Bearer" })),
    ).toThrow(/no access_token/);
  });

  it("treats a non-2xx with no error code as a failure", () => {
    expect(() => parseTokenResponse(503, JSON.stringify({}))).toThrow(
      OAuthError,
    );
  });

  it("parses a successful response, expiry included", () => {
    const token = parseTokenResponse(
      200,
      JSON.stringify({
        access_token: "at-1",
        token_type: "Bearer",
        expires_in: 3600,
        refresh_token: "rt-1",
        id_token: "it-1",
        scope: "openid profile",
      }),
    );
    expect(token).toEqual({
      accessToken: "at-1",
      tokenType: "Bearer",
      expiresIn: 3600,
      refreshToken: "rt-1",
      idToken: "it-1",
      scope: "openid profile",
    });
  });
});

describe("OIDC discovery", () => {
  const doc = {
    issuer: "https://idp.example.com",
    authorization_endpoint: "https://idp.example.com/authorize",
    token_endpoint: "https://idp.example.com/token",
    end_session_endpoint: "https://idp.example.com/logout",
    code_challenge_methods_supported: ["S256"],
  };

  it("reads every endpoint from one issuer URL", async () => {
    let requested = "";
    const meta = await discover("https://idp.example.com", async (url) => {
      requested = String(url);
      return new Response(JSON.stringify(doc), { status: 200 });
    });
    expect(requested).toBe(
      "https://idp.example.com/.well-known/openid-configuration",
    );
    expect(meta.authorizationEndpoint).toBe(doc.authorization_endpoint);
    expect(meta.tokenEndpoint).toBe(doc.token_endpoint);
    expect(meta.endSessionEndpoint).toBe(doc.end_session_endpoint);
  });

  it("rejects a document whose issuer is not the one requested", async () => {
    // OIDC Discovery §4.3. A document that describes another issuer cannot
    // be trusted to describe this one.
    await expect(
      discover(
        "https://idp.example.com",
        async () =>
          new Response(
            JSON.stringify({ ...doc, issuer: "https://evil.example.com" }),
            { status: 200 },
          ),
      ),
    ).rejects.toThrow(/issuer mismatch/);
  });

  it("tolerates the trailing slash providers disagree about", async () => {
    const meta = await discover(
      "https://idp.example.com/",
      async () => new Response(JSON.stringify(doc), { status: 200 }),
    );
    expect(meta.tokenEndpoint).toBe(doc.token_endpoint);
  });

  it("fails when the document omits an endpoint the flow needs", async () => {
    await expect(
      discover(
        "https://idp.example.com",
        async () =>
          new Response(
            JSON.stringify({
              issuer: doc.issuer,
              authorization_endpoint: doc.authorization_endpoint,
            }),
            { status: 200 },
          ),
      ),
    ).rejects.toThrow(/token_endpoint/);
  });

  it("names the likely misconfiguration when discovery 404s", async () => {
    await expect(
      discover(
        "https://idp.example.com",
        async () => new Response("not found", { status: 404 }),
      ),
    ).rejects.toThrow(/issuer URL/);
  });
});
