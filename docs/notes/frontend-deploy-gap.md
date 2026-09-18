# Frontends are second-class in deploy — findings and options

Working notes from dogfooding forge on a marketing site (Hounders). Written
against forge at `9a648007`, verified by scaffolding a probe project rather than
from reading code alone.

> ## ⚠️ Read `forge-deploy` first — most of the design half of this is already built
>
> The `forge-deploy` branch (`c2e76444`, worktree at
> `~/.reliant/worktrees/reliant-labs/forge-deploy-882e308d/forge`) is a
> preservation commit of a deploy-tier spike that **already implements** the
> generalisation this note argues for, and goes considerably further:
>
> - `schema StaticSite` — bucket + `public_dir` + `base_path` + `bundle` +
>   `cache_control` + `cdn`, with `releases/<digest>/` immutable archive and a
>   mutable `live/` prefix, so rollback and promotion re-point at an existing
>   digest instead of rebuilding.
> - `schema SimpleBackend` — one container, explicitly a *constrained profile
>   over* `K8sCluster` rather than a parallel mechanism; `render.k` projects it
>   onto the same `RenderedWorkload` the k8s adapter already emits.
> - `Frontend.deploy?: FirebaseHosting | StaticSite | K8sCluster` and
>   `Service.deploy?: … | SimpleBackend | …`
> - `static-site` is a real provider id in `deploytarget.go`, with
>   `staticsite.go`, `staticstage.go`, `observe_staticsite.go`, plus
>   `pkg/deploystate` (decide/policy/store) and `internal/deployartifact`.
> - 14 KCL positive/negative/closed-schema tests for the new schemas.
>
> It still builds green on its own tree, and is **32 commits behind main**.
>
> Two design notes there that are better than what I wrote below and are worth
> lifting verbatim into whatever ships:
>
> 1. **A closed schema IS the enforcement mechanism.** "A hosted tier that must
>    reject a field is a tier maintaining an allowlist that drifts from the
>    schema; a field that does not exist needs no allowlist, and KCL rejects it
>    for free with a message naming the schema." That is why `SimpleBackend` has
>    no `replicas`, no `security_context`, no raw passthrough.
> 2. **CDN invalidation defaults to `entrypoints`, not `all`.** Content-hashed
>    assets never need invalidating; only HTML entry points and the runtime
>    config document do. `all` would hit provider quotas around push
>    40-something, "long after the habit formed."
>
> **What the spike does NOT close is the gap this note is actually about.** On
> that branch too, `forge.Frontend` is emitted by the scaffold in exactly one
> place — `internal/templates/deploy/kcl/dev/main.k.tmpl`. Staging and prod
> templates still declare no frontend workload. So the schemas get richer while
> remaining equally invisible to a user who never opens `kcl/schema.k`.
>
> The recommendation at the end of this document therefore stands, and its
> ordering changes: items 1 and 2 (emit the capability at the decision point,
> warn on the silent no-op) are *scaffold* work that is independent of the
> spike, unblocked today, and would have prevented this entire detour. Item 3
> (generalise `FirebaseHosting`) is superseded — `StaticSite` already did it
> better.

## The finding, in one line

`forge.Frontend` is a **dev-loop primitive that grew a deploy field**, and the
scaffold never tells a user the deploy field exists.

## Evidence

Scaffold a project with `--frontend web`, then grep the generated KCL:

```
$ grep -rn "forge.Frontend" deploy/kcl/
deploy/kcl/dev/main.k:529:    frontends = [forge.Frontend {
```

One occurrence. In `dev`. Staging and prod declare **no frontend workload at
all** — they get `config.k` (the runtime `/config.js` projection) but nothing
that ships the app. `workloads.k`, which the scaffold's own comment calls the
place that "declares each workload COMPLETELY", contains no frontend.

So a user who scaffolds a frontend and runs `forge env deploy prod` deploys
their Go services and does not deploy their frontend.

**Scope note, to be accurate about what I actually observed:** on the probe,
`forge env deploy prod --dry-run` exits 1 at the declared-cluster guard before
frontend dispatch is reached, so I did *not* directly witness a silent
frontend no-op end to end. What is directly verified is the cause: staging and
prod render no frontend workload, and the frontend's name appears nowhere in
the deploy output. Whether that surfaces as silence or as an unrelated error
depends on what else the env declares — which is arguably worse, since the
failure mode is inconsistent.

Meanwhile the schema already supports it:

```kcl
schema Frontend:
    deploy?: FirebaseHosting | K8sCluster
```

with a docstring explaining both branches well. The capability is built; the
scaffold just never emits it, so it is invisible unless you read `kcl/schema.k`.

## Why `forge.External` is the wrong answer here

External works — I verified `deploy_cmd = "vercel deploy --prod --yes"`
dispatching correctly with zero k8s objects rendered. But reaching for it to
deploy a *frontend* means forge is shelling out to a competitor to do the thing
forge claims to do. It should be the escape hatch for a target forge does not
model, not the path of least resistance for the most common frontend in the
world.

