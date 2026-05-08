import type { AuthProvider, AuthUser } from "./provider";

const MOCK_USER: AuthUser = {
  id: "mock-user-1",
  email: "dev@localhost",
  name: "Dev User",
  roles: ["admin"],
};

export function createStubAuthProvider(): AuthProvider {
  const isMock = process.env.NEXT_PUBLIC_MOCK_API === "true";
  const listeners = new Set<(user: AuthUser | null) => void>();

  if (!isMock) {
    console.warn(
      "[auth] Using stub auth provider. Implement AuthProvider and pass it to <AuthContext.Provider> to enable real auth."
    );
  }

  return {
    getToken: async () => (isMock ? "mock-token-dev" : null),
    getUser: () => (isMock ? MOCK_USER : null),
    isAuthenticated: () => isMock,
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
