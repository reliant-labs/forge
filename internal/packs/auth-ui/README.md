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

## Provider variants

Templates branch on `{{ .PackConfig.provider }}`:

- **jwt-auth** — POSTs to `/auth/login`, expects `{token, user}` in JSON,
  persists to localStorage. Default.
- **clerk** — wraps `@clerk/nextjs` `<SignIn>`, `<SignUp>`, `<UserButton>`.
- **firebase-auth** — `firebase/auth` SDK with email + Google providers.

The right SDK is pulled in via the manifest's
`provider_npm_dependencies` map at install time, so a `provider=jwt-auth`
install does not install `@clerk/nextjs` or `firebase`.
