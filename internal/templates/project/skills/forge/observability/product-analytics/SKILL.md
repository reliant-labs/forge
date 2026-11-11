---
name: observability/product-analytics
description: The built-in product analytics dashboards every hosted forge app gets with no code — the Overview's Traffic and Users sections, and the Users, Product and Acquisition dashboards (pkg/obsbuiltin) — with every definition they count by (cookieless visitor-day, visit, bounce, channel table, source, same-day stitch, person, active/new/returning, sign-up attribution, activation), engaged time and the Content section (page_engaged), how to extend them (the KCL forge.Analytics block: goals, activation, channels, content_routes, and its merge rules; forge.Funnel by visitor/person), override one by a same-named file, or opt out (dashboards = False).
---

# Built-in product analytics

A forge app on Reliant hosting gets product analytics with nothing declared:
the Overview's **Traffic** and **Users** sections, and three dashboards in the
console's Product group, **Users**, **Product** and **Acquisition**. Every panel
is a `sql` panel over the hosted per-org `product_events` table, which the
platform fills from the events forge already emits: `page_viewed` from the
browser runtime, the standard `auth_*` events, the automatic CRUD events, the
app's own `track()` / `observe.Track` events. Where those come from:
`observability/product-events`.

`forge generate` writes them into the dashboard spec and `forge env build`
ships them in the release bundle, like every generated dashboard. They are
**hosted only**: the local ClickStack has no `product_events` table, so no
HyperDX file is written and the local Overview is unchanged. The builders are
the public package `forge/pkg/obsbuiltin`, so the control plane serves the same
definitions for an environment that has no bundle yet.

| Dashboard | id | Opens on | When |
|---|---|---|---|
| Overview, Traffic section | `overview` | 24h | the project has a browser frontend |
| Overview, Users section | `overview` | 24h | always |
| Users | `users` | 30d, compared with the previous period | always |
| Product | `product` | 7d, compared with the previous period | always; it **replaces** the old generated _Product usage_ (`usage`), so stars and copies move to it |
| Acquisition | `acquisition` | 30d, compared with the previous period | the project has a browser frontend |

A static site's environment (no process) keeps Users, Product and Acquisition
beside its frontend dashboard.

## The definitions

Every number on the four dashboards is one of these. They are the same rules
`forge obs users` and the console's Users card count by.

**Visitor.** The platform's ingest edge hashes each browser request into a
cookieless daily visitor id, `DayVisitor`: a hash of a salt that is deleted
after the day, the address and the user agent. Nothing is stored on the device
and no fingerprinting script runs. A hash is new every UTC day, so a visitor is
a **visitor-day**: one person on two days, or on two devices, is two. Its day
is the day the edge **received** the batch, not the event's (an event of 23:59
sent after midnight carries the next day's hash), so the dashboards key a
visitor by `(EnvId, DayVisitor)` alone and never pair it with
`toDate(Timestamp)`. Bots get no hash and are never counted. People behind one
office NAT with the same browser collapse into one visitor.

**Web rows.** The events of the `web` and `landing` clients that carry a hash,
except the ones that are not interaction: `auth_signed_in` (an identity
heartbeat), `auth_signed_out`, `flag_evaluated`, `page_engaged` and the
platform's `auth_accounts`.

**Visit.** One visitor's web rows in time order; a new visit starts after a gap
of more than 30 minutes. Visits are built at query time, so there is no session
id on the wire. A visit that crosses midnight inside one batch stays one visit.

- **Bounce**: a visit with exactly one `page_viewed` and nothing else.
- **Visit duration**: last event minus first; a bounce lasts 0 and counts.
- **Where a visit came from**: its first `page_viewed`'s `referrer_host`,
  `referrer_path`, `utm_source` (else `ref`), `utm_medium`, `utm_campaign`,
  `utm_term`, `utm_content` and `click_id`.
- **First touch**: a visitor-day's first visit. Visitors are credited to the
  channel, source, landing page, campaign, country, device, browser and OS of
  their first touch.

**Channel.** The first rule that matches wins. The project's own
`forge.Analytics.channels` rules are tried first (a rule matches when every
condition it sets matches: `host` is the referrer host or a subdomain of it,
`source` the `utm_source`, `medium` the `utm_medium`, case-insensitive). Then:

