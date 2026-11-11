// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/token-store.ts — where the signed-in session's tokens live.
//
// Tokens go in the platform keystore through expo-secure-store (iOS Keychain,
// Android Keystore-encrypted preferences), one key per token:
//
//   - keychainAccessible = AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY. A refresh
//     triggered by a background fetch or a push still works once the device
//     has been unlocked after boot, and the item is NOT carried to a new
//     device by a backup restore. (The default, WHEN_UNLOCKED, would make
//     background refresh fail; the biometric `requireAuthentication` option is
//     deliberately unused because it would break silent refresh.)
//   - Keys are namespaced by issuer + client_id, so pointing a build at a
//     different IdP or client can never read — or be confused by — another
//     environment's session. SecureStore keys may only contain
//     [A-Za-z0-9._-], hence storageNamespace().
//
// FIRST-LAUNCH WIPE. iOS keeps Keychain items when an app is uninstalled, so a
// reinstall would silently resurrect the previous install's session. We detect
// "this is a new install" with a marker stored OUTSIDE the keychain — one that
// IS deleted with the app — and wipe our keys before first use. The default
// marker is React Native's own `Settings` module (NSUserDefaults on iOS): it
// needs no extra native dependency, lives in the app container (gone on
// uninstall), and is synchronous. Android needs no marker — its keys live in
// app-private storage that uninstall removes, and the expo-secure-store config
// plugin excludes them from backups — so there the default marker reports
// "already initialised". Inject `launchMarker` to use something else (e.g. a
// file via expo-file-system) without touching the rest.
//
// Nothing here ever logs a token or puts one in an error message.

import { Platform, Settings } from "react-native";
import * as ExpoSecureStore from "expo-secure-store";

import { OidcError } from "./oidc";

export interface StoredTokens {
  accessToken: string;
  refreshToken: string | null;
  /** Required: it names the user. A stored record without one is no session. */
  idToken: string;
  /** Epoch milliseconds. Required: a record without one is no session (see load()). */
  expiresAt: number;
}

/** The device has no secure storage (web, or a build without the native module). */
export class StorageUnavailableError extends OidcError {
  constructor() {
    super("secure storage is not available on this device");
  }
}

/** The keystore rejected a read/write/delete (locked device, corrupted entry, ...). The session is untouched. */
export class StorageError extends OidcError {
  constructor(operation: string, cause: unknown) {
    super(`secure storage ${operation} failed`, { cause });
  }
}

export interface TokenStore {
  /**
   * The saved session, or null when there is none — including an incomplete
   * record, which load() deletes. Rejects with StorageError / StorageUnavailableError.
   * Operations are serialized: they take effect in call order.
   */
  load(): Promise<StoredTokens | null>;
  save(tokens: StoredTokens): Promise<void>;
  /** Remove the session. The forced-login flag survives (see setForceLogin). */
  clear(): Promise<void>;
  /** True when the next sign-in must send `prompt=login` (set by sign-out). */
  getForceLogin(): Promise<boolean>;
  setForceLogin(force: boolean): Promise<void>;
}

/** The slice of expo-secure-store this module uses; inject a fake in tests. */
export interface SecureStoreLike {
  isAvailableAsync(): Promise<boolean>;
  getItemAsync(key: string, options?: SecureStoreCallOptions): Promise<string | null>;
  setItemAsync(key: string, value: string, options?: SecureStoreCallOptions): Promise<void>;
  deleteItemAsync(key: string, options?: SecureStoreCallOptions): Promise<void>;
  AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY: number;
}

export interface SecureStoreCallOptions {
  keychainAccessible?: number;
}

/** A flag kept outside the keychain that disappears when the app is uninstalled. */
export interface LaunchMarker {
  isSet(): boolean | Promise<boolean>;
  set(): void | Promise<void>;
}

export interface TokenStoreOptions {
  /** Result of storageNamespace(issuer, clientId). */
  namespace: string;
  secureStore?: SecureStoreLike;
  launchMarker?: LaunchMarker;
}

// ── key naming ──────────────────────────────────────────────────────────

