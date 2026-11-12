// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/native.ts — the device bindings for the auth core: expo-crypto,
// expo-web-browser, expo-secure-store and the global fetch, assembled into a
// provider. The core modules (oidc.ts, oidc-provider.ts, token-store.ts) never
// import these natives directly, which is what lets Jest drive them with fakes.
//
//   const config = readOidcConfig();            // null → native auth not wired
//   const auth = config && createNativeAuthProvider(config);
//
// Expo Go cannot register an OAuth redirect: use a development client.

import * as Crypto from "expo-crypto";
import * as WebBrowser from "expo-web-browser";
import { Platform } from "react-native";

import type { OidcConfig } from "./config";
import type { CryptoLike, FetchLike } from "./oidc";
import { createOidcAuthProvider, type BrowserLike, type OidcAuthProvider } from "./oidc-provider";
import { session } from "./session";
import { createTokenStore, storageNamespace, type TokenStore } from "./token-store";

export const nativeCrypto: CryptoLike = {
  getRandomBytes: (n) => Crypto.getRandomBytes(n),
  sha256Base64: (input) =>
    Crypto.digestStringAsync(Crypto.CryptoDigestAlgorithm.SHA256, input, {
      encoding: Crypto.CryptoEncoding.BASE64,
    }),
};

export const nativeBrowser: BrowserLike = {
  // The redirect URI is passed through as-is. For an https universal/app link
  // `preferUniversalLinks` makes iOS 17.4+ intercept it with
  // ASWebAuthenticationSession's .https callback (it needs the Associated
  // Domains entitlement, plugins/with-forge-links.js); Android ignores it and a
  // verified App Link is delivered to the app instead.
  openAuthSession: (url, redirectUri, options) =>
    WebBrowser.openAuthSessionAsync(url, redirectUri, {
      preferEphemeralSession: options.preferEphemeralSession,
      preferUniversalLinks: options.preferUniversalLinks,
    }),
};

// The global fetch is what the Metro-served bundle talks to the IdP with; it
// satisfies FetchLike structurally (only status/ok/text are read).
const nativeFetch: FetchLike = (url, init) => fetch(url, init);

export function createNativeTokenStore(config: OidcConfig): TokenStore {
  return createTokenStore({ namespace: storageNamespace(config.issuer, config.clientId) });
}

export function createNativeAuthProvider(
  config: OidcConfig,
  overrides: Partial<Parameters<typeof createOidcAuthProvider>[1]> = {},
): OidcAuthProvider {
  return createOidcAuthProvider(config, {
    fetch: nativeFetch,
    crypto: nativeCrypto,
    browser: nativeBrowser,
    store: createNativeTokenStore(config),
    session,
    allowInsecureLoopback: typeof __DEV__ !== "undefined" && __DEV__,
    platform: { os: Platform.OS, version: Platform.Version },
    ...overrides,
  });
}
