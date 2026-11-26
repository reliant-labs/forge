---
name: ml/discover
description: "`forge ml discover` — read a GPU cluster read-only through your own kube context (no control plane, no project needed) and propose the env's ml.k: the accelerators (NVIDIA first; AMD, TPU, Neuron, Gaudi at allocation level), the ML components already running (GPU Operator, DCGM exporter, Kueue, JobSet, KServe, KubeRay, ...) to ADOPT, what is absent (every slot optional: an install is suggested only where a finding calls for one), and the recommendations worst first. The exact read set (get/list only, never Secrets, pods/log or nodes/proxy), the human output, the forge.dev/ml-discover/v1 JSON document and its schema, the coverage model (one row per signal: DCGM field groups, NPD and GPU rules, kubelet/apiserver tracing, collectors, continuous profiling, workload tracing; collected/partial/missing/unknown and how it was determined), ConfigMaps read by name and redacted, the opt-in --probe and the pods/proxy grant it needs, --write and the diff over an existing ml.k, `observed` and why the KCL refuses a duplicate install, Ray as adopt-only, dismissing a finding with forge.RecommendationDecision, wiring ml.k into main.k, snapshots, and testing without GPUs (run-ai/fake-gpu-operator on k3d). Use before declaring an MLStack, when a finding needs explaining, or to grant a team the read-only identity discovery needs."
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

The read has two phases. The first lists, paged 500 objects at a time; the
second GETs, **by exact name**, the ConfigMaps the found components reference.
This table is printed by `forge ml discover --help` from the one place the
collector reads it (`internal/mlcollect`), and a test fails any read outside it,
or any read with another verb:

