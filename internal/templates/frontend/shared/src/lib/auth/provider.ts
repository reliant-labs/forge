export interface AuthUser {
  id: string;
  email?: string;
  name?: string;
  /**
   * Every claim the id token carried, decoded but NOT signature-verified.
   *
   * Present so a UI can read the claims your IdP actually issues — an org id,
   * a role, a plan — without each app re-implementing base64url decoding of a
   * JWT it already has. `id`/`email`/`name` above are the three claims forge
   * can name portably; everything else is issuer-specific and lives here.
   *
   * PRESENTATION ONLY. These are read in the browser with no signature check,
   * so they are exactly as trustworthy as the user's devtools — which is to
   * say, not at all. Use them to decide what to SHOW (hide an admin tab,
   * render a plan badge); never to decide what is ALLOWED. Authorization is
   * the backend's job: it validates the same token's signature against the
   * issuer's JWKS on every RPC, and a caller who edits a claim here changes
   * the UI and nothing else.
   */
  claims?: Readonly<Record<string, unknown>>;
}

export interface AuthProvider {
  /** Get the current access token, or null if not authenticated */
  getToken(): Promise<string | null>;
  /** Get the current user, or null if not authenticated */
  getUser(): AuthUser | null;
  /** Whether the user is currently authenticated */
  isAuthenticated(): boolean;
  /** Whether auth state is still loading */
  isLoading(): boolean;
  /** Sign in — implementation-specific (redirect, popup, etc.) */
  login(): Promise<void>;
  /** Sign out */
  logout(): Promise<void>;
  /** Subscribe to auth state changes. Returns unsubscribe function. */
  onAuthStateChange(callback: (user: AuthUser | null) => void): () => void;
}
