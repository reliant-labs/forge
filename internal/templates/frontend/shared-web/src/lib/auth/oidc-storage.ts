// yours: scaffolded once, never touched again — forge will not overwrite this file
//
// ═══════════════════════════════════════════════════════════════════════
//  WHERE THE TOKEN LIVES IN THIS BROWSER — the decision, and the tradeoff
// ═══════════════════════════════════════════════════════════════════════
//
// This file is the WHOLE answer to "where is my access token kept?", and it
// is scaffolded into your project rather than hidden in a forge library
// precisely because it is a decision you may need to change and must be able
// to audit. Everything else in the auth flow reads its storage through the
// three functions at the bottom of this file.
//
// ── What forge chose: THE ACCESS TOKEN IS IN MEMORY ONLY ──────────────
//
// The access token, refresh token and id token live in a module-level
// variable in this file's closure. They are NEVER written to localStorage,
// NEVER written to sessionStorage, and NEVER written to a cookie.
//
//   value            where                     lifetime
//   ─────────────    ───────────────────────    ──────────────────────────
//   access token     memory (this closure)      until tab close OR reload
//   refresh token    memory (this closure)      until tab close OR reload
//   id token         memory (this closure)      until tab close OR reload
//   code verifier    sessionStorage             one login redirect, then
//                                               deleted on first read
//   state (CSRF)     sessionStorage             one login redirect, then
//                                               deleted on first read
//   post-login dest  sessionStorage             one login redirect, then
//                                               deleted on first read
//
// ── The tradeoff, stated plainly ──────────────────────────────────────
//
// COST: a full page reload signs the user out of THIS TAB's memory. There is
// no credential on disk to rehydrate from.
//
// That cost is smaller than it sounds, and this is the load-bearing part of
// the reasoning: the user is still signed in AT THE IDENTITY PROVIDER, whose
// session cookie is on the provider's own domain and survives your reload.
// So calling `login()` again after a refresh is normally a redirect that
// bounces straight back with a fresh code and no prompt — a flicker, not a
// login form. What you pay is a round trip, not the user's password.
//
// Nothing here does that automatically, deliberately: an automatic redirect
// on every cold load would send an ANONYMOUS visitor to your IdP before they
// have asked to sign in, and a public page would become a gated one. If you
// want silent restore, call `login()` from a layout effect on the routes that
// actually require identity — that way the redirect happens where a signed-in
// session is genuinely expected. (Providers that support it will honour
// `prompt=none` for a non-interactive attempt; pass it through the `extra`
// parameter on the authorization request.)
//
// BENEFIT: there is no window in which a credential outlives the page that
// held it. An XSS payload on this origin can still read the in-memory token
// while it runs — nothing in a browser prevents that, and any doc that claims
// otherwise is selling you something — but it cannot read a token that was
// never persisted, so it cannot mint access from a stored credential later,
// and a token cannot leak through a shared-machine browser profile.
//
// ── The alternative, and what it would cost you ───────────────────────
//
// Persisting the token (localStorage, sessionStorage, or a cookie) buys
// exactly one thing: surviving reload with no redirect. It costs:
//
//   - localStorage / sessionStorage: readable by ANY script on this origin.
//     One XSS, one bad npm postinstall, one compromised analytics snippet,
//     and the credential is exfiltrated — and it keeps working until it
//     expires, wherever the attacker replays it from.
//   - a cookie: to be safe from script it must be HttpOnly, which means
//     JavaScript cannot set it, which means the exchange has to move to a
//     server you own (a BFF) — a real architectural change, not a flag. And
//     because a cookie is sent automatically, you then owe the app CSRF
//     defence (SameSite=Lax/Strict plus a per-request token) that the
//     Authorization header never needed, since a header is not attached to
//     a cross-site request by the browser.
//
// If you want persistence anyway, the honest version is the BFF: move
// `exchangeCode` to a route on your own server, set an HttpOnly SameSite
// cookie there, and have this file's `getAccessToken()` return null while
// the transport relies on the cookie. Do NOT simply swap the lines below for
// `localStorage.setItem` — that trades a redirect for a permanently
// exfiltratable credential, which is the wrong side of this trade for almost
// every app.
//
// ── Why the verifier and state ARE persisted ──────────────────────────
//
// They have to be: the whole point of the redirect is that the page is
// destroyed and rebuilt by the provider, so a value in memory is gone by the
// time the callback runs. sessionStorage is the right shape for them because
// it is per-tab and dies with the tab, and because these are single-use
// values, not credentials:
//
//   - the code verifier proves THIS browser started THIS login. It is
//     useless without the matching authorization code, which arrives once,
//     over TLS, and is redeemable exactly once.
//   - the state is a nonce to be echoed back and compared.
//
// Both are deleted on first read (see `takePendingLogin`), so a completed or
// abandoned login leaves nothing behind. That "take, don't get" shape also
// means a replayed callback URL finds no verifier and fails closed.