| Read | Verb | Kept | For |
|---|---|---|---|
| `/version`, `/api`, `/apis` | get | full | server version, served API groups and versions (DRA's `resource.k8s.io`) |
| `customresourcedefinitions.apiextensions.k8s.io` | list | names only (metadata API) | which CRDs are installed; CRD schemas are never fetched |
| `nodes` | list | full | accelerator capacity/allocatable, labels, conditions (NPD's included), images, nodeInfo |
| `pods` (all namespaces) | list | full | exporters, profilers, serving pods by image; args and env names (tracing hooks); accelerator requests |
| `daemonsets`, `deployments`, `statefulsets` (apps) | list | full | device plugins, exporters, NPD and its monitor args, operators, collectors |
| `jobs` (batch), `horizontalpodautoscalers` (autoscaling/v2) | list | full | training jobs, how serving scales |
| `resourceslices.resource.k8s.io` | list | count | whether a DRA driver publishes devices |
| each installed ML CRD (Kueue, JobSet, Trainer, KubeRay, KServe, Inference Extension, KEDA, Karpenter, Prometheus Operator, GMP, OTel Instrumentation, ...) | list | count (metadata API) | a CRD with objects vs a leftover |
| `clusterpolicies.nvidia.com`, `deviceconfigs.amd.com`, `opentelemetrycollectors.opentelemetry.io` | list | redacted | whether the GPU Operator runs its DCGM exporter and which counters ConfigMap it names; a collector's receivers and pipelines |
| `configmaps` | **get by name, never list** | redacted | a DCGM counters CSV, NPD monitor JSON, a collector's config, `kube-system/kubeadm-config` and `kube-system/kubelet-config` (tracing) |
| `pods/proxy` | get, **only with `--probe`** | names | an exporter's `/metrics`: metric names and label keys, never values |

What is kept, and what never is:

- every object loses its `managedFields` and every annotation;
- container env **values** are blanked (names and `valueFrom` stay), except
  `DCGM_EXPORTER_COLLECTORS`, `DCGM_EXPORTER_KUBERNETES` and `DCGM_EXPORTER_INTERVAL`;
- **redaction** of ConfigMap data and custom object specs: JSON or YAML is kept
  with every value under a key matching
  `token|password|passwd|secret|key|auth|authorization|header|credential|bearer|cookie`
  (any case, any depth, nested YAML strings included) replaced by `REDACTED`; a
  DCGM counters CSV is kept minus its comments; a Grafana Alloy config is kept
  as its component block headers alone (`pyroscope.ebpf "default" {}`), every
  attribute dropped; anything else (a script, a PEM block, binary data) is
  dropped, its key kept with a `DROPPED` marker; a data key that itself matches
  is replaced whole;
- container **command and args** keep their shape (model URLs, config paths and
  flags are what discovery reads) with every credential in them replaced by
  `REDACTED`. Command and args are redacted together, as the one argv the
  kubelet runs, and a shell script in one argument is split into its words and
  commands. Replaced: the value of a flag or variable named like a credential
  (`--token=…`, `--api-key …` even when the flag ends the command and the value
  starts the args, `HF_TOKEN=…` in a script, a presigned URL's signature), a
  `user:password` (`curl -u`, `--user=`), any `-H`/`--header` value, a short
  password flag's value (`-p …` unless a port or a path, `mysql -p…`,
  `redis-cli -a`, `openssl -k`/`-passin`), a sensitive header written into a
  command (`Authorization: …`), a URL's password and any token of a well-known
  vendor format (`sk-…`, `hf_…`, `ghp_…`, `AKIA…`, a JWT). A custom object's
  string lists (an operator's `args`) are redacted by the same rules; HTTP
  probe and lifecycle hook header values are replaced, their names kept;
- the snapshot file is written readable by its owner only (`0600`);
- it **never** reads Secrets, `pods/log`, `pods/exec`, `nodes/proxy` (effectively
  exec on the node), never lists ConfigMaps, and uses no write verb.

A refused read is reported under "could not see" and in `warnings`, never
fatal: discovery says what it could not see rather than claim a component is
absent. Only an unreachable API server fails the run (exit 2).

To run it under a dedicated identity, grant exactly the read set: list on the
resources above, get on `configmaps` and on `/version`, `/api`, `/apis` and their
children. RBAC cannot pin ConfigMap names (they depend on what discovery finds),
so the grant is `get` on configmaps cluster-wide; the collector never lists them.
The k3d e2e runs discovery as such a ServiceAccount and requires zero refused
reads.

### `--probe` widens the grant

`--probe` additionally GETs each exporter's `/metrics` through the API server's
pod proxy (`/api/v1/namespaces/<ns>/pods/<pod>:<port>/proxy/metrics`): at most one
running pod per component and node pool, 10 s and 8 MiB each, keeping only metric
family names and label keys. It is the strongest evidence of which DCGM fields an
exporter really exports, and whether its samples carry `pod`/`namespace` labels.
It needs `get pods/proxy`, which reaches **any port of any pod**, so it is off
unless asked and kept out of the base grant. A failed probe (forbidden, timeout,
not the Prometheus text format) is recorded on that probe and the coverage falls
back to the next source.

## Coverage

`coverage[]` has one row per observability signal an ML cluster should have,
always (a gap is a row, never an absence), for a console's coverage-gap view:

| Category | Signals |
|---|---|
| accelerator | `accel.utilisation`, `accel.profiling` (DCGM_FI_PROF_*), `accel.memory`, `accel.xid`, `accel.ecc`, `accel.retired-pages`, `accel.row-remap`, `accel.nvlink`, `accel.pcie`, `accel.throttle`, `accel.power-thermal`, `accel.pod-attribution` |
| node-health | `node.health`, `node.health.gpu-rules` (a monitor that turns Xid/NVRM kernel lines into a condition) |
| tracing | `kubelet.tracing`, `apiserver.tracing`, `tracing.training`, `tracing.inference` |
| telemetry | `telemetry.collector`, `telemetry.otlp`, `telemetry.k8s-events`, `telemetry.kubeletstats`, `telemetry.container-logs` |
| profiling | `profiling.continuous` (Parca, Pyroscope/Alloy, OTel eBPF profiler, Elastic, Datadog, ...), `profiling.gpu` |
| inference | `metrics.inference` |

Each row: `status` (`collected`, `partial`, `missing`, `unknown`,
`not-applicable`), `determined_by` (strongest first: `probe`, `config`, `args`,
`version-default`, `presence`, `unreadable`), `sources`, `expected` / `present` /
`missing` parts (DCGM fields, receivers, condition types), `nodes_covered` /
`nodes_total`, `versions`, a `note` when the status needs one (a managed control
plane's tracing config is unreadable, so `unknown`), and the `findings` the gap
raised. For DCGM the field set comes from the probe, else the counters CSV the
exporter mounts, else the default CSV of the exporter version found, else
presence alone. `components[].config` carries what discovery read of each
component's configuration: DCGM fields, NPD monitors, conditions and reasons, a
collector's receivers, processors, exporters and pipelines, and settings.

## What it prints

A table of sections, then the findings, then the proposal:

```
cluster        gke_acme_us-central1_gpu (v1.31.2-gke.1000, gke)
accelerators   8 × NVIDIA-H100-80GB-HBM3 (nvidia.com/gpu) on 1 node (pool h100; driver 550.127.05; 2 allocated)
               FOUND gpu-operator v24.9.0 (Deployment/gpu-operator/gpu-operator, 1 pod) — adopt
gpu metrics    FOUND dcgm-exporter 3.3.9 via gpu-operator (DaemonSet/gpu-operator/nvidia-dcgm-exporter :9400) — adopt
node health    none found — optional; node-problem-detector if you want bad nodes detected (see finding no-node-health)
queueing       FOUND kueue v0.10.1 (Deployment/kueue-system/kueue-controller-manager, 5 objects) — adopt
training       none found — optional; kubeflow-trainer only if you need a training operator; kuberay is adopt-only
serving        none found — optional; kserve is adopted when present, never installed by default
monitoring     none found — optional
image pulls    none found — optional (no spegel or dragonfly pull cache)
```

After the sections, a `coverage` block lists every signal by category, gaps in
upper case, with how each was determined and the parts missing:

```
coverage       26 signals: 6 collected, 3 partial, 11 missing, 3 unknown, 3 not-applicable
  accelerator (nvidia)
    collected accel.utilisation         version-default DaemonSet/gpu-operator/nvidia-dcgm-exporter; 1/1 nodes; dcgm-exporter 3.3.9-3.6.1-ubuntu22.04
                                        the field set is the found release's default counters file; a custom file would change it
    PARTIAL   accel.profiling           version-default DaemonSet/gpu-operator/nvidia-dcgm-exporter; 1/1 nodes; dcgm-exporter 3.3.9-3.6.1-ubuntu22.04
                                        missing PROF_SM_ACTIVE, PROF_SM_OCCUPANCY
                                        finding dcgm-profiling-off
  tracing
    unknown   kubelet.tracing           unreadable
                                        GKE manages the kubelet configuration; it is not readable from the Kubernetes API
```

The sections are accelerators (with the operator and device plugin), gpu metrics,
node health, queueing, training, serving, autoscaling (only when found),
monitoring and image pulls. `FOUND … — adopt` is someone else's component;
`— installed by forge` carries `forge.dev/component-owner`; `CRD ONLY` is CRDs
with no controller running. An empty slot reads `none found — optional`: every
slot is opt-in, piece by piece, so the line names what forge would suggest and,
when a finding fired for that slot, `(see finding <id>)`. Findings follow worst first: id, severity, title,
evidence (objects as kind/namespace/name), the fix, and dismissed findings last,
marked with their reason and expiry.

`--json` prints the **forge.dev/ml-discover/v1** document on stdout and nothing
else: `schema`, `context`, `server_version`, `collected_at`, `cloud`,
`accelerators[]`, `machines[]`, `components[]` (each with `config`),
`coverage[]`, `findings[]`,
`proposal {path, written, exists, ml_k, diff}`, `snapshot`, `warnings[]`. Its
JSON Schema is `forge project schema ml-discover`. Fields are only ever added.

Exit codes: 0 the read and the proposal succeeded (findings are not failures); 1
bad input (a flag, a snapshot file, a kube context the kubeconfig lacks); 2 could
not look (the cluster is unreachable).

## Adopt, opt in, never a duplicate

The policy the proposal follows: **discover, adopt what is there, and opt in to the
rest piece by piece**. Nothing is all or nothing: every slot is optional, an empty
one is not an error, and an install is suggested (commented out) only where a
finding calls for one, from pinned upstream charts declared in KCL. forge never
upgrades, reconfigures or removes a component it did not install. The charts it
reaches for when you opt in to an empty slot: Kueue with JobSet (queueing),
Kubeflow Trainer (training), node-problem-detector (node health), the vendor's
exporter (gpu metrics). KServe is
adopted when present and never installed by default. **KubeRay is adopt-only**: the
KCL refuses `forge.Install {kind = "kuberay"}` unless it states a `reason`.

The proposed ml.k holds one `forge.MLStack` for the cluster: every found component
as a `forge.Adopt` (kind, namespace, version and a note: a record of what runs),
each suggested `forge.Install` written but **commented out** for you to review,
and `observed`: every component kind discovery found running. The KCL refuses an
`Install` of a kind listed in `observed`.

An adopted exporter discovery can scrape is also written as a plain
`forge.Telemetry` in a separate `ml_telemetry` list, for the env's
`observability.telemetry`:

```python
ml_telemetry = [
    forge.Telemetry {
        service_name = "dcgm-exporter", scope = "platform", cluster = "gke_acme_us-central1_gpu"
        namespaces = ["gpu-operator"], selector = {"app": "nvidia-dcgm-exporter"}
        metrics = [forge.MetricsScrape {port = 9400, allow = "DCGM_FI_DEV_GPU_UTIL|DCGM_FI_DEV_FB_USED"}]
    }
]
```

`allow` is the field set discovery found the exporter serving, else the
families the Machines view and the rules read; names taken from an exporter's
docs rather than a live node are marked unverified.

`observed` is the guard against the classic failure: a second DCGM exporter on
nodes that already run one (the GPU Operator's, or GKE's managed DCGM) breaks both
exporters' profiling counters. A GPU Operator whose ClusterPolicy disables its
exporter does not count as a DCGM exporter; discover lists `dcgm-exporter` in
`observed` only when one runs.

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
    # When the proposal adopts an exporter it can scrape:
    observability = forge.Observability {
        telemetry = ml.ml_telemetry   # beside any telemetry the env already lists
    }
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

Its exporter serves `DCGM_FI_DEV_GPU_UTIL`, `DCGM_FI_DEV_FB_USED` and
`DCGM_FI_DEV_FB_FREE` on `:9400`, so `forge ml discover --probe` shows
`accel.utilisation` collected (by probe) and `accel.profiling` missing. forge's own
e2e (`TestE2EMLDiscoverAgainstFakeGPUs`) asserts exactly that, then plants a
second exporter on the same nodes and expects `accel-exporter-duplicate`.
What only real hardware proves (profiling counters, real Xid and ECC events, MIG
reconfiguration) is out of reach of any fake.

## Not yet

Instrumenting what runs on the GPUs (OTLP from vLLM and Triton, metric allow lists,
`/dev/shm`, probes) is a later slice of the ML work; `forge ml discover` today
reads and proposes, and its findings say what to change.
