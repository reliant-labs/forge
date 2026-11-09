// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/oidc.ts — the OpenID Connect / OAuth 2.0 wire protocol for a
// PUBLIC native client: authorization code + PKCE (S256), no client secret.
//
// Everything here is pure: `fetch` and the crypto primitives are INJECTED, so
// the protocol is tested with plain fakes and the same code runs on a device
// (expo-crypto + global fetch, see native.ts). It mirrors forge's Go
// pkg/oauth2 invariants: the state check, reserved-parameter protection, and
// never putting a token, code or verifier in an error message or a log.
//
// What this file does NOT do: verify an id token's signature. Tokens arrive
// straight from the token endpoint over TLS, and the audience that matters —
// your API — validates every access token itself on every request. Decoding
// the id token (decodeJwtClaims) is for presentation (who is signed in) and
// for the nonce / issuer / audience sanity checks below; do not authorise
// anything on the client from its claims.
//
// Hermes has no WebCrypto (`crypto.subtle`) and no guaranteed
// `crypto.getRandomValues`, hence the injected CryptoLike.

import { isSecureEndpoint, parseUri, type OidcConfig } from "./config";

// ── errors ──────────────────────────────────────────────────────────────

/** Base of every error this module throws, so callers can `instanceof` once. */
export class OidcError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message);
    Object.setPrototypeOf(this, new.target.prototype);
    this.name = new.target.name;
    if (options?.cause !== undefined) (this as { cause?: unknown }).cause = options.cause;
  }
}

/** The device could not reach the IdP (network down, DNS, TLS, timeout). The session is intact. */
export class OfflineError extends OidcError {}

/** The request was cancelled because the identity epoch moved (sign-out). */
export class AbortedError extends OidcError {}

/** The discovery document is missing, malformed, or names a different issuer. */
export class DiscoveryError extends OidcError {}

/** The IdP answered, but not with a well-formed OAuth response. */
export class ProtocolError extends OidcError {}

/** The authorization callback's `state` did not match, or the callback is not for us. */
export class StateMismatchError extends OidcError {}

/** The callback carried no authorization code. */
export class MissingCodeError extends OidcError {}

/** The callback carried `error=` (the user denied, the IdP refused, ...). */
export class AuthorizationError extends OidcError {
  readonly code: string;
  readonly description: string | null;
  constructor(code: string, description: string | null) {
    super(`authorization failed: ${code}${description ? ` (${description})` : ""}`);
    this.code = code;
    this.description = description;
  }
}

/** The token or revocation endpoint answered with an OAuth error. */
export class TokenEndpointError extends OidcError {
  readonly code: string;
  readonly description: string | null;
  readonly status: number;
  constructor(status: number, code: string, description: string | null) {
    super(`token endpoint returned ${status}: ${code}${description ? ` (${description})` : ""}`);
    this.status = status;
    this.code = code;
    this.description = description;
  }
  /** The refresh token / grant is dead for good: sign the user out. */
  get isInvalidGrant(): boolean {
    return this.code === "invalid_grant";
  }
  /** A server-side, timeout or rate-limit failure worth retrying later; the session is intact. */
  get isRetryable(): boolean {
    return this.status >= 500 || this.status === 429 || this.status === 408;
  }
  /**
   * The IdP refused a REFRESH request for good. RFC 6749 says `invalid_grant`,
   * but Zitadel answers a revoked or rotated-away refresh token with
   * `400 invalid_request` (Errors.OIDCSession.RefreshTokenInvalid), so any
   * non-retryable 4xx on the refresh grant means the grant is dead. Only
   * meaningful for refresh_token requests, never for the code exchange.
   */
  get isDeadRefreshGrant(): boolean {
    return this.isInvalidGrant || (this.status >= 400 && this.status < 500 && !this.isRetryable);
  }
}

/** The id token is missing, malformed, or fails the nonce / issuer / audience checks. */
export class IdTokenError extends OidcError {}

/** True when a failure says "try again later", not "you are signed out". */
export function isTransientAuthError(e: unknown): boolean {
  return e instanceof OfflineError || (e instanceof TokenEndpointError && e.isRetryable);
}

