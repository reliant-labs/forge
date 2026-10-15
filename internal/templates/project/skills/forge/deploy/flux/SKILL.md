---
name: deploy/flux
description: Flux in forge — what forge no longer does (install Flux into a deploy target, write a desired-state pointer at one), who applies a real env instead, and the one thing that remains (`forge.flux_chart()`, the pinned chart a consumer declares when it runs a Flux of its own). Use when a skill or an old note says an env without a control plane reconciles through an in-cluster Flux.
---

# Flux

**forge never installs Flux into a deploy target and never writes a
desired-state pointer (OCIRepository + Kustomization) at one.** An earlier
design did both for an env with no control plane; #461 removed it and the code
has since been deleted. If a note says `forge cluster up` installs
`source-controller`/`kustomize-controller`, or that `forge env deploy` "writes the
pointer and waits", it is describing a path that does not exist.

## Who applies a real environment

| the env declares | applied by |
|---|---|
| `forge.ControlPlane` and clusters bound to connected clusters (`forge cluster connect`) | the control plane's hub Flux converges the recorded bundle; forge records and waits (`deploy/cluster-connect`) |
| `hub_converges = [units]` | the hub converges **those delivery units** from the recorded bundle; forge applies the rest directly. This is how a real env moves onto Flux one unit at a time (`deploy/shape`) |
| `applier = "forge"` | **forge**, directly, for now: no unit has moved to the hub yet. A transitional state, the same as `hub_converges = []`; every real env ends on the hub (`deploy/shape`) |
| `lifecycle = "local"` / `"ephemeral"` | forge, directly: the clusters are scratch it creates and deletes |
| none of these | forge, directly, with one notice that this will be refused (F-REFUSE-DIRECT) once an env must say which of the above it is |

Direct apply is where every env starts and where only dev and ephemeral envs end.
For a real env it is a stage, not a mode: list its units in `hub_converges` as
they move, and the declaration ends when the list is complete.

A Flux on a cluster exists only because a control plane declared one. Flux is a
platform dependency of the control plane, not something forge puts in your
cluster as a side effect.

## What remains: `forge.flux_chart()`

An env that needs Flux for its own reasons (control-plane's e2e runs one to
reconcile hosted workloads) declares forge's pinned chart as an ordinary bootstrap
`forge.HelmChart`:

```kcl
helm_charts = [
    forge.flux_chart(values = {
        # Merged OVER forge's posture, last-wins.
        sourceController.serviceAccount.annotations = {
            "iam.gke.io/gcp-service-account" = "flux@proj.iam.gserviceaccount.com"
        }
    })
    # Multi-cluster: re-target it like any other chart.
    # forge.flux_chart(cluster = _control_plane)
]
```

`forge cluster up <env>` installs the env's declared bootstrap charts (Flux
first, then charts that ship a CRD bundle, then the rest) into the env's
forge-managed k3d clusters; `forge env deploy <env> --target <chart>` installs
just that one.

The chart is **version-pinned with no knob**, and it runs **two controllers, not
six** (`source-controller`, `kustomize-controller`; helm, notification and the two
image controllers are off, a security decision rather than a resource one).

### Hardened and sized by default

A Flux that reconciles bundles for more than one author is a shared control
point, so the defaults are the hardened ones. Everything is overridable.

