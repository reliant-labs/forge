// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/config.ts — the native OIDC client's configuration, read from
// the EXPO_PUBLIC_FORGE_OIDC_* variables forge delivers.
//
//   EXPO_PUBLIC_FORGE_OIDC_ISSUER                  issuer URL the DEVICE can reach (under
//                                                  `forge env up --device-host` the Mac's LAN address)
//   EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID               the native app's client_id
//   EXPO_PUBLIC_FORGE_OIDC_SCOPES                  space-separated (optional)
//   EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI            where the IdP sends the user back
//   EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI  after end-session (optional)
//
// Absent ISSUER or CLIENT_ID means "native auth is not wired": readOidcConfig()
// returns null and the app keeps its mock/unwired provider. A value that IS
// present but wrong throws OidcConfigError — a typo must be loud, never a
// silent fallback to a signed-out app.
//
// Expo inlines EXPO_PUBLIC_* variables at bundle time ONLY for the static form
// `process.env.NAME`. Dynamic reads (`process.env[name]`) are undefined on a
// device, so every variable is read by its literal name below.
//
// REDIRECT URIS ARE OPAQUE FULL URIs. Today forge ships a private-use scheme
// (`com.<project>.<app>:/oauthredirect`, RFC 8252 §7.1); an https universal/app
// link (`https://host/oauthredirect`) is accepted as-is so moving to one later
// is a configuration change, not a code change. Nothing in this library assumes
// a custom scheme.

export interface OidcConfig {
  issuer: string;
  clientId: string;
  scopes: string[];
  redirectUri: string;
  postLogoutRedirectUri: string | null;
}

export interface RawOidcEnv {
  issuer?: string;
  clientId?: string;
  scopes?: string;
  redirectUri?: string;
  postLogoutRedirectUri?: string;
}

export interface ParseOptions {
  /** Allow http:// to a loopback host. Defaults to __DEV__. */
  allowInsecureLoopback?: boolean;
}

export const DEFAULT_SCOPES = ["openid", "profile", "email", "offline_access"];

export class OidcConfigError extends Error {
  readonly variable: string;
  constructor(variable: string, message: string) {
    super(`${variable}: ${message}`);
    Object.setPrototypeOf(this, new.target.prototype);
    this.name = "OidcConfigError";
    this.variable = variable;
  }
}

export interface ParsedUri {
  scheme: string;
  /** null when the URI has no (or an empty) `//authority` part (e.g. `com.app:/path`). */
  authority: string | null;
  path: string;
  query: string;
  fragment: string | null;
}

