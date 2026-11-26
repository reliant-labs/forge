---
name: ml/discover
description: "`forge ml discover` — read a GPU cluster read-only through your own kube context (no control plane, no project needed) and propose the env's ml.k: the accelerators (NVIDIA first; AMD, TPU, Neuron, Gaudi at allocation level), the ML components already running (GPU Operator, DCGM exporter, Kueue, JobSet, KServe, KubeRay, ...) to ADOPT, what is missing to INSTALL, and the recommendations worst first. The exact read set (get/list only, never Secrets, pods/log or nodes/proxy), the human output, the forge.dev/ml-discover/v1 JSON document and its schema, --write and the diff over an existing ml.k, `observed` and why the KCL refuses a duplicate install, Ray as adopt-only, dismissing a finding with forge.RecommendationDecision, wiring ml.k into main.k, snapshots, and testing without GPUs (run-ai/fake-gpu-operator on k3d). Use before declaring an MLStack, when a finding needs explaining, or to grant a team the read-only identity discovery needs."
---

# Discover a GPU cluster: `forge ml discover`

```bash
forge ml discover                                 # the kubeconfig's current-context
forge ml discover --context gke_acme_us-central1_gpu --env prod
forge ml discover --env prod --write              # write deploy/kcl/prod/ml.k
```

It reads the cluster **read-only, through your own kube context**, so it works on
a private cluster your laptop can reach. It needs no control plane, no sign-in and
no forge project. It never writes to the cluster, and it writes a file only with
`--write`.

## What it reads

Every read is a `list` (or a discovery `get`), paged 500 objects at a time. This
table is printed by `forge ml discover --help` from the one place the collector
reads it (`internal/mlcollect`), and a test fails any read outside it:

