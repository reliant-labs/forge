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
forge scaffold frontend web --output static --routes none
```

- `--output static` makes `npm run build` a static export into `out/`. A static
  runtime publishes a directory, so it needs one: the default, `standalone`,
  is a Node server.
- `--routes none` generates no CRUD pages. They are dynamic `[id]` routes,
  which a static export refuses to build.
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
forge env up dev --no-deploy    # the frontend's dev server; prints its URL
```

`--no-deploy` is needed until #401 lands: an env that declares no cluster
still runs the deploy phase against your current kubectl context.

## 3. Ship it

```bash
forge env render prod                      # 0 cluster objects; hosted: web (static)
forge ci validate-kcl                      # the control plane would admit it
forge env build prod --release v0.1.0 --plan   # what would be pushed and recorded; builds nothing
forge env build prod --release v0.1.0      # build once, push, record the release
forge env deploy prod v0.1.0               # publish; the platform syncs it and writes config.js
forge env status prod                      # the release prod runs, and its health
```

A hosted deploy ships only what a release froze, never a local build. Without a
release it refuses, and the error prints the cut-then-deploy command.
Promotion re-points another env at the same digest:
`forge env deploy staging v0.1.0` and then `forge env deploy prod v0.1.0` ship
byte-identical sites. Recovery rolls forward to a new release; there is no
rollback.

Signed in to Reliant means signed in to the control plane: there is no second
`forge login`. Your own hostname: load `deploy/domains`.

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
reaches it through `runtime_config` (`workloadURL`). If the frontend then needs
dynamic routes or server rendering, switch `output` to `standalone` and ship it
as a workload instead (`deploy`, `frontend/serving`).
