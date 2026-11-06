---
name: migrations/v1.1.0/identity-v2
description: Teach a login broker scaffolded before v1.1.0 the identity-v2 sign-in methods — passkeys, magic links, Google/GitHub, invitations, address verification and open or domain sign-up — by reading the delivered idp_login_policy and mounting devidp.SignInRoutes. Without it a forge.ManagedIdentity that declares methods, social or a non-invite signup is converged and silently ignored by the sign-in screen. Use when internal/app/login_broker.go does not call devidp.ParseLoginPolicy, or `forge lint` reports forgeconv-identity-methods.
version: v1.1.0
detection: test -f internal/app/login_broker.go && ! grep -qs 'ParseLoginPolicy' internal/app/login_broker.go && exit 0; exit 1
---

# Identity v2 in an existing login broker

**Use when** `internal/app/login_broker.go` exists and does not call
`devidp.ParseLoginPolicy`, or `forge lint` reports `forgeconv-identity-methods`.

## What changed

`forge.ManagedIdentity` gained `signup` (`invite` | `open` | `domain`),
`methods` (`password`, `passkey`, `magic_link`), `passkey_rp_id`, `social` and
`owners`. Whoever converges the identity delivers the result to the app as one
JSON document, `idp_login_policy` (`IDP_LOGIN_POLICY`). A broker scaffolded
since v1.1.0 parses it and mounts the matching routes; their handlers are
`devidp.SignInRoutes` in forge, so the broker only decides which are served.

`login_broker.go` is yours, so forge does not rewrite it.

**An app that does nothing keeps working:** with no document the broker serves
the legacy policy, today's behaviour. Only a declaration that turns something
on needs the edits, and `forge lint` names each one it would otherwise ignore.

The steps below copy from a scratch project of the same shape
(`forge project new` in a temporary directory), which has the current broker
and pages.

## 1. Declare the config field

In the config proto that declares `idp_broker_token` (usually
`proto/config/v1/config.proto`), add a field with an unused number:

```protobuf
  string idp_login_policy = 49 [(forge.v1.config) = {
    env_var: "IDP_LOGIN_POLICY"
    flag: "idp-login-policy"
    sensitive: true
    description: "The LOGIN_POLICY document the login broker enforces. Delivered by the identity converge; empty keeps the legacy policy."
  }];
```

Run `forge generate`. The identity fills it; no `config.k` line names it.

## 2. Edit `internal/app/login_broker.go`

1. **Route constants** — after the `const (...)` block of `LoginPath` etc.:

   ```go
   const (
   	PasskeyBeginPath          = devidp.PasskeyBeginPath
   	PasskeyFinishPath         = devidp.PasskeyFinishPath
   	PasskeyRegisterBeginPath  = devidp.PasskeyRegisterBeginPath
   	PasskeyRegisterFinishPath = devidp.PasskeyRegisterFinishPath
   	PasskeysPath              = devidp.PasskeysPath
   	MagicRequestPath          = devidp.MagicRequestPath
   	MagicConsumePath          = devidp.MagicConsumePath
   	SocialStartPath           = devidp.SocialStartPath
   	SocialCallbackPath        = devidp.SocialCallbackPath
   	InviteAcceptPath          = devidp.InviteAcceptPath
   	VerifyPath                = devidp.VerifyPath
   	VerifyResendPath          = devidp.VerifyResendPath
   )
   ```

2. **Parse the policy** — first lines of `RegisterLoginBroker`:

   ```go
   policy, policyErr := devidp.ParseLoginPolicy(cfg.IdpLoginPolicy)
   if policyErr != nil {
   	logger.Error("idp_login_policy is unusable; sign-up is OFF and every session needs a verified address until it is fixed", "error", policyErr)
   }
   ```

3. **The struct** — add to `loginBroker`, and rename the existing
   `policy policyCache` field to `passwordRules policyCache` (and its uses in
   `passwordPolicy`):

   ```go
   	legacySelfSignup bool
   	policy           devidp.LoginPolicy
   	methods          *devidp.SignInRoutes
   ```

