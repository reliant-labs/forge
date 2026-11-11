---
name: observability/product-events/auth-events
description: The standard auth product events the console counts users from — auth_signed_up, auth_signed_in, auth_signed_out — emitted from Go (observe.TrackAuth, the auth middleware's daily resumed heartbeat), from the provider itself (forge/pkg/authhooks webhook handlers for Supabase, Clerk and Auth0; Supabase's pg_net trigger SQL), the browser (identifyUser, trackAuthEvent), Node, or any other language's OTel logger, and the edge visitor-day hash that links a server-side sign-up to its anonymous visit.
---

# Auth events: who signed up, signed in, signed out

Three product event names are reserved for a fixed auth vocabulary, the same
whatever the auth provider. The console counts users over a range from them
and the rest of the table: **active** = distinct users with any user-attributed
event (`obsquery.ActiveEvidence`: every row with a `UserId` except
`auth_signed_out` and the platform's `auth_accounts`; a sign-in or the auth
middleware's daily heartbeat is enough); **new** = distinct users with
`auth_signed_up` in the range, or, in an env that reported no sign-up in the
90 days before the range's end, the users first seen in the range
(`obsquery.NewUsersSource`); **returning** = active users who are not new.
Every count is distinct by `user.id`, so the same sign-in reported twice
(server and browser, two replicas, a retry) counts once. `forge obs users
<env> [--series 30] [--json]` prints them; the console's Users card and the
generated dashboards compile from the same two fragments.

| event | when | props |
|---|---|---|
| `auth_signed_up` | an account created by the person: self sign-up, or an accepted invitation (an admin's invite is not a sign-up until accepted) | `method`, `provider`, `verified` |
| `auth_signed_in` | a session established: `kind=credential` (a credential was just verified) or `kind=resumed` (an existing session seen; at most once per user per UTC day per emitter) | `kind`, `method` (credential only), `provider` |
| `auth_signed_out` | the person ended the session. No count depends on it | `provider` |

Bounded values (anything else is recorded as `other`): `method` is `password`,
`passkey`, `magic_link`, `social`, `invite`, `verify`, `reset`, `sso`;
`social_provider` (with `method=social`) is `google`, `github`, `microsoft`,
`apple`; `provider` is `forge`, `auth0`, `clerk`, `supabase`, `firebase`,
`cognito`, `jwt`, `custom`, or any name matching `^[a-z][a-z0-9_]{0,31}$`.

`user.id` is the provider's **opaque subject** (`auth0|abc`, a Clerk `user_…`,
a Zitadel id), never an email: every emitter refuses an id that is empty,
longer than 128 characters or contains `@`, and warns once.

**On forge's managed auth they come for free**, from `forge/pkg/devidp` (the
library every login broker calls), so an existing project gets them with a
forge upgrade and no edit: every completed sign-in records `auth_signed_in
{kind: credential, provider: forge}` with its method (password, passkey,
magic link, social with its `social_provider`), every self sign-up records
`auth_signed_up` with `verified`, and an accepted invitation records
`auth_signed_up {method: invite}` (an admin's invite records nothing). A new
project's broker also records `auth_signed_out` on logout and marks the
sign-in after a password reset `method: reset`
(`devidp.WithAuthMethod(ctx, observe.AuthMethodReset)`); its auth context calls
`identifyUser`. These are forge's automatic events: `product_events = False`
switches them off with the CRUD ones.

**Emit them from the server wherever a server sees the auth.** It is the
authoritative emitter: browser events are lost to ad blockers, consent banners
and the redirect right after a sign-in, and anyone can post one.

```go
// at your own sign-up / sign-in handlers (Auth0, Clerk, your own):
observe.TrackAuth(ctx, observe.AuthSignedUp, sub,
    observe.WithMethod(observe.AuthMethodPassword), observe.WithProvider("auth0"), observe.WithVerified(false))
observe.TrackAuth(ctx, observe.AuthSignedIn, sub,
    observe.WithMethod(observe.AuthMethodSocial), observe.WithSocialProvider("google"), observe.WithProvider("auth0"))
```

`TrackAuth` takes the id explicitly, because at sign-in the request is still
anonymous. Like `Track` it is never switched off by `product_events = False`:
an event your code records is your code's decision.

**Active users need no code at all behind forge's auth middleware.** When
`observe.Chain` has an `Auth` interceptor (every scaffolded server), the first
authenticated call of each user per UTC day records one `auth_signed_in
{kind: resumed}` from that process (`observe.AuthHeartbeatInterceptor`), for
any JWT provider. That counts a user with a 30-day session or a refresh token
as active, server-side, where no ad blocker reaches. API-key callers and
anonymous calls record nothing. `provider` is `forge` under managed identity,
else `jwt`; name yours with `observe.Deps{AuthProvider: "auth0"}`. It is one
of forge's automatic events (`product_events = False` switches it off), and
costs at most one log record per user per replica per day.

**Server events can carry the visitor's daily hash.** Reliant hosting's proxy
stamps `X-Reliant-Visitor-Day` (and `X-Reliant-Privacy` under GPC/DNT, then no
hash) on every request it forwards, and strips any copy a client sent once
control-plane #2517 is deployed. `observe.EdgeContextMiddleware` (on every
serverkit listener) and `observe.EdgeContextInterceptor` (in `observe.Chain`)
read them only when the process sets `FORGE_TRUSTED_EDGE=reliant`; then
`Track` and `TrackAuth` record `visitor.day`, linking a server-side sign-up to
the anonymous visit that produced it. forge does not render that variable for
hosted workloads yet, so until it does the headers are ignored and nothing
carries `visitor.day`. Never set it on a workload a client can reach without
that proxy: the headers would be the client's.

