# auth-ui

Frontend pack: opinionated login / signup / session UI that pairs with one
of the backend auth packs.

| Backend pack    | `--config provider=…`     |
|-----------------|---------------------------|
| `jwt-auth`      | `jwt-auth` (default)      |
| `clerk`         | `clerk`                   |
| `firebase-auth` | `firebase-auth`           |

## Install

```bash
forge pack install <backend-pack>
forge pack install auth-ui --config provider=<provider>
```

If `--config provider=…` is omitted, the pack defaults to `jwt-auth`.

The pack installs into every frontend declared in `forge.yaml` at
`src/components/auth/`. See the rendered `README.md` in that folder for
wiring instructions.

## What you get

- `LoginForm` / `SignupForm` — backed by `react-hook-form` + `zod`.
- `SessionNav` — header avatar + dropdown with sign-out and an optional
  tenant switcher.
- `DevModeBanner` — visible warning when the backend pack is running with
  `dev_mode: true`.
- `auth-store.ts` — Zustand store: `{user, session, isLoading,
  isAuthenticated}`.
- `src/app/auth/callback/page.tsx` — OAuth callback page. Provider-aware
  (see below).

## Provider variants

Templates branch on `{{ .PackConfig.provider }}`:

- **jwt-auth** — POSTs to `/auth/login`, expects `{token, user}` in JSON,
  persists to localStorage. Default.
- **clerk** — wraps `@clerk/nextjs` `<SignIn>`, `<SignUp>`, `<UserButton>`.
- **firebase-auth** — `firebase/auth` SDK with email + Google providers.

The right SDK is pulled in via the manifest's
`provider_npm_dependencies` map at install time, so a `provider=jwt-auth`
install does not install `@clerk/nextjs` or `firebase`.

## OAuth callback

OAuth redirect flows land on a typed callback page at
`src/app/auth/callback/page.tsx`. What gets generated depends on the
provider:

- **jwt-auth** — full code-exchange page. POSTs `{code, state}` to
  `jwt_oauth_exchange_path` (default `/auth/oauth/exchange`), expects
  `{token, user}` back, calls `useAuthStore.setSession(...)` and
  `persistSession(...)`, then redirects to a same-origin `returnTo` or
  `/`.
- **firebase-auth** — calls `getRedirectResult()` to consume the OAuth
  round-trip the Firebase SDK started via `signInWithRedirect()`. Pushes
  the user into the auth-store and navigates. If your app uses
  `signInWithPopup()` exclusively, you can delete this file — popup auth
  never hits a callback URL.
- **clerk** — thin wrapper around Clerk's
  `<AuthenticateWithRedirectCallback />`. Clerk owns the code-exchange,
  session storage, and error UX; the wrapper is just our shell. Delete
  the file if you only use Clerk's modal sign-in.

### Load-bearing pieces

All four show up in the rendered file as comments; they are also part of
the canonical pattern documented in the `frontend/patterns` skill:

1. **`callbackSchema`** — the only thing in the file that touches raw
   search params, parsed via `useTypedSearchParams` from
   `@/lib/search-schemas` (provided by the Next.js scaffold).
2. **`assertSafeReturnTo`** — open-redirect guard. Refuses any
   `?returnTo=…` that isn't a same-origin relative path.
3. **`exchanged.current`** — `useRef` guard that makes the code-exchange
   idempotent under React strict-mode double-mount.
4. **Error surface** — renders a "Back to sign in" button rather than
   silently bouncing to `/` with no session.

### Disabling

Set `--config oauth_callback=false` at install time (or edit
`auth_ui.oauth_callback` in forge.yaml) to skip the file. Note: the file
is `overwrite: once`, so on a re-install with `oauth_callback=true` it
will be created only if absent — existing customizations are preserved.

### Backend dependency

The jwt-auth variant assumes the backend exposes an OAuth exchange
endpoint at `jwt_oauth_exchange_path` (default `/auth/oauth/exchange`).
The current `jwt-auth` backend pack does not ship this endpoint —
implementing the server-side OAuth code-exchange is left to the
project. Override the prop on the page if you mount the exchange under a
different path:

```tsx
<OAuthCallbackPage exchangePath="/api/auth/oauth/exchange" />
```
