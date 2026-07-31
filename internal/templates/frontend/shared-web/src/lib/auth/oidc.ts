// OIDC authorization-code + PKCE mechanism. Browser half of forge/pkg/oauth2.
//
// # Scope
//
// This module OBTAINS tokens. It does not validate them — a forge server
// validates bearer tokens statelessly (pkg/auth), and the two never talk to
// each other. The flow here runs entirely between the browser and the
// identity provider; the forge server is not a participant and has no
// endpoint in it. That is why there is no `/auth/login` route to POST to.
//
// Nothing here is specific to any identity provider. Endpoints arrive as
// arguments, either literally or from `discover()`, so the same code drives
// Auth0, Clerk, Keycloak, Zitadel, Okta or Logto with only configuration
// changing.
//
// # Relationship to forge/pkg/oauth2
//
// The Go package is the reference. Every security property below is the same
// property, spelled in TypeScript:
//
//   - S256 only. `challenge()` computes BASE64URL(SHA256(ASCII(verifier)))
//     with padding omitted (RFC 7636 §4.2) via WebCrypto. `plain` is not
//     implemented at all — RFC 7636 §7.2 permits it only for clients that
//     cannot hash, and a browser with `crypto.subtle` has no such excuse.
//   - Randomness comes from `crypto.getRandomValues`, never `Math.random`,
//     which is not a CSPRNG and is seeded predictably in several engines.
//   - State is compared before anything in the callback is trusted, and an
//     empty value on either side is rejected rather than compared (so a
//     provider that echoes no state cannot satisfy the check by matching
//     "" against "").
//   - An OAuth `error` response is surfaced as a thrown [OAuthError], never
//     as an empty-but-successful token. A blank token would authenticate
//     nothing and resurface later as an unexplained 401, far from its cause.
//   - The verifier is a secret and resists disclosure: [Verifier] holds it in
//     a `#private` field and renders as "[REDACTED]" under String(),
//     template interpolation, console logging and JSON.stringify.
//
// # Divergence from the Go package, on purpose
//
// Go's `Verifier.MarshalJSON` FAILS with an error rather than emitting a
// placeholder, because a silent "[REDACTED]" inside a server-side session
// cookie would be a login that mysteriously stops working. Here it redacts
// instead of throwing: the verifier is never serialized as part of a session
// (it is written to sessionStorage deliberately, via `.secret()`), so the
// failure mode Go is defending against does not exist — while a throw from
// inside `JSON.stringify` of some enclosing object WOULD take down a render.

/** RFC 7636 §4.1 bounds: a code verifier is 43-128 unreserved characters. */
export const MIN_VERIFIER_LENGTH = 43;
export const MAX_VERIFIER_LENGTH = 128;

/**
 * The "unreserved" production of RFC 3986 §2.3, which RFC 7636 §4.1 names as
 * the code verifier's alphabet: ALPHA / DIGIT / "-" / "." / "_" / "~".
 */
const UNRESERVED = /^[A-Za-z0-9\-._~]+$/;

/** What secret-bearing values render as when something tries to print them. */
const REDACTED = "[REDACTED]";

