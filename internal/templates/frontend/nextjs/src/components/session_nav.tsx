"use client";

// yours: scaffolded once, never touched again — forge will not overwrite this file
// SessionNav; owned scaffold. See `forge skill load auth`.
/**
 * SessionNav — who is signed in, and a way out. Drop into your header or the
 * sidebar footer:
 *
 *   import { SessionNav } from "@/components/session_nav";
 *   <header><SessionNav /></header>
 *
 * Reads the app's AuthProvider through `useAuth()` — the SAME seam
 * `providers.tsx` feeds the Connect transport's bearer token from. There is
 * no second session store and no auth endpoint of its own: what this renders
 * and what your RPCs send are the same identity by construction.
 *
 * Which provider sits behind that seam is decided by CONFIGURATION, in
 * `selectAuthProvider()` (src/lib/auth/oidc-provider.ts):
 *
 *   - with an OIDC issuer + client id configured, the real
 *     authorization-code + PKCE provider, which redirects to your IdP;
 *   - otherwise (and always in pure-mock mode) `createSessionAuthProvider()`
 *     — a fixture session while the mock transport answers, and signed out
 *     with a first-use warning against a real backend.
 *
 * Nothing in this file changes when you configure an IdP: it reads the same
 * `useAuth()` either way.
 *
 * Built from forge's base UI primitives (`Avatar`, `Button`).
 */
import { useEffect, useRef, useState } from "react";

import Avatar from "@/components/ui/avatar";
import Button from "@/components/ui/button";
import { useAuth } from "@/lib/auth/context";

export function SessionNav() {
  const { user, isAuthenticated, isLoading, login, logout } = useAuth();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  // Dismissal, both ways in:
  //
  //   - Outside click, decided by CONTAINMENT rather than by a
  //     stopPropagation guard on the wrapper. A wrapper that swallows every
  //     click is a <div> pressed into service as a control — no role, no tab
  //     stop, no keyboard path — and it also eats clicks that anything above
  //     this component legitimately wants to see.
  //   - Escape, with focus handed back to the trigger. Click-away alone
  //     leaves a keyboard user with a menu they can open and not close, and
  //     closing while focus sits on "Sign out" would otherwise drop focus to
  //     <body> and lose their place on the page.
  useEffect(() => {
    if (!open) return;
    function onPointerDown(event: MouseEvent) {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      setOpen(false);
      rootRef.current?.querySelector("button")?.focus();
    }
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  const handleSignOut = async () => {
    setOpen(false);
    await logout();
  };

  if (isLoading) {
    return (
      <div className="h-8 w-24 animate-pulse rounded-md bg-surface-muted" />
    );
  }

  // Signed out: hand the click to the provider's own login(), whatever that
  // means for this app — a redirect to an IdP, or the mock's local session
  // flip. Calling the seam rather than linking to `/auth/sign-in` keeps this
  // button working in both modes and needs no route at all; the sign-in page
  // forge does emit exists for the OTHER entry points (a deep link into a
  // gated route, a bookmark), where there is no button to click.
  if (!isAuthenticated || !user) {
    return (
      <Button variant="outline" size="sm" onClick={() => void login()}>
        Sign in
      </Button>
    );
  }

  return (
    <div ref={rootRef} className="relative">
      <Button
        variant="outline"
        size="sm"
        onClick={() => setOpen((v) => !v)}
        // Disclosure, not a menu: the panel below is a text block plus one
        // button, so aria-expanded is the whole contract. aria-haspopup
        // would promise a role="menu" that is not there.
        aria-expanded={open}
        className="gap-2"
      >
        <Avatar size="xs" name={user.name ?? user.email ?? undefined} />
        <span className="max-w-[12rem] truncate">
          {user.name ?? user.email ?? "User"}
        </span>
      </Button>

      {open ? (
        <div className="absolute right-0 z-50 mt-1 w-56 rounded-md border border-border bg-surface py-1 shadow-lg">
          <div className="border-b border-border px-3 py-2 text-xs text-ink-subtle">
            Signed in as
            <div className="truncate text-sm font-medium text-ink">
              {user.email ?? user.id}
            </div>
          </div>

          <button
            type="button"
            onClick={handleSignOut}
            className="block w-full px-3 py-1.5 text-left text-sm text-ink-muted transition-colors hover:bg-surface-muted hover:text-ink"
          >
            Sign out
          </button>
        </div>
      ) : null}
    </div>
  );
}
