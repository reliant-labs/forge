import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import { QueryClientProvider } from "@tanstack/react-query";

import { installDevLogging } from "@reliantlabs/forge-web-runtime";

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

// Mirror console output and uncaught errors to the dev server, which writes
// them to .forge/logs/<env>/frontend_<name>.log — so a browser-side failure is
// visible on disk instead of only in devtools. Delete this block and the
// devLogPlugin() entry in vite.config.ts to opt out.
//
// The `if` is what keeps this out of a production bundle, and it has to be a
// statement-level guard rather than an argument. `import.meta.env.DEV` folds
// to `false` at build time; wrapping the CALL lets the minifier drop the whole
// statement (and then tree-shake the import), whereas passing it as an
// argument only folded the argument — leaving a live `installDevLogging({})`
// in the production bundle. Verified against a real `vite build`.
if (import.meta.env.DEV) {
  installDevLogging({ dev: true });
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
