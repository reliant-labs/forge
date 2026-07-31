"use client";

// yours: scaffolded once, never touched again — forge will not overwrite this file
//
// The App Router adapter for the sign-in screen. The screen itself lives in
// `src/lib/auth/auth-screens.tsx` and is shared byte-for-byte with the Vite
// SPA scaffold; this file supplies only the Next.js-specific navigation.
//
// Delete this route if your product is gated at the edge (a proxy that
// authenticates before the app loads) and never shows its own sign-in page.

import { useRouter } from "next/navigation";
import { useCallback } from "react";

import { SignInScreen } from "@/lib/auth/auth-screens";
import { useAuth } from "@/lib/auth/context";

export default function SignInPage() {
  const router = useRouter();
  const { login, isAuthenticated } = useAuth();

  // `replace`, not `push`: the sign-in page is a waypoint, and leaving it in
  // history means Back lands a signed-in user on a page that immediately
  // bounces them forward again.
  const navigate = useCallback(
    (path: string) => router.replace(path),
    [router],
  );

  return (
    <SignInScreen
      login={login}
      isAuthenticated={isAuthenticated}
      navigate={navigate}
    />
  );
}