4. **Construction** — in the `&loginBroker{...}` literal: rename
   `selfSignup:` to `legacySelfSignup:`, add `policy: policy,`, and add
   `RequireVerifiedEmail: policy.RequireVerifiedEmail || policyErr != nil,` to the
   `devidp.FlowConfig`. After the literal, before the first `mux.Handle`:

   ```go
   broker.selfSignup = broker.legacySelfSignup
   if policyErr != nil {
   	broker.selfSignup = false // an unusable policy fails closed
   }
   if policy.Present {
   	broker.selfSignup = policy.RegisterServed()
   }
   broker.mountSignInMethods(mux, cfg)
   ```

5. **`mountSignInMethods`** — copy the method of that name from the scratch
   project's `internal/app/login_broker.go`. It builds
   `devidp.NewSignInRoutes` from the broker's client, flow, policy and
   `appBase`, with `SignedIn` calling `b.setSession` and `SessionToken`
   reading `SessionCookieName`, and mounts each route inside
   `if b.policy.Offers(...)` / `if len(b.policy.Social) > 0`. Set
   `ProjectID: cfg.JwtAudience` if your config declares `jwt_audience`
   (the project a `signup_role` is granted on).

6. **Options** — `handleOptions` becomes:

   ```go
   opts := devidp.OptionsFor(b.policy, b.selfSignup)
   opts.SelfSignup = b.selfSignup
   writeAuthJSON(w, http.StatusOK, opts)
   ```

   and delete the `authOptions` type.

7. **Sign-up** — first lines of `handleRegister`:

   ```go
   if b.methods != nil {
   	b.methods.HandleRegister(w, r)
   	return
   }
   ```

8. **Unverified address** — in `handleLogin`, before `if err != nil {`:

   ```go
   if errors.Is(err, devidp.ErrEmailUnverified) {
   	writeAuthJSON(w, http.StatusForbidden, map[string]string{
   		"error": "Confirm your email address first: use the link we sent, or ask for a new one.",
   		"code":  "email_unverified",
   	})
   	return
   }
   ```

9. **Password rules** — first lines of `passwordPolicy` (a hosted instance's
   Settings API is not routed; the document carries the rules):

   ```go
   if p, ok := b.policy.PasswordPolicy(); ok {
   	return p, nil
   }
   ```

   If you carry a text-matching workaround for the 404 from before v1.1.0,
   drop it: `errors.Is(err, devidp.ErrPasswordPolicyUnavailable)` is the check.

## 3. Add the frontend pages

From the scratch project's frontend (same kind), copy:

- `src/lib/auth/native-login.ts` (or merge: `AuthOptions` grew `signup`,
  `methods`, `social`, `mail`; `AuthError` grew `code`; `signUp` may answer
  `VerifySent`; the passkey, magic-link, social, invite and verify calls are
  appended) and `src/lib/auth/use-auth-options.ts`;
- `src/components/{sign-in-methods,magic-link-form,verify-email-form,accept-invite-form,social-callback,passkeys-panel}.tsx`;
- Next.js: `src/app/auth/{magic,invite,verify,social/callback}/page.tsx`.
  Vite: the `magicRoute`, `inviteRoute`, `verifyRoute` and
  `socialCallbackRoute` blocks of `src/routes.tsx`, and their entries in
  `addChildren`;
- `src/lib/auth/route-guard.tsx`'s `EMAIL_LINK_PATHS` (the pages are public);
- in the sign-in screen: render `<SignInMethods options={options} …/>`, hide
  the password field when `options.methods` lacks `"password"`, show
  `created.message` when `isVerifySent(created)`, and `<UnverifiedNotice>`
  on `err.code === "email_unverified"`.

## 4. Check

`forge lint` reports no `forgeconv-identity-methods`. `forge env up dev`, then
`GET /auth/options` lists the declared methods.