It also gives up everything forge is for. Under External, forge does not inject
config or secrets (the external command's own platform does), and promotion is
bookkeeping rather than a byte reference, because the external platform is
git-driven. The user gets forge's *vocabulary* and none of its *guarantees*.

## The static-first question

**Should static be the default for `type = "nextjs"`?** No — but a static target
should exist and be the default *where it applies*.

The blocker is concrete and already documented in the scaffold: `output:
"export"` requires `generateStaticParams()` on every dynamic segment, and
forge's own generated CRUD pages (`/<slug>/[id]`, `/<slug>/[id]/edit`) are
dynamic client routes whose ids only exist at runtime. I confirmed this still
fails on Next 16.3.5:

```
Page "/items/[id]/edit" is missing "generateStaticParams()"
```

So static-by-default would break every project that has an entity — which is
every project forge's CRUD generation is aimed at. Static is correct for a
marketing site and wrong for the thing forge optimises for.

The honest split:

| Frontend shape | Right default |
|---|---|
| `type = "vite"` | static — it already is a static SPA |
| `type = "nextjs"` with no dynamic routes | static |
| `type = "nextjs"` with generated CRUD | standalone container |

That is a decision forge can *make for the user* by inspecting whether any
dynamic route exists, rather than a flag the user has to understand.

## `SimpleBackend` — a better mechanism than a new schema

The instinct behind `forge.SimpleBackend` is right: most apps are one process
and a database, and making that easy is the competitive move. But adding a
schema per deployment *shape* multiplies: SimpleBackend, then SimpleFrontend,
then SimpleWorker, each with its own fields and its own lowering.

The generalisation already present in the codebase is the **provider id**.
`deploytarget.go` dispatches on `g.ProviderID` over `k8s-cluster`, `external`,
`compose`, `host-infra`, `firebase`. Providers are the axis that is already
working. What is missing is not a new workload schema — it is more *providers*,
and a generic static one in particular:

```kcl
# What FirebaseHosting should have been, minus the vendor.
schema StaticHosting:
    provider: str          # "firebase" | "s3" | "cloudflare" | "netlify" | ...
    public_dir: str
    base_path?: str
    rewrites: [{str: any}] = []
```

`FirebaseHosting` is already 90% of this — `public_dir`, `base_path`,
`rewrites`, `bundle` are all vendor-neutral concepts. Only `project`/`site`/
`target` are Firebase's. Lifting the shape and keeping Firebase as one provider
behind it costs little and makes the next target additive.

If `SimpleBackend` does ship, the thing that makes it valuable is not the schema
— it is having a **forge-operated place to deploy to**, so that the default path
does not route through someone else's platform. The schema is the easy half.

## Making features discoverable — the actual problem

`forge project capabilities` is genuinely excellent and lists every verb. But it
lists *commands*, and the thing users miss here is a *schema field*. Grepping it
for the deploy targets returns one incidental hit.

Discoverability is not a docs problem, it is a **placement** problem. Three
mechanisms, cheapest first:

1. **Emit the capability commented-out at the decision point.** Staging and prod
   `main.k` should carry a commented `frontends = [...]` block showing both
   `FirebaseHosting` and `K8sCluster`, the way the rest of the scaffold already
   teaches through comments. A user reading prod config to deploy their frontend
   finds it exactly where they are already looking. This is the highest-value
   change in this document and it is nearly free.

2. **Warn on the silent no-op.** `forge env deploy <env>` should say
   `frontend "web" declared but has no deploy target for this env — it will not
   ship` rather than succeeding quietly. Forge is otherwise excellent at
   fail-closed; this is the one place it fails silent.

3. **`forge project shapes` for schemas.** `capabilities` covers verbs; there is
   no equivalent that answers "what can a Frontend *be*". The schemas carry good
   docstrings already — surface them.

The pattern across all three: forge teaches well *in the files it generates*,
and the gap is wherever a capability has no generated file to live in.

## Recommended order

Revised after reading `forge-deploy`. Items 1, 2 and 4 are scaffold/CLI work
that does not depend on the spike landing and could ship against `main` now.

1. **Emit the frontend deploy options, commented, into the staging/prod
   `main.k` templates.** Cheap, highest value, unblocked. Today the only
   template carrying `forge.Frontend` is `dev/main.k.tmpl` — on `main` *and* on
   `forge-deploy`.
2. **Warn when a declared frontend has no deploy target for the env being
   deployed.** Forge is fail-closed nearly everywhere else; this is the one
   place a whole workload can go missing without comment.
3. ~~Generalise `FirebaseHosting` → `StaticHosting`~~ — **superseded.**
   `StaticSite` on `forge-deploy` already does this, with release digests and
   CDN invalidation policy the earlier note did not consider.
4. **Auto-select static vs standalone by detecting dynamic routes**, rather
   than making the user understand the `output` field. Still open on both
   branches.
5. **Decide what to do with the spike.** It builds green and is 32 commits
   behind main. The design questions in this note are answered there; the
   open question is rebase-and-review cost, not design.