| # | Channel | When |
|---|---|---|
| 1 | Paid search | medium `cpc`, `ppc`, `paid`, `paidsearch`, `paid_search`; or a `google`/`microsoft` click id |
| 2 | Paid social | medium `paid_social`, `paidsocial`, `social_paid`, `paid-social`; or a `meta`/`tiktok`/`linkedin` click id |
| 3 | Display | medium `display`, `banner`, `cpm`, `programmatic` |
| 4 | Email | medium `email`, `e-mail`, `e_mail`, `newsletter` |
| 5 | Affiliate | medium `affiliate` |
| 6 | AI assistants | referrer `chatgpt.com`, `chat.openai.com`, `perplexity.ai`, `claude.ai`, `gemini.google.com`, `copilot.microsoft.com`, `you.com` (or a subdomain); or source `chatgpt.com`, `chatgpt`, `perplexity`, `claude` |
| 7 | Organic social | medium `social`, `social-network`, `social-media`, `sm`; or referrer facebook, instagram, linkedin, lnkd, reddit, youtube, tiktok, pinterest, threads, bsky, mastodon, t (`.com`, `.co`, `.in`, `.app`, `.social`, `.net`), `x.com`, `news.ycombinator.com` |
| 8 | Organic search | medium `organic`; or referrer google, bing, duckduckgo, yahoo, baidu, yandex, ecosia, brave, kagi, startpage, qwant |
| 9 | Other campaign | any other `utm_source`, `ref` or `utm_medium` |
| 10 | Direct | no referrer |
| 11 | Referral | anything else |

**Source.** `utm_source` (else `?ref=`), else the referrer host, else `(direct)`.

**Same-day stitch.** A visitor-day is linked to a user when exactly one user id
appears on that hash's rows that day: a browser row after sign-in, or a server
row, since the hosted proxy stamps the hash on the requests it forwards, so
devidp's `auth_signed_up` carries it with no browser code. A hash two users share
links nobody, a browser under Global Privacy Control or Do Not Track is never
linked, and nothing is linked across days without the `full` tier's consent.

**Person.** The signed-in user; else the user the stitch links the row's
visitor-day to; else the consented persistent visitor id (`full` tier); else the
visitor-day. Goals, events by name, flag exposures and `by = "person"` funnels
count persons.

**User.** A user id per environment, the way the Users card counts it, so a
dashboard over several environments equals the sum of their cards.

- **Active**: any user-attributed product event except `auth_signed_out` and
  `auth_accounts` (`obsquery.ActiveEvidence`): a sign-in, the daily session
  heartbeat, a CRUD write, an app event.
- **New**: a user whose first `auth_signed_up` is in the range. An environment
  that reported no sign-up in the 90 days before the end of the range counts the
  users first seen in the range instead (`obsquery.NewUsersSource`); the Users
  dashboard's _Where auth events come from_ table says when.
- **Returning**: active and not new.

