# StaticSite vs Frontend, and where SimpleBackend belongs

Design notes answering three questions raised while dogfooding forge on a
marketing site. Written against `main` at `71abfe39` and the `forge-deploy`
spike at `c2e76444`.

## The axis, stated once

The framing that resolves all three questions:

> **The type is WHAT is being run. The provider is WHERE it runs.**

A workload schema (`Frontend`, `Service`) says what the thing *is* — where its
source lives, how to build it, what config it needs. A deploy target says where
the built artifact *goes*. They are different questions and neither one is a
special case of the other.

k8s is not the centre of this model. It is the escape hatch for workloads whose
shape genuinely needs a pod spec.

## 1. StaticSite vs Frontend — not duplicative, composed

They overlap on two field names and that is all:

| | Frontend | StaticSite |
|---|---|---|
| `name`, `type`, `path`/`source`, `dev_runner`, `port`, `env_file`, `env_vars`, `config` | ✓ | — |
| `bucket`, `cache_control`, `cdn`, `keep_releases` | — | ✓ |
| `public_dir`, `base_path` | — | ✓ |

`Frontend` answers *what is this app and how do I build and dev-serve it*.
`StaticSite` answers *where do the built bytes land and how is the edge cache
handled*. The relationship is already correct on `forge-deploy`:

```kcl
deploy?: FirebaseHosting | StaticSite | K8sCluster
```

`StaticSite` is a **value of** `Frontend.deploy`, not a sibling of `Frontend`.
One frontend can deploy to Firebase in staging and a bucket in prod by changing
only the `deploy` block, which is exactly the property you want.

**So: do not reconcile them.** The genuine redundancy is elsewhere —
`FirebaseHosting` and `StaticSite` are two schemas for one concept:

| | FirebaseHosting | StaticSite |
|---|---|---|
| `public_dir`, `base_path`, `bundle` | ✓ | ✓ |
| `rewrites` | ✓ | — |
| `cache_control`, `cdn`, `keep_releases` | — | ✓ |
| vendor coordinates | `project`, `site`, `target` | `bucket` |

Everything except the last row is vendor-neutral. `StaticSite` is the better
schema — it has the `releases/<digest>/` archive and the invalidation policy —
and `FirebaseHosting` predates it. The reconciliation worth doing is folding
Firebase into `StaticSite` as one provider behind a neutral shape, leaving
**one** static target with a provider field rather than one schema per vendor.

That also stops the next vendor (S3, R2, Netlify) from adding a fourth
near-identical schema.

## 2. SimpleBackend — it is already the right thing, and it is not a provider

The spike's own docstring has the answer:

> "It is a CONSTRAINED PROFILE over K8sCluster, not a parallel mechanism —
> `render_manifests` projects it onto the same `RenderedWorkload{deploy =
> K8sCluster}` the k8s adapter already emits."

That is correct and should not be turned into a provider. A provider answers
*where it runs*; `SimpleBackend` and `K8sCluster` run in the **same place** —
a cluster — and differ only in how much of the pod spec the user may touch. It
is a **capability tier over one provider**, not a second provider.

The enforcement mechanism is the part worth keeping verbatim:

> "A hosted tier that must reject a field is a tier maintaining an allowlist
> that drifts from the schema; a field that does not exist needs no allowlist,
> and KCL rejects it for free with a message naming the schema."

That is why `SimpleBackend` has no `replicas`, no `security_context`, no raw
passthrough. The closed schema *is* the policy. An allowlist would be a second
copy of the rules, which is the same class of mistake as a hand-written list of
deploy targets.

Where this leaves the naming: `SimpleBackend` describes a *tier*, and reads
like it describes a *workload type*. If it acquires siblings it will want a
shape like `tier = "simple" | "advanced"` on one schema rather than
`SimpleBackend` / `ComplicatedBackend`. Not urgent, but the name will shape how
people reach for it.

## 3. What is missing — a forge-operated place to deploy to

The competitive gap is not schema coverage. Between `K8sCluster`,
`SimpleBackend`, `StaticSite`, `Compose`, `HostInfra` and `External`, forge can
*describe* nearly anything.

What every path has in common today is that the **destination belongs to
someone else** — the user's cluster, their GCS bucket, their Firebase project,
or a competitor's platform via `External`. `SimpleBackend`'s own summary is "a
single container, running in a cluster forge did not create."

Deploying a marketing site currently means: bring a cluster, or bring a bucket,
or shell out to Vercel. A user who wants none of those has no forge answer.

If the goal is competing with Vercel, the schema is the easy half and it is
already done. The hard half is a default destination that forge operates, so
that `forge env deploy prod` on a fresh project does something without the user
first provisioning infrastructure elsewhere. `StaticSite`'s release-digest
model is the right substrate for it — immutable `releases/<digest>/`, mutable
`live/`, promotion as a re-point — and that design survives whoever owns the
bucket.

## 4. Whether to work in the spike's worktree

Recommendation: **not yet, and the reason is sequencing rather than caution.**

The spike is 32 commits behind `main`, was committed explicitly as "not cleaned
up, squashed, rebased or reviewed", and predates both the single-module merge
and v0.1.15. New work landing on it inherits that rebase, and rebasing ~180
files of unreviewed design work while adding to it is two hard jobs at once.

The order that avoids it:

1. **Rebase the spike onto `main` first, changing nothing.** That is a
   self-contained job with a clear success condition — it builds and its tests
   pass — and it is the prerequisite for everything else.
2. **Then decide what ships.** `StaticSite` and the `static-site` provider are
   the valuable half and are close to independently landable. `SimpleBackend`
   is a bigger product question, since it implies who operates the cluster.
3. **Fold `FirebaseHosting` into `StaticSite`** as part of landing it, while
   there is exactly one caller to migrate.

Two things already landed on `main` that make this cheaper and were deliberately
built to be spike-agnostic:

- `forge project shapes --kind deploy-target` reflects the union members out of
  the embedded schema, so `StaticSite` and `SimpleBackend` become discoverable
  the moment they merge, with no code change. Verified against a frozen copy of
  the spike's `schema.k`.
- The undeployed-frontend warning names valid targets from that same reflection,
  so its hint text picks them up for free too.

The one thing worth doing in the worktree *before* the rebase is answering
whether the `static-site` provider actually executes — the schema is declarable
on that branch, and `shapes` will list it on merge regardless. Declarable and
executable are tracked separately today, which is the open seam noted at the end
of `71abfe39`.
