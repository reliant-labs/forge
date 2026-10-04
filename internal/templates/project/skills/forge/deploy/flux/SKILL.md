---
name: deploy/flux
description: Envs with no control plane reconcile through a Flux in their own cluster — what `forge cluster up` installs, the shared-Flux rule, how the desired-state pointer works, prune guards on stateful objects, and the three exit codes.
---

# The in-cluster reconciler

Every real environment goes **bundle → version store → reconciler**. An env
with no control plane uses the **machine ledger** as its version store, and the
reconciler — Flux — runs **in that env's own cluster**.

`forge env deploy` for such an env does not apply your objects. It writes two
small objects per cluster and watches:

```
OCIRepository   flux-system/<env>     "the bundle is at this digest"
Kustomization   flux-system/<env>     "apply this path out of it"
```

Flux applies the env. That separation is the point, and it buys three things a
client-side apply cannot:

- a half-finished `forge` run cannot leave a cluster half-applied;
- the cluster re-converges after a node replacement with no forge present;
- a hand-edited field is reverted on Flux's own interval, so the cluster
  matches what was recorded rather than what somebody last typed.

## Which envs this applies to

All three must hold:

1. **no declared lifecycle** — the env does not say `lifecycle = "local"` or
   `"ephemeral"`;
2. **no control plane** — its version store is this machine's ledger;
3. **it targets a cluster** — there is something for a reconciler to converge.

An env that declares **nothing** and targets a cluster is treated as a REAL
environment and gets a reconciler. That default is deliberate: forgetting to
declare does not quietly opt an env out.

```kcl
# A developer's own cluster — direct apply IS the point. No reconciler.
output = forge.render(forge.Bundle {
    project = "acme"
    lifecycle = "local"
    ...
})

# A throwaway per-run cluster (CI / e2e). No reconciler: routing it through
# one would add a control loop with nothing to converge to.
lifecycle = "ephemeral"

# Declares neither, targets a cluster => reconciled through in-cluster Flux.
```

## What `forge cluster up` installs

`forge cluster up <env>` installs Flux into every **forge-managed k3d cluster**
the env declares, when that env reconciles. It is idempotent — a warm run
re-applies the same version-pinned manifests and kubectl reports them
unchanged — and it is silent for every env that applies directly.

It installs **two controllers, not six**: `source-controller` (fetches the OCI
artifact) and `kustomize-controller` (applies it). `helm-controller`,
`notification-controller`, `image-reflector-controller` and
`image-automation-controller` are **off**, and that is a security decision
rather than a resource one — every controller installed is attack surface that
has to be patched, and none of those four is on this path. There is no Helm
release to reconcile, no alerting fan-out and no image-tag automation.

The posture (chart values):

- `--no-remote-bases` — the apply is hermetic: the OCI artifact is the only
  thing that can affect cluster state.
- `--no-cross-namespace-refs` — a Kustomization may only name a source in its
  own namespace.
- **No `multitenancy` block.** It makes kustomize-controller impersonate
  `flux-system:default`, which cannot apply the Namespaces and CRDs your bundle
  carries (measured: every apply failed Forbidden). forge is the only writer,
  so there is no tenant boundary to enforce. A consumer running tenants through
  the same Flux turns it on via `values`.

The chart is **version-pinned with no knob**. The pointer forge writes names an
API version, and the `lastAppliedRevision` comparison the wait depends on is a
property of a specific `source-controller` — a consumer able to pin a different
Flux could pin one where the wait can never pass.

> **Chart version ≠ Flux version.** There is no fluxcd-published chart; forge
> uses the community chart at `oci://ghcr.io/fluxcd-community/charts/flux2`,
> whose version has its own lineage. Chart **2.19.1** is Flux **v2.9.5**. On a
> bump, re-derive it — do not increment:
> `helm show chart oci://ghcr.io/fluxcd-community/charts/flux2 --version <v> | grep appVersion`

## The shared-Flux rule

**forge installs Flux into a cluster only when no declared chart already
provides it.** Two Flux installs in one cluster collide on the same
cluster-scoped CRDs and the same `flux-system` namespace, and the collision is
not a clean failure: whichever applied last owns the CRDs, and the other's
Kustomizations reconcile against a controller that was replaced underneath
them.

If your env needs Flux for its own reasons — hosted tenants, a reconciler you
drive yourself — **declare forge's chart**:

```kcl
helm_charts = [
    forge.flux_chart(values = {
        # Merged OVER forge's posture, last-wins: an annotation does not cost
        # you --no-remote-bases or the disabled controllers.
        sourceController.serviceAccount.annotations = {
            "iam.gke.io/gcp-service-account" = "flux@proj.iam.gserviceaccount.com"
        }
    })

    # Multi-cluster: re-target it like any other chart.
    # forge.flux_chart(cluster = _control_plane)
]
```

**Detection is by chart reference, never by name.** A `HelmChart`'s `name` is
the `--target` selector you pick freely, so a name heuristic would let a naming
choice decide whether forge installed a colliding second copy. forge compares
your chart's `oci` against `forge.FLUX_CHART_OCI`. Declare that reference and
you are recognised; call the chart whatever you like.

A chart with **no** `cluster` is treated as covering every cluster the env
declares — the safe direction, since the alternative is installing a second
Flux beside yours.