**Sign-up attribution.** A sign-up (a user's first `auth_signed_up`) is credited
to the **first touch of its own day**: its own hash, else the one hash the stitch
links to the user that day. A sign-up with no visit that day (through the API,
on another device) is **Unattributed**, never Direct. This is the cookieless
limit, stated on the panels: a visitor who came from a newsletter on Monday and
typed the URL to sign up on Wednesday is credited to Wednesday's source.

**Activation.** The declared `forge.Analytics.activation` event, else any
`<entity>_created` (the automatic CRUD event, so it needs no code), within
`activation_window` (default `7d`) of signing up.

**Page view's engaged time.** The web runtime ends each page view with one
`page_engaged {route, engaged_ms, scroll_pct}`: the time the page was visible,
focused and in use (a 30-second idle cutoff, at most 30 minutes) and the deepest
the reader scrolled (`observability/product-events`, "Engaged time"). A view the
reader returned to reports again, `resumed` and cumulative, so a **view** is a
run of one visitor-day's reports of one route starting at a report that is not
`resumed`, and its figures are the run's largest. Medians are exact and
interpolated (`quantileExactInclusive`): the median of 10 s and 20 s is 15 s.
A view of less than half a second reports nothing, so it is in no median.

**Content route.** A route matching one of `forge.Analytics.content_routes`
(`*` matches any run of characters, slashes included, so `"/blog/*"` is every
post and not `/blog` itself); every route when none is declared.

**Onward conversion.** A visit that, after viewing a content route, reached a
declared goal or signed up: a row carrying the visitor's hash (the browser's,
or a hosted backend's request the edge stamped) that is an `auth_signed_up`, a
goal's event or a page view of a goal's route, later than the view and no more
than 30 minutes after the visit's last browser event.

## What each dashboard shows

**Overview.** The existing panels form its _Health_ section, first. _Traffic_:
visitors now (the last 5 minutes), visitors, page views and views per visit,
bounce rate, the top 5 pages and channels, JS errors per 1,000 page views
(`error_events`, kept 14 days by default). _Users_: active, new and returning
users over the last 7 days beside the 7 before (fixed, whatever range is
picked, as the Users card), accounts and 30 days of daily active users.

**Users.** _At a glance_: accounts (the platform's daily `auth_accounts`
snapshot; an environment with none shows its users seen in 90 days), active,
new, returning, daily/weekly/monthly active users, stickiness (average DAU over
average 30-day active users). _Engagement_: weekly lifecycle (new, retained,
resurrected, and churned below zero; the current week counts no churn until it
ends), power users by days active in the last 28 days, time from sign-up to
activation, and a weekly sign-up cohort retention heatmap (W0 is 100%; a week
not yet happened is an empty cell). _Authentication_: sign-ups by method,
credential sign-ins by method, verified share, sign-ups by provider, and which
clients report auth events. _People_: the most active users by opaque id.

**Product.** _At a glance_: page views, visits, views per visit, bounce rate,
visit duration, every product event. _Pages_: each route's views, visitors,
signed-in users, entries and exits; entry pages with bounce and same-day
sign-up rates; exit pages with exit rate; the median and 75th-percentile engaged
time per route. _Content_: per content route, views, visitor-days, the median
engaged time, the shares of views read to 50% and 90%, entries, exits, and
onward conversions (count and share of the visits that viewed it). The answer
to "how many read the post, for how long, and did they sign up" is its row; where
its readers came from is Acquisition's landing pages. _Goals and events_: one row per
declared goal (persons, total, conversion over visitor-days), every other event
with its persons and last seen, and the CRUD writes per entity. _Funnels_:
the built-in visit → sign-up → activation funnel (the first step counts
visitor-days, the others persons; sign-ups with no visit count, and the share
that came from a visit is its own figure), and one funnel per `forge.Funnel`
with `by = "visitor"` (per visitor-day) or `"person"`. _Experiments_: each flag
and variant's exposed persons and the share that activated within the window
after their first exposure; descriptive, not a significance test.

**Acquisition.** _At a glance_: visitors, visits, sign-ups, visit-to-sign-up
(attributed sign-ups over visitor-days). _Channels_: visitors by channel per day,
the channel table (visitors, visits, bounce, sign-ups, conversion, and the share
of the sign-ups that activated: channel quality, not just volume), sign-ups by
channel per week. _Sources_: sources, referrers (host and path), campaigns
(`utm_*`), landing pages by channel with the median engaged time of the landing
view itself. _Audience_: countries (ISO codes),
devices, browsers, operating systems. _Quality_: the share of each channel's
sign-ups active in their fourth week, over the sign-ups of 35 to 90 days before
the end of the range; a channel with fewer than 20 shows none.

## Declaring product analytics in KCL: `forge.Analytics`

Nothing needs declaring: a frontend with no `analytics` block runs in the
cookieless `identified` mode: nothing is stored on the device, and a signed-in
user's events carry their opaque id. The block sets the browser's mode and adds goals,
an activation event and channel rules to the project's built-in product
dashboards. It is per frontend and per environment, beside the frontend's
other telemetry:

```kcl
forge.Frontend {
    name = "web"
    telemetry = forge.BrowserTelemetry {
        analytics = forge.Analytics {
            mode = "identified"                # "off" | "aggregate" | "identified" (default) | "full"
            goals = [
                forge.Goal {id = "checkout", event = "purchase"},
                forge.Goal {id = "pricing", title = "Saw pricing", route = "/pricing"},
            ]
            activation = "order_created"       # unset: the user's first <entity>_created
            activation_window = "7d"           # unset: 7d; at most 90d
            channels = [forge.Channel {host = "news.ycombinator.com", channel = "Organic social"}]
            content_routes = ["/blog/*"]       # unset: every route
            # dashboards = False               # drop the built-in product dashboards
        }
        funnels = [forge.Funnel {id = "buy", steps = ["view_product", "purchase"], by = "person", window = "7d"}]
    }
}
```

| Field | Meaning | Checked at render |
|---|---|---|
| `mode` | the MOST the app allows a browser. A browser runs in the lowest of it, the consent the app signalled and its privacy signals (GPC/DNT caps it at `aggregate`: counted, never identified). `off`: no browser product events. `aggregate`: strictly anonymous, an opt-down: nothing stored on the device, a visitor counted only by the platform's daily edge hash, no user id on browser events. `identified` (the default): also cookieless, nothing stored on the device, and a signed-in user's browser events carry their opaque user id. `full`: once the app calls `grantAnalyticsConsent()`, a persistent visitor id and multi-day attribution too. Server events (`observe.Track`, CRUD, sign-ins) are never gated by it | one of the four |
| `goals` | conversions the Product dashboard counts: persons, total, and conversion over visitors. Exactly one of `event` (a product event) and `route` (a `page_viewed` route, id segments as `:id`) | unique ids; at most 50; a route is a path with no query or fragment |
| `activation` | the event that marks a user activated | a product event name |
| `activation_window` | how long after signing up a user has to activate | `30m`, `1h`, `7d`…, at most `90d` |
| `channels` | rules tried in order **ahead of** the built-in channel table; a visit whose first page view matches every condition a rule sets (`host`: the referrer host or a subdomain of it; `source`/`medium`: `utm_source`/`utm_medium`, case-insensitive) is credited to `channel`, a built-in name or a new one | at least one condition; a host name, not a URL; no two rules with the same conditions |
| `content_routes` | the routes the Product dashboard's _Content_ section reports on (a blog, docs, marketing pages): a route as `page_viewed` records it, `*` matching any run of characters, slashes included (`"/blog/*"` is every post, not `/blog` itself). Unset: every route | at most 50; each starts with `/`, no query, fragment or whitespace |
| `dashboards` | `False` drops the built-in Users, Product and Acquisition dashboards and the Overview's Traffic and Users sections; collection still follows `mode` | |

`mode` reaches the browser as `ANALYTICS_MODE` in the runtime config document
(`config.js`, or a hosted site's StaticSite spec), so one build promotes across
environments and each sets its own. Only a non-default mode renders the key:
absent means `identified`. When the environment switches browser telemetry off
(`signals.browser = False`), no mode is derived and any `ANALYTICS_MODE` you set
in `runtime_config` is forced to `off`; otherwise your own `runtime_config`
entry outranks the derived one.

`forge.Funnel.by` counts a journey by browser `session` or `user` (a `funnel`
panel on the frontend dashboard, over browser spans), or by `visitor` (the
cookieless daily visitor, so a window of at most `1d`) or `person` (the
signed-in user, or the same-day visitor one sign-in links to them). The last two
read `product_events` and belong to the built-in Product dashboard.

The dashboards are the project's, while each env declares its frontends, so
`forge generate` merges every env's and frontend's declaration: an equal goal or
channel rule collapses, a goal id or channel conditions declared two different
ways is refused naming both, `activation` and `activation_window` must agree
wherever they are set (unset defers), `content_routes` is the union of every
declaration (none anywhere is every route), and any `dashboards = False` drops
them.

What the dashboards draw from these declarations, and every definition they
count by, are the sections above. The tiers `mode` caps:
`observability/product-events`.

## Extend, override, opt out

- **Extend by declaration**, no JSON: `forge.Analytics {goals, activation,
  activation_window, channels, content_routes}` and `forge.Funnel {by = "visitor" | "person"}`
  regenerate the panels on the next `forge generate`. Syntax and the merge
  rules across frontends and environments: "Declaring product analytics in
  KCL" above.
- **Override one dashboard** with a spec-form file of the same name:
  `deploy/observability/dashboards/<project>-users.json` replaces
  `<project>-users_gen.json` in the bundle (the same for `-product`,
  `-acquisition` and `-overview`). Start from the generated one: `forge env
  build` puts it in the bundle, and the panel ids above are stable. `forge obs
  dashboards check` validates it. A project dashboard declaring
  `"replaces": ["users"]` takes over the generated one's stars instead.
- **Opt out**: `forge.Analytics {dashboards = False}` drops the three
  dashboards and the Overview's Traffic and Users sections. Collection still
  follows `mode`; `mode = "off"` stops browser product events.

## Writing your own panel with the same rules

The fragments are Go in `forge/pkg/obsbuiltin` (`fragments.go`); `reliant forge
project libraries obsbuiltin` prints the package's API. A panel of your own
reaches the same answer by keying visitors on `(EnvId, DayVisitor)`, users on
`(EnvId, UserId)`, and active users on `obsquery.ActiveEvidence`:

```sql
SELECT Props['route'] AS route, uniqExact((EnvId, DayVisitor)) AS visitors
FROM product_events
WHERE Timestamp >= {from:DateTime64(9)} AND Timestamp < {to:DateTime64(9)}
  AND (empty({env_ids:Array(String)}) OR has({env_ids:Array(String)}, EnvId))
  AND EventName = 'page_viewed' AND DayVisitor != '' AND DeviceType != 'bot'
GROUP BY route ORDER BY visitors DESC LIMIT 20
```

The edge columns (`DayVisitor`, `Country`, `Region`, `DeviceType`, `Browser`,
`OS`, `Privacy`, `SaltScope`) are `''` on rows stored before the platform added
them, and `SaltScope = 'local'` marks a day a proxy counted with its own salt
(visitors slightly over-counted).
