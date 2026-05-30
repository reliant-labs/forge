import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import { QueryClientProvider } from "@tanstack/react-query";

import "./index.css";
import { router } from "./routes";
import { queryClient } from "@/lib/query-client";
import { EventBusProvider } from "@/lib/event-context";
import { AuthContextProvider, useAuth } from "@/lib/auth/context";
import { setAuthTokenGetter } from "@/lib/connect";
import { useEffect } from "react";

/**
 * AuthTokenBridge wires the auth provider's token getter to the Connect
 * transport. Lives outside the router so token retrieval is available before
 * the first matched route renders.
 */
function AuthTokenBridge() {
  const { getToken } = useAuth();
  useEffect(() => {
    setAuthTokenGetter(getToken);
  }, [getToken]);
  return null;
}

function App() {
  const isDev = import.meta.env.DEV;
  return (
    <QueryClientProvider client={queryClient}>
      <EventBusProvider devMode={isDev}>
        <AuthContextProvider>
          <AuthTokenBridge />
          <RouterProvider router={router} />
        </AuthContextProvider>
      </EventBusProvider>
    </QueryClientProvider>
  );
}

const rootEl = document.getElementById("root");
if (!rootEl) {
  throw new Error("Root element #root not found");
}

createRoot(rootEl).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
