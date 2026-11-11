---
name: observability/product-events
description: Product events — the contract (one INFO log record, event.name product.<name>), emitting from Go (observe.Track) and the browser (track, setTelemetryUser), the browser's cookieless analytics tiers (off < aggregate < identified < full, identified by default; grantAnalyticsConsent, optOutOfAnalytics, GPC/DNT), automatic page_viewed with its referrer/utm/click_id props, flag exposures (trackExposure, forgeExposureHook), the reserved event-name prefixes, the standard auth events the console counts users from (observe.TrackAuth, trackAuthEvent, identifyUser; the record for any other language), the automatic <entity>_created|_updated|_deleted events generated CRUD emits, their off switch (signals.product_events) and log-ingest cost, retention, and sql/funnel panels over the hosted product_events table.
---

# Product events

A **product event** is a named thing a user did or that happened to them
(`project_created`, `subscription_started`). On the hosted platform they land in
the per-environment ClickHouse `product_events` table, which keeps them far
longer than logs or traces. Dashboards read it with `sql` and `funnel` panels.

## The contract

An event is one OTel **log record** at INFO (never a span: server traces are
sampled) carrying these attributes. This is the seam between the emitters and the
table; all of them follow it.

| attribute | meaning |
|---|---|
| `event.name` | `product.<snake_name>`; required, and the only thing the table selects on |
| `user.id` | the authenticated user id (the auth subject); empty when anonymous |
| `visitor.id` | the anonymous visitor id, when known |
| `session.id` | the client session id, when known |
| `app.client` | `backend`, `web`, `electron` or `landing` |
| `app.version` | the emitting app's version |
| `product.<key>` | one property per key; stored in `Props` with the `product.` prefix stripped |

Bounds, enforced by both emitters and again by the table's view: at most 32
properties, each value at most 256 characters (longer is truncated). Product
events are not a way to keep logs longer. Browser events keep their existing
path (HyperDX `custom-action` spans into the same table); the web runtime
stamps `app.client` on them, and `visitor.id` only in the consented `full`
analytics tier (below).

## Emitting from Go

```go
observe.Track(ctx, "project_created", slog.String("template", "go"), slog.Int("repos", 2))
```

`observe.Track(ctx, name, attrs...)` writes the record through the context's
logger (`observe.FromContext`), sets `app.client="backend"` and `app.version`
(from `observe.Config.ServiceVersion`, or `observe.SetAppVersion`), and takes
`user.id` from the claims forge's auth interceptor put in `ctx`: a handler
needs no extra step, a background job has no user and so emits an anonymous
event. A name that is not snake_case (`^[a-z][a-z0-9_]{0,63}$`) is dropped with
a warning. Property values become strings; keep them low-cardinality and
never put secrets or personal data in them. A `slog.Group` flattens to
`group.key`.

## Automatic events from generated CRUD

Every generated Create, Update and Delete rpc records a product event once its
write lands: `<entity>_created`, `<entity>_updated`, `<entity>_deleted`, with
`<entity>` the entity's snake_case name (`OrderItem` is `order_item_created`).
It lives in `forge/pkg/crud`, which every generated CRUD shim delegates to, so a
project gets it from a forge upgrade alone: no regenerate, no edit to the
handler files it owns. The event carries the caller's `user.id` and nothing
else: no primary key and no field values (ids are unbounded cardinality and
values may be personal data). A refused or failed write records nothing. A
write inside a transaction of your own records its event when the statement
succeeds, before your transaction commits.

They are **on by default**. Each one is one INFO log record, which the hosted
platform bills as log ingest like any other log line (a few hundred bytes per
write; `product_events` itself is derived from it and not billed). To switch
them off for an environment:

```kcl
observability = forge.Observability {
    signals = forge.ObservabilitySignals {product_events = False}
}
```

That renders `FORGE_PRODUCT_EVENTS=off` on every workload (set it yourself on a
workload to decide per workload; `observe.AutoProductEvents()` reports the
result). Events your own code emits with `observe.Track` are never switched off.

## Emitting from the browser