/** base64url encoding without padding (RFC 4648 §5), the PKCE wire form. */
function base64UrlEncode(bytes: Uint8Array): string {
  let ascii = "";
  for (const byte of bytes) {
    ascii += String.fromCharCode(byte);
  }
  return btoa(ascii).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/**
 * `n` cryptographically secure random bytes, base64url-encoded. 32 bytes
 * encode to exactly the 43 characters RFC 7636 §4.1 recommends.
 *
 * Fails loudly when WebCrypto is absent rather than reaching for a fallback:
 * the only fallback available in a browser is `Math.random`, and a PKCE
 * verifier an attacker can predict removes the entire point of PKCE. Every
 * context this ships into (every browser with `crypto.subtle`, jsdom >= 20,
 * Node >= 18) has it.
 */
function randomBase64Url(byteLength: number): string {
  if (typeof globalThis.crypto?.getRandomValues !== "function") {
    throw new Error(
      "oidc: crypto.getRandomValues is unavailable, so no unguessable code " +
        "verifier or state can be generated. This module requires WebCrypto " +
        "(a secure context: https:// or localhost). It deliberately has no " +
        "Math.random fallback — a predictable verifier defeats PKCE.",
    );
  }
  const bytes = new Uint8Array(byteLength);
  globalThis.crypto.getRandomValues(bytes);
  return base64UrlEncode(bytes);
}

/**
 * Verifier is a PKCE code verifier: the high-entropy secret whose SHA-256
 * hash is published in the authorization request and whose plaintext is
 * revealed only to the token endpoint.
 *
 * The value lives in a `#private` field, which is genuinely unreachable from
 * outside the class — not a naming convention. The type then actively refuses
 * to disclose it:
 *
 *   - `String(v)`, `` `${v}` `` and `console.log(v)` print "[REDACTED]".
 *   - `JSON.stringify(v)` and `JSON.stringify({verifier: v})` produce
 *     "[REDACTED]" rather than the secret.
 *
 * A verifier must outlive the redirect to the provider. Persist
 * `verifier.secret()` for the duration of ONE login and rebuild it with
 * [parseVerifier]. See `oidc-storage.ts` for where that goes and why.
 */
export class Verifier {
  readonly #value: string;

  constructor(value: string) {
    const problem = verifierProblem(value);
    if (problem) {
      throw new Error(problem);
    }
    this.#value = value;
  }

  /**
   * The code verifier in the clear. Every call site is a place the secret can
   * escape: send it to the token endpoint, or persist it for one login. Do
   * not log it.
   */
  secret(): string {
    return this.#value;
  }

  /** Redacted placeholder — see the class doc. */
  toString(): string {
    return REDACTED;
  }

  /** Redacted placeholder for JSON.stringify, including when nested. */
  toJSON(): string {
    return REDACTED;
  }

  /**
   * The S256 code challenge: BASE64URL(SHA256(ASCII(verifier))) with padding
   * omitted (RFC 7636 §4.2).
   *
   * There is no `plain` variant anywhere in this module. Under `plain` the
   * challenge IS the verifier, so an attacker who can observe the
   * authorization request can redeem the code — which is the attack PKCE
   * exists to stop.
   */
  async challenge(): Promise<string> {
    if (typeof globalThis.crypto?.subtle?.digest !== "function") {
      throw new Error(
        "oidc: crypto.subtle is unavailable, so the S256 code challenge " +
          "cannot be computed. WebCrypto requires a secure context — serve " +
          "the app over https:// or from localhost. This module does not " +
          "fall back to the `plain` challenge method (RFC 7636 §7.2): under " +
          "plain the challenge is the verifier itself, which offers no " +
          "protection against an attacker who can see the authorization URL.",
      );
    }
    const digest = await globalThis.crypto.subtle.digest(
      "SHA-256",
      new TextEncoder().encode(this.#value),
    );
    return base64UrlEncode(new Uint8Array(digest));
  }
}

/**
 * verifierProblem reports why `value` is not a legal code verifier, or null
 * when it is fine. The messages never include `value` itself — callers pass
 * secrets in.
 */
function verifierProblem(value: string): string | null {
  if (value.length < MIN_VERIFIER_LENGTH || value.length > MAX_VERIFIER_LENGTH) {
    return `oidc: code verifier length ${value.length} is outside [${MIN_VERIFIER_LENGTH}, ${MAX_VERIFIER_LENGTH}] (RFC 7636 §4.1)`;
  }
  if (!UNRESERVED.test(value)) {
    return "oidc: code verifier contains a character outside the unreserved set (RFC 7636 §4.1)";
  }
  return null;
}

/**
 * A fresh code verifier from 32 bytes of CSPRNG output, base64url-encoded
 * into the 43 characters RFC 7636 §4.1 recommends.
 */
export function createVerifier(): Verifier {
  return new Verifier(randomBase64Url(32));
}

/**
 * Rebuilds a Verifier from a previously generated string, rejecting anything
 * RFC 7636 §4.1 would not accept. Use it to restore the verifier that was
 * persisted across the authorization redirect.
 */
export function parseVerifier(value: string): Verifier {
  return new Verifier(value);
}

/**
 * An opaque CSRF token for the `state` parameter: 32 bytes of CSPRNG output,
 * base64url-encoded.
 *
 * State travels in a URL and is returned as a plain string rather than a
 * secret-bearing type — it is a nonce to be echoed back and compared, not a
 * credential. Bind it to this browser (see `oidc-storage.ts`) and check it
 * with [statesMatch] before trusting anything in the callback.
 */
export function createState(): string {
  return randomBase64Url(32);
}

/**
 * statesMatch reports whether the state echoed by the provider is the state
 * that was sent.
 *
 * Empty on either side is NEVER a match, and that is checked before the
 * comparison rather than falling out of it: a provider that echoes no state,
 * paired with a session that recorded none, would otherwise compare "" to ""
 * and pass the CSRF check without either side having proved anything.
 *
 * The comparison accumulates over every character instead of returning at the
 * first difference. JavaScript cannot offer real constant-time string
 * comparison — the engine interns, and `.length` is observable — so this is
 * defence in depth rather than a guarantee, which is the honest claim to
 * make. It costs nothing, and `state` is a single-use nonce that an attacker
 * has one round trip to guess, so the timing channel was never the weak
 * point here.
 */
export function statesMatch(sent: string, received: string): boolean {
  if (!sent || !received) {
    return false;
  }
  if (sent.length !== received.length) {
    return false;
  }
  let diff = 0;
  for (let i = 0; i < sent.length; i++) {
    diff |= sent.charCodeAt(i) ^ received.charCodeAt(i);
  }
  return diff === 0;
}

/**
 * OAuthError is an OAuth 2.0 error response (RFC 6749 §5.2) as a thrown
 * error, from either leg of the flow: `?error=` on the callback URL, or an
 * error body from the token endpoint.
 *
 * The provider's machine-readable code is preserved so callers can branch:
 * `invalid_grant` means the code is spent or expired and the user should
 * start over, while `invalid_client` is a deployment problem no retry fixes.
 */
export class OAuthError extends Error {
  readonly code: string;
  readonly description: string;
  readonly uri: string;
  readonly status: number;

  constructor(init: {
    code: string;
    description?: string;
    uri?: string;
    status?: number;
  }) {
    const parts = [`oidc: authorization server returned error "${init.code}"`];
    if (init.status) {
      parts.push(` (HTTP ${init.status})`);
    }
    if (init.description) {
      parts.push(`: ${init.description}`);
    }
    super(parts.join(""));
    this.name = "OAuthError";
    this.code = init.code;
    this.description = init.description ?? "";
    this.uri = init.uri ?? "";
    this.status = init.status ?? 0;
  }
}

/** Standard OAuth 2.0 error codes (RFC 6749 §5.2), for comparing `code`. */
export const OAUTH_ERROR_INVALID_REQUEST = "invalid_request";
export const OAUTH_ERROR_INVALID_CLIENT = "invalid_client";
export const OAUTH_ERROR_INVALID_GRANT = "invalid_grant";
export const OAUTH_ERROR_UNAUTHORIZED_CLIENT = "unauthorized_client";
export const OAUTH_ERROR_ACCESS_DENIED = "access_denied";

/**
 * The query parameters `buildAuthorizeUrl` sets itself. `extra` may not
 * contain them: a duplicated `code_challenge` or `redirect_uri` is a
 * parameter-injection foothold, not a convenience.
 */
const RESERVED_AUTHORIZE_PARAMS = new Set([
  "response_type",
  "client_id",
  "redirect_uri",
  "scope",
  "state",
  "code_challenge",
  "code_challenge_method",
]);

/** The authorization request a user agent is redirected to (RFC 6749 §4.1.1). */
export interface AuthorizeRequest {
  /** The provider's authorization endpoint. Required. */
  endpoint: string;
  /** Identifies the client to the provider. Required. */
  clientId: string;
  /**
   * Must match a URI registered with the provider. Required: omitting it lets
   * a provider fall back to a registered default, which is how a callback
   * lands somewhere the caller did not intend.
   */
  redirectUri: string;
  /** Sent space-delimited. Include "openid" for OIDC. */
  scopes: string[];
  /** The CSRF token from [createState]. Required. */
  state: string;
  /** The S256 challenge from `Verifier.challenge()`. Required. */
  challenge: string;
  /** Provider-specific parameters (prompt, audience, login_hint, ...). */
  extra?: Record<string, string>;
}

/**
 * Renders the authorization request as an absolute URL, every parameter
 * percent-encoded by URLSearchParams.
 *
 * It throws rather than returning a half-built URL. A request missing a
 * challenge, a state or a redirect URI is a flow with a security property
 * silently switched off, and the provider is not obliged to complain.
 */
export function buildAuthorizeUrl(req: AuthorizeRequest): string {
  const missing = (
    [
      ["endpoint", req.endpoint],
      ["clientId", req.clientId],
      ["redirectUri", req.redirectUri],
      ["state", req.state],
      ["challenge", req.challenge],
    ] as const
  )
    .filter(([, value]) => !value)
    .map(([name]) => name);
  if (missing.length > 0) {
    throw new Error(
      `oidc: authorization request is missing required field(s): ${missing.join(", ")}`,
    );
  }

  const url = new URL(req.endpoint);
  if (url.protocol !== "https:" && !isLoopbackHost(url.hostname)) {
    throw new Error(
      `oidc: authorization endpoint ${req.endpoint} must use https (loopback excepted for local development)`,
    );
  }

  // Start from any query the endpoint already publishes — some multi-tenant
  // issuers carry one — so it survives.
  const params = url.searchParams;
  for (const [key, value] of Object.entries(req.extra ?? {})) {
    if (RESERVED_AUTHORIZE_PARAMS.has(key)) {
      throw new Error(
        `oidc: extra authorization parameter ${key} collides with one this module sets; use the corresponding AuthorizeRequest field`,
      );
    }
    params.set(key, value);
  }
  params.set("response_type", "code");
  params.set("client_id", req.clientId);
  params.set("redirect_uri", req.redirectUri);
  params.set("state", req.state);
  params.set("code_challenge", req.challenge);
  // S256, always and only. See Verifier.challenge().
  params.set("code_challenge_method", "S256");
  if (req.scopes.length > 0) {
    params.set("scope", req.scopes.join(" "));
  }
  url.search = params.toString();
  return url.toString();
}

/** http:// is tolerated only on loopback, where there is no network to sniff. */
function isLoopbackHost(hostname: string): boolean {
  return (
    hostname === "localhost" ||
    hostname === "127.0.0.1" ||
    hostname === "[::1]" ||
    hostname === "::1"
  );
}

/** A successful token endpoint response (RFC 6749 §5.1). */
export interface TokenResponse {
  accessToken: string;
  tokenType: string;
  /** Seconds until expiry, or 0 when the provider sent none. */
  expiresIn: number;
  refreshToken: string;
  idToken: string;
  scope: string;
}

/** An authorization-code redemption (RFC 6749 §4.1.3 + RFC 7636 §4.5). */
export interface ExchangeRequest {
  /** The provider's token endpoint. Required. */
  endpoint: string;
  /** Must be the client the code was issued to. Required. */
  clientId: string;
  /**
   * Must byte-for-byte match the authorization request's. Required — a
   * mismatch is the most common cause of an unexplained invalid_grant.
   */
  redirectUri: string;
  /** The authorization code from the callback. Required. */
  code: string;
  /** The verifier whose challenge was sent with the authorization request. */
  verifier: Verifier;
  /** Injectable for tests; defaults to global fetch. */
  fetchImpl?: typeof fetch;
}

/**
 * Redeems an authorization code for a token, proving possession of the PKCE
 * verifier.
 *
 * There is no `client_secret` parameter. A browser SPA is a PUBLIC client:
 * anything shipped in the bundle is readable by anyone who loads the page, so
 * a "secret" there is not one. PKCE is what replaces it. A provider that
 * demands a client secret for this grant is telling you the exchange belongs
 * on a server (a BFF), not in the browser.
 *
 * Throws [OAuthError] for a provider error response, and a plain Error for a
 * 200 that carries no access token. Neither is reported as success.
 */
export async function exchangeCode(
  req: ExchangeRequest,
): Promise<TokenResponse> {
  const missing = (
    [
      ["endpoint", req.endpoint],
      ["clientId", req.clientId],
      ["redirectUri", req.redirectUri],
      ["code", req.code],
    ] as const
  )
    .filter(([, value]) => !value)
    .map(([name]) => name);
  if (missing.length > 0) {
    throw new Error(
      `oidc: token request is missing required field(s): ${missing.join(", ")}`,
    );
  }

  const body = new URLSearchParams({
    grant_type: "authorization_code",
    code: req.code,
    code_verifier: req.verifier.secret(),
    client_id: req.clientId,
    redirect_uri: req.redirectUri,
  });

  const doFetch = req.fetchImpl ?? globalThis.fetch;
  const response = await doFetch(req.endpoint, {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      Accept: "application/json",
    },
    body: body.toString(),
  });

  const raw = await response.text();
  return parseTokenResponse(response.status, raw);
}

/**
 * Turns a token endpoint reply into a TokenResponse or throws.
 *
 * Split out from `exchangeCode` so the response-handling rules — where the
 * security-relevant decisions live — are testable with no network.
 *
 * Decides by SHAPE rather than by status alone: RFC 6749 §5.2 mandates 400
 * for most error codes, but providers exist that answer 200 with an error
 * body, and others that answer 401 with one.
 */
export function parseTokenResponse(
  status: number,
  raw: string,
): TokenResponse {
  let parsed: Record<string, unknown> | null = null;
  try {
    parsed = JSON.parse(raw) as Record<string, unknown>;
  } catch {
    // Some providers answer errors with a form-encoded body.
    const form = new URLSearchParams(raw);
    if ([...form.keys()].length > 0) {
      parsed = Object.fromEntries(form.entries());
    }
  }

  if (!parsed || typeof parsed !== "object") {
    throw new Error(
      `oidc: token endpoint returned an unparseable body (HTTP ${status})`,
    );
  }

  const errorCode = stringField(parsed, "error");
  if (errorCode) {
    throw new OAuthError({
      code: errorCode,
      description: stringField(parsed, "error_description"),
      uri: stringField(parsed, "error_uri"),
      status,
    });
  }

  // A non-2xx with no recognisable error code still is not a success.
  if (status < 200 || status > 299) {
    throw new OAuthError({
      code: "invalid_token_response",
      description: `token endpoint returned HTTP ${status} with no OAuth error code`,
      status,
    });
  }

  const accessToken = stringField(parsed, "access_token");
  if (!accessToken) {
    // Deliberately a hard failure, not an empty success: a blank token is a
    // credential-shaped value that authenticates nothing, and the resulting
    // 401 would surface far from this line.
    throw new Error(
      "oidc: token response contained no access_token (HTTP " + status + ")",
    );
  }

  const expiresIn = parsed["expires_in"];
  return {
    accessToken,
    tokenType: stringField(parsed, "token_type") || "Bearer",
    expiresIn: typeof expiresIn === "number" ? expiresIn : 0,
    refreshToken: stringField(parsed, "refresh_token"),
    idToken: stringField(parsed, "id_token"),
    scope: stringField(parsed, "scope"),
  };
}

/** A refresh-token redemption (RFC 6749 §6). */
export interface RefreshRequest {
  /** The provider's token endpoint. Required. */
  endpoint: string;
  /** Must be the client the refresh token was issued to. Required. */
  clientId: string;
  /** The refresh token to redeem. Required. */
  refreshToken: string;
  /** Injectable for tests; defaults to global fetch. */
  fetchImpl?: typeof fetch;
}

/** The result of a successful [refreshToken] call. */
export interface RefreshResult {
  /** The provider's token response. */
  token: TokenResponse;
  /**
   * What to present on the NEXT refresh. ALWAYS populated: the rotated token
   * when the provider issued one, otherwise the token just presented. Store it
   * unconditionally — there is no branch for a caller to get wrong.
   */
  refreshToken: string;
  /**
   * Whether the provider issued a NEW refresh token and therefore invalidated
   * the previous one. Informational; `refreshToken` above is already correct.
   */
  rotated: boolean;
}

/**
 * Redeems a refresh token for a new access token (RFC 6749 §6). The browser
 * twin of forge/pkg/oauth2's `Exchanger.Refresh`.
 *
 * There is no `client_secret`, for the same reason `exchangeCode` has none: a
 * browser SPA is a public client.
 *
 * ── ROTATION, and why it is handled here ──────────────────────────────
 *
 * A provider MAY return a new refresh token, and RFC 6749 §6 then requires the
 * client to discard the old one. Many rotate on EVERY refresh and treat a
 * re-presented old token as theft — revoking the whole grant, which signs the
 * user out completely rather than failing one call. (Verified against Logto
 * 1.41.0: it rotates every time, and outside a ~3s grace window a replayed
 * token returns invalid_grant AND kills the grant, so even the newest token
 * dies.)
 *
 * So this returns what to present next rather than leaving the caller to work
 * it out from an optionally-present field. That grace window is precisely why
 * this cannot be left to chance: a mistake here USUALLY appears to work, and
 * only detonates when a straggling request lands late.
 *
 * Throws [OAuthError] for a provider error response, and a plain Error for a
 * 200 with no access token. Neither is reported as success.
 */
export async function refreshToken(
  req: RefreshRequest,
): Promise<RefreshResult> {
  const missing = (
    [
      ["endpoint", req.endpoint],
      ["clientId", req.clientId],
      ["refreshToken", req.refreshToken],
    ] as const
  )
    .filter(([, value]) => !value)
    .map(([name]) => name);
  if (missing.length > 0) {
    throw new Error(
      `oidc: refresh request is missing required field(s): ${missing.join(", ")}`,
    );
  }

  const body = new URLSearchParams({
    grant_type: "refresh_token",
    refresh_token: req.refreshToken,
    client_id: req.clientId,
  });

  const doFetch = req.fetchImpl ?? globalThis.fetch;
  const response = await doFetch(req.endpoint, {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      Accept: "application/json",
    },
    body: body.toString(),
  });

  const token = parseTokenResponse(response.status, await response.text());
  const rotated =
    token.refreshToken !== "" && token.refreshToken !== req.refreshToken;
  return {
    token,
    refreshToken: rotated ? token.refreshToken : req.refreshToken,
    rotated,
  };
}

