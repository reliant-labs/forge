// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { describe, expect, it } from "@jest/globals";
import {
  canUseHttpsRedirect,
  chooseRedirect,
  iosSupportsHttpsCallback,
  isHttpsRedirect,
  parseIosVersion,
} from "../src/lib/auth/redirect";
import type { OidcConfig } from "../src/lib/auth/config";
import { matchesRedirect } from "../src/lib/auth/oidc";

const HTTPS = "https://app.acme.example/oauthredirect";
const HTTPS_OUT = "https://app.acme.example/signedout";
const SCHEME = "com.acme.app:/oauthredirect";
const SCHEME_OUT = "com.acme.app:/signedout";

const both: Pick<
  OidcConfig,
  "redirectUri" | "postLogoutRedirectUri" | "fallbackRedirectUri" | "fallbackPostLogoutRedirectUri"
> = {
  redirectUri: HTTPS,
  postLogoutRedirectUri: HTTPS_OUT,
  fallbackRedirectUri: SCHEME,
  fallbackPostLogoutRedirectUri: SCHEME_OUT,
};
const schemeOnly = {
  redirectUri: SCHEME,
  postLogoutRedirectUri: SCHEME_OUT,
  fallbackRedirectUri: null,
  fallbackPostLogoutRedirectUri: null,
};

describe("iOS version", () => {
  it.each([
    ["17.4", true],
    ["17.4.1", true],
    ["17.10", true],
    ["18", true],
    ["18.0", true],
    ["26.1", true],
    ["17.3.1", false],
    ["17.0", false],
    ["16.7", false],
    ["15", false],
    [17.4, true],
    [17, false],
  ])("%s -> https callback %s", (version, want) => {
    expect(iosSupportsHttpsCallback(version)).toBe(want);
  });

  it("is not supported when the version is unknown or garbage", () => {
    for (const v of [null, undefined, "", "beta", "v17.4"])
      expect(iosSupportsHttpsCallback(v)).toBe(false);
    expect(parseIosVersion("17.4.1")).toEqual([17, 4]);
    expect(parseIosVersion("17")).toEqual([17, 0]);
  });
});

describe("canUseHttpsRedirect — OS x version", () => {
  it.each([
    [{ os: "android", version: 34 }, true],
    [{ os: "android", version: 23 }, true],
    [{ os: "ios", version: "17.4" }, true],
    [{ os: "ios", version: "17.3" }, false],
    [{ os: "ios", version: "16.0" }, false],
    [{ os: "web", version: undefined }, false],
    [{ os: "windows", version: "10" }, false],
  ])("%j -> %s", (platform, want) => {
    expect(canUseHttpsRedirect(platform)).toBe(want);
  });
});

describe("chooseRedirect — decision table (OS x version x scheme)", () => {
  const cases: [string, typeof both, { os: string; version: string | number }, string, boolean][] =
    [
      ["android + https", both, { os: "android", version: 34 }, HTTPS, true],
      ["ios 17.4 + https", both, { os: "ios", version: "17.4" }, HTTPS, true],
      ["ios 18 + https", both, { os: "ios", version: "18.1" }, HTTPS, true],
      [
        "ios 17.3 + https -> the private-use fallback",
        both,
        { os: "ios", version: "17.3.1" },
        SCHEME,
        false,
      ],
      [
        "ios 16 + https -> the private-use fallback",
        both,
        { os: "ios", version: "16.7" },
        SCHEME,
        false,
      ],
      ["web + https -> the private-use fallback", both, { os: "web", version: "" }, SCHEME, false],
      ["android + scheme only", schemeOnly, { os: "android", version: 34 }, SCHEME, false],
      ["ios 17.4 + scheme only", schemeOnly, { os: "ios", version: "17.4" }, SCHEME, false],
      ["ios 16 + scheme only", schemeOnly, { os: "ios", version: "16.0" }, SCHEME, false],
    ];
  it.each(cases)("%s", (_name, config, platform, want, universal) => {
    const choice = chooseRedirect(config, platform);
    expect(choice.redirectUri).toBe(want);
    expect(choice.preferUniversalLinks).toBe(universal);
  });

  it("returns the matching post-logout URI with the chosen redirect", () => {
    expect(chooseRedirect(both, { os: "android", version: 34 }).postLogoutRedirectUri).toBe(
      HTTPS_OUT,
    );
    expect(chooseRedirect(both, { os: "ios", version: "16.0" }).postLogoutRedirectUri).toBe(
      SCHEME_OUT,
    );
  });

  it("never invents a redirect: https with no fallback keeps https", () => {
    const only = { ...both, fallbackRedirectUri: null, fallbackPostLogoutRedirectUri: null };
    const choice = chooseRedirect(only, { os: "ios", version: "16.0" });
    expect(choice.redirectUri).toBe(HTTPS);
    expect(choice.preferUniversalLinks).toBe(false);
  });

  it("keeps the configured post-logout when the fallback has none", () => {
    const noOut = { ...both, fallbackPostLogoutRedirectUri: null };
    expect(chooseRedirect(noOut, { os: "ios", version: "16.0" }).postLogoutRedirectUri).toBe(
      HTTPS_OUT,
    );
  });
});

describe("isHttpsRedirect and callback matching", () => {
  it("tells the two forms apart", () => {
    expect(isHttpsRedirect(HTTPS)).toBe(true);
    expect(isHttpsRedirect(SCHEME)).toBe(false);
    expect(isHttpsRedirect(null)).toBe(false);
  });

  it("matches a callback by scheme + authority + path for either form", () => {
    expect(matchesRedirect(`${HTTPS}?code=c&state=s`, HTTPS)).toBe(true);
    expect(matchesRedirect(`${SCHEME}?code=c&state=s`, SCHEME)).toBe(true);
    // The other form is NOT a match: one attempt, one redirect.
    expect(matchesRedirect(`${SCHEME}?code=c`, HTTPS)).toBe(false);
    expect(matchesRedirect(`${HTTPS}?code=c`, SCHEME)).toBe(false);
    // A different host or path on https is not ours.
    expect(matchesRedirect("https://evil.example/oauthredirect?code=c", HTTPS)).toBe(false);
    expect(matchesRedirect("https://app.acme.example/other?code=c", HTTPS)).toBe(false);
    expect(matchesRedirect("https://APP.acme.example/oauthredirect/?code=c", HTTPS)).toBe(true);
  });
});