```ts
import { track, setTelemetryUser } from "@reliantlabs/forge-web-runtime";

track("onboarding_completed", { plan: "pro" });   // after initBrowserTelemetry
setTelemetryUser(session.user.id);                // the opaque subject, never an email
```

`initBrowserTelemetry({ appClient })` stamps `app.client` (`electron` inside
Electron, else `web`; pass `landing` from a marketing site) on every event.
Everything is behind one gate: nothing is sent until `initBrowserTelemetry`
runs, and nothing after `disableTelemetry()`.

### Analytics tiers: cookieless by default

What the browser sends and keeps is decided by its **tier**, ordered `off` <
`aggregate` < `identified` < `full`: the lowest of the app's `analytics` mode
(the runtime config's `ANALYTICS_MODE`; absent means `identified`), the
consent the app signalled, and the browser's own privacy signals:

| tier | when | stored on the device | ids on browser events |
|---|---|---|---|
| `off` | `analytics: "off"`, or the person called `optOutOfAnalytics()` | only the opt-out flag (`forge.analytics_opt_out`) | none: no product events, no user id (errors and traces still flow) |
| `aggregate` | mode `aggregate`, and always under Global Privacy Control / Do Not Track | **nothing**: no cookie, no localStorage, no sessionStorage (the RUM session id is kept in memory) | none, even when signed in; the platform's ingest edge counts visitors with a daily-salted hash it forgets |
| `identified` | **the default** mode, once a user is named (`identifyUser` / `setTelemetryUser`, or the generated session identity), no GPC/DNT | **nothing**, as aggregate | the opaque user id |
| `full` | `analytics: "full"` **and** `grantAnalyticsConsent()` | `forge.visitor_id`, `forge.attribution`, the `__rum_sid` session cookie | user id + persistent `visitor.id`; `user_identified`; replay when configured |

```ts
import {
  grantAnalyticsConsent, revokeAnalyticsConsent, optOutOfAnalytics, analyticsTier,
} from "@reliantlabs/forge-web-runtime";

// analytics: "full" (runtime config ANALYTICS_MODE = "full"), then, from your
// consent manager, on every page load once the person said yes:
grantAnalyticsConsent();   // the session moves to the cookie, visitor.id starts
revokeAnalyticsConsent();  // back to identified; what full stored is erased
optOutOfAnalytics();       // the person objects to analytics at all: tier off, remembered
```

The runtime never remembers consent; your consent manager does, and calls
`grantAnalyticsConsent()` each page load. Consent given mid-page keeps the
running session (it moves from memory to the cookie) and starts replay on the
next load. A start in a mode that can never reach `full` erases the ids an
earlier page load stored. GPC / DNT cap any mode at `aggregate` **with no user
id**: such a browser is counted, never identified and never stitched. Server
events (CRUD, `Track`, `TrackAuth`) are not gated by any of this.

In the `full` tier only, the runtime reads `rid` and `utm_*` from the landing
URL into storage, and the first `setTelemetryUser(id)` for a user emits one
`user_identified {visitor_id, utm_*}`, which is how a funnel joins a visitor
to a user across days. A marketing site wraps its outbound links in
`withAttribution(url)` (a no-op below `full`) so the app sees the `rid` and
utm values. `initBrowserTelemetry({ attribution })` and `enableAttribution()`
are deprecated spellings that keep working: passing `attribution` at all means
mode `full`, `true` also grants consent, and `enableAttribution()` raises the
default mode to `full` and grants.

### Page views, with where the visit came from

Every generated web frontend (Next.js, Vite) records `page_viewed` for each
route the user lands on, in every tier above `off`:

| prop | value |
|---|---|
| `route` | the pathname only: basePath stripped, id-looking segments as `:id` (`/orders/:id`) |
| `entry` | `"true"` on the first view of a page load (absent after) |
| `referrer_host`, `referrer_path` | the referrer's host and scrubbed path, on the entry view only, and only when it is another site (not this host, its `www.` twin or a `pageViews.siteHosts` entry) |
| `utm_source` … `utm_content`, `ref` | from the URL's query, only when present, at most 128 characters; `ref` is `?ref=` or `?source=`; a value containing `@` is dropped |
| `click_id` | which ad network's click id the URL carries: `google` (gclid, gbraid, wbraid, dclid), `microsoft`, `meta`, `tiktok`, `linkedin` or `other`. **Never the id itself**: it is a persistent cross-site identifier |

So a campaign reaches the console with nothing stored. Country, device,
browser and OS are derived at the ingest edge, not sent by the page. With
native sign-in the frontend also identifies the signed-in user by itself: the
runtime reads the `subject` of `/auth/session` (through `src/lib/api-base.ts`,
as sign-in does), the same id `observe.Track` writes as `user.id`, so browser
and backend events join on `UserId`. An environment turns either off per
frontend with `telemetry = forge.BrowserTelemetry {page_views = False}` /
`{identify_user = False}` (rendered as `PAGE_VIEWS` / `IDENTIFY_USER = "false"`
in its runtime config). How both work: `frontend-runtime/telemetry`.

### Flag exposures

`trackExposure(flag, variant, { provider })` records one
`flag_evaluated {flag, variant, provider}` per person, flag and variant per UTC
day (deduped in memory). An app on any OpenFeature provider registers the hook
once and needs no other code:

```ts
import { OpenFeature } from "@openfeature/web-sdk";
import { forgeExposureHook } from "@reliantlabs/forge-web-runtime";

OpenFeature.addHooks(forgeExposureHook);
```

A failed evaluation is no exposure; an object-valued flag with no variant name
is recorded as `object`, never its contents.

### Reserved event names

The prefixes `auth_`, `page_`, `flag_` and `web_` are forge's own events.
`track()` still sends an app event that uses one, and warns once per page load:
rename it so the console's built-in panels stay exact.

## Auth events: who signed up, signed in, signed out

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

## Retention

`forge.Observability.retention.product_events_days` (1 to 400, default 90) is
how long the environment asks the platform to keep product events. The platform
may grant less (a plan can cap it); a new value applies to rows written after
it, not to rows already stored.

## Panels over `product_events`

Columns: `Timestamp`, `ServiceName`, `EnvId`, `SessionId`, `UserId`, `EventName`
(the name without the `product.` prefix) and `Props` (a map, `Props['plan']`).
`DESCRIBE product_events` lists what else the table keeps. A `sql` panel binds
the dashboard scope (`{from:DateTime64(9)}`, `{to:DateTime64(9)}`,
`{step:UInt32}`, `{env_ids:Array(String)}`, `{envs:Array(String)}`,
`{services:Array(String)}`); a first `DateTime` column draws a time series. The
table's environment column is `EnvId`, so scope by environment with
`(empty({env_ids:Array(String)}) OR has({env_ids:Array(String)}, EnvId))`, as
the generated Product usage panels do.

Daily active users:

```json
{
  "id": "dau",
  "title": "Daily active users",
  "kind": "sql",
  "sql": "SELECT toStartOfDay(Timestamp) AS day, uniqExact(UserId) AS users FROM product_events WHERE Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)} AND UserId != '' GROUP BY day ORDER BY day"
}
```

Events by name, and a property breakdown (swap in a property you emit):

```json
{
  "id": "events-by-name",
  "title": "Events by name",
  "kind": "sql",
  "sql": "SELECT EventName, count() AS events, uniqExact(UserId) AS users FROM product_events WHERE Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)} GROUP BY EventName ORDER BY events DESC LIMIT 50"
}
```

```json
{
  "id": "subscriptions-by-plan",
  "title": "Subscriptions started by plan",
  "kind": "sql",
  "sql": "SELECT toStartOfDay(Timestamp) AS day, Props['plan'] AS plan, count() AS started FROM product_events WHERE EventName = 'subscription_started' AND Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)} GROUP BY day, plan ORDER BY day"
}
```

A funnel counted per user:

```json
{
  "id": "activation",
  "title": "Activation funnel",
  "kind": "funnel",
  "signal": "traces",
  "funnel": { "steps": ["user_identified", "project_created", "onboarding_completed"], "window": "7d", "by": "user" }
}
```

Put them in a dashboard file you own (`deploy/observability/dashboards/<name>.json`,
see `observability/dashboards`, Overriding); `forge obs dashboards check` validates it.