import { parseVerifier, type Verifier } from "./oidc";

/** sessionStorage keys. Namespaced so they cannot collide with app state. */
const VERIFIER_KEY = "forge.oidc.verifier";
const STATE_KEY = "forge.oidc.state";
const RETURN_TO_KEY = "forge.oidc.returnTo";

/**
 * The in-memory session. A module-level binding, so it is per-tab, per page
 * load, and unreachable from anywhere that has not imported this module.
 */
interface MemorySession {
  accessToken: string;
  refreshToken: string;
  idToken: string;
  /** Epoch ms at which the access token expires, or 0 when unknown. */
  expiresAt: number;
}

let session: MemorySession | null = null;

/**
 * Subscribers notified whenever the session appears or disappears.
 *
 * This exists so the AuthProvider does not have to be TOLD about a login it
 * did not perform. The callback route calls `completeLogin()`, which lands
 * here; the provider is subscribed, so `onAuthStateChange` fires and the UI
 * updates. Without this the provider instance would hold a stale signed-out
 * user after a client-side navigation away from the callback — the session
 * being module-level while the provider's copy of it is not.
 */
const sessionListeners = new Set<() => void>();

/** Subscribe to session changes. Returns an unsubscribe function. */
export function onSessionChange(cb: () => void): () => void {
  sessionListeners.add(cb);
  return () => {
    sessionListeners.delete(cb);
  };
}

function notifySessionChange(): void {
  for (const cb of sessionListeners) cb();
}

/** Stores the tokens for this page's lifetime. Nothing touches disk. */
export function setSession(next: {
  accessToken: string;
  refreshToken?: string;
  idToken?: string;
  expiresIn?: number;
}): void {
  session = {
    accessToken: next.accessToken,
    refreshToken: next.refreshToken ?? "",
    idToken: next.idToken ?? "",
    expiresAt: next.expiresIn ? Date.now() + next.expiresIn * 1000 : 0,
  };
  notifySessionChange();
}

/** Drops the in-memory session. */
export function clearSession(): void {
  session = null;
  notifySessionChange();
}

/**
 * The current access token, or null.
 *
 * Returns null once the token has expired rather than handing back a value
 * that will 401: a caller can then treat "no token" as the single
 * signed-out condition instead of having to distinguish "absent" from
 * "present but dead". A provider that sends no `expires_in` gets no expiry
 * check — there is nothing to check against, and inventing a default would
 * sign users out on a schedule forge made up.
 */
export function getAccessToken(): string | null {
  if (!session) {
    return null;
  }
  if (session.expiresAt !== 0 && Date.now() >= session.expiresAt) {
    return null;
  }
  return session.accessToken;
}

/** The current id token, or null. Used to derive the user for the UI. */
export function getIdToken(): string | null {
  return session?.idToken || null;
}

/** The current refresh token, or null. */
export function getRefreshToken(): string | null {
  return session?.refreshToken || null;
}

/**
 * Whether the access token is expired, or will be within `skewMs`.
 *
 * Separate from `getAccessToken()` because the two answer different questions:
 * that one answers "is there a usable token right now", this one answers
 * "should we refresh". The skew exists so a request does not race the expiry
 * boundary — a token with 200ms left passes a bare `now >= expiresAt` check and
 * then 401s in flight, having been spent on a request that was doomed when it
 * left.
 *
 * A session with NO expiry (a provider that sent no `expires_in`) is never
 * reported as expiring: there is nothing to compare against, and treating
 * unknown as expired would refresh on every single call.
 */
