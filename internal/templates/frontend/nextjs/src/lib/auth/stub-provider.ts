import type { AuthProvider, AuthUser } from "./provider";

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

// Read the active scenario lazily — and only when running in the browser.
// On the server, mock-transport's URL inspection would no-op, but skipping
// the import entirely keeps SSR builds from pulling in scenario fixtures
// when neither mock mode is enabled.
function readActiveScenarioAuth(): "bypass" | "required" | undefined {
  if (typeof window === "undefined") return undefined;
  try {
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const { activeScenario } = require("@/lib/mock-transport");
    return activeScenario?.auth;
  } catch {
    return undefined;
  }
}

export function createStubAuthProvider(): AuthProvider {
  const mode = process.env.NEXT_PUBLIC_MOCK_API;
  const isMock = mode === "true";
  const isHybrid = mode === "hybrid";
  const listeners = new Set<(user: AuthUser | null) => void>();

  if (!isMock && !isHybrid) {
    console.warn(
      "[auth] Using stub auth provider. Implement AuthProvider and pass it to <AuthContext.Provider> to enable real auth."
    );
  }

  // In hybrid mode the scenario decides whether we bypass auth. Default is
  // "no opinion" → null token → real login flow exercised. Scenarios that
  // explicitly opt in via `auth: "bypass"` get the sentinel, which the
  // backend's dev-auth middleware swaps for synthetic claims.
  const tokenForRequest = (): string | null => {
    if (isMock) return "mock-token-dev";
    if (isHybrid) {
      return readActiveScenarioAuth() === "bypass" ? DEV_BYPASS_TOKEN : null;
    }
    return null;
  };

  const treatAsAuthed = () =>
    isMock || (isHybrid && readActiveScenarioAuth() === "bypass");

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
