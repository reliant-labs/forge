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

## 2. SimpleBackend — it IS a provider, and the earlier draft got this wrong

An earlier version of this note argued `SimpleBackend` is "a capability tier
over `K8sCluster`, not a provider", reasoning that both run in a cluster and
differ only in how much of the pod spec is reachable.

**That reasoning was wrong, and it was wrong because it reasoned from the
rendering rather than from the user.** Corrected after being pointed at
control-plane's `forge-deploy-product` branch.

The user of `SimpleBackend` does not know a cluster is involved, and that is
the entire point. `K8sCluster` requires `cluster`, `namespace` and `registry` —
coordinates of infrastructure the user brought. `SimpleBackend`'s own summary
is "a single container, running in a cluster forge did not create." The answer
to *where does this run* is **"on Reliant's infrastructure"**, which is a
different answer from "in the cluster you named", and *where it runs* is
precisely what a provider is.

That it happens to *render* onto `RenderedWorkload{deploy = K8sCluster}` is an
implementation detail of the lowering, and reusing the k8s adapter is good
engineering. But a shared lowering is not a shared provider, any more than two
languages compiling to the same IR are the same language. The earlier draft
mistook the IR for the abstraction.

### This is not speculative — it is built

`control-plane`'s `forge-deploy-product` branch (`a7dd8541`) already ships the
managed side of exactly this:

- `api/v1alpha1/simplebackend_types.go` (492 lines) — the CRD
- `internal/operators/simplebackend/controller.go` (681 lines) + `database.go`
- `internal/operators/shared/namespace.go` — per-tenant namespaces with Pod
  Security Admission enforce/audit/warn labels, patched on drift so a
  runtime-class change (gVisor → Kata) takes effect without recreating the
  namespace
- `internal/buildservice/tenant_limits.go`, tenant RPCs, and cross-tenant
  isolation tests that compose across tiers
- Four deploy tiers total: `StaticSite`, `SimpleBackend`, `ManagedDatabase` on
  Cloud SQL, and a hosted `ImageBuild` service

So the tenant cluster, the scoping and the lockdown are real today. The whole
product is deliberately inert — every reconciler registers only when its
provider is non-nil, each provider builds only when its own on-switch config is
set, the reconcile worker is not in `AllWorkers`, and 11 of 17 deploy RPCs are
`ScaffoldStub` — but inert-by-gate is a shipping decision, not an absence.

### What follows for the forge side

If `SimpleBackend` is a provider, it should look like one in forge:

- It wants its own **provider id** in `internal/deploytarget` — alongside
  `k8s-cluster`, `external`, `compose`, `host-infra`, `firebase`,
  `static-site` — rather than being folded into `k8s-cluster` dispatch on the
  grounds that it renders that way. The observe/deploy path for "Reliant runs
  it" is genuinely different from "your kubectl context runs it": the user has
  no cluster to point at and no context to be guarded against.
- The `cluster` / `namespace` fields it carries are **platform-filled
  coordinates, not app-facing knobs** — the schema docstring already says
  exactly this. A provider whose coordinates the platform supplies is the
  correct shape; it is what makes `forge env deploy prod` work on a machine
  that has never run `gcloud container clusters get-credentials`.
- The **closed schema stays the enforcement mechanism**, and is *more*
  important under this reading, not less. If the user is on someone else's
  infrastructure, the fields they must not reach are a tenancy boundary rather
  than a style guide.

The name is worth revisiting under this framing too. `SimpleBackend` describes
a capability level, which is what the earlier draft latched onto; if the
distinguishing fact is *whose infrastructure*, the name could say so.

The enforcement mechanism is the part worth keeping verbatim:

> "A hosted tier that must reject a field is a tier maintaining an allowlist
> that drifts from the schema; a field that does not exist needs no allowlist,
> and KCL rejects it for free with a message naming the schema."

That is why `SimpleBackend` has no `replicas`, no `security_context`, no raw
passthrough. The closed schema *is* the policy. An allowlist would be a second
copy of the rules, which is the same class of mistake as a hand-written list of
deploy targets.

## 3. The competitive gap — mostly closed already, on the control-plane side

An earlier draft of this note claimed forge's problem was that "every path ends
at a destination someone else owns" and that a forge-operated destination was
the missing hard half. That is **half wrong**, for the same reason as §2: the
managed destination exists on `forge-deploy-product`, with tenant namespaces,
PSA enforcement, per-org registry paths with retention, CI-to-promote and an
at-rest keyring. Four tiers, gated off.

What is genuinely missing is narrower and more tractable: **the two halves do
not meet yet.** forge's `kcl/schema.k` (on `forge-deploy`) declares
`SimpleBackend` and `StaticSite`; control-plane implements the operators that
reconcile them. Both are on unmerged branches, one of which vendors the other's
KCL into `.forge-kcl/` — which is how ~180 files of the forge-side spike came
to exist in only one uncommitted working tree until it was preserved.

So the sequencing question in §4 is not "when do we build the managed tier" but
"in what order do two already-built halves land so they meet". That is a
smaller and much better problem to have than the earlier draft assumed.

For the record, the piece that remains true from the earlier draft: `StaticSite`'s
immutable `releases/<digest>/` against a mutable `live/` is the right substrate
for promotion whoever owns the bucket, and it survives the user bringing their
own.

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

### The coordination hazard, now that both halves are known

The two branches are coupled and neither repo's CI can see it:

| | branch | carries |
|---|---|---|
| `forge` | `forge-deploy` | `SimpleBackend` + `StaticSite` KCL schemas, `static-site` provider, deploy-state layer |
| `control-plane` | `forge-deploy-product` | the CRDs and operators that reconcile them, tenancy, registry, keyring |

`forge-deploy-product` **vendors forge's KCL into `.forge-kcl/`**, so the schema
is duplicated across repos by copy. That is the mechanism by which the forge
spike survived only as a vendored copy in another repo — the preservation commit
says so explicitly. Any schema change to `SimpleBackend` or `StaticSite` now has
to land in forge and be re-vendored, and nothing fails loudly if it is not.

Practical consequence for ordering: **land the forge side first.** A schema that
control-plane vendors should not be changing underneath it, and the forge side
is the smaller, more reviewable half. Then re-vendor, then land the product
side. Doing it the other way round means re-vendoring twice and reviewing the
operators against a schema that is still moving.
