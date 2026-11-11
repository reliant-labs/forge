// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { afterEach, describe, expect, it } from "@jest/globals";

import { OidcConfigError, parseOidcConfig, readOidcConfig } from "../src/lib/auth/config";

const base = {
  issuer: "https://idp.example.com",
  clientId: "cid",
  redirectUri: "com.acme.app:/oauthredirect",
};

describe("parseOidcConfig", () => {
  it("returns null when issuer or client id is absent (native auth not wired)", () => {
    expect(parseOidcConfig({})).toBeNull();
    expect(parseOidcConfig({ issuer: base.issuer })).toBeNull();
    expect(parseOidcConfig({ clientId: "cid" })).toBeNull();
    expect(parseOidcConfig({ issuer: "  ", clientId: "" })).toBeNull();
  });

  it("parses a full private-use-scheme configuration", () => {
    const cfg = parseOidcConfig({
      ...base,
      scopes: "openid profile  email offline_access urn:zitadel:iam:org:project:id:1:aud",
      postLogoutRedirectUri: "com.acme.app:/signedout",
    });
    expect(cfg).toEqual({
      issuer: "https://idp.example.com",
      clientId: "cid",
      scopes: [
        "openid",
        "profile",
        "email",
        "offline_access",
        "urn:zitadel:iam:org:project:id:1:aud",
      ],
      redirectUri: "com.acme.app:/oauthredirect",
      postLogoutRedirectUri: "com.acme.app:/signedout",
      fallbackRedirectUri: null,
      fallbackPostLogoutRedirectUri: null,
    });
  });

  it("defaults the scopes and leaves post-logout null", () => {
    const cfg = parseOidcConfig(base);
    expect(cfg?.scopes).toEqual(["openid", "profile", "email", "offline_access"]);
    expect(cfg?.postLogoutRedirectUri).toBeNull();
  });

  it("accepts an https universal/app link redirect unchanged", () => {
    const cfg = parseOidcConfig({
      ...base,
      redirectUri: "https://app.example.com/oauthredirect",
      postLogoutRedirectUri: "https://app.example.com/signedout",
    });
    expect(cfg?.redirectUri).toBe("https://app.example.com/oauthredirect");
    expect(cfg?.postLogoutRedirectUri).toBe("https://app.example.com/signedout");
  });

  it("parses an https redirect with its private-use fallback pair", () => {
    const cfg = parseOidcConfig({
      ...base,
      redirectUri: "https://app.example.com/oauthredirect",
      postLogoutRedirectUri: "https://app.example.com/signedout",
      fallbackRedirectUri: "com.acme.app:/oauthredirect",
      fallbackPostLogoutRedirectUri: "com.acme.app:/signedout",
    });
    expect(cfg?.fallbackRedirectUri).toBe("com.acme.app:/oauthredirect");
    expect(cfg?.fallbackPostLogoutRedirectUri).toBe("com.acme.app:/signedout");
  });

  it("refuses an https fallback: it is what an https redirect falls back FROM", () => {
    expect(() =>
      parseOidcConfig({
        ...base,
        redirectUri: "https://app.example.com/oauthredirect",
        fallbackRedirectUri: "https://other.example.com/oauthredirect",
      }),
    ).toThrow(/private-use scheme/);
    expect(() =>
      parseOidcConfig({ ...base, fallbackRedirectUri: "javascript:1" }),
    ).toThrow(OidcConfigError);
  });

  it("allows http only to loopback and only in development", () => {
    const dev = { allowInsecureLoopback: true };
    expect(parseOidcConfig({ ...base, issuer: "http://localhost:8080" }, dev)?.issuer).toBe(
      "http://localhost:8080",
    );
    expect(parseOidcConfig({ ...base, issuer: "http://127.0.0.1:8080" }, dev)?.issuer).toBe(
      "http://127.0.0.1:8080",
    );
    expect(
      parseOidcConfig({ ...base, redirectUri: "http://localhost:19006/oauthredirect" }, dev),
    ).not.toBeNull();
    expect(() =>
      parseOidcConfig(
        { ...base, issuer: "http://localhost:8080" },
        { allowInsecureLoopback: false },
      ),
    ).toThrow(OidcConfigError);
    expect(() => parseOidcConfig({ ...base, issuer: "http://idp.example.com" }, dev)).toThrow(
      /https/,
    );
    expect(() =>
      parseOidcConfig({ ...base, redirectUri: "http://evil.example.com/cb" }, dev),
    ).toThrow(/loopback/);
    expect(() =>
      parseOidcConfig(
        { ...base, redirectUri: "http://localhost/cb" },
        { allowInsecureLoopback: false },
      ),
    ).toThrow(/loopback/);
  });

  it("allows an http issuer on a private-network address only in development (forge env up --device-host)", () => {
    const dev = { allowInsecureLoopback: true };
    for (const host of [
      "192.168.1.20:8080",
      "10.0.0.110:8080",
      "172.16.0.5:8080",
      "172.31.255.1:8080",
      "169.254.10.2:8080",
      "100.64.0.9:8080",
      "my-mac.local:8080",
      "[fd12:3456::1]:8080",
    ]) {
      expect(parseOidcConfig({ ...base, issuer: `http://${host}` }, dev)?.issuer).toBe(
        `http://${host}`,
      );
    }
    expect(() =>
      parseOidcConfig(
        { ...base, issuer: "http://192.168.1.20:8080" },
        { allowInsecureLoopback: false },
      ),
    ).toThrow(OidcConfigError);
    for (const host of [
      "8.8.8.8:8080",
      "172.32.0.1:8080",
      "192.169.1.1:8080",
      "idp.example.com",
      "300.1.1.1",
    ]) {
      expect(() => parseOidcConfig({ ...base, issuer: `http://${host}` }, dev)).toThrow(/https/);
    }
    // Only the issuer widens: an http callback still has to be loopback.
    expect(() =>
      parseOidcConfig({ ...base, redirectUri: "http://192.168.1.20/oauthredirect" }, dev),
    ).toThrow(/loopback/);
  });

  it.each([
    ["javascript:alert(1)", /cannot receive an OAuth callback/],
    ["data:text/html,x", /cannot receive an OAuth callback/],
    ["file:///etc/passwd", /cannot receive an OAuth callback/],
    ["exp://192.168.1.2:8081/--/oauthredirect", /cannot receive an OAuth callback/],
    ["not a uri", /not a valid URI/],
    ["com.acme.app:/oauthredirect#frag", /fragment/],
    ["https:///nohost", /host|valid/],
  ])("rejects redirect %s with a clear error", (redirectUri, message) => {
    const call = () => parseOidcConfig({ ...base, redirectUri });
    expect(call).toThrow(OidcConfigError);
    expect(call).toThrow(message);
  });

  it("names the offending variable", () => {
    try {
      parseOidcConfig({ ...base, redirectUri: "javascript:1" });
      throw new Error("expected throw");
    } catch (e) {
      expect(e).toBeInstanceOf(OidcConfigError);
      expect((e as OidcConfigError).variable).toBe("EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI");
      expect((e as Error).message).toContain("EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI");
    }
  });

  it("requires a redirect URI once the client is configured", () => {
    expect(() => parseOidcConfig({ issuer: base.issuer, clientId: "cid" })).toThrow(
      /REDIRECT_URI.*required/,
    );
  });

  it("requires the openid scope and a clean issuer", () => {
    expect(() => parseOidcConfig({ ...base, scopes: "profile email" })).toThrow(/openid/);
    expect(() => parseOidcConfig({ ...base, issuer: "https://idp.example.com/?a=b" })).toThrow(
      /query/,
    );
    expect(() => parseOidcConfig({ ...base, issuer: "idp.example.com" })).toThrow(/https/);
  });
});