// ── injected capabilities ───────────────────────────────────────────────

export interface HttpResponse {
  ok: boolean;
  status: number;
  text(): Promise<string>;
}

export type FetchLike = (
  url: string,
  init: { method: string; headers: Record<string, string>; body?: string; signal?: AbortSignal },
) => Promise<HttpResponse>;

export interface CryptoLike {
  /** n cryptographically secure random bytes. */
  getRandomBytes(n: number): Uint8Array;
  /** SHA-256 of the UTF-8 string, returned as standard (padded) base64. */
  sha256Base64(input: string): Promise<string>;
}

export interface RequestOptions {
  signal?: AbortSignal;
  /** Per-request timeout; a hung IdP must not wedge single-flight refresh. */
  timeoutMs?: number;
  /** Allow http:// endpoints on loopback or a private-network address (development). */
  allowInsecureLoopback?: boolean;
}

const DEFAULT_TIMEOUT_MS = 15_000;

// ── encoding ────────────────────────────────────────────────────────────

const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

export function base64UrlEncode(bytes: Uint8Array): string {
  let out = "";
  for (let i = 0; i < bytes.length; i += 3) {
    const b0 = bytes[i];
    const b1 = i + 1 < bytes.length ? bytes[i + 1] : 0;
    const b2 = i + 2 < bytes.length ? bytes[i + 2] : 0;
    out += B64[b0 >> 2] + B64[((b0 & 3) << 4) | (b1 >> 4)];
    if (i + 1 < bytes.length) out += B64[((b1 & 15) << 2) | (b2 >> 6)];
    if (i + 2 < bytes.length) out += B64[b2 & 63];
  }
  return out;
}

export function base64UrlDecode(input: string): Uint8Array {
  const s = input.replace(/=+$/, "").replace(/\+/g, "-").replace(/\//g, "_");
  if (!/^[A-Za-z0-9_-]*$/.test(s) || s.length % 4 === 1) throw new Error("invalid base64url");
  const out: number[] = [];
  let bits = 0;
  let acc = 0;
  for (const ch of s) {
    acc = (acc << 6) | B64.indexOf(ch);
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out.push((acc >> bits) & 0xff);
    }
  }
  return Uint8Array.from(out);
}