// RFC 3986 appendix B, restricted to a mandatory scheme. Hand-rolled because
// React Native's URL implementation is partial (and absent in some contexts).
const URI_RE = /^([a-zA-Z][a-zA-Z0-9+.-]*):(?:\/\/([^/?#]*))?([^?#]*)(?:\?([^#]*))?(?:#(.*))?$/;

export function parseUri(uri: string): ParsedUri | null {
  const m = URI_RE.exec(uri);
  if (!m) return null;
  return {
    scheme: m[1].toLowerCase(),
    // `scheme:/p` and `scheme:///p` are the same place (empty authority): one form.
    authority: m[2] ? m[2].toLowerCase() : null,
    path: m[3] ?? "",
    query: m[4] ?? "",
    fragment: m[5] === undefined ? null : m[5],
  };
}

export function isLoopbackHost(authority: string | null): boolean {
  if (!authority) return false;
  const host = authority.replace(/^.*@/, "").replace(/:\d+$/, "");
  return (
    host === "localhost" || host.endsWith(".localhost") || host === "127.0.0.1" || host === "[::1]"
  );
}

/**
 * True for a host a development IdP is reached at from a device: loopback, or a
 * private-network address (RFC 1918 IPv4, RFC 6598, link-local, IPv6 ULA and
 * link-local) or a `.local` mDNS name. `forge env up --device-host <LAN address>`
 * makes the dev IdP's issuer that address, over plain http. Public hosts are never
 * included, and this is only consulted when `allowInsecureLoopback` (__DEV__) is on.
 */
export function isDevNetworkHost(authority: string | null): boolean {
  if (isLoopbackHost(authority)) return true;
  if (!authority) return false;
  const host = authority.replace(/^.*@/, "").replace(/:\d+$/, "");
  if (host.endsWith(".local")) return true;
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(host);
  if (v4) {
    const [a, b, c, d] = v4.slice(1).map(Number);
    if ([a, b, c, d].some((n) => n > 255)) return false;
    return (
      a === 10 ||
      (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && b === 168) ||
      (a === 169 && b === 254) ||
      (a === 100 && b >= 64 && b <= 127)
    );
  }
  if (host.startsWith("[") && host.endsWith("]")) {
    const v6 = host.slice(1, -1).toLowerCase();
    return /^f[cd][0-9a-f]{2}:/.test(v6) || /^fe[89ab][0-9a-f]:/.test(v6);
  }
  return false;
}

// Schemes that can never be an app callback, plus Expo Go's own (`exp://`,
// which cannot be registered with an IdP — native auth needs a dev client).
const FORBIDDEN_SCHEMES = new Set([
  "javascript",
  "data",
  "file",
  "blob",
  "about",
  "vbscript",
  "ftp",
  "ws",
  "wss",
  "exp",
  "exps",
]);

/** True for https, or http to a dev-network host (loopback or private LAN) when `allowInsecureLoopback`. */
export function isSecureEndpoint(uri: ParsedUri, allowInsecureLoopback: boolean): boolean {
  if (!uri.authority) return false;
  if (uri.scheme === "https") return true;
  return uri.scheme === "http" && allowInsecureLoopback && isDevNetworkHost(uri.authority);
}

function isDev(): boolean {
  return typeof __DEV__ === "undefined" ? false : __DEV__;
}

function parseIssuer(name: string, value: string, allowInsecureLoopback: boolean): string {
  const uri = parseUri(value);
  if (!uri || !isSecureEndpoint(uri, allowInsecureLoopback)) {
    throw new OidcConfigError(
      name,
      `must be an https:// URL (http:// only to localhost or a private-network address in development), got ${JSON.stringify(value)}`,
    );
  }
  if (uri.query || uri.fragment !== null) {
    throw new OidcConfigError(name, "must not carry a query or fragment");
  }
  return value;
}

function parseRedirect(name: string, value: string, allowInsecureLoopback: boolean): string {
  const uri = parseUri(value);
  if (!uri) {
    throw new OidcConfigError(name, `is not a valid URI: ${JSON.stringify(value)}`);
  }
  if (uri.fragment !== null) {
    throw new OidcConfigError(name, "must not carry a fragment (RFC 6749 §3.1.2)");
  }
  if (uri.scheme === "http") {
    if (!(allowInsecureLoopback && isLoopbackHost(uri.authority))) {
      throw new OidcConfigError(
        name,
        "http:// is only allowed to a loopback host in development; use https:// or a private-use scheme",
      );
    }
  } else if (uri.scheme === "https") {
    if (!uri.authority) throw new OidcConfigError(name, "https:// redirect needs a host");
  } else if (FORBIDDEN_SCHEMES.has(uri.scheme)) {
    throw new OidcConfigError(
      name,
      `scheme "${uri.scheme}:" cannot receive an OAuth callback; use a private-use scheme such as com.example.app:/oauthredirect or an https:// link`,
    );
  }
  return value;
}

function present(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  return trimmed ? trimmed : undefined;
}

/**
 * Validate a raw variable bag. Exported for tests and for apps that source
 * their configuration somewhere other than process.env.
 */
export function parseOidcConfig(raw: RawOidcEnv, options: ParseOptions = {}): OidcConfig | null {
  const allowInsecureLoopback = options.allowInsecureLoopback ?? isDev();
  const issuer = present(raw.issuer);
  const clientId = present(raw.clientId);
  if (!issuer || !clientId) return null;

  const redirect = present(raw.redirectUri);
  if (!redirect) {
    throw new OidcConfigError(
      "EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI",
      "is required when EXPO_PUBLIC_FORGE_OIDC_ISSUER and EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID are set",
    );
  }
  const scopesRaw = present(raw.scopes);
  const scopes = scopesRaw ? scopesRaw.split(/\s+/) : [...DEFAULT_SCOPES];
  if (!scopes.includes("openid")) {
    throw new OidcConfigError("EXPO_PUBLIC_FORGE_OIDC_SCOPES", 'must include "openid"');
  }
  const postLogout = present(raw.postLogoutRedirectUri);
  return {
    issuer: parseIssuer("EXPO_PUBLIC_FORGE_OIDC_ISSUER", issuer, allowInsecureLoopback),
    clientId,
    scopes: [...new Set(scopes)],
    redirectUri: parseRedirect(
      "EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI",
      redirect,
      allowInsecureLoopback,
    ),
    postLogoutRedirectUri: postLogout
      ? parseRedirect(
          "EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI",
          postLogout,
          allowInsecureLoopback,
        )
      : null,
  };
}

/** The configuration baked into this bundle, or null when native auth is not wired. */
export function readOidcConfig(): OidcConfig | null {
  return parseOidcConfig({
    issuer: process.env.EXPO_PUBLIC_FORGE_OIDC_ISSUER,
    clientId: process.env.EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID,
    scopes: process.env.EXPO_PUBLIC_FORGE_OIDC_SCOPES,
    redirectUri: process.env.EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI,
    postLogoutRedirectUri: process.env.EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI,
  });
}
