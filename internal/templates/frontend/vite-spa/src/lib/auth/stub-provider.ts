import type { AuthProvider, AuthUser } from "./provider";
import { activeScenario } from "@/lib/mock-transport";

// Must match jwtauth.DevBypassToken in the jwt-auth pack
// (internal/packs/jwt-auth/templates/dev_auth.go.tmpl). The backend only
// honors this when ENVIRONMENT=development, so it is inert in any other
// environment — but keep the strings in sync.
const DEV_BYPASS_TOKEN = "dev-bypass-do-not-use-in-prod";

const MOCK_USER: AuthUser = {
  id: "mock-user-1",
  email: "dev@localhost",
  name: "Dev User",
  roles: ["admin"],
};

export function createStubAuthProvider(): AuthProvider {
  const mode = import.meta.env.VITE_MOCK_API;
  const isMock = mode === "true";
  const isHybrid = mode === "hybrid";
  const listeners = new Set<(user: AuthUser | null) => void>();

  if (!isMock && !isHybrid) {
    console.warn(
      "[auth] Using stub auth provider. Implement AuthProvider and pass it to <AuthContextProvider> to enable real auth."
    );
  }

  // In hybrid mode the scenario decides whether we bypass auth. Default is
  // "no opinion" → null token → real login flow exercised. Scenarios that
  // explicitly opt in via `auth: "bypass"` get the sentinel, which the
  // backend's dev-auth middleware swaps for synthetic claims.
  const tokenForRequest = (): string | null => {
    if (isMock) return "mock-token-dev";
    if (isHybrid) {
      return activeScenario.auth === "bypass" ? DEV_BYPASS_TOKEN : null;
    }
    return null;
  };

  // In hybrid + bypass mode we still report an authenticated MOCK_USER so
  // route guards (`isAuthenticated()`) let the page render. The "real"
  // user identity for hybrid+required is the one the real provider
  // returns once login completes — this stub is the wrong layer for that
  // case; consumers should swap in a real provider for required scenarios.
  const treatAsAuthed = () =>
    isMock || (isHybrid && activeScenario.auth === "bypass");

  return {
    getToken: async () => tokenForRequest(),
    getUser: () => (treatAsAuthed() ? MOCK_USER : null),
    isAuthenticated: () => treatAsAuthed(),
    isLoading: () => false,
    login: async () => {
      console.warn("[auth] login() called on stub provider — implement a real AuthProvider");
    },
    logout: async () => {
      console.warn("[auth] logout() called on stub provider — implement a real AuthProvider");
    },
    onAuthStateChange: (cb) => {
      listeners.add(cb);
      return () => listeners.delete(cb);
    },
  };
}
