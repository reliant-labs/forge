// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/oidc-provider.ts — the native sign-in session: an AuthProvider
// (src/lib/auth/provider.ts) backed by a standard OIDC authorization-code +
// PKCE flow against the identity provider, plus the session lifecycle the
// seam does not cover: restore, sign-in, forced refresh, sign-out.
//
// Everything with a side effect is INJECTED through OidcProviderDeps (fetch,
// crypto, browser, token store, identity-epoch session, clock), so the flows
// are exercised in Jest with in-memory fakes. native.ts builds the real deps
// from expo-web-browser / expo-crypto / expo-secure-store.
//
// BEHAVIOUR (each point is pinned by tests/auth-provider.test.ts)
//
//   signIn()           System-browser sign-in. Resolves to the user, or null when
//                      the user cancelled/dismissed the browser (NOT an error).
//                      A second call while one is running joins the first.
//                      Typed errors for state mismatch, missing code, `error=`.
//   getToken()         The bearer token, refreshed first when it expires within
//                      60s. Refresh is SINGLE-FLIGHT: any number of concurrent
//                      callers share one request. Offline: a still-valid token
//                      is returned; otherwise an OfflineError is thrown and the
//                      session is KEPT. `invalid_grant` ends the session (wipe,
//                      signed out) — once, with no retry loop.
//   onTokenRejected()  The API said 401 for a token. Forces one refresh; resolves
//                      true if a different token is now available.
//   logout()/signOut() Bumps the identity epoch (aborting everything in flight),
//                      wipes the store, revokes the refresh token best-effort
//                      with a short timeout, and makes the NEXT signIn send
//                      `prompt=login`. signOut() also returns the IdP's
//                      end-session URL when one exists.
//   getUser()          Derived from the ID TOKEN CLAIMS, decoded, NOT verified:
//                      the signature is checked by your API on every request;
//                      the client only needs to show who is signed in.
//
// Tokens are never logged and never appear in an error message.

import type { AuthProvider, AuthUser } from "./provider";
import type { OidcConfig } from "./config";
import {
  AbortedError,
  OidcError,
  TokenEndpointError,
  IdTokenError,
  authorizeUrl,
  checkIdTokenClaims,
  createPkce,
  decodeJwtClaims,
  discover,
  endSessionUrl,
  exchangeCode,
  isTransientAuthError,
  parseCallback,
  randomToken,
  refreshTokens,
  revokeToken,
  type CryptoLike,
  type DiscoveryCache,
  type DiscoveryDocument,
  type FetchLike,
  type RequestOptions,
} from "./oidc";
import type { IdentitySession } from "./session";
import type { StoredTokens, TokenStore } from "./token-store";

/** What the system browser reports (a structural subset of expo-web-browser's result). */
export type BrowserResult = { type: "success"; url: string } | { type: string; url?: undefined };

export interface BrowserLike {
  /**
   * Open `url`, resolve when the browser navigates to `redirectUri`.
   *
   * The redirect URI is passed through UNCHANGED as the return URL. That is
   * right for the private-use scheme forge ships today
   * (`com.<project>.<app>:/oauthredirect`). For an https universal/app link on
   * iOS 17.4+, expo-web-browser (SDK 58) additionally needs
   * `preferUniversalLinks: true` and the Associated Domains entitlement —
   * add it in native.ts when you move to https; nothing else changes.
   */
  openAuthSession(
    url: string,
    redirectUri: string,
    options: { preferEphemeralSession: boolean },
  ): Promise<BrowserResult>;
}

export type SessionEndReason = "logout" | "invalid_grant" | "expired" | "account_changed";

export interface OidcProviderDeps {
  fetch: FetchLike;
  crypto: CryptoLike;
  browser: BrowserLike;
  store: TokenStore;
  session: IdentitySession;
  now?: () => number;
  discoveryCache?: DiscoveryCache;
  /** http:// loopback IdP allowed (development). */
  allowInsecureLoopback?: boolean;
  /** Refresh this long before expiry. Default 60s. */
  skewMs?: number;
  /** Network timeout for token/discovery calls. Default 15s. */
  requestTimeoutMs?: number;
  /** Timeout for the best-effort revocation on sign-out. Default 5s. */
  revokeTimeoutMs?: number;
  /**
   * Called after the signed-in identity ended or changed for any reason other
   * than the app asking. Clear the React Query cache here (see session.ts
   * endSession) so the previous account's data cannot outlive it.
   */
  onSessionEnded?: (reason: SessionEndReason) => void;
}

/** The browser could not start because another auth session is already open. */
export class BrowserBusyError extends OidcError {}

