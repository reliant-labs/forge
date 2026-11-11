---
name: v1.4.0
description: Upgrade a project to forge v1.4.0. (1) Session replay is a per-frontend, per-environment switch, forge.BrowserTelemetry {replay = True}, which renders the frontend's OTEL_REPLAY and opts the env in to replay ingestion; the env-wide Observability.signals.replay is deprecated (it still opts in) and an explicit runtime_config OTEL_REPLAY outranks the new switch. (2) Browser analytics are cookieless by default: the web runtime stores nothing on the device (no visitor id, no session cookie, no stored campaign) and runs replay only with consent; ANALYTICS_MODE = "full" plus grantAnalyticsConsent() restores the persistent visitor id, multi-day attribution and replay. Use when a deploy/kcl env declares ObservabilitySignals {replay = …} or a frontend's runtime_config OTEL_REPLAY, or when a web frontend has browser telemetry (src/lib/otel_gen.ts, or code calling initBrowserTelemetry / enableAttribution / withAttribution).
version: v1.4.0
detection: grep -rqsE 'ObservabilitySignals *\{[^}]*replay *=|OTEL_REPLAY *=' --include='*.k' deploy/kcl && exit 0; ls frontends/*/src/lib/otel_gen.ts >/dev/null 2>&1 && exit 0; grep -rqsE --exclude-dir=node_modules --include='*.ts' --include='*.tsx' 'initBrowserTelemetry|enableAttribution|withAttribution' . && exit 0; exit 1
---

# Upgrading to v1.4.0

## 1. Session replay moves onto the frontend

**Use when** an env's KCL (`deploy/kcl/<env>/main.k`) declares
`observability = forge.Observability {signals = forge.ObservabilitySignals {replay = …}}`
or a frontend's `runtime_config = {OTEL_REPLAY = …}`. Environments scaffolded
with the `_session_replay` switch before v1.4.0 declare both.

### What changed

Replay is a choice about one frontend in one environment, so it is declared
there:

```kcl
_web_frontend = forge.Frontend {
    name = "web"
    # …
    telemetry = forge.BrowserTelemetry {replay = _session_replay}
}
```

`telemetry.replay = True` renders that frontend's `OTEL_REPLAY = "true"` into
its runtime config (a hosted site's StaticSite spec too, so the platform reads
it live) and opts the env's observability policy in to replay, so the platform
keeps the events. Off renders no key: unset is off. A hosted frontend with it on
needs the env to declare `observability`. With observability or browser
telemetry off, nothing records, whatever is declared.

Nothing breaks: `signals.replay` still opts the env in (it is deprecated, and
switches no frontend on) and a `runtime_config` `OTEL_REPLAY` still reaches the
browser. Upgrading changes no env's behavior and rewrites none of your KCL.

### What to do

In each env that uses `_session_replay` (or replay at all):

1. On each web frontend, set `telemetry = forge.BrowserTelemetry {replay = _session_replay}`
   (in the dev env scaffold: `replay = _observability and _session_replay`).
   An existing `telemetry` block takes the `replay` field instead.
2. Delete the frontend's `runtime_config` `OTEL_REPLAY` entry. **It outranks
   `telemetry.replay`**: an explicit `"false"` left behind keeps that frontend
   off with the switch on.
3. Drop `signals = forge.ObservabilitySignals {replay = _session_replay}` from
   `observability` (leave `observability = forge.Observability {}` if it was
   the only field). The frontend's switch now opts the env in.

Then `forge env render <env>` and check the frontend's `runtime_config` (or
`runtime_config_spec` when hosted) carries `OTEL_REPLAY = "true"` exactly when
`_session_replay` is True, and `observability.policy.opt_in` lists `replay`.

## 2. Browser analytics are cookieless by default

**Use when** a web frontend has browser telemetry: a generated
`src/lib/otel_gen.ts`, or your own code calling `initBrowserTelemetry`,
`enableAttribution` or `withAttribution` from `@reliantlabs/forge-web-runtime`.

### What changed

The web runtime runs product analytics in a tier (`off` < `aggregate` <
`identified` < `full`; `frontend-runtime/telemetry`, "Analytics tiers"). The
default is now **`identified`**: a signed-in user's events carry their opaque
id, as before, but **nothing** is stored on the device:

- no `forge.visitor_id`, no `forge.attribution`, no `user_identified` event,
  and `withAttribution(url)` returns the URL unchanged;
- the HyperDX `__rum_sid` session cookie is kept in memory (one session per
  page load), and no replay sampling decision is kept;
- **session replay does not record**, even with `telemetry.replay = True`:
  replay records a person, which needs consent;
- the ids an earlier page load stored under the old default are erased at
  startup.

Product events, page views and the signed-in user's id still flow, so the
console's counts keep working: the platform's ingest edge counts anonymous
visitors. `ANALYTICS_MODE = "aggregate"` makes browser events strictly
anonymous (no user id even when signed in); `"off"` stops product analytics. Under GPC / DNT a browser's events now carry **no user
id** either. Before v1.4.0 the runtime created a persistent visitor id and the
session cookie with no consent at all; this reduces what is stored, never
increases it. No API changed signature, and server events are not affected.

### What to do

Nothing, if cookieless analytics are what you want (no visitor id, no replay
without consent).

To keep the persistent visitor id, multi-day campaign attribution and replay:

1. Set the frontend's `runtime_config = {ANALYTICS_MODE = "full"}` in each
   env's KCL that should have them.
2. Call `grantAnalyticsConsent()` (from `@reliantlabs/forge-web-runtime`) on
   every page load once your consent manager says yes, before
   `initTelemetry()` runs if replay should start on that load; call
   `revokeAnalyticsConsent()` when consent is withdrawn. An internal tool whose
   users have agreed to recording grants at startup.
3. Offer the person a way to object: `optOutOfAnalytics()` stops product
   events from that browser and is remembered.

Hand-written callers: `initBrowserTelemetry({ attribution: true })` keeps its
old meaning (mode `full`, consent granted at start) and `{ attribution: false }`
plus `enableAttribution()` keeps working; both are deprecated in favour of
`analytics: "full"` plus `grantAnalyticsConsent()`. A caller that passed
neither now gets the cookieless default.

Then reload a page of the frontend and check, in the browser's devtools, that
Application → Cookies and Local/Session Storage hold nothing of forge's (in
`full`, after `grantAnalyticsConsent()`: `__rum_sid` and `forge.visitor_id`).