function fnv1a(input: string, seed: number): string {
  let h = seed >>> 0;
  for (let i = 0; i < input.length; i++) {
    h ^= input.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h.toString(16).padStart(8, "0");
}

/**
 * A SecureStore-safe namespace for one issuer + client_id: a readable slug of
 * the issuer host plus a 64-bit FNV-1a of the exact pair. Collision-resistant
 * enough to separate environments; it is a namespace, not a secret.
 */
export function storageNamespace(issuer: string, clientId: string): string {
  const host = issuer.replace(/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//, "").split(/[/?#]/)[0];
  const slug =
    host
      .replace(/[^A-Za-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 24) || "issuer";
  const pair = `${issuer}\n${clientId}`;
  return `${slug}.${fnv1a(pair, 0x811c9dc5)}${fnv1a(pair, 0x01000193)}`;
}

const NAME_RE = /^[A-Za-z0-9._-]+$/;

// ── default launch marker ───────────────────────────────────────────────

export function settingsLaunchMarker(key: string): LaunchMarker {
  if (Platform.OS !== "ios") {
    return { isSet: () => true, set: () => undefined };
  }
  return {
    isSet: () => Settings.get(key) === 1,
    set: () => Settings.set({ [key]: 1 }),
  };
}

// ── store ───────────────────────────────────────────────────────────────

export function createTokenStore(options: TokenStoreOptions): TokenStore {
  const ns = options.namespace;
  if (!NAME_RE.test(ns)) {
    throw new Error(`token store namespace must match [A-Za-z0-9._-]+, got ${JSON.stringify(ns)}`);
  }
  const store: SecureStoreLike =
    options.secureStore ?? (ExpoSecureStore as unknown as SecureStoreLike);
  const marker = options.launchMarker ?? settingsLaunchMarker(`forge.auth.installed.${ns}`);
  const callOptions: SecureStoreCallOptions = {
    keychainAccessible: store.AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY,
  };
  const key = (name: string) => `forge.auth.${ns}.${name}`;
  const K = {
    access: key("access"),
    refresh: key("refresh"),
    id: key("id"),
    expires: key("expires"),
    relogin: key("relogin"),
  };

  const guard = async <T>(operation: string, fn: () => Promise<T>): Promise<T> => {
    try {
      return await fn();
    } catch (e) {
      if (e instanceof OidcError) throw e;
      throw new StorageError(operation, e);
    }
  };

  const del = (k: string) => store.deleteItemAsync(k, callOptions);
  // Access token first: once it is gone `load()` reports "no session" even if
  // a later delete fails.
  const clearEntries = async () => {
    for (const k of [K.access, K.refresh, K.id, K.expires]) await del(k);
  };

  let ready: Promise<void> | null = null;
  const ensureReady = (): Promise<void> => {
    if (!ready) {
      ready = (async () => {
        if (!(await store.isAvailableAsync())) throw new StorageUnavailableError();
        if (await marker.isSet()) return;
        for (const k of Object.values(K)) await del(k);
        await marker.set();
      })();
      // A failure (unavailable, keystore error) is retried on the next call.
      ready.catch(() => {
        ready = null;
      });
    }
    return ready;
  };

  // Every operation runs through ONE queue, in call order. save() and clear()
  // are several keychain round trips each; interleaved, a sign-out's clear()
  // could finish before an earlier save() wrote the access token, leaving the
  // previous account's token on disk. A failed operation does not poison the
  // queue.
  let queue: Promise<unknown> = Promise.resolve();
  const run = <T>(operation: string, fn: () => Promise<T>): Promise<T> => {
    const result = queue.then(() =>
      guard(operation, async () => {
        await ensureReady();
        return fn();
      }),
    );
    queue = result.catch(() => undefined);
    return result;
  };

  return {
    load: () =>
      run("read", async () => {
        const [access, refresh, id, expires] = await Promise.all([
          store.getItemAsync(K.access, callOptions),
          store.getItemAsync(K.refresh, callOptions),
          store.getItemAsync(K.id, callOptions),
          store.getItemAsync(K.expires, callOptions),
        ]);
        const expiresAt = expires === null ? NaN : Number(expires);
        if (access && id && Number.isFinite(expiresAt)) {
          return {
            accessToken: access,
            refreshToken: refresh || null,
            idToken: id,
            expiresAt,
          };
        }
        // An incomplete record (a torn save or clear) is no session: without
        // the id token there is no user, without the expiry the access token
        // would look immortal. Remove what is left rather than serve it.
        if (access || refresh || id || expires) await clearEntries();
        return null;
      }),

    save: (tokens) =>
      run("write", async () => {
        // The refresh token goes first: it is the one a torn write must not lose.
        const set = (k: string, v: string | null) =>
          v === null ? del(k) : store.setItemAsync(k, v, callOptions);
        await set(K.refresh, tokens.refreshToken);
        await set(K.id, tokens.idToken);
        await set(K.expires, String(tokens.expiresAt));
        await set(K.access, tokens.accessToken);
      }),

    clear: () => run("delete", clearEntries),

    getForceLogin: () =>
      run("read", async () => (await store.getItemAsync(K.relogin, callOptions)) === "1"),

    setForceLogin: (force) =>
      run("write", async () => {
        if (force) await store.setItemAsync(K.relogin, "1", callOptions);
        else await del(K.relogin);
      }),
  };
}