## Where the bundle has to be

A machine-ledger env's bundle is written into the ledger's **local OCI
layout** — a directory — because a local env's bundle is not shipped. Flux
cannot read a directory on your laptop, so the deploy **publishes** the
recorded bundle to the env's own registry before pointing at it.

That is not a second placement rule. The bundle is still *recorded* in the
machine ledger; publishing is the reconciler's fetch requirement, satisfied
from bytes the ledger already holds. It is content-addressed, so republishing
is idempotent and cannot change what is deployed.

**A cluster workload's image must name a registry host.** An environment does
not declare a registry; a workload does, as part of its image. The bundle goes
beside those images, under the same host and path (`localhost:5050/acme/api:1`
puts the bundle at `localhost:5050/acme/bundle.v1/<env>`).

For a k3d cluster the host pushes to `localhost:<port>` and **nothing inside
the cluster can resolve that**, so the pointer names
`host.k3d.internal:<port>` — the Docker host-gateway alias forge keeps in every
managed cluster's CoreDNS. forge does that rewrite for you. (`registry.localhost`
is the *kubelet's* name, resolved through the node's `/etc/hosts`;
`source-controller` is a pod and does not get it.)

## Prune guards on stateful objects

The Kustomization forge writes sets `prune: true`, which is what makes the
bundle authoritative — an object the path no longer carries is deleted, so a
removed workload actually goes away.

That is right for a Deployment and catastrophic for a PersistentVolumeClaim,
and **the trigger is not somebody deciding to delete anything**. It is a render
that merely came out one document short: a `--target` that selected a subset, a
KCL conditional that went false, a workload renamed in one place and not
another.

So forge stamps, **at bundle write time**:

```yaml
metadata:
  annotations:
    kustomize.toolkit.fluxcd.io/prune: disabled
```

onto every object that holds data:

- the **well-known kinds**, by kind, with no declaration needed —
  `PersistentVolumeClaim`, `PersistentVolume`, `StatefulSet`, `Secret`,
  `Namespace`, `CustomResourceDefinition`;
- objects of a **workload the env declared stateful**, in practice a
  `forge.ManagedDatabase` — a `postgresql.cnpg.io/v1 Cluster` is just a CR to
  Kubernetes, so its kind says nothing.

It is stamped **inside the bundle** rather than configured on the
Kustomization, which means every reconciler that ever applies those bytes
honours it, it is sealed under the bundle's digest, and no future
pointer-writing path has to remember it.

It does **not** stop you, forge, or `kubectl delete` from removing the object.
It removes one actor's ability to do it as a side effect of a render.

## The wait, and the exit codes

A Kustomization is **converged** when both hold:

```
lastAppliedRevision == the bundle digest    AND    Ready=True
```

Neither alone is the answer, and the two traps are different:

- **Ready=True alone** is the *previous* release reporting success. A
  Kustomization that converged to v5 and has not yet fetched v6 is Ready the
  whole time, so a readiness-only wait passes instantly for a release that was
  never applied.
- **the revision alone** is an apply that was admitted and then failed its
  health check — `wait: true` makes Flux health-check what it applied, so Ready
  goes False while the revision has already advanced.

`forge env deploy` polls every Kustomization it wrote:

| Exit | Meaning | What to do |
|---|---|---|
| **0** | every Kustomization reports the digest and `Ready=True` | the release is live |
| **1** | a Kustomization reports `Ready=False` for a real reason | read it — the message is **Flux's own** |
| **8** | the budget expired while still progressing | retry the **wait**, not the deploy |

`Progressing` and `DependencyNotReady` are **not** failures: every healthy
deploy passes through them for as long as its rollout takes.

```bash
forge env deploy dev-k8s --yes          # record, point, wait
forge env deploy dev-k8s --no-wait      # point and return; Flux still converges
forge env deploy dev-k8s --dry-run      # print the exact pointer, write nothing
forge env status dev-k8s                # convergence, read from the cluster
```

`forge env status` reports `converged` / `failed` / `progressing` / `pending`.
The pair worth distinguishing is the last two: **`pending` means no pointer is
in the cluster at all** — nobody has deployed, or somebody deleted it — so the
fix is a deploy, not a wait.

## Debugging

forge relays Flux's verdict rather than re-deriving one, so Flux's own tools
are the next step and they will agree:

```bash
kubectl --context <ctx> -n flux-system get kustomizations,ocirepositories
kubectl --context <ctx> -n flux-system describe kustomization <env>
```

Things worth checking in order:

- **`pending`** — no pointer. Has `forge env deploy` run? Did somebody
  `kubectl delete` it?
- **the OCIRepository is not Ready** — a fetch problem. Check the `url` is the
  *cluster's* name for the registry (`host.k3d.internal`, not `localhost`), and
  that the digest is actually in that registry.
- **the Kustomization applied nothing and is Ready** — the `spec.path` is not
  in the artifact. This is the failure that reports success, which is why forge
  builds one Kustomization per path the bundle *says it carries*
  (`BundleDoc.cluster_paths`) rather than per declared cluster.
- **`forge cluster up` did not install Flux** — does the env declare a chart
  that already provides it? That is the shared-Flux rule working.