From the browser, `identifyUser` is the one call every app should make, wherever
it learns who is signed in (Clerk's `useUser`, Auth0's `useAuth0`, a session
refresh). It attaches the id to every span and replay (`setTelemetryUser`) and
sends one `auth_signed_in {kind: "resumed"}` per user per UTC day per browser,
which is what counts a user with a long-lived session as active.
`trackAuthEvent` is for an app whose auth never touches a server of its own:

```ts
import { identifyUser, trackAuthEvent } from "@reliantlabs/forge-web-runtime";

identifyUser(user?.id ?? null, { provider: "clerk" });   // on every "who is signed in" change
trackAuthEvent("signed_up", { userId: user.id, method: "magic_link", provider: "clerk" });
trackAuthEvent("signed_out", { userId: user.id, provider: "clerk" });   // before the session is gone
```

Browser auth events honour Global Privacy Control and Do Not Track (no auth
event is sent and no id is attached: the browser is capped at `aggregate`),
send nothing in the `off` tier, and store nothing below `full` (the daily
`resumed` marker is kept in memory there, so a reload can send one more; the
counts are distinct by user). A Node server (a Next.js route handler, an Express
backend) uses `trackAuthEvent` from `@reliantlabs/forge-web-runtime/node`,
which writes the same log record as `TrackAuth`.

**Any other language** writes the record itself with its own OTel logger: one
INFO log record with `event.name = product.auth_signed_in`, `user.id = <subject>`,
`app.client = backend`, and the props as `product.kind = credential`,
`product.method = password`, `product.provider = <name>`. It lands in the same
table.

## Using another auth provider: three ways to emit the events

An app on Supabase, Auth0, Clerk, Firebase or its own auth emits the same
three events, and nothing is declared in KCL or config: the provider is
self-reported, a `provider` prop on each event. Pick any of the three ways, or
combine them (the counts are distinct by user):

1. **The browser adapter** for the provider, one line next to its client:
   `@reliantlabs/forge-web-runtime/auth/<provider>` (Supabase, Clerk, Auth0,
   Firebase; `frontend-runtime/telemetry`). Joins sessions to users and counts
   them as active. Lossy and GPC/DNT-gated, like every browser event.