export interface SignOutResult {
  /** Open this in the system browser to end the IdP's own session, or null when it has none. */
  endSessionUrl: string | null;
}

export interface OidcAuthProvider extends AuthProvider {
  /** Load the persisted session. Rejects with StorageUnavailableError / StorageError. */
  restore(): Promise<AuthUser | null>;
  signIn(): Promise<AuthUser | null>;
  onTokenRejected(rejectedToken?: string | null): Promise<boolean>;
  signOut(): Promise<SignOutResult>;
}

const DEFAULT_SKEW_MS = 60_000;
const DEFAULT_REVOKE_TIMEOUT_MS = 5_000;
// expires_in is only RECOMMENDED by RFC 6749. A stored session must carry an
// expiry (a missing one is "no session"), so an IdP that omits it gets a short
// assumed lifetime: the token is renewed soon rather than trusted forever.
const UNKNOWN_LIFETIME_MS = 5 * 60_000;

export function userFromIdToken(idToken: string | null): AuthUser | null {
  if (!idToken) return null;
  try {
    const claims = decodeJwtClaims(idToken);
    if (typeof claims.sub !== "string" || claims.sub === "") return null;
    const text = (v: unknown) => (typeof v === "string" && v !== "" ? v : undefined);
    const user: AuthUser = { id: claims.sub };
    const email = text(claims.email);
    const name = text(claims.name) ?? text(claims.preferred_username);
    if (email) user.email = email;
    if (name) user.name = name;
    return user;
  } catch {
    return null;
  }
}

