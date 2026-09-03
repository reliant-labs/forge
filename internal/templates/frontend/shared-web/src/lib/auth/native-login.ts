// yours: scaffolded once, never touched again — forge will not overwrite this file
//
// The browser half of NATIVE SIGN-IN — and it is deliberately small.
//
// ── What this file does NOT do ────────────────────────────────────────
//
// It does not know what OIDC is. There is no issuer URL here, no client
// id, no PKCE, no discovery document, no redirect to a provider, no hidden
// iframe, no token parsing, and no token storage. The browser never
// contacts the identity provider at all.
//
// Everything below is an ordinary POST to this app's own API. Your server
// runs the whole OIDC flow (internal/app/login_broker.go) and answers with
// an HttpOnly session cookie.
//
//	sign in  →  POST /auth/login     → cookie set, identity returned
//	sign up  →  POST /auth/register  → cookie set, identity returned
//	who am I →  GET  /auth/session   → identity, or not authenticated
//	sign out →  POST /auth/logout    → cookie cleared
//
// ── Why the token is not here ─────────────────────────────────────────
//
// The session cookie is HttpOnly, so this code CANNOT read it — and that
// is the point rather than a limitation. A token no script can reach is a
// token an XSS cannot steal. The cookie rides along automatically on
// same-origin requests, so nothing here has to attach an Authorization
// header.
//
// What you get instead is an `Identity`: who is signed in, for rendering.
// Use it to decide what to SHOW. Never to decide what is ALLOWED — the
// server validates the real token on every request.

/** Where the API lives. Empty means same-origin, which is the common case. */
function apiOrigin(): string {
  const fromRuntime =
    typeof window !== "undefined"
      ? (window as { __FORGE_CONFIG__?: { API_URL?: unknown } })
          .__FORGE_CONFIG__?.API_URL
      : undefined;
  const configured =
    fromRuntime ||
    (typeof process !== "undefined" && process.env?.NEXT_PUBLIC_API_URL) ||
    "";
  return String(configured).replace(/\/+$/, "");
}

/** Who is signed in. Presentation only — see the file header. */
export interface Identity {
  authenticated: boolean;
  email?: string;
  name?: string;
  subject?: string;
}

export interface Credentials {
  email: string;
  password: string;
  /** Only meaningful for an account with TOTP enrolled. */
  totp?: string;
}

export interface Registration {
  email: string;
  password: string;
  givenName?: string;
  familyName?: string;
}

/**
 * Thrown when the API refuses a request. `status` lets a screen tell
 * "wrong credentials" (401) from "the service is down" (5xx), which are
 * very different things to tell a user.
 */
export class AuthError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "AuthError";
    this.status = status;
  }
}

/** Signs in. Resolves with the identity; the session cookie is already set. */
export async function signIn(credentials: Credentials): Promise<Identity> {
  return post("/auth/login", {
    email: credentials.email,
    password: credentials.password,
    ...(credentials.totp ? { totp: credentials.totp } : {}),
  });
}

/**
 * Creates an account and signs in, in one call.
 *
 * The server does both, so there is no window where someone has an account
 * but no session — no bouncing back to a login form to retype the password
 * they just chose.
 */
export async function signUp(details: Registration): Promise<Identity> {
  return post("/auth/register", {
    email: details.email,
    password: details.password,
    ...(details.givenName ? { givenName: details.givenName } : {}),
    ...(details.familyName ? { familyName: details.familyName } : {}),
  });
}

/** Clears the session cookie. */
export async function signOut(): Promise<Identity> {
  return post("/auth/logout", {});
}

/**
 * Asks the server who is signed in.
 *
 * This is how the app learns its own auth state at load: the cookie is
 * unreadable to scripts, so the server is the only thing that can answer.
 * One cheap same-origin request, and no provider round trip.
 */
export async function fetchIdentity(): Promise<Identity> {
  try {
    const response = await fetch(`${apiOrigin()}/auth/session`, {
      // See the note in post(): cross-origin in dev, so the cookie only
      // travels with "include".
      credentials: "include",
      headers: { Accept: "application/json" },
    });
    if (!response.ok) return { authenticated: false };
    return (await response.json()) as Identity;
  } catch {
    // A transport failure is not a signed-out user, but it is the safe
    // answer to render: closed rather than open.
    return { authenticated: false };
  }
}

async function post(path: string, body: unknown): Promise<Identity> {
  let response: Response;
  try {
    response = await fetch(`${apiOrigin()}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      // "include", not "same-origin": in the dev loop the API is on its
      // own port, so every auth call is cross-origin and "same-origin"
      // silently DROPS the session cookie — login returns 200 and the user
      // stays signed out, with nothing in the console to explain it. The
      // server pairs this with cors_allow_credentials.
      credentials: "include",
      body: JSON.stringify(body),
    });
  } catch {
    // Distinguished from a credential failure so a user does not retype a
    // correct password at a server that is simply unreachable.
    throw new AuthError("Could not reach the sign-in service.", 0);
  }

  if (!response.ok) {
    throw new AuthError(await errorMessage(response), response.status);
  }
  return (await response.json()) as Identity;
}

/**
 * The server's message when it has one.
 *
 * It is already safe to display: sign-in failures are deliberately generic
 * (a form that distinguishes "no such user" from "wrong password" is a
 * user-enumeration oracle), while sign-up failures are specific because
 * the person filling in the form has to read them to fix it.
 */
async function errorMessage(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // Fall through.
  }
  return response.status >= 500
    ? "The sign-in service is unavailable. Please try again."
    : "Those credentials were not accepted.";
}