/** Reads a string field, tolerating a provider that sends a number. */
function stringField(obj: Record<string, unknown>, key: string): string {
  const value = obj[key];
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number") {
    return String(value);
  }
  return "";
}

/**
 * The subset of an OpenID Provider configuration (OIDC Discovery 1.0 §3, RFC
 * 8414) this flow needs.
 */
export interface ProviderMetadata {
  issuer: string;
  authorizationEndpoint: string;
  tokenEndpoint: string;
  endSessionEndpoint: string;
  jwksUri: string;
  codeChallengeMethodsSupported: string[];
}

/**
 * Fetches and validates a provider's configuration from
 * `issuer + "/.well-known/openid-configuration"`.
 *
 * Discovery is why ONE issuer URL is enough configuration: the authorization
 * endpoint, the token endpoint and the end-session endpoint all come from the
 * document, so switching providers is a config change and never a code
 * change.
 *
 * The document's declared issuer must match the one it was fetched from
 * (OIDC Discovery §4.3, modulo the trailing slash providers are inconsistent
 * about). A mismatch means the document cannot be trusted to describe the
 * issuer the caller asked about, so it is rejected rather than used.
 */
export async function discover(
  issuer: string,
  fetchImpl?: typeof fetch,
): Promise<ProviderMetadata> {
  if (!issuer) {
    throw new Error("oidc: discovery requires an issuer URL");
  }
  const metadataUrl = `${issuer.replace(/\/$/, "")}/.well-known/openid-configuration`;
  const doFetch = fetchImpl ?? globalThis.fetch;
  const response = await doFetch(metadataUrl, {
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    throw new Error(
      `oidc: fetch ${metadataUrl}: HTTP ${response.status}. Check that the issuer URL is the OIDC issuer (the value that appears in the token's iss claim), not the provider's dashboard URL.`,
    );
  }
  const doc = (await response.json()) as Record<string, unknown>;

  const declared = stringField(doc, "issuer");
  if (declared.replace(/\/$/, "") !== issuer.replace(/\/$/, "")) {
    throw new Error(
      `oidc: discovery document issuer mismatch: requested "${issuer}", document declared "${declared}" (OIDC Discovery §4.3)`,
    );
  }

  const authorizationEndpoint = stringField(doc, "authorization_endpoint");
  const tokenEndpoint = stringField(doc, "token_endpoint");
  const absent = [
    ["authorization_endpoint", authorizationEndpoint],
    ["token_endpoint", tokenEndpoint],
  ]
    .filter(([, value]) => !value)
    .map(([name]) => name);
  if (absent.length > 0) {
    throw new Error(
      `oidc: discovery document from ${metadataUrl} omits required field(s): ${absent.join(", ")}`,
    );
  }

  const methods = doc["code_challenge_methods_supported"];
  return {
    issuer: declared,
    authorizationEndpoint,
    tokenEndpoint,
    endSessionEndpoint: stringField(doc, "end_session_endpoint"),
    jwksUri: stringField(doc, "jwks_uri"),
    codeChallengeMethodsSupported: Array.isArray(methods)
      ? methods.filter((m): m is string => typeof m === "string")
      : [],
  };
}