| Read | Kept | For |
|---|---|---|
| `/version`, `/api`, `/apis` | full | server version, served API groups (DRA's `resource.k8s.io`) |
| `customresourcedefinitions.apiextensions.k8s.io` | names only (metadata API) | which CRDs are installed; CRD schemas are never fetched |
| `nodes` | full | accelerator capacity/allocatable, labels, conditions, images, nodeInfo |
| `pods` (all namespaces) | full | exporters and serving pods by image, accelerator requests |
| `daemonsets`, `deployments`, `statefulsets` (apps) | full | device plugins, exporters, operators, controllers |
| `jobs` (batch), `horizontalpodautoscalers` (autoscaling/v2) | full | training jobs, how serving scales |
| `resourceslices.resource.k8s.io` | count | whether a DRA driver publishes devices |
| each installed ML CRD (Kueue, JobSet, Trainer, KubeRay, KServe, Inference Extension, KEDA, Karpenter, Prometheus Operator, GMP, ...) | count (metadata API) | a CRD with objects vs a leftover |
| `clusterpolicies.nvidia.com`, `deviceconfigs.amd.com` | full | whether the GPU Operator runs its DCGM exporter |

Before anything is kept, every object loses its `managedFields`, every
annotation, and the literal value of every container env var (the name and any
`valueFrom` reference stay). It **never** reads Secrets, `pods/log`,
`pods/exec`, `nodes/proxy` (effectively exec on the node), and it uses no write
verb. A refused list is reported under "could not see" and in `warnings`, never
fatal: discovery says what it could not see rather than claim a component is
absent. Only an unreachable API server fails the run (exit 2).

To run it under a dedicated identity, grant exactly the read set: list on the
resources above, get on `/version`, `/api`, `/apis` and their children. Nothing
more is needed (the k3d e2e runs discovery as such a ServiceAccount and requires
zero refused reads).

## What it prints

A table of sections, then the findings, then the proposal:

```
cluster        gke_acme_us-central1_gpu (v1.31.2-gke.1000, gke)
accelerators   8 × NVIDIA-H100-80GB-HBM3 (nvidia.com/gpu) on 1 node (pool h100; driver 550.127.05; 2 allocated)
               FOUND gpu-operator v24.9.0 (Deployment/gpu-operator/gpu-operator, 1 pod) — adopt
gpu metrics    FOUND dcgm-exporter 3.3.9 via gpu-operator (DaemonSet/gpu-operator/nvidia-dcgm-exporter :9400) — adopt
node health    MISSING node-problem-detector — install (forge's default)
queueing       FOUND kueue v0.10.1 (Deployment/kueue-system/kueue-controller-manager, 5 objects) — adopt
training       MISSING kubeflow-trainer — install (forge's default); kuberay is adopt-only
serving        none found (kserve is adopted when present, never installed by default)
monitoring     none found
image pulls    none found (no spegel or dragonfly pull cache)
```

The sections are accelerators (with the operator and device plugin), gpu metrics,
node health, queueing, training, serving, autoscaling (only when found),
monitoring and image pulls. `FOUND … — adopt` is someone else's component;
`— installed by forge` carries `forge.dev/component-owner`; `CRD ONLY` is CRDs
with no controller running. Findings follow worst first: id, severity, title,
evidence (objects as kind/namespace/name), the fix, and dismissed findings last,
marked with their reason and expiry.

`--json` prints the **forge.dev/ml-discover/v1** document on stdout and nothing
else: `schema`, `context`, `server_version`, `collected_at`, `cloud`,
`accelerators[]`, `machines[]`, `components[]`, `findings[]`,
`proposal {path, written, exists, ml_k, diff}`, `snapshot`, `warnings[]`. Its
JSON Schema is `forge project schema ml-discover`. Fields are only ever added.

Exit codes: 0 the read and the proposal succeeded (findings are not failures); 1
bad input (a flag, a snapshot file, a kube context the kubeconfig lacks); 2 could
not look (the cluster is unreachable).

## Adopt, install, never a duplicate

The policy the proposal follows: **discover, adopt what is there, install only what
is missing**, from pinned upstream charts declared in KCL. forge never upgrades,
reconfigures or removes a component it did not install. The defaults it suggests
when a slot is empty: Kueue with JobSet (queueing), Kubeflow Trainer (training),
node-problem-detector (node health), the vendor's exporter (gpu metrics). KServe is
adopted when present and never installed by default. **KubeRay is adopt-only**: the
KCL refuses `forge.Install {kind = "kuberay"}` unless it states a `reason`.

The proposed ml.k holds one `forge.MLStack` for the cluster: every found component
as a `forge.Adopt` (with its namespace, selector and metrics port, so its metrics
are scraped), each suggested `forge.Install` written but **commented out** for you
to review, and `observed`: every component kind discovery found running. The KCL
refuses an `Install` of a kind listed in `observed`. That is the guard against the
classic failure: a second DCGM exporter on nodes that already run one (the GPU
Operator's, or GKE's managed DCGM) breaks both exporters' profiling counters. A
GPU Operator whose ClusterPolicy disables its exporter does not count as a DCGM
exporter; discover lists `dcgm-exporter` in `observed` only when one runs.

## --write, --out and the diff

| Flags | The ml.k goes to |
|---|---|
| none | printed only |
| `--env <env>` (inside a project) | `deploy/kcl/<env>/ml.k`: printed, or its diff against the file there |
| `--out <file>` | that file (the only way outside a project) |
| `--write` with either | written; over an existing file the diff is printed, then the file is written (it is in git: review it with `git diff`) |

`--write` with no place to write is refused, and nothing is ever written without
it. Then wire the file into the env once, in `deploy/kcl/<env>/main.k`:

```python
import .ml

bundle = forge.Bundle {
    # ...
    ml_stacks = ml.ml_stacks
}
```

## Dismissing a finding

A finding you have decided to live with is dismissed in the env, with a reason
and an expiry, never silenced:

```python
decisions = [forge.RecommendationDecision {
    id = "kubelet-tracing-off", decision = "dismiss"
    reason = "GKE does not expose kubelet tracing config", expires = "2027-06-01"
}]
```

`forge ml discover --env <env>` renders the env and applies the decisions of the
`ml_stack` whose `cluster` is the context it read. A dismissed finding is still
listed, marked, and sorted last; it applies again the day after `expires`. An env
that does not render says so in `warnings` and applies none.

## The rules

`forge ml discover --rules` lists every rule (id, severity, vendor, what it detects,
the fix); `--rules --json` prints `{"rules": [...]}`. A rule id is stable: a
`RecommendationDecision` names it. Each finding's `fix.kind` is `kcl` (a change to
ml.k, in `fix.kcl_diff`), `command` (a setting outside forge's declarations, such
as a node-pool flag, in `fix.command`) or `manual` (something only the owner of a
component can do).

## Snapshots

Inside a project every live read is saved to
`.forge/state/ml-inventory-<context>.json` (the context made a file name), for the
lint that refuses an `Install` of a component the inventory shows present.
`--save-snapshot <file>` writes it anywhere; `--from-snapshot <file>` runs discovery
on a saved read (forge.dev/ml-snapshot/v1) with no cluster, which is how fixtures
and bug reports are replayed.

## Testing without GPUs

[run-ai/fake-gpu-operator](https://github.com/run-ai/fake-gpu-operator) makes a k3d
node look like a GPU node: label it, install the chart, and it advertises
`nvidia.com/gpu` with GFD-style labels and runs a DCGM-shaped exporter on `:9400`.

```bash
kubectl label node <node> run.ai/simulated-gpu-node-pool=default
kubectl create namespace gpu-operator
kubectl label namespace gpu-operator pod-security.kubernetes.io/enforce=privileged
helm upgrade -i gpu-operator oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator \
  --namespace gpu-operator --version 0.2.1 --wait
forge ml discover --context k3d-<cluster>
```

forge's own e2e (`TestE2EMLDiscoverAgainstFakeGPUs`) does exactly this, then
plants a second exporter on the same nodes and expects `accel-exporter-duplicate`.
What only real hardware proves (profiling counters, real Xid and ECC events, MIG
reconfiguration) is out of reach of any fake.

## Not yet

Instrumenting what runs on the GPUs (OTLP from vLLM and Triton, metric allow lists,
`/dev/shm`, probes) is a later slice of the ML work; `forge ml discover` today
reads and proposes, and its findings say what to change.