/** Standard base64 (padded, +/) to base64url (unpadded, -_). */
export function base64ToBase64Url(b64: string): string {
  return b64.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function formEncode(fields: Record<string, string>): string {
  return Object.entries(fields)
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`)
    .join("&");
}

function parseQuery(query: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const pair of query.split("&")) {
    if (!pair) continue;
    const eq = pair.indexOf("=");
    const rawKey = eq < 0 ? pair : pair.slice(0, eq);
    const rawVal = eq < 0 ? "" : pair.slice(eq + 1);
    try {
      const key = decodeURIComponent(rawKey.replace(/\+/g, " "));
      if (!out.has(key)) out.set(key, decodeURIComponent(rawVal.replace(/\+/g, " ")));
    } catch {
      // A malformed escape is dropped; the field it would have carried is then "missing".
    }
  }
  return out;
}

// ── HTTP ────────────────────────────────────────────────────────────────

async function send(
  fetch: FetchLike,
  url: string,
  init: { method: string; headers: Record<string, string>; body?: string },
  options: RequestOptions,
): Promise<{ status: number; ok: boolean; body: string }> {
  const controller = new AbortController();
  const outer = options.signal;
  const onAbort = () => controller.abort();
  if (outer?.aborted) throw new AbortedError("request aborted: identity changed");
  outer?.addEventListener("abort", onAbort);
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, options.timeoutMs ?? DEFAULT_TIMEOUT_MS);
  try {
    const res = await fetch(url, { ...init, signal: controller.signal });
    return { status: res.status, ok: res.ok, body: await res.text() };
  } catch (e) {
    if (outer?.aborted) throw new AbortedError("request aborted: identity changed", { cause: e });
    // fetch rejects only when it never got an HTTP answer: that is "offline".
    throw new OfflineError(
      timedOut ? "identity provider timed out" : "cannot reach identity provider",
      {
        cause: e,
      },
    );
  } finally {
    clearTimeout(timer);
    outer?.removeEventListener("abort", onAbort);
  }
}

function parseJson(body: string): Record<string, unknown> | null {
  try {
    const v: unknown = JSON.parse(body);
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

const str = (v: unknown): string | null => (typeof v === "string" && v !== "" ? v : null);

function tokenEndpointError(status: number, body: string): TokenEndpointError {
  const json = parseJson(body);
  return new TokenEndpointError(
    status,
    str(json?.error) ?? (status >= 500 ? "server_error" : "invalid_response"),
    str(json?.error_description),
  );
}

// ── discovery ───────────────────────────────────────────────────────────

export interface DiscoveryDocument {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  revocation_endpoint: string | null;
  end_session_endpoint: string | null;
}

export type DiscoveryCache = Map<string, Promise<DiscoveryDocument>>;

const sharedDiscoveryCache: DiscoveryCache = new Map();

export function clearDiscoveryCache(cache: DiscoveryCache = sharedDiscoveryCache): void {
  cache.clear();
}

/**
 * Fetch `<issuer>/.well-known/openid-configuration`, cached per issuer (a
 * failure is never cached). The document's `issuer` must equal the configured
 * one exactly (OIDC Discovery §4.3): it is what stops a hostile or
 * misconfigured host from vouching for another issuer's endpoints.
 */
export function discover(
  issuer: string,
  fetch: FetchLike,
  options: RequestOptions & { cache?: DiscoveryCache } = {},
): Promise<DiscoveryDocument> {
  const cache = options.cache ?? sharedDiscoveryCache;
  let shared = cache.get(issuer);
  if (!shared) {
    // The fetch is SHARED by every caller of this issuer, so it answers to its
    // own timeout only: the first caller's abort signal must not be able to
    // reject what later callers are awaiting (or leave it cached).
    const pending = fetchDiscovery(issuer, fetch, { ...options, signal: undefined });
    shared = pending;
    cache.set(issuer, pending);
    pending.catch(() => {
      if (cache.get(issuer) === pending) cache.delete(issuer);
    });
  }
  return options.signal ? abortable(shared, options.signal) : shared;
}

/** Stop WAITING for `promise` when `signal` aborts; the promise itself carries on for other waiters. */
function abortable<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  const aborted = () => new AbortedError("request aborted: identity changed");
  if (signal.aborted) return Promise.reject(aborted());
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(aborted());
    signal.addEventListener("abort", onAbort, { once: true });
    promise.then(
      (v) => {
        signal.removeEventListener("abort", onAbort);
        resolve(v);
      },
      (e: unknown) => {
        signal.removeEventListener("abort", onAbort);
        reject(e);
      },
    );
  });
}

async function fetchDiscovery(
  issuer: string,
  fetch: FetchLike,
  options: RequestOptions,
): Promise<DiscoveryDocument> {
  const url = `${issuer.replace(/\/+$/, "")}/.well-known/openid-configuration`;
  const res = await send(
    fetch,
    url,
    { method: "GET", headers: { Accept: "application/json" } },
    options,
  );
  if (!res.ok) throw new DiscoveryError(`discovery returned HTTP ${res.status}`);
  const json = parseJson(res.body);
  if (!json) throw new DiscoveryError("discovery document is not a JSON object");
  if (json.issuer !== issuer) {
    throw new DiscoveryError(
      `discovery issuer ${JSON.stringify(json.issuer)} does not match configured issuer`,
    );
  }
  const allowInsecure = options.allowInsecureLoopback ?? false;
  const endpoint = (name: string, required: boolean): string | null => {
    const v = str(json[name]);
    if (!v) {
      if (required) throw new DiscoveryError(`discovery document has no ${name}`);
      return null;
    }
    const uri = parseUri(v);
    if (!uri || !isSecureEndpoint(uri, allowInsecure)) {
      throw new DiscoveryError(
        `discovery ${name} must be https (http only to loopback or a private-network address in development)`,
      );
    }
    return v;
  };
  return {
    issuer,
    authorization_endpoint: endpoint("authorization_endpoint", true) as string,
    token_endpoint: endpoint("token_endpoint", true) as string,
    revocation_endpoint: endpoint("revocation_endpoint", false),
    end_session_endpoint: endpoint("end_session_endpoint", false),
  };
}

// ── PKCE, state, nonce ──────────────────────────────────────────────────

export interface Pkce {
  verifier: string;
  challenge: string;
  method: "S256";
}

/** RFC 7636: 32 random bytes → 43-char base64url verifier; challenge = BASE64URL(SHA256(verifier)). */
export async function createPkce(crypto: CryptoLike): Promise<Pkce> {
  const verifier = randomToken(crypto, 32);
  const challenge = base64ToBase64Url(await crypto.sha256Base64(verifier));
  return { verifier, challenge, method: "S256" };
}

/** `bytes` random bytes as base64url (32 bytes = 256 bits for state and nonce). */
export function randomToken(crypto: CryptoLike, bytes = 32): string {
  const raw = crypto.getRandomBytes(bytes);
  if (raw.length !== bytes)
    throw new ProtocolError("crypto returned the wrong number of random bytes");
  return base64UrlEncode(raw);
}

// ── authorization request / callback ────────────────────────────────────

const RESERVED_AUTH_PARAMS = new Set([
  "response_type",
  "client_id",
  "redirect_uri",
  "scope",
  "state",
  "nonce",
  "code_challenge",
  "code_challenge_method",
  "prompt",
]);

export interface AuthorizeUrlInput {
  discovery: Pick<DiscoveryDocument, "authorization_endpoint">;
  config: Pick<OidcConfig, "clientId" | "redirectUri" | "scopes">;
  state: string;
  nonce: string;
  codeChallenge: string;
  /** `login` forces the IdP to re-authenticate (used after a sign-out). */
  prompt?: string;
  /** Extra provider-specific parameters. Reserved names are refused. */
  extraParams?: Record<string, string>;
}

export function authorizeUrl(input: AuthorizeUrlInput): string {
  for (const key of Object.keys(input.extraParams ?? {})) {
    if (RESERVED_AUTH_PARAMS.has(key)) {
      throw new ProtocolError(`extra authorize parameter ${JSON.stringify(key)} is reserved`);
    }
  }
  const fields: Record<string, string> = {
    response_type: "code",
    client_id: input.config.clientId,
    redirect_uri: input.config.redirectUri,
    scope: input.config.scopes.join(" "),
    state: input.state,
    nonce: input.nonce,
    code_challenge: input.codeChallenge,
    code_challenge_method: "S256",
  };
  if (input.prompt) fields.prompt = input.prompt;
  Object.assign(fields, input.extraParams);
  return appendQuery(input.discovery.authorization_endpoint, formEncode(fields));
}

function appendQuery(url: string, query: string): string {
  const hashAt = url.indexOf("#");
  const base = hashAt < 0 ? url : url.slice(0, hashAt);
  const hash = hashAt < 0 ? "" : url.slice(hashAt);
  const joiner = base.includes("?") ? (base.endsWith("?") || base.endsWith("&") ? "" : "&") : "?";
  return `${base}${joiner}${query}${hash}`;
}

const trimSlash = (p: string): string => (p.length > 1 ? p.replace(/\/+$/, "") : p);

/**
 * Does `callback` target `redirectUri`? Compared by scheme + authority + path,
 * ignoring query and fragment, and without assuming a custom scheme: it holds
 * for `com.app:/oauthredirect` and for `https://host/oauthredirect` alike.
 */
export function matchesRedirect(callback: string, redirectUri: string): boolean {
  const a = parseUri(callback);
  const b = parseUri(redirectUri);
  if (!a || !b) return false;
  return (
    a.scheme === b.scheme && a.authority === b.authority && trimSlash(a.path) === trimSlash(b.path)
  );
}

export interface CallbackExpectation {
  redirectUri: string;
  state: string;
  /** When the IdP returns `iss` (RFC 9207) it must equal this. */
  issuer?: string;
}

/**
 * Validate the URL the browser returned and extract the authorization code.
 * Order matters: the callback must be for our redirect, then `state` must
 * match (so a forged `error=` cannot steer us), then `error=`, then `code`.
 */
export function parseCallback(url: string, expected: CallbackExpectation): { code: string } {
  if (!matchesRedirect(url, expected.redirectUri)) {
    throw new StateMismatchError("callback URL does not match the configured redirect URI");
  }
  const parsed = parseUri(url);
  const params = parseQuery(parsed?.query ?? "");
  const state = params.get("state");
  if (!state || !constantTimeEqual(state, expected.state)) {
    throw new StateMismatchError("authorization state mismatch");
  }
  const iss = params.get("iss");
  if (expected.issuer && iss && iss !== expected.issuer) {
    throw new StateMismatchError("authorization response issuer mismatch");
  }
  const error = params.get("error");
  if (error) throw new AuthorizationError(error, params.get("error_description") ?? null);
  const code = params.get("code");
  if (!code) throw new MissingCodeError("authorization callback carried no code");
  return { code };
}

function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

// ── token endpoint ──────────────────────────────────────────────────────

export interface TokenSet {
  accessToken: string;
  refreshToken: string | null;
  idToken: string | null;
  /** Epoch milliseconds, or null when the IdP did not say. */
  expiresAt: number | null;
}

type Clock = () => number;

function toTokenSet(body: string, now: Clock): TokenSet {
  const json = parseJson(body);
  const accessToken = str(json?.access_token);
  if (!json || !accessToken) throw new ProtocolError("token response carried no access_token");
  const type = str(json.token_type);
  if (type && type.toLowerCase() !== "bearer") {
    throw new ProtocolError(`unsupported token_type ${JSON.stringify(type)}`);
  }
  const expiresIn = typeof json.expires_in === "number" ? json.expires_in : Number(json.expires_in);
  return {
    accessToken,
    refreshToken: str(json.refresh_token),
    idToken: str(json.id_token),
    expiresAt:
      Number.isFinite(expiresIn) && json.expires_in != null ? now() + expiresIn * 1000 : null,
  };
}

async function tokenRequest(
  fetch: FetchLike,
  endpoint: string,
  fields: Record<string, string>,
  now: Clock,
  options: RequestOptions,
): Promise<TokenSet> {
  const res = await send(
    fetch,
    endpoint,
    {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" },
      body: formEncode(fields),
    },
    options,
  );
  if (!res.ok) throw tokenEndpointError(res.status, res.body);
  return toTokenSet(res.body, now);
}

export interface ExchangeCodeInput {
  discovery: Pick<DiscoveryDocument, "token_endpoint">;
  config: Pick<OidcConfig, "clientId" | "redirectUri">;
  code: string;
  codeVerifier: string;
  fetch: FetchLike;
  now?: Clock;
  options?: RequestOptions;
}

/** Public-client code exchange: client_id and the PKCE verifier, no secret. */
export function exchangeCode(input: ExchangeCodeInput): Promise<TokenSet> {
  return tokenRequest(
    input.fetch,
    input.discovery.token_endpoint,
    {
      grant_type: "authorization_code",
      code: input.code,
      redirect_uri: input.config.redirectUri,
      client_id: input.config.clientId,
      code_verifier: input.codeVerifier,
    },
    input.now ?? Date.now,
    input.options ?? {},
  );
}

export interface RefreshInput {
  discovery: Pick<DiscoveryDocument, "token_endpoint">;
  config: Pick<OidcConfig, "clientId">;
  refreshToken: string;
  fetch: FetchLike;
  now?: Clock;
  options?: RequestOptions;
}

/**
 * Refresh grant. The IdP may rotate the refresh token or omit it; this
 * function returns what the response said (null = "keep the old one"), the
 * caller merges.
 */
export function refreshTokens(input: RefreshInput): Promise<TokenSet> {
  return tokenRequest(
    input.fetch,
    input.discovery.token_endpoint,
    {
      grant_type: "refresh_token",
      refresh_token: input.refreshToken,
      client_id: input.config.clientId,
    },
    input.now ?? Date.now,
    input.options ?? {},
  );
}

export interface RevokeInput {
  discovery: Pick<DiscoveryDocument, "revocation_endpoint">;
  config: Pick<OidcConfig, "clientId">;
  token: string;
  tokenTypeHint?: "refresh_token" | "access_token";
  fetch: FetchLike;
  options?: RequestOptions;
}

/** RFC 7009 revocation. Returns false when the IdP publishes no revocation endpoint. */
export async function revokeToken(input: RevokeInput): Promise<boolean> {
  const endpoint = input.discovery.revocation_endpoint;
  if (!endpoint) return false;
  const fields: Record<string, string> = { token: input.token, client_id: input.config.clientId };
  if (input.tokenTypeHint) fields.token_type_hint = input.tokenTypeHint;
  const res = await send(
    input.fetch,
    endpoint,
    {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" },
      body: formEncode(fields),
    },
    input.options ?? {},
  );
  if (!res.ok) throw tokenEndpointError(res.status, res.body);
  return true;
}

export interface EndSessionInput {
  discovery: Pick<DiscoveryDocument, "end_session_endpoint">;
  config: Pick<OidcConfig, "clientId" | "postLogoutRedirectUri">;
  idTokenHint?: string | null;
  state?: string;
}

/** The RP-initiated logout URL (OIDC RP-Initiated Logout 1.0), or null when the IdP has none. */
export function endSessionUrl(input: EndSessionInput): string | null {
  const endpoint = input.discovery.end_session_endpoint;
  if (!endpoint) return null;
  const fields: Record<string, string> = { client_id: input.config.clientId };
  if (input.idTokenHint) fields.id_token_hint = input.idTokenHint;
  if (input.config.postLogoutRedirectUri) {
    fields.post_logout_redirect_uri = input.config.postLogoutRedirectUri;
    if (input.state) fields.state = input.state;
  }
  return appendQuery(endpoint, formEncode(fields));
}

// ── id token (decode only) ──────────────────────────────────────────────

export type IdTokenClaims = Record<string, unknown>;

/** Decode a JWT's payload WITHOUT verifying its signature. Presentation only. */
export function decodeJwtClaims(jwt: string): IdTokenClaims {
  const parts = jwt.split(".");
  if (parts.length !== 3) throw new IdTokenError("id token is not a JWT");
  try {
    const bytes = base64UrlDecode(parts[1]);
    const text = decodeURIComponent(
      Array.from(bytes, (b) => `%${b.toString(16).padStart(2, "0")}`).join(""),
    );
    const claims: unknown = JSON.parse(text);
    if (!claims || typeof claims !== "object" || Array.isArray(claims))
      throw new Error("not an object");
    return claims as IdTokenClaims;
  } catch (e) {
    throw new IdTokenError("id token payload is malformed", { cause: e });
  }
}

export interface IdTokenExpectation {
  issuer: string;
  clientId: string;
  /** Required on a fresh sign-in; absent on refresh (the nonce binds only the first response). */
  nonce?: string;
}

/** Sanity-check decoded claims (issuer, audience, nonce). NOT a signature check. */
export function checkIdTokenClaims(claims: IdTokenClaims, expected: IdTokenExpectation): void {
  if (claims.iss !== expected.issuer) throw new IdTokenError("id token issuer mismatch");
  const aud = claims.aud;
  const audiences = Array.isArray(aud) ? aud : [aud];
  if (!audiences.includes(expected.clientId)) throw new IdTokenError("id token audience mismatch");
  if (typeof claims.sub !== "string" || claims.sub === "")
    throw new IdTokenError("id token has no subject");
  if (expected.nonce !== undefined) {
    if (typeof claims.nonce !== "string" || !constantTimeEqual(claims.nonce, expected.nonce)) {
      throw new IdTokenError("id token nonce mismatch");
    }
  }
}
