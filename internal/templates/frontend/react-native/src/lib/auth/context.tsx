import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import type { AuthProvider, AuthUser } from "./provider";
import { createStubAuthProvider } from "./stub-provider";

const defaultProvider = createStubAuthProvider();

interface AuthContextValue {
  user: AuthUser | null;
  isAuthenticated: boolean;
  isLoading: boolean;
  login: () => Promise<void>;
  logout: () => Promise<void>;
  getToken: () => Promise<string | null>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthContextProvider({
  provider,
  children,
}: {
  provider?: AuthProvider;
  children: ReactNode;
}) {
  const auth = provider ?? defaultProvider;
  const [user, setUser] = useState<AuthUser | null>(auth.getUser());

  useEffect(() => {
    return auth.onAuthStateChange(setUser);
  }, [auth]);

  const value: AuthContextValue = {
    user,
    isAuthenticated: auth.isAuthenticated(),
    isLoading: auth.isLoading(),
    login: () => auth.login(),
    logout: () => auth.logout(),
    getToken: () => auth.getToken(),
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used within an <AuthContextProvider>");
  }
  return ctx;
}
