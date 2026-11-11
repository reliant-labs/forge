// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { afterEach, beforeEach, describe, expect, it } from "@jest/globals";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

const withForgeLinks = require("../plugins/with-forge-links");
const { applyAppLinks, resolveSettings, CONFIG_FILE } = withForgeLinks;

const HOST = "app.acme.example";
const settings = (over = {}) => ({ domains: [HOST], developer: false, ...over });

describe("with-forge-links: applyAppLinks", () => {
  it("does nothing without a domain, returning the config untouched", () => {
    const config = { name: "x", ios: { bundleIdentifier: "com.acme.x" } };
    expect(applyAppLinks(config, { domains: [], developer: false })).toBe(config);
  });

  it("declares applinks and webcredentials for each host (both services)", () => {
    const out = applyAppLinks({}, settings({ domains: [HOST, "acme.example"] }));
    expect(out.ios.associatedDomains).toEqual([
      `applinks:${HOST}`,
      `webcredentials:${HOST}`,
      "applinks:acme.example",
      "webcredentials:acme.example",
    ]);
  });

  it("developer mode appends ?mode=developer to both services, and only then", () => {
    expect(applyAppLinks({}, settings({ developer: true })).ios.associatedDomains).toEqual([
      `applinks:${HOST}?mode=developer`,
      `webcredentials:${HOST}?mode=developer`,
    ]);
    for (const entry of applyAppLinks({}, settings()).ios.associatedDomains) {
      expect(entry).not.toContain("mode=developer");
    }
  });

  it("adds one autoVerify https intent filter with the callback path prefixes", () => {
    const [filter, ...rest] = applyAppLinks({}, settings()).android.intentFilters;
    expect(rest).toHaveLength(0);
    expect(filter).toEqual({
      action: "VIEW",
      autoVerify: true,
      data: [
        { scheme: "https", host: HOST, pathPrefix: "/oauthredirect" },
        { scheme: "https", host: HOST, pathPrefix: "/signedout" },
      ],
      category: ["BROWSABLE", "DEFAULT"],
    });
  });

  it("keeps entitlements and intent filters the app already has", () => {
    const own = { action: "VIEW", data: [{ scheme: "myapp" }], category: ["BROWSABLE", "DEFAULT"] };
    const out = applyAppLinks(
      { ios: { associatedDomains: ["applinks:other.example"] }, android: { intentFilters: [own] } },
      settings(),
    );
    expect(out.ios.associatedDomains).toContain("applinks:other.example");
    expect(out.android.intentFilters).toContainEqual(own);
  });

  it("keeps an app-owned autoVerify filter on the same host for another path", () => {
    const own = {
      action: "VIEW",
      autoVerify: true,
      data: [{ scheme: "https", host: HOST, pathPrefix: "/documents" }],
      category: ["BROWSABLE", "DEFAULT"],
    };
    const out = applyAppLinks({ android: { intentFilters: [own] } }, settings());
    expect(out.android.intentFilters).toContainEqual(own);
    expect(out.android.intentFilters).toHaveLength(2);
  });

  it("is idempotent, and a mode change replaces rather than duplicates", () => {
    const once = applyAppLinks({}, settings({ developer: true }));
    expect(applyAppLinks(once, settings({ developer: true }))).toEqual(once);
    const released = applyAppLinks(once, settings({ developer: false }));
    expect(released.ios.associatedDomains).toEqual([`applinks:${HOST}`, `webcredentials:${HOST}`]);
    expect(released.android.intentFilters).toHaveLength(1);
  });
});

describe("with-forge-links: resolveSettings", () => {
  let root;
  beforeEach(() => {
    root = fs.mkdtempSync(path.join(os.tmpdir(), "forge-links-"));
  });
  afterEach(() => fs.rmSync(root, { recursive: true, force: true }));

  it("is empty with neither the environment nor the file", () => {
    expect(resolveSettings({}, root)).toEqual({ domains: [], developer: false });
  });

  it("reads the comma-separated environment, trimming and de-duplicating", () => {
    expect(
      resolveSettings(
        {
          FORGE_ASSOCIATED_DOMAINS: ` ${HOST}, acme.example,${HOST} `,
          FORGE_APP_LINK_MODE: "developer",
        },
        root,
      ),
    ).toEqual({ domains: [HOST, "acme.example"], developer: true });
  });

  it("falls back to the file forge writes, and the environment wins", () => {
    fs.writeFileSync(
      path.join(root, CONFIG_FILE),
      JSON.stringify({ FORGE_ASSOCIATED_DOMAINS: HOST, FORGE_APP_LINK_MODE: "developer" }),
    );
    expect(resolveSettings({}, root)).toEqual({ domains: [HOST], developer: true });
    expect(
      resolveSettings({ FORGE_ASSOCIATED_DOMAINS: "acme.example", FORGE_APP_LINK_MODE: "" }, root),
    ).toEqual({
      domains: ["acme.example"],
      developer: false,
    });
  });

  it("refuses anything that is not a bare hostname", () => {
    for (const bad of [
      "https://app.acme.example",
      "app.acme.example:8443",
      "*.acme.example",
      "app.acme.example/x",
      "localhost",
    ]) {
      expect(() => resolveSettings({ FORGE_ASSOCIATED_DOMAINS: bad }, root)).toThrow(
        /bare lowercase hostname/,
      );
    }
  });

  it("ignores a malformed file rather than failing the prebuild", () => {
    fs.writeFileSync(path.join(root, CONFIG_FILE), "{not json");
    expect(resolveSettings({}, root)).toEqual({ domains: [], developer: false });
  });
});

describe("with-forge-links: the plugin entry point", () => {
  it("is a function of the config that does nothing in a domain-less environment", () => {
    const saved = process.env.FORGE_ASSOCIATED_DOMAINS;
    delete process.env.FORGE_ASSOCIATED_DOMAINS;
    try {
      const config = { name: "x" };
      expect(withForgeLinks(config)).toEqual(config);
    } finally {
      if (saved !== undefined) process.env.FORGE_ASSOCIATED_DOMAINS = saved;
    }
  });
});
