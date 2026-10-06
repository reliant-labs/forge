---
name: deploy/static-site
description: Ship a landing page, marketing site, docs site or SPA as a static site — when forge is the right call for one, the static export, binding it to hosted static hosting (the platform owns bucket + CDN) or your own bucket, and the build-once → promote release flow.
---

# Static sites

A landing page, a marketing site, a docs site or a SPA over someone else's API
is a **frontend with no backend**. Forge ships it the same way it ships an app:
declared per env, built once, promoted by digest. What you get over dropping
files on a host is the part that is hard to retrofit later:

- **Environments from day 0**: a `staging` that promotes the exact bytes to
  `prod`, not a rebuild that happens to look the same.
- **Hosted static hosting.** `forge.OnHosted {}` publishes to the control
  plane, which owns the bucket, the CDN and a platform hostname. Custom
  domains are `forge domain`.
- **Config without a rebuild.** A hosted site's `config.js` is written per env
  by the platform, so one bundle serves every env.
- **A path to an app.** When the site grows a waitlist, checkout or login,
  `forge scaffold service <entity>` adds the Go API next to it. The site is
  already the frontend of a forge project and nothing has to move.

If none of that matters — a one-off page that will never have a second env —
a plain `index.html` is fine, and forge is the wrong shape for it.

## 1. Scaffold a static frontend

```bash
forge project new site --mod github.com/acme/site --disable orm,migrations
cd site
forge scaffold frontend web --routes none
```

- `npm run build` is a static export into `out/`. That is the scaffold default
  (`output: static` in `forge.yaml`). A static runtime publishes a directory,
  and `out/` is that directory.
- `--routes none` generates no CRUD pages, because a site with no backend has
  no entities. A site that has entities keeps its generated pages, which are
  static routes (`/<entity>/view?id=…`), so the export still builds.
- `--kind vite-spa` is the other static-capable shape (its build lands in
  `dist/`).

The scaffolded UI is starter code. Before designing the page, load
`frontend/design`; it asks for the brief first.

## 2. Bind it to a static runtime, per env

A frontend states where it runs in each env's `deploy/kcl/<env>/main.k`:

| `runtime =` | Where the bytes go |
|---|---|
| `forge.OnHosted {}` | the control plane's static hosting: the platform owns the bucket, the CDN and the hostname |
| `forge.OnBucket {bucket = ...}` | your own bucket: `releases/<digest>/` + `live/`, then CDN invalidation |
| `forge.OnFirebase {project, site}` | `firebase deploy` |

Hosted static hosting accepts only a static export, and **a static site alone
is free** (one site per org, no billing). Past that — a second site, or any hosted
workload or managed database beside it — the deploy needs billing and is
queued, not refused, until it is set up (exit 7; `deploy/hosting`).

A site with no backend needs nothing else in the env. This is a complete
hosted `prod`:

```kcl
import forge

output = forge.render(forge.Bundle {
    project = "site"
    control_plane = forge.ControlPlane {}    # Reliant cloud; needed by OnHosted
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        public_dir = "out"                    # where the static export lands
        image = "ghcr.io/acme/site-web"       # a hosted site's release is an OCI artifact
        runtime = forge.OnHosted {}
    }]
})
```

`image` is required on `OnHosted`: the release is pushed to a registry, and the
registry is part of the reference. forge appends the platform's own `static.v1`
layout segment, so `forge env build` pushes
`ghcr.io/acme/site-web/static.v1`.