export function accessTokenExpiresWithin(skewMs: number): boolean {
  if (!session || session.expiresAt === 0) {
    return false;
  }
  return Date.now() + skewMs >= session.expiresAt;
}

/**
 * Replaces the tokens after a refresh, keeping the session identity.
 *
 * Distinct from `setSession` because the semantics differ in one way that
 * matters: a refresh response often omits the id token, and the user's identity
 * must NOT be dropped just because this response did not restate it — that
 * would blank the UI's user mid-session. So the id token is carried forward
 * unless a new one arrives.
 *
 * The rotated refresh token replaces the old one IN MEMORY, the same place the
 * old one lived. Nothing here touches a persistent store; see this file's
 * header for why that posture is what it is.
 */
export function updateSessionTokens(next: {
  accessToken: string;
  refreshToken: string;
  idToken?: string;
  expiresIn?: number;
}): void {
  session = {
    accessToken: next.accessToken,
    refreshToken: next.refreshToken,
    idToken: next.idToken || session?.idToken || "",
    expiresAt: next.expiresIn ? Date.now() + next.expiresIn * 1000 : 0,
  };
  notifySessionChange();
}

/**
 * Records the values that must survive the redirect to the provider.
 *
 * Throws when sessionStorage is unavailable (Safari private mode with
 * storage disabled, an embedding that blocks it). Failing here is right: a
 * login started with nowhere to keep the verifier can only fail LATER, at
 * the callback, where the cause is no longer visible.
 */
export function storePendingLogin(args: {
  verifier: Verifier;
  state: string;
  returnTo: string;
}): void {
  try {
    sessionStorage.setItem(VERIFIER_KEY, args.verifier.secret());
    sessionStorage.setItem(STATE_KEY, args.state);
    sessionStorage.setItem(RETURN_TO_KEY, args.returnTo);
  } catch (cause) {
    throw new Error(
      "oidc: cannot start sign-in because sessionStorage is unavailable, so " +
        "the PKCE code verifier could not be stored across the redirect. " +
        "This is usually a browser privacy mode or an embedding that blocks " +
        "storage.",
      { cause },
    );
  }
}

/** What a callback needs to complete a login it did not start. */
export interface PendingLogin {
  verifier: Verifier;
  state: string;
  returnTo: string;
}

/**
 * Reads and DELETES the pending login. Named "take" because the removal is
 * the point, not a cleanup detail:
 *
 *   - a verifier is single-use by construction, so keeping it after the
 *     exchange only widens the window in which it can leak;
 *   - a callback URL that is reloaded, shared or replayed finds nothing and
 *     fails closed rather than re-running an exchange.
 *
 * Returns null when there is nothing pending, which the callback must treat
 * as a failure — it means this browser never started the login whose code it
 * is holding.
 */
export function takePendingLogin(): PendingLogin | null {
  let rawVerifier: string | null = null;
  let state: string | null = null;
  let returnTo: string | null = null;
  try {
    rawVerifier = sessionStorage.getItem(VERIFIER_KEY);
    state = sessionStorage.getItem(STATE_KEY);
    returnTo = sessionStorage.getItem(RETURN_TO_KEY);
    sessionStorage.removeItem(VERIFIER_KEY);
    sessionStorage.removeItem(STATE_KEY);
    sessionStorage.removeItem(RETURN_TO_KEY);
  } catch {
    // Unreadable storage is indistinguishable from empty storage for this
    // decision: either way there is no verifier, so the flow cannot and
    // must not continue.
    return null;
  }

  if (!rawVerifier || !state) {
    return null;
  }
  try {
    return {
      verifier: parseVerifier(rawVerifier),
      state,
      returnTo: returnTo || "/",
    };
  } catch {
    // A stored value that is not a legal verifier means storage was tampered
    // with or written by something else. Fail closed.
    return null;
  }
}