describe("readOidcConfig", () => {
  const keys = [
    "EXPO_PUBLIC_FORGE_OIDC_ISSUER",
    "EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID",
    "EXPO_PUBLIC_FORGE_OIDC_SCOPES",
    "EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI",
    "EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI",
    "EXPO_PUBLIC_FORGE_OIDC_FALLBACK_REDIRECT_URI",
    "EXPO_PUBLIC_FORGE_OIDC_FALLBACK_POST_LOGOUT_REDIRECT_URI",
  ] as const;
  const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
  afterEach(() => {
    for (const k of keys) {
      if (saved[k] === undefined) delete process.env[k];
      else process.env[k] = saved[k];
    }
  });

  it("is null when unset", () => {
    for (const k of keys) delete process.env[k];
    expect(readOidcConfig()).toBeNull();
  });

  it("reads each EXPO_PUBLIC_FORGE_OIDC_* variable", () => {
    process.env.EXPO_PUBLIC_FORGE_OIDC_ISSUER = "https://idp.example.com";
    process.env.EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID = "cid";
    process.env.EXPO_PUBLIC_FORGE_OIDC_SCOPES = "openid offline_access";
    process.env.EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI = "com.acme.app:/oauthredirect";
    process.env.EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI = "com.acme.app:/signedout";
    expect(readOidcConfig()).toEqual({
      issuer: "https://idp.example.com",
      clientId: "cid",
      scopes: ["openid", "offline_access"],
      redirectUri: "com.acme.app:/oauthredirect",
      postLogoutRedirectUri: "com.acme.app:/signedout",
      fallbackRedirectUri: null,
      fallbackPostLogoutRedirectUri: null,
    });
  });

  it("reads the fallback redirect variables", () => {
    process.env.EXPO_PUBLIC_FORGE_OIDC_ISSUER = "https://idp.example.com";
    process.env.EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID = "cid";
    process.env.EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI = "https://app.example.com/oauthredirect";
    process.env.EXPO_PUBLIC_FORGE_OIDC_FALLBACK_REDIRECT_URI = "com.acme.app:/oauthredirect";
    process.env.EXPO_PUBLIC_FORGE_OIDC_FALLBACK_POST_LOGOUT_REDIRECT_URI = "com.acme.app:/signedout";
    const cfg = readOidcConfig();
    expect(cfg?.redirectUri).toBe("https://app.example.com/oauthredirect");
    expect(cfg?.fallbackRedirectUri).toBe("com.acme.app:/oauthredirect");
    expect(cfg?.fallbackPostLogoutRedirectUri).toBe("com.acme.app:/signedout");
  });

  it("throws on a present-but-invalid value instead of falling back", () => {
    process.env.EXPO_PUBLIC_FORGE_OIDC_ISSUER = "https://idp.example.com";
    process.env.EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID = "cid";
    process.env.EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI = "javascript:1";
    expect(() => readOidcConfig()).toThrow(OidcConfigError);
  });
});