export function createOidcAuthProvider(
  config: OidcConfig,
  deps: OidcProviderDeps,
): OidcAuthProvider {
  const now = deps.now ?? Date.now;
  const skewMs = deps.skewMs ?? DEFAULT_SKEW_MS;
  const { store, session } = deps;

  let tokens: StoredTokens | null = null;
  let user: AuthUser | null = null;
  let forceLogin = false;
  let restoring: Promise<AuthUser | null> | null = null;
  let signingIn: Promise<AuthUser | null> | null = null;
  let refreshing: Promise<void> | null = null;
  let signingOut: Promise<SignOutResult> | null = null;

  const requestOptions = (signal?: AbortSignal, timeoutMs?: number): RequestOptions => ({
    signal,
    timeoutMs: timeoutMs ?? deps.requestTimeoutMs,
    allowInsecureLoopback: deps.allowInsecureLoopback,
  });

  const getDiscovery = (signal?: AbortSignal, timeoutMs?: number): Promise<DiscoveryDocument> =>
    discover(config.issuer, deps.fetch, {
      ...requestOptions(signal, timeoutMs),
      cache: deps.discoveryCache,
    });

  const setTokens = (next: StoredTokens | null, nextUser?: AuthUser | null) => {
    tokens = next;
    user = next ? (nextUser !== undefined ? nextUser : userFromIdToken(next.idToken)) : null;
  };

  const isFresh = (t: StoredTokens): boolean => t.expiresAt - skewMs > now();
  const isExpired = (t: StoredTokens): boolean => t.expiresAt <= now();

  // Sign out locally: new epoch (aborts in-flight work), memory and keystore
  // wiped. A keystore failure is swallowed here because the memory is already
  // clear and these tokens are dead; the next restore re-reads and re-expires.
  async function endLocalSession(reason: SessionEndReason): Promise<void> {
    session.bumpEpoch();
    setTokens(null);
    try {
      await store.clear();
    } catch {
      // see above
    }
    deps.onSessionEnded?.(reason);
  }

  // Write the session to the keystore, then re-check the identity epoch. The
  // store runs its operations in call order, so a sign-out's clear() lands
  // after this save; the re-check covers every path where nothing queued a
  // clear (the epoch moved but no session was left to own the keystore). When
  // a NEW session already holds the memory, the keystore is theirs: leave it.
  // Returns false when the save was superseded.
  async function persist(next: StoredTokens, epoch: number): Promise<boolean> {
    await store.save(next);
    if (session.isCurrent(epoch)) return true;
    if (tokens === null) {
      try {
        await store.clear();
      } catch {
        // memory is already clear; the next load drops an incomplete record
      }
    }
    return false;
  }

  // ── refresh (single-flight) ───────────────────────────────────────────

  function refresh(): Promise<void> {
    if (!refreshing) {
      refreshing = doRefresh().finally(() => {
        refreshing = null;
      });
    }
    return refreshing;
  }

  async function doRefresh(): Promise<void> {
    const current = tokens;
    if (!current) return;
    const epoch = session.epoch();
    const signal = session.currentSignal();
    if (!current.refreshToken) {
      // Nothing can renew this session: an expired access token ends it.
      if (isExpired(current)) await endLocalSession("expired");
      return;
    }
    const discovery = await getDiscovery(signal);
    if (!session.isCurrent(epoch)) throw new AbortedError("refresh dropped: identity changed");
    let next;
    try {
      next = await refreshTokens({
        discovery,
        config,
        refreshToken: current.refreshToken,
        fetch: deps.fetch,
        now,
        options: requestOptions(signal),
      });
    } catch (e) {
      if (e instanceof TokenEndpointError && e.isDeadRefreshGrant) {
        // The grant is dead (revoked, expired, rotated away). Sign out ONCE —
        // no retry — unless someone already moved the identity on.
        if (session.isCurrent(epoch)) await endLocalSession("invalid_grant");
        return;
      }
      throw e;
    }
    if (!session.isCurrent(epoch)) throw new AbortedError("refresh dropped: identity changed");
    if (next.idToken) {
      const claims = decodeJwtClaims(next.idToken);
      checkIdTokenClaims(claims, { issuer: config.issuer, clientId: config.clientId });
      if (user && claims.sub !== user.id) {
        // The IdP vouches for someone else under this refresh token. The
        // rotated-away token is useless and the old account's data must go:
        // end the session rather than keep either half.
        await endLocalSession("account_changed");
        return;
      }
    }
    const merged: StoredTokens = {
      accessToken: next.accessToken,
      // The IdP may rotate the refresh token or omit it (= keep using the old one).
      refreshToken: next.refreshToken ?? current.refreshToken,
      idToken: next.idToken ?? current.idToken,
      expiresAt: next.expiresAt ?? now() + UNKNOWN_LIFETIME_MS,
    };
    // Memory first: the rotated token is usable immediately even if the
    // keystore write below fails (and that failure is then reported).
    setTokens(merged, next.idToken ? undefined : user);
    if (!(await persist(merged, epoch)))
      throw new AbortedError("refresh dropped: identity changed");
  }

  // ── AuthProvider ──────────────────────────────────────────────────────

  async function getToken(): Promise<string | null> {
    const current = tokens;
    if (!current) return null;
    if (isFresh(current)) return current.accessToken;
    try {
      await refresh();
    } catch (e) {
      if (e instanceof AbortedError) return null;
      // Offline / IdP outage: a token that has not actually expired still works.
      if (isTransientAuthError(e) && !isExpired(current)) return current.accessToken;
      throw e;
    }
    return tokens?.accessToken ?? null;
  }

  async function onTokenRejected(rejectedToken?: string | null): Promise<boolean> {
    // null = the request went out with no token. One exists now (sign-in
    // finished meanwhile): replay with it; there is nothing to renew.
    if (rejectedToken === null) return tokens !== null;
    const before = rejectedToken ?? tokens?.accessToken ?? null;
    if (!tokens || !before) return false;
    // Someone already replaced the rejected token: just retry with the new one.
    if (tokens.accessToken !== before) return true;
    try {
      await refresh();
    } catch (e) {
      if (e instanceof AbortedError) return false;
      throw e;
    }
    const after = tokens?.accessToken ?? null;
    return after !== null && after !== before;
  }

  function restore(): Promise<AuthUser | null> {
    if (!restoring) {
      restoring = doRestore().finally(() => {
        restoring = null;
      });
    }
    return restoring;
  }

  async function doRestore(): Promise<AuthUser | null> {
    const epoch = session.epoch();
    const saved = await store.load();
    // A sign-in or sign-out that finished while the keystore was being read
    // owns the session now; a slow restore must not overwrite it.
    if (!session.isCurrent(epoch) || tokens !== null) return user;
    if (!saved) {
      setTokens(null);
      return null;
    }
    setTokens(saved);
    if (!isFresh(saved)) {
      try {
        await refresh();
      } catch (e) {
        // Offline at launch: stay signed in; getToken() reports it when a token is needed.
        if (e instanceof AbortedError || isTransientAuthError(e)) return user;
        // Anything else: the app is about to report "signed out", so nothing
        // of this session may stay in memory or on disk.
        if (session.isCurrent(epoch)) await endLocalSession("expired");
        throw e;
      }
    }
    return user;
  }

  // ── sign-in ───────────────────────────────────────────────────────────

  function signIn(): Promise<AuthUser | null> {
    if (!signingIn) {
      signingIn = doSignIn().finally(() => {
        signingIn = null;
      });
    }
    return signingIn;
  }

  async function doSignIn(): Promise<AuthUser | null> {
    const epoch = session.epoch();
    const signal = session.currentSignal();
    const discovery = await getDiscovery(signal);
    if (!session.isCurrent(epoch)) return null;
    const pkce = await createPkce(deps.crypto);
    const state = randomToken(deps.crypto, 32);
    const nonce = randomToken(deps.crypto, 32);
    const prompt = forceLogin || (await store.getForceLogin()) ? "login" : undefined;
    const url = authorizeUrl({
      discovery,
      config,
      state,
      nonce,
      codeChallenge: pkce.challenge,
      prompt,
    });

    // iOS: ephemeral so the IdP's web cookies are not shared with Safari — "switch account" works and sign-out is real.
    const result = await deps.browser.openAuthSession(url, config.redirectUri, {
      preferEphemeralSession: true,
    });
    if (result.type === "locked")
      throw new BrowserBusyError("another authentication session is already open");
    if (result.type !== "success" || typeof result.url !== "string") return null; // cancel / dismiss / opened
    if (!session.isCurrent(epoch)) return null;

    const { code } = parseCallback(result.url, {
      redirectUri: config.redirectUri,
      state,
      issuer: config.issuer,
    });
    let granted;
    try {
      granted = await exchangeCode({
        discovery,
        config,
        code,
        codeVerifier: pkce.verifier,
        fetch: deps.fetch,
        now,
        options: requestOptions(signal),
      });
    } catch (e) {
      if (e instanceof AbortedError) return null;
      throw e;
    }
    if (!session.isCurrent(epoch)) return null;
    if (!granted.idToken)
      throw new IdTokenError("token response carried no id_token (is the openid scope granted?)");
    const claims = decodeJwtClaims(granted.idToken);
    checkIdTokenClaims(claims, { issuer: config.issuer, clientId: config.clientId, nonce });

    const previous = user;
    const next: StoredTokens = {
      accessToken: granted.accessToken,
      refreshToken: granted.refreshToken,
      idToken: granted.idToken,
      expiresAt: granted.expiresAt ?? now() + UNKNOWN_LIFETIME_MS,
    };
    const nextUser = userFromIdToken(next.idToken);
    if (previous && nextUser && previous.id !== nextUser.id) {
      // A different account replaces the old one: nothing of it may linger.
      session.bumpEpoch();
      deps.onSessionEnded?.("account_changed");
    }
    setTokens(next, nextUser);
    // The epoch may have just moved (account change above): that is ours.
    if (!(await persist(next, session.epoch()))) return null;
    forceLogin = false;
    try {
      await store.setForceLogin(false);
    } catch {
      // Harmless: the next sign-in just asks for credentials once more.
    }
    return nextUser;
  }

  // ── sign-out ──────────────────────────────────────────────────────────

  function signOut(): Promise<SignOutResult> {
    if (!signingOut) {
      signingOut = doSignOut().finally(() => {
        signingOut = null;
      });
    }
    return signingOut;
  }

  async function doSignOut(): Promise<SignOutResult> {
    const leaving = tokens;
    // New epoch first: in-flight requests abort and late responses are dropped.
    session.bumpEpoch();
    setTokens(null);
    forceLogin = true;

    let storageError: unknown = null;
    try {
      await store.setForceLogin(true);
      // Wipe BEFORE the network round-trip: if the app is killed mid-revoke the
      // tokens are already gone from the device.
      await store.clear();
    } catch (e) {
      storageError = e;
    }

    let result: SignOutResult = { endSessionUrl: null };
    if (leaving) {
      // Best effort and bounded: sign-out must complete even when the IdP is unreachable.
      try {
        const discovery = await getDiscovery(
          undefined,
          deps.revokeTimeoutMs ?? DEFAULT_REVOKE_TIMEOUT_MS,
        );
        const token = leaving.refreshToken ?? leaving.accessToken;
        try {
          await revokeToken({
            discovery,
            config,
            token,
            tokenTypeHint: leaving.refreshToken ? "refresh_token" : "access_token",
            fetch: deps.fetch,
            options: requestOptions(undefined, deps.revokeTimeoutMs ?? DEFAULT_REVOKE_TIMEOUT_MS),
          });
        } catch {
          // Offline or refused: the local session is already gone.
        }
        result = {
          endSessionUrl: endSessionUrl({ discovery, config, idTokenHint: leaving.idToken }),
        };
      } catch {
        // No discovery (offline): skip revocation and the end-session URL.
      }
    }
    if (storageError) throw storageError;
    return result;
  }

  return {
    getToken,
    getUser: () => user,
    logout: async () => {
      await signOut();
    },
    restore,
    signIn,
    onTokenRejected,
    signOut,
  };
}