> **Scaffold gaps (forge #401).** A frontend-only project still scaffolds a
> `migrate` workload, a cluster and a `database_url` secret into its envs, and
> `forge scaffold frontend` writes an inline frontend that
> `forge env new --bind web=hosted` cannot rebind. Until #401 lands, replace
> each env file with a frontends-only Bundle: the shape above for `staging`
> and `prod`, and for `dev` the same Bundle with no `control_plane`, plus
> `port = plugin.resolve_port("site-dev-web", 3000)` (after
> `import kcl_plugin.forge as plugin`) and `runtime = forge.OnHost {}` on the
> frontend. `forge env render prod` should then report `0 object(s)` and
> `hosted: web (static)`.

## Run it locally

```bash
forge env up dev    # the frontend's dev server; prints its URL
```

No cluster is needed: nothing in the dev env runs in one, so `env up` leaves
its declared `k3d-<project>` cluster alone and says so.

## 3. Ship it

```bash
forge lint --static-export                 # what the export can't serve, with file:line
forge env render prod                      # 0 cluster objects; hosted: web (static)
forge ci validate-kcl                      # the control plane would admit it
forge env build prod --release v0.1.0 --plan   # what would be pushed and recorded; builds nothing
forge env build prod --release v0.1.0      # build once, push, record the release
forge env deploy prod v0.1.0               # publish; the platform syncs it and writes config.js
forge env status prod                      # the release prod runs, and its health
```

`forge env render` (and every deploy and build) refuses a Next.js frontend on
a static runtime whose forge.yaml `output` is not `static`: its build would be
a Node server, with nothing in `out/` to publish. The error names the
frontend, the env and both fixes. `forge lint --static-export` runs offline and
reports, each with file:line, what `next build` would refuse for an export: a
`[id]` route without `generateStaticParams`, server actions, `next/headers`,
next/image without `unoptimized`. It also warns on what the export drops
silently, such as middleware and rewrites. The real `next build` in CI stays
authoritative. The lane only tells you sooner.

A hosted deploy ships only what a release froze, never a local build. Without a
release it refuses, and the error prints the cut-then-deploy command.
Promotion re-points another env at the same digest:
`forge env deploy staging v0.1.0` and then `forge env deploy prod v0.1.0` ship
byte-identical sites. Recovery rolls forward to a new release; there is no
rollback.

Signed in to Reliant means signed in to the control plane: there is no second
`forge login`. `reliant forge …` and Reliant agents' shells set
`$FORGE_CREDENTIAL_HELPER`, which mints forge a short-lived token from your
Reliant session. Your own hostname: load `deploy/domains`.

## Runtime config

A static bundle cannot read env vars at runtime, so per-env values (an API URL,
a public analytics key) go on the frontend's `runtime_config`, declared in
each env. The bundle reads them as `window.__FORGE_CONFIG__`:

```kcl
runtime_config = {
    API_URL = forge.WorkloadURL { workload = "api" }   # a workload's URL in the same env
    SITE_NAME = "acme"
}
```

On `OnHosted` the references are kept in the published spec, and the platform
writes `config.js` after every sync, so the release carries no `config.js` and
promotes across envs unchanged.

Secrets do not belong in a static site: everything in `config.js` is public.
A value that must stay secret belongs on a service (`forge secret set`).

## Growing a backend

`forge scaffold service <entity>` adds the Go API to the same project, and a
`fw.Workload` bound `forge.OnHosted {}` runs it beside the site. The frontend
reaches it through `runtime_config` (`workloadURL`). The CRUD pages generated
for the new entities are static routes, so the site still exports. Write your
own pages the same way: put the id in the query string and never use a `[id]`
segment (`frontend/pages`). A frontend scaffolded by forge ≤ v0.1.43 has
`[id]` pages; `forge skill load migrations/v0.1.44` converts them.

Two cases leave static hosting behind: server rendering, and request-time
server APIs such as server actions, middleware, and `cookies()`. For those,
switch `output` to `standalone` and ship the frontend as a workload instead
(`deploy`, `frontend/serving`).

## How hosted static hosting resolves a path

A request for `/books/view?id=…` is served `books/view.html`. The export
writes one `.html` file per route because `trailingSlash` is off, and the
hosted origin tries `<path>`, then `<path>.html`, then `<path>/index.html`.
Anything else route-shaped falls back to the site's root `index.html`. Every
link the scaffold generates resolves to the file for its own route.
`frontend/serving` covers hosts that only map directories to `index.html`.
