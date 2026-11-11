// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/redirect.ts — which redirect URI one sign-in attempt uses.
//
// The IdP has two redirects registered for the app, in order: an https
// universal link / App Link (`https://<host>/oauthredirect`) and the
// private-use scheme (`com.<project>.<app>:/oauthredirect`). Any app on a device
// can claim a private-use scheme (RFC 8252 §8.6); an https callback on a host
// the app is associated with can only be delivered to the signed app. So the
// https redirect is preferred WHEN THE PLATFORM CAN COMPLETE IT:
//
//   android       a verified App Link is handed to the app by the Custom Tab
//                 (the Linking `url` event expo-web-browser races on).
//   ios >= 17.4   expo-web-browser builds ASWebAuthenticationSession with
//                 `.https(host:path:)` when `preferUniversalLinks` is set.
//   ios <  17.4   the session can only intercept a custom scheme, so an https
//                 redirect would never complete: the private-use entry is used.
//   anything else (web, an unknown version) takes the private-use entry.
//
// The chosen URI is returned ONCE per attempt and used for the authorize
// request's `redirect_uri`, for matching the callback, and for the token
// exchange (RFC 6749 §4.1.3 requires the same value). It is a plain value, so
// the caller carries it through the attempt; nothing here holds state.
//
// A config with no fallback redirect (a project with no associated domains, or
// one that registered only https) always uses its one redirect.

import type { OidcConfig } from "./config";
import { parseUri } from "./config";

/** What the decision needs to know about the device. */
export interface PlatformInfo {
  os: string;
  /** Platform.Version: a string like "17.4" on iOS, an API level number on Android. */
  version: string | number | null | undefined;
}

/** iOS 17.4 is the first with ASWebAuthenticationSession.Callback.https. */
export const MIN_IOS_FOR_HTTPS_CALLBACK: readonly [number, number] = [17, 4];

/** The (major, minor) of an iOS version string, or null when it is not one. */
export function parseIosVersion(version: string | number | null | undefined): [number, number] | null {
  if (version === null || version === undefined) return null;
  const m = /^(\d+)(?:\.(\d+))?/.exec(String(version).trim());
  if (!m) return null;
  return [Number(m[1]), m[2] === undefined ? 0 : Number(m[2])];
}

export function iosSupportsHttpsCallback(version: string | number | null | undefined): boolean {
  const v = parseIosVersion(version);
  if (!v) return false;
  const [major, minor] = MIN_IOS_FOR_HTTPS_CALLBACK;
  return v[0] > major || (v[0] === major && v[1] >= minor);
}

export function isHttpsRedirect(uri: string | null | undefined): boolean {
  return !!uri && parseUri(uri)?.scheme === "https";
}

/** Can this device complete an https callback? */
export function canUseHttpsRedirect(platform: PlatformInfo): boolean {
  if (platform.os === "android") return true;
  if (platform.os === "ios") return iosSupportsHttpsCallback(platform.version);
  return false;
}

/** The redirect pair of one attempt, and whether to ask the browser for universal links. */
export interface RedirectChoice {
  redirectUri: string;
  postLogoutRedirectUri: string | null;
  /** True when `redirectUri` is the https link: pass `preferUniversalLinks`. */
  preferUniversalLinks: boolean;
}

type RedirectConfig = Pick<
  OidcConfig,
  "redirectUri" | "postLogoutRedirectUri" | "fallbackRedirectUri" | "fallbackPostLogoutRedirectUri"
>;

/**
 * Pick the redirect for one sign-in attempt. https when configured and the
 * platform can complete it, else the private-use fallback; a config with no
 * fallback keeps its single redirect.
 */
export function chooseRedirect(config: RedirectConfig, platform: PlatformInfo): RedirectChoice {
  const https = isHttpsRedirect(config.redirectUri);
  if (https && !canUseHttpsRedirect(platform) && config.fallbackRedirectUri) {
    return {
      redirectUri: config.fallbackRedirectUri,
      postLogoutRedirectUri: config.fallbackPostLogoutRedirectUri ?? config.postLogoutRedirectUri,
      preferUniversalLinks: false,
    };
  }
  return {
    redirectUri: config.redirectUri,
    postLogoutRedirectUri: config.postLogoutRedirectUri,
    preferUniversalLinks: https && canUseHttpsRedirect(platform),
  };
}
