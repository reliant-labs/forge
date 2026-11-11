// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// plugins/with-forge-links.js — an Expo config plugin that associates the app
// with the https hosts of its universal links (iOS) and App Links (Android), so
// the sign-in callback can be `https://<host>/oauthredirect` instead of a
// private-use scheme another app on the device could claim.
//
// It reads two values from the environment of the `expo prebuild` / `expo run`
// that runs it. `forge env up` sets them in its launch env and ALSO writes them
// to .forge-app-links.json beside this app (prebuild is a command you run, not
// a child of forge), which is the fallback; an environment variable wins.
//
//   FORGE_ASSOCIATED_DOMAINS  comma-separated bare hostnames (app.acme.example)
//   FORGE_APP_LINK_MODE       "developer" appends ?mode=developer to the iOS
//                             entitlements, so the device fetches the
//                             association file from the host directly instead
//                             of from Apple's CDN (which can lag by hours).
//                             Honoured only by a dev-signed build; forge sets
//                             it for a local env and never for a deployed one.
//
// With no domain it does NOTHING: a project that declares none ships no claim
// it cannot verify. What it adds:
//
//   ios.associatedDomains   applinks:<host> and webcredentials:<host>
//   android.intentFilters   a VIEW filter, autoVerify, https <host> with the
//                           /oauthredirect and /signedout path prefixes
//
// The files that bind the app to those hosts are rendered by forge at deploy
// (/.well-known/apple-app-site-association and assetlinks.json).

const fs = require("node:fs");
const path = require("node:path");

const CONFIG_FILE = ".forge-app-links.json";
const DOMAIN_RE = /^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$/;
const CALLBACK_PATHS = ["/oauthredirect", "/signedout"];

function readFileConfig(root) {
  try {
    return JSON.parse(fs.readFileSync(path.join(root, CONFIG_FILE), "utf8"));
  } catch {
    return {};
  }
}

/** The plugin's settings: environment first, then the file forge writes. */
function resolveSettings(env = process.env, root = process.cwd()) {
  const file = readFileConfig(root);
  const raw = env.FORGE_ASSOCIATED_DOMAINS ?? file.FORGE_ASSOCIATED_DOMAINS ?? "";
  const mode = env.FORGE_APP_LINK_MODE ?? file.FORGE_APP_LINK_MODE ?? "";
  const domains = [];
  for (const d of raw.split(",")) {
    const host = d.trim();
    if (!host) continue;
    if (!DOMAIN_RE.test(host)) {
      throw new Error(
        `with-forge-links: FORGE_ASSOCIATED_DOMAINS entry ${JSON.stringify(host)} is not a bare lowercase hostname (no scheme, port, path or wildcard)`,
      );
    }
    if (!domains.includes(host)) domains.push(host);
  }
  return { domains, developer: mode === "developer" };
}

function associatedDomains(domains, developer) {
  const suffix = developer ? "?mode=developer" : "";
  return domains.flatMap((host) => [
    `applinks:${host}${suffix}`,
    `webcredentials:${host}${suffix}`,
  ]);
}

function intentFilter(domains) {
  return {
    action: "VIEW",
    autoVerify: true,
    data: domains.flatMap((host) =>
      CALLBACK_PATHS.map((pathPrefix) => ({ scheme: "https", host, pathPrefix })),
    ),
    category: ["BROWSABLE", "DEFAULT"],
  };
}

function sameFilter(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

/**
 * Pure: the Expo config with the associations applied. Idempotent, and it keeps
 * every entitlement and intent filter the app already declares.
 */
function applyAppLinks(config, settings) {
  if (!settings.domains.length) return config;
  const ios = config.ios ?? {};
  const have = ios.associatedDomains ?? [];
  const want = associatedDomains(settings.domains, settings.developer);
  // A previous run's entry for the same host in the other mode is replaced, not
  // duplicated: developer mode must not stick once the env no longer sets it.
  const hostOf = (entry) => entry.replace(/\?mode=developer$/, "");
  const wantedHosts = new Set(want.map(hostOf));
  const kept = have.filter((entry) => !wantedHosts.has(hostOf(entry)));

  const android = config.android ?? {};
  const filter = intentFilter(settings.domains);
  // Do not remove an app-owned filter merely because it shares our host:
  // it may route unrelated paths. The plugin's addition is idempotent by its
  // complete structure, not by the authority alone.
  const filters = [...(android.intentFilters ?? [])];
  if (!filters.some((existing) => sameFilter(existing, filter))) filters.push(filter);

  return {
    ...config,
    ios: { ...ios, associatedDomains: [...kept, ...want] },
    android: { ...android, intentFilters: filters },
  };
}

/** The Expo config plugin entry point: `plugins: ["./plugins/with-forge-links"]`. */
function withForgeLinks(config) {
  return applyAppLinks(config, resolveSettings());
}

module.exports = withForgeLinks;
module.exports.applyAppLinks = applyAppLinks;
module.exports.resolveSettings = resolveSettings;
module.exports.CONFIG_FILE = CONFIG_FILE;