| Default | What it does | Turn it off / change it |
|---|---|---|
| `rbac = "namespaced"` | Flux CRs cluster-wide; leader election in `flux-system`; impersonation (pinned by `resourceNames` to one ServiceAccount) and Secret reads only where a RoleBinding names `flux-tenant-reconciler` / `flux-tenant-source`. Multitenancy on, no cluster-admin binding | `rbac = "pinned"` (cluster-wide reads, impersonation pinned), `rbac = "chart"` (the chart's own), `impersonate_service_account` |
| `flux_namespace_bindings(ns)` | the two RoleBindings a namespace holding Flux objects needs | declare per namespace |
| `DisableConfigWatchers` | config watcher off (it needs a cluster-wide Secret list) | `feature_gates = {DisableConfigWatchers = False}` with `rbac = "pinned"` |
| `CancelHealthCheckOnNewRevision` | a fix-forward revision cancels a stuck health check | `feature_gates = {...}` |
| `concurrency = 16`, `concurrency_ssa = 8`, `source_concurrency = 8`, `--requeue-dependency=10s` | one 13-minute rollout holds 1 of 4 default workers; this keeps a slow rollout from blocking everyone | the three arguments; or `values` |
| requests/limits, `priority_class_name = "forge-delivery"` | sized starting points; the PriorityClass is declared with the chart | `values`; `priority_class_create = False` to name a class that exists |
| NetworkPolicies | `allow-egress` only: **ingress to every `flux-system` pod only from `flux-system` pods** (an invariant, tested); no `allow-webhooks`; `allow-scraping` only for `scrape_namespaces` | `scrape_namespaces = ["observability"]`; `network_policies = "chart"` |
| `pod_security = "restricted"` | Pod Security enforce/warn/audit on `flux-system` | `pod_security = "baseline"` or `""` |

Opt-in placement, for a dedicated delivery node pool (forge has no node-pool
primitive, so provision it yourself, tainted `reliant.dev/pool=delivery-v1:NoSchedule`):

```kcl
forge.flux_chart(
    placement = forge.flux_placement_delivery_v1(
        runtime_class_name = forge.FLUX_RUNTIME_CLASS_GVISOR,   # only where the target has it
    )
)
```

`runtime_class_name` is never defaulted on: a pod naming a RuntimeClass the
cluster lacks never schedules. The chart has no `runtimeClassName` value, so it
reaches the pods through `HelmChart.patches` (`"Deployment/kustomize-controller" = {...}`),
which fails the render, naming the candidates, if a chart bump renames a Deployment.
The same field patches any chart's rendered objects.

**gVisor.** GKE Sandbox's documented gVisor limits (no privileged containers,
no hostPath, no port-forward, seccomp/AppArmor unsupported) are not features the
two controllers use: they run non-root with a read-only root filesystem and
`/tmp` / `/data` emptyDirs, listen on 8080/9090/9440 and call the API server.
Compatibility has not been run under a real sandbox here: verify on the target
(`kubectl -n flux-system get pods -o jsonpath='{.items[*].spec.runtimeClassName}'`,
a reconcile to a new revision, and `kubectl top`) before relying on it, and fall
back to the pool alone if it fails. Patching and CVE watch:
`docs/runbooks/flux-patch-and-upgrade.md`.

> **Chart version ≠ Flux version.** There is no fluxcd-published chart; forge
> uses the community chart at `oci://ghcr.io/fluxcd-community/charts/flux2`, whose
> version has its own lineage. Chart **2.19.1** is Flux **v2.9.5**. On a bump,
> re-derive it, do not increment:
> `helm show chart oci://ghcr.io/fluxcd-community/charts/flux2 --version <v> | grep appVersion`

### Namespace labels, and what a re-apply prunes

`flux-system` is created by forge (`helm template` never emits a chart's
Namespace), so its metadata is declared on the chart:

```kcl
forge.flux_chart() | {
    namespace_labels = {
        "pod-security.kubernetes.io/enforce" = "restricted"
        "pod-security.kubernetes.io/enforce-version" = "latest"
    }
}
```

They converge under server-side apply: a changed value is updated, a key you
stop declaring is removed, a key another manager set is left alone.

A re-apply also **deletes the objects the chart stopped rendering**: every object
is stamped `forge.dev/chart-owner=<project>.<env>`, and after the chart's apply
succeeds the ones carrying that stamp that the new render lacks are removed. It
keeps (and reports) CRDs, Namespaces and stateful kinds (PV, PVC, StatefulSet,
Secret); anything annotated `kustomize.toolkit.fluxcd.io/prune: disabled`; and
anything with `ownerReferences`. `prune = False` turns it off for a chart.
Objects dropped before forge began stamping are never deleted: remove those by
hand once.

Detection of "this env already provides Flux" is **by chart reference, never by
name**: a chart's `name` is the `--target` selector its author picks freely, so a
name heuristic would let a naming choice decide whether a colliding second Flux
was installed. Declare `forge.FLUX_CHART_OCI` and you are recognised, whatever you
call the chart.
