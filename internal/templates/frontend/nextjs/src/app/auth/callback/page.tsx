"use client";

// yours: scaffolded once, never touched again — forge will not overwrite this file
//
// The App Router adapter for the OAuth callback. Register this URL —
// `<origin>/auth/callback` — as an allowed redirect URI with your identity
// provider.
//
// The screen itself lives in `src/lib/auth/auth-screens.tsx`, shared with the
// Vite SPA scaffold.
//
// ── Why window.location.search and not useSearchParams() ──────────────
//
// `useSearchParams()` opts a route into client-side rendering in a way that
// makes `next build` fail unless the component is wrapped in <Suspense>, and
// under `output: "export"` it de-opts the whole page. This route is already
// `"use client"` and does its work in an effect after mount, where
// `window.location.search` is available, exact, and needs no boundary. There
// is nothing useSearchParams would add — the params are read once and never
// re-subscribed to, because a callback URL's code is single-use.

import { useRouter } from "next/navigation";
import { useCallback, useEffect, useState } from "react";

import { AuthCallbackScreen } from "@/lib/auth/auth-screens";

export default function AuthCallbackPage() {
  const router = useRouter();
  // Read after mount: on the server there is no location, and pre-rendering
  // this route must not depend on one.
  const [search, setSearch] = useState<string | null>(null);
  useEffect(() => {
    setSearch(window.location.search);
  }, []);

  // `replace`, so Back never returns to a URL holding a spent code.
  const navigate = useCallback(
    (path: string) => router.replace(path),
    [router],
  );

  if (search === null) {
    return null;
  }
  return <AuthCallbackScreen search={search} navigate={navigate} />;
}