2. **A webhook handler** the provider posts to, from `forge/pkg/authhooks`:
   exact sign-ups and credential sign-ins from the issuer itself. Below.
3. **`observe.TrackAuth` yourself**, in the app's own sign-up and sign-in code
   (Go), or `trackAuthEvent` from `@reliantlabs/forge-web-runtime/node`.

The auth middleware's daily heartbeat counts any JWT provider's active users
on top: name the provider in code (`auth/external-provider`).

### The webhook handlers (`forge/pkg/authhooks`)

A plain `http.Handler` per provider. forge mounts nothing and reads no
configuration: the app reads the secret from wherever it keeps secrets and
mounts the handler on its own mux, outside its auth chain (the provider holds
no user token; the secret authenticates it). The path is a convention:

```go
mux.Handle(authhooks.Path("supabase"), authhooks.Supabase(authhooks.Options{Secret: secret}))
// authhooks.Clerk(...), authhooks.Auth0(...); authhooks.Handler(src, opts) for a Source of your own
```

Each handler verifies the delivery (anything unverified, or a body over
64 KiB, gets 401 with no detail), dedupes it (by session, user or log id, for
24 hours) and records it with `TrackAuth` under its provider's name. A
verified payload it does not map gets 204, so the provider never retries it
or turns the hook off. These are server events, so GPC/DNT and the analytics
tiers do not gate them; an app that wants none mounts no handler.

**Supabase.** Supabase's own Auth Hooks run synchronously and fail the sign-in
when they fail, so forge ships triggers instead: on `auth.users` (a real
account appears, or an anonymous one is upgraded → `auth_signed_up`) and on
`auth.sessions` (a new session → `auth_signed_in {kind: credential}`), posting
through `pg_net` after commit. Every function swallows its own errors, so a
down endpoint or a missing secret never blocks auth. The payload is the user's
opaque id, the sign-in provider's name, whether the account is verified and has
a password, and a dedupe key: never the email, IP or user agent. Anonymous users
send nothing. Set it up once per Supabase project:

```bash
forge auth hooks sql supabase > deploy/supabase/forge_auth_events.sql   # commit it
```

```sql
-- Supabase SQL editor, as postgres: the endpoint and the secret go in Vault, not in git.
select vault.create_secret('https://api.example.com/_forge/auth/hooks/supabase', 'forge_auth_hook_url');
select vault.create_secret('<the handler''s Options.Secret>', 'forge_auth_hook_secret');
-- then run deploy/supabase/forge_auth_events.sql (idempotent: re-run it to upgrade)
-- check:  select status_code, error_msg from net._http_response order by created desc limit 5;
-- remove: drop trigger forge_auth_user on auth.users; drop trigger forge_auth_session on auth.sessions;
```

Sign-outs are not reported from Supabase (deleting a session is also expiry);
the browser adapter reports them. `pg_net` does not retry a failed POST, so a
few events can be lost while the endpoint is down.

**Clerk.** Clerk Dashboard → Webhooks → add the endpoint URL, subscribe to
`user.created`, `session.created`, `session.ended` and `session.removed`, and
pass the endpoint's signing secret (`whsec_…`) as `Options.Secret`. Deliveries
are verified per Standard Webhooks (Svix's signature, a 5-minute timestamp
window).

**Auth0.** Auth0 Dashboard → Monitoring → Streams → Custom Webhook, with the
endpoint as the Payload URL and `Bearer <Options.Secret>` as the Authorization
Token; any content format. Log events `ss`, `s` and `slo` become signed_up,
signed_in and signed_out, with the method from the connection's strategy;
filter the stream to Signup, Login and Logout to save traffic.

**Firebase** has no push source without Identity Platform's blocking functions,
so it relies on the heartbeat and the browser adapter (`isNewUser` gives an
exact sign-up there).
