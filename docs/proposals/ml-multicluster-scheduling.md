# Multi-cluster scheduling for ML: placement across clusters and regions, open source

**Status:** proposal. Nothing here is built. It answers the owner's input of
2026-11-26 (below) and takes the direction of 15:45Z the same day: the stack, the
scoring terms, explanations, the serving floor, fencing and dataset locality
([section 3](#3-direction-taken-owner-2026-11-26)). The choices that remain are
open owner decisions in [section 15](#15-open-owner-decisions), numbered from 33
to continue [reliant-for-ml.md](reliant-for-ml.md) (decisions 23–32).
**Date:** 2026-11-26
**Feeds from:** [reliant-for-ml.md](reliant-for-ml.md) (resilience §4, hooks §5,
settled decisions 23–32), [ml-workloads.md](ml-workloads.md) (the cluster, the ML
stack, `forge.Training`, the testing approach), and the ML spec's
[object model](ml-spec/00-object-model.md) (run → phase → attempt).
**Related:** `docs/design/per-workload-placement.md` (placement is a property of
each workload), [start-anywhere.md](start-anywhere.md),
[autoscaling.md](autoscaling.md).

> **Owner input (2026-11-26, verbatim):** "very likely our control plane needs to
> support multi-cluster workloads, since gpu trains might be resource
> constrained, and need to deploy across regions. in fact if we need to come up
> with a special multi-cluster scheduler that could also be our secret sauce
> (although i'd like to open source that....)"

Unverified upstream claims are marked "(verify)". They are listed again in the
[appendix](#appendix-a-verification-list), to be checked against pinned versions
before they become rendered configuration, rules or user-facing copy.

## Summary

GPU capacity is regional, often zonal, and scarce. A team with H100s in
`us-central1`, a reservation in `europe-west4` and an on-prem pool waits hours in
one queue while another sits idle. Its data, checkpoints and images live in one
of those places, so moving work has a cost. Each cluster can say what it has;
no tool today says **where a given job will finish soonest and cheapest, and why**.

The proposal:

1. **The atom never splits.** One synchronous training attempt (all of its
   data-, tensor-, pipeline- and expert-parallel ranks) runs in one cluster, on
   one RDMA fabric ([section 1](#1-what-can-and-cannot-span-clusters)). We place
   whole attempts, phases, eval jobs, sweep trials and inference replica sets,
   never a slice of a collective.
2. **Dispatch is adopted, not built.** Each worker cluster runs Kueue (with
   Kubeflow Trainer `TrainJob` for training). A management cluster runs Kueue's
   **MultiKueue**, which reserves quota, copies the workload to the clusters it is
   told about, and guarantees that only one of them runs it.
3. **The placement brain is ours, and open source.** A small controller plugs into
   MultiKueue's **external dispatcher** seam: it writes
   `Workload.status.nominatedClusterNames` (released in Kueue v0.13; verify on the
   pinned version). It filters clusters on hard eligibility, then scores the
   feasible ones on **expected completion time plus total cost**, writes a reason
   for every decision, and records predicted against actual.
4. **The secret sauce is the joined signals, not the algorithm.** Queue-wait
   forecasts, measured throughput of this recipe on each hardware shape,
   restart-time breakdowns, bad-node history, spot reclaim rates, checkpoint and
   data locality, egress prices, release and eval gates, budgets. Only Reliant
   joins these, per org, from the training-run ledger and the telemetry it
   already collects. Without them the controller still runs, on what each
   cluster can say about itself.
5. **Failover moves an attempt, not a job's state machine.** When capacity is
   lost (spot reclaim, a cluster gone, preemption with no re-admission in sight)
   and the expected wait outweighs the cost of moving, the next attempt resumes
   from a replicated checkpoint in another cluster, with the image and weights
   pre-pulled and the old attempt **fenced** so it cannot publish over its
   successor.
6. **Everything is declared** in `deploy/kcl/<env>/`: cluster pools, placement
   policy, residency, budgets, the serving capacity floor and checkpoint
   replication. forge renders the Kueue and MultiKueue configuration and the
   controller's settings. The hub's Flux delivers them.

The recommendation is the **hybrid**, option (d) in
[section 6](#6-architecture-options): MultiKueue as the dispatch and safety
layer, our controller as its external dispatcher (plus an optional
AdmissionCheck for budgets and approvals), and the hosted signals behind an
open interface. The [rollout](#16-rollout) measures before it acts: observed
placement first, then the controller in shadow mode, recording what it would
have chosen against what happened.

## 1. What can and cannot span clusters

### 1.1 Why one synchronous job stays in one fabric

A data-parallel step all-reduces the gradients. A ring all-reduce moves about
twice the gradient size per participant
([nccl-tests PERFORMANCE.md](https://github.com/NVIDIA/nccl-tests/blob/master/doc/PERFORMANCE.md)).
In bf16 (2 bytes per parameter), the arithmetic below (ours) gives:

| Model | Gradient | 400 Gb/s InfiniBand per GPU | 100 Gb/s WAN link | 10 Gb/s WAN link |
| ----- | -------- | --------------------------- | ----------------- | ---------------- |
| 7B    | 14 GB    | ~0.6 s                      | ~2.2 s            | ~22 s            |
| 70B   | 140 GB   | ~5.6 s (sharded: ~0.2 s)    | ≥22 s             | ~224 s           |

Across sites, a hierarchical all-reduce pushes the whole gradient over the one
inter-site link every step, against step times of a few seconds. That
comparison is the whole argument:

- **Tensor and sequence parallelism** all-reduce activations inside every layer.
  They need an NVLink domain (8 GPUs on HGX, 72 on NVL72) and never cross a node
  without RDMA, let alone a cluster (Narayanan et al. 2021,
  [arXiv:2104.04473](https://arxiv.org/abs/2104.04473)).
- **Pipeline parallelism** tolerates more latency but still moves tens of GB per
  step per stage boundary.
- **Expert parallelism** does an all-to-all per MoE layer and is the most
  network-sensitive (DeepSeek-V3 designs around node-limited routing for this
  reason, [arXiv:2412.19437](https://arxiv.org/abs/2412.19437); verify).
- **RDMA does not cross a WAN.** RoCE needs a lossless fabric and InfiniBand its
  own subnet manager. Across sites NCCL falls back to TCP sockets
  ([NCCL environment](https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html)),
  and a NCCL communicator has fixed membership: one lost rank stops the
  collective.

So the unit of placement for a synchronous job is **one RDMA fabric**: usually
one cluster, one zone and one placement group or superpod. We call it the
_atom_. Every other rule in this document follows from never splitting it.

### 1.2 What can be spread

| Work                                      | Coupling between pieces                           | Spread how                                                                                                                                   |
| ----------------------------------------- | ------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| Phases of a run (pretrain, SFT, RL, eval) | a checkpoint hand-off                             | each phase attempt placed alone; a later phase prefers where its input checkpoint is                                                         |
| Attempts of one phase                     | sequential, through a checkpoint                  | the next attempt may land elsewhere if a resumable checkpoint is reachable there ([section 8](#8-failover-of-an-attempt-to-another-cluster)) |
| Sweep trials                              | none; only results flow back                      | fan out, one trial per atom                                                                                                                  |
| Eval jobs                                 | read one checkpoint (weights only, 2 B/param)     | near the checkpoint, or anywhere it can be copied in minutes                                                                                 |
| Batch inference shards                    | none                                              | data-parallel over the inputs; move work to the GPUs                                                                                         |
| Inference replica sets                    | none between replicas; tensor parallel inside one | replicas per region for latency and failover, each replica one node or NVLink domain                                                         |
| RL rollout workers                        | weights broadcast every k steps                   | tolerant of staleness (async RL, e.g. INTELLECT-2, [arXiv:2505.07291](https://arxiv.org/abs/2505.07291); verify)                             |

### 1.3 What a cross-cluster move costs

A full resumable checkpoint is about 12–16 bytes per parameter (bf16 weights,
fp32 master weights and two Adam moments; ZeRO,
[arXiv:1910.02054](https://arxiv.org/abs/1910.02054)). Times are ours,
size over throughput:

| Payload                       | 1 Gb/s | 10 Gb/s | 100 Gb/s |
| ----------------------------- | ------ | ------- | -------- |
| 70B weights only (140 GB)     | 19 min | 1.9 min | 11 s     |
| 70B full checkpoint (~1 TB)   | 2.2 h  | 13 min  | 80 s     |
| A 4 TB checkpoint or data set | 8.9 h  | 53 min  | 5.3 min  |
| 405B full checkpoint (~6 TB)  | 13 h   | 80 min  | 8 min    |

Egress for 1 TB costs about $20 between US regions and $85–120 across clouds
over the internet (list prices; verify against
[AWS](https://aws.amazon.com/ec2/pricing/on-demand/),
[GCP](https://cloud.google.com/vpc/network-pricing) and
[Azure](https://azure.microsoft.com/pricing/details/bandwidth/)). An 8×H100 node
costs roughly $30–100 an hour (verify). So for checkpoints the **time** is the
cost, not the bill. Pretraining corpora (tens of TB) are the exception: a
data-heavy phase stays in its data's region or uses a replica.

### 1.4 Most failures are local

Meta reported 419 unexpected interruptions in 54 days of Llama 3 405B training
on 16,384 H100s, about 78% of them hardware
([arXiv:2407.21783](https://arxiv.org/abs/2407.21783), §3.3.4; verify the
figures). Those are fixed inside the cluster, by the recovery tiers of
[reliant-for-ml.md §4.6](reliant-for-ml.md#46-replace-without-a-full-restart): an
in-job restart, a hot spare, a full restart. **Cross-cluster failover is for lost
capacity**: a spot reclaim wave, a quota taken back, a zone or cluster outage,
preemption by a higher-priority queue with no re-admission in sight.

### 1.5 Low-communication training, later

DiLoCo ([arXiv:2311.08105](https://arxiv.org/abs/2311.08105)), INTELLECT-1
(10B across three continents, [arXiv:2412.01152](https://arxiv.org/abs/2412.01152)),
Streaming DiLoCo ([arXiv:2501.18512](https://arxiv.org/abs/2501.18512)) and DeMo
([arXiv:2411.19870](https://arxiv.org/abs/2411.19870)) train one model across
islands that synchronise every H steps (hundreds), not every step. To a
scheduler, such a job is **several atoms plus an outer-sync coordinator**, with
islands that may join or leave between syncs. Open runs so far reach about
10–15B parameters, and mainstream stacks do not ship it as a default (verify:
torchft has DiLoCo prototypes). This design keeps room for a _multi-island_ job
kind ([decision 42](#15-open-owner-decisions)) and builds none of it now.

## 2. Units of placement

The object model is the spec's ([00 §2](ml-spec/00-object-model.md)): project →
experiment → run → phase → attempt → pod/rank → step. What we place:

| Unit                                  | Placed?                               | Atomic in one cluster | May move between clusters                | Notes                                                                                                                 |
| ------------------------------------- | ------------------------------------- | --------------------- | ---------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| Run                                   | No: it is a DAG of phases             | —                     | —                                        | Its identity survives every move. Cost, goodput and lineage stay one record                                           |
| **Phase attempt**                     | **Yes: the main unit**                | Yes, the atom         | Between attempts, through a checkpoint   | One Kueue `Workload` per attempt. The attempt records its `cluster_id`                                                |
| Sweep trial                           | Yes                                   | Yes                   | Like an attempt; often restarted instead | Sweeps are not in v1; the unit is designed now so backfill works later ([§7.5](#75-backfilling-interruptible-trials)) |
| Eval job (phase `eval` or standalone) | Yes                                   | Yes                   | Yes                                      | Prefers the cluster holding its checkpoint; weights-only copies are cheap                                             |
| Batch inference shard                 | Yes                                   | Yes                   | Yes                                      | Later slice                                                                                                           |
| Inference replica set                 | Declared, recommended, not dispatched | Each replica          | By a reviewed change to the declaration  | Serving is long-running and Flux-delivered, not admission-queued ([§7.4](#74-the-serving-capacity-floor))             |
| Smoke run (per pull request)          | Yes                                   | Yes                   | —                                        | CPU by default (decision 30); goes where the preview env is                                                           |
| Multi-island job                      | Later (decision 42)                   | Each island           | Islands join or leave between syncs      | Not built                                                                                                             |

**Never split:** a tensor-parallel group, a pipeline chain, an expert-parallel
group or the NCCL communicator of a data-parallel group. A placement that would
is refused at eligibility, not scored low.

## 3. Direction taken (owner, 2026-11-26)

Two inputs from the owner on 2026-11-26 shape this proposal:

- the multi-cluster input quoted at the top. It is appended verbatim to the
  [spec's owner input log](ml-spec/README.md#owner-input-log) in the same PR;
- at 15:45Z, the owner's own words: "I'd start with KServe + vLLM for serving,
  Add Kueue when teams or batch jobs compete for GPUs. Add KubeRay when you want
  to write distributed Python applications or independently scale stages of an
  inference pipeline." These are managed add-ons, "similar to how we host
  standard parts of a user's app", and "let's start simpler … maybe we skip
  datasets if that's overly complicated?". With it came
  notes the owner shared to "take/choose as you see fit". The multi-cluster parts
  of those notes were taken, and are listed below.

Items 1 (the serving and Kueue order and the managed add-ons) and 6 are the
owner's words. The rest were taken from the shared notes. They are this
proposal's working direction, not owner decisions, and the architecture itself
is open [decision 33](#15-open-owner-decisions):

1. **The stack.** Kubeflow Trainer `TrainJob` plus Kueue in each worker cluster,
   **MultiKueue** in the management cluster, and our open-source placement
   controller plugged in through MultiKueue's **external dispatcher**
   (`status.nominatedClusterNames`). Serving starts with KServe and vLLM, and
   Kueue arrives when GPUs are contended. Kueue, KServe and KubeRay are managed
   add-ons, the way forge already manages Zitadel, the OTel pipeline and CNPG:
   lifecycle, upgrades, telemetry and adopt-if-present.
2. **The score.** Feasible clusters are scored on **expected completion time plus
   total cost**: queue delay, provisioning, data and checkpoint staging time (4 TB
   at 10 Gb/s is about 53 minutes), **measured throughput of this recipe on each
   hardware shape**, and expected recovery overhead. **Hard eligibility comes
   first**: healthy contiguous hosts on the required fabric, runtime
   compatibility, data residency and tenant access.
3. **Explainability.** Every decision carries a concrete reason. Predicted and
   actual values are recorded so later decisions improve.
4. **A serving capacity floor**, so training and sweeps cannot consume the
   headroom inference needs to fail over. Interruptible trials backfill, chosen
   by checkpoint age.
5. **Attempt write fencing** for a cross-cluster resume. A checkpoint is
   published only when all of its shards are durable. **Resizing** (a different
   world size or mesh on another cluster) only for recipes tested with that
   change.
6. **Datasets, v1:** no catalog. A run records a dataset **reference, a digest or
   snapshot id, and a region**, and placement uses that for locality.
7. **One synchronous run stays in one cluster and fabric.**

The prior art the shared notes name is in
[section 5](#5-prior-art-and-upstream-options): SkyPilot, Armada and Sia.

The standing rules apply unchanged: we never provision GPUs, declarations are in
KCL, forge never stands in for another tool, every piece works alone, ML access is
gated per org, recovery rolls forward, and forge is semver at major 1.

## 4. Where it stands today

No code chooses a cluster. The pieces a multi-cluster scheduler builds on exist.

| Building block                             | State                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| An org-owned registry of customer clusters | control-plane `internal/clusterregistry`. A cluster is connected by address, CA and a scoped ServiceAccount token, and "nothing is installed in it" (`contract.go:1-5`). Every method takes the org and puts it in the WHERE clause, so another org's cluster reads as not found (`contract.go:7-11`). Table `deploy_tenant_clusters` (control-plane `db/migrations/00069_deploy_topology.up.sql:100`; connect columns in `20261003201821_cluster_connect_auth.up.sql:43-79`). In this row and the next five, unqualified paths are control-plane's. |
| Connect                                    | `forge cloud cluster connect` creates a least-privilege ServiceAccount through the owner's kube context and uploads its token (`internal/clusterregistry/connect.go:21-26`; forge `internal/cli/cluster_connect.go`), through the `ConnectCluster` RPC (`proto/services/cluster/v1/cluster.proto:80`).                                                                                                                                                                                                                                               |
| What a registered cluster records          | `ID, OrgID, Name, Provider, Connection, EnvironmentIDs, CreatedAt, Address, CAFingerprint, UpdatedAt` (`contract.go:157-179`). **No region, zone, labels, capacity, accelerators or live health.** `Connection` is derived from credential completeness and token expiry only (`service.go:150-166`); the old agent `last_seen_at` was dropped (`20261004024558_drop_cluster_agent_registration.up.sql:45-49`).                                                                                                                                      |
| One env bound to several clusters          | `deploy_environment_cluster_bindings (environment_id, cluster, cluster_id)`, keyed by forge's rendered kube context, "the multi-cluster successor of `deploy_environments.tenant_cluster_id`" (`20261003201821_cluster_connect_auth.up.sql:183-229`). `BindEnvironment` replaces the set, because the bindings come from a declaration (`contract.go:73-80`).                                                                                                                                                                                        |
| Delivery                                   | Flux runs on our hub, hub-and-spoke: the OCIRepository and Kustomization are applied to the hub, the destination namespace and impersonation ServiceAccount, Role and RoleBinding to the destination cluster (`internal/fluxtenant/doc.go:13-22`). Sources are digest-pinned OCI artifacts. A separate `kustomize-controller-connected` applies to customer clusters with the uploaded credential (`internal/fluxhub/controller_rbac.go:13-37`).                                                                                                     |
| Placement in forge                         | "PLACEMENT IS A PROPERTY OF EACH WORKLOAD, NEVER OF THE ENV" (forge `internal/cli/placement.go:3`). `forge.OnCluster {target: ClusterTarget}` (`kcl/workload.k:350-356`); "a second target is a second cluster" (`kcl/schema.k:893-925`). control-plane's own prod already spans two clusters: workloads on `_prod_k8s` (`deploy/kcl/prod/main.k:211`) and daemon manifests on `prod-daemon-v2`.                                                                                                                                                     |
| Placement decisions anywhere               | **None.** Daemon placement is a fixed per-env declaration (`control-plane/deploy/kcl/lib/daemon_placement.k:59-73`). `internal/placementgraph` orders a bundle's dependencies; it does not schedule. `GetClusterNodes`, the only capacity-shaped read, covers our own clusters and is admin-only (`proto/services/daemon_admin/v1/daemon_admin.proto:120-131`).                                                                                                                                                                                      |
| Kueue, JobSet, Trainer, MultiKueue         | No code in forge, control-plane or reliant. The ML proposals plan to adopt them ([reliant-for-ml.md §2](reliant-for-ml.md#2-new-mechanisms-or-reuse-oci-flux-and-the-rest)).                                                                                                                                                                                                                                                                                                                                                                         |
| Hub unreachable                            | Not handled or tested. Objects already applied are plain Kubernetes objects and keep running; new rollouts, drift correction and pruning stop until the hub returns (inferred from the architecture, not a tested property).                                                                                                                                                                                                                                                                                                                         |

So the registry, the env-to-N-clusters binding, hub delivery and per-workload
placement are the foundation. The gaps are an inventory and capacity feed, a
health signal, a job and queue primitive, and anything that decides.

## 5. Prior art and upstream options

| Project                                                                                                                                               | Licence and maturity                                                                                                         | What it places, and how it picks a cluster                                                                                                                                                                                                                                                                                                                                                                                                                              | Extension seam for our scorer                                                                                                                                                                                             | Verdict                                                                                                                                                                      |
| ----------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Kueue MultiKueue** ([docs](https://kueue.sigs.k8s.io/docs/concepts/multikueue/))                                                                    | Apache-2.0, kubernetes-sigs. MultiKueue alpha in v0.6, beta and on by default since v0.9. Latest stable v0.20.8 (2026-11-26) | A whole Kueue `Workload` (Job, JobSet, TrainJob, PyTorchJob, MPIJob, RayJob, AppWrapper, Deployment, external CRDs with a `managedBy` field). Quota is reserved on the manager, the Workload is copied to the nominated workers, the first worker to admit wins, and the others' copies are deleted before the real job is created there, so it never starts twice (KEP-693). Dispatchers: AllAtOnce (default), Incremental (3 clusters per 5 minutes) and **External** | **External dispatcher**: a controller writes `status.nominatedClusterNames` (at most 10), released in v0.13. An AdmissionCheck can gate (Ready / Retry / Rejected) and patch pod templates, but cannot choose the cluster | **Adopt as the dispatch layer**                                                                                                                                              |
| **Armada** ([repo](https://github.com/armadaproject/armada))                                                                                          | Apache-2.0, CNCF sandbox, in production at G-Research since 2020                                                             | Pods and annotated gangs. A central scheduler holds a node-level model of every cluster, does dominant-resource fair share and greedy bin-packing, and binds pods to nodes itself, bypassing kube-scheduler. A lost executor's lease expires and the job is requeued anywhere                                                                                                                                                                                           | None pluggable; config or a fork (verify)                                                                                                                                                                                 | **Borrow the lease model.** Adoption is all-in (Pulsar, Postgres, Redis) and conflicts with Kueue                                                                            |
| **SkyPilot** ([repo](https://github.com/skypilot-org/skypilot); NSDI'23 [paper](https://www.usenix.org/conference/nsdi23/presentation/yang-zongheng)) | Apache-2.0 (verify)                                                                                                          | A task or managed job. An optimizer ranks (cloud, region, zone, accelerator) by cost or time from a price catalog; on no capacity it blocklists the location and re-ranks. Managed jobs relaunch after preemption, resuming from a bucket checkpoint. Kubernetes contexts are treated like regions (`allowed_contexts`) (verify)                                                                                                                                        | Its own optimizer; no queue-wait model; egress only between pipeline stages (verify)                                                                                                                                      | **Borrow the optimise-and-blocklist loop** and deadline-aware spot use (Can't Be Late, [NSDI'24](https://www.usenix.org/conference/nsdi24/presentation/wu-zhanghao); verify) |
| **Sia** (CMU, SOSP'23; Jayaram Subramanya et al., "Sia: heterogeneity-aware, goodput-optimized ML-cluster scheduling") (verify)                       | Research code (verify licence)                                                                                               | Allocations across GPU types inside one cluster. It learns a per-job, per-GPU-type throughput model with little profiling and solves an integer program for goodput each round, building on Pollux's goodput (OSDI'21) (verify)                                                                                                                                                                                                                                         | n/a                                                                                                                                                                                                                       | **Borrow the per-shape throughput model.** We place across clusters, and resize only tested recipes                                                                          |
| Karmada ([repo](https://github.com/karmada-io/karmada))                                                                                               | Apache-2.0, CNCF graduated (README; verify date)                                                                             | One object per cluster. A filter/score scheduler and a per-cluster scheduler-estimator that asks "how many of these fit" against real nodes. A multiple-pod-templates proposal keeps a TrainJob or PyTorchJob in one cluster (release verify). Cluster and application failover                                                                                                                                                                                         | Out-of-tree plugins compiled into a scheduler build; a custom estimator (verify)                                                                                                                                          | Credible all-in-one alternative; a second API server and no Kueue-grade quota. **Copy the estimator idea**                                                                   |
| Open Cluster Management ([repo](https://github.com/open-cluster-management-io/ocm))                                                                   | Apache-2.0, CNCF incubating                                                                                                  | Placement over ManagedClusters with prioritizers, including **AddOnPlacementScore** CRs that any controller publishes per cluster. A `kueue-addon` generates MultiKueueConfig and MultiKueueCluster from Placement decisions                                                                                                                                                                                                                                            | AddOnPlacementScore                                                                                                                                                                                                       | A good inventory and score-publishing seam; not needed to start                                                                                                              |
| SIG-Multicluster ClusterProfile (KEP-4322)                                                                                                            | Apache-2.0; KEP provisional, alpha (verify)                                                                                  | Inventory only: a cluster list with properties and credential providers. MultiKueue accepts it for worker credentials (gate beta in v0.21)                                                                                                                                                                                                                                                                                                                              | Properties written by a controller                                                                                                                                                                                        | **Emit it** from our registry when it stabilises ([decision 40](#15-open-owner-decisions))                                                                                   |
| Volcano and volcano-global                                                                                                                            | Volcano Apache-2.0, CNCF incubating. volcano-global's README says alpha; last commit 2026-03                                 | volcano-global queues jobs on Karmada; Karmada picks the member                                                                                                                                                                                                                                                                                                                                                                                                         | Karmada plugins                                                                                                                                                                                                           | Too early to build on                                                                                                                                                        |
| Slurm federation ([docs](https://slurm.schedmd.com/federation.html))                                                                                  | GPLv2                                                                                                                        | Sibling copies of a job on every eligible cluster; the first to take the origin's lock starts it, and the others are revoked. A requeue resubmits siblings                                                                                                                                                                                                                                                                                                              | None global                                                                                                                                                                                                               | Prior art for "copy, first to start wins". "Not intended as a high-throughput environment"                                                                                   |
| Liqo, Admiralty                                                                                                                                       | Apache-2.0. Admiralty unpushed since 2024-11                                                                                 | Per pod, through virtual nodes                                                                                                                                                                                                                                                                                                                                                                                                                                          | —                                                                                                                                                                                                                         | **Unfit**: a gang can split across clusters                                                                                                                                  |
| dstack, Run:ai / KAI, Lepton                                                                                                                          | dstack MPL-2.0 (verify); KAI Apache-2.0                                                                                      | dstack takes the cheapest offer across backends, idle capacity first. KAI gangs and queues inside one cluster. Run:ai manages many clusters, but the user picks the cluster per workload (verify)                                                                                                                                                                                                                                                                       | —                                                                                                                                                                                                                         | Ideas only                                                                                                                                                                   |

What none of them has, and what this proposal adds: a **queue-wait forecast** per
cluster (they learn about capacity by failing to get it), **data and checkpoint
locality priced into a single job's placement**, **node-health history across
clusters**, the **measured cost of a move** (checkpoint size and age, restart
time) in the relaunch decision, **budgets and fair share across clusters**, and
an **explanation with predicted against actual** for every decision.

## 6. Architecture options

| Option                                                        | Shape                                                                                                                                                                                                                                                                                                                                   | For                                                                                                                                                                                                                     | Against                                                                                                                                                                                                                                                                                                                                                             |
| ------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **(a) MultiKueue + our brain as an AdmissionCheck or scorer** | Users submit to a management cluster; MultiKueue dispatches; our logic is an AdmissionCheck controller                                                                                                                                                                                                                                  | Fully upstream; one place for quota                                                                                                                                                                                     | An AdmissionCheck cannot choose the cluster: it can only pass, retry or reject and patch pod templates. With the default AllAtOnce dispatcher, placement is a race between workers, which optimises nothing                                                                                                                                                         |
| **(b) Armada-style standalone**                               | Our own global queue, fair share and node-level model, binding pods ourselves, with an executor per cluster                                                                                                                                                                                                                             | Total control; the fastest possible decisions                                                                                                                                                                           | Replaces Kueue in every cluster, against the direction taken (Kueue adopted, §3 item 1; decision 24's "upstream first") and "never stand in for another tool". A large surface to keep correct (gang reservation, preemption, double-start). Armada itself shows the cost: Pulsar, Postgres, Redis                                                                  |
| **(c) Our own hub-side scheduler over Flux**                  | The control plane picks a cluster and delivers the job through the existing hub Flux path                                                                                                                                                                                                                                               | Reuses delivery, the registry and bindings; no management cluster                                                                                                                                                       | Flux converges desired state: it is not an admission queue, so there is no quota reservation, no "admitted somewhere, delete elsewhere" and no requeue on cluster loss, and each would be ours to build. Placement stops whenever the hub cannot reach a cluster. It needs launch-many semantics Flux does not have. A re-implementation of MultiKueue's guarantees |
| **(d) Hybrid (recommended)**                                  | MultiKueue in a management cluster does quota, copies, the single-winner guarantee and requeue on worker loss. **Our controller is its external dispatcher**, nominating normally one cluster. An optional AdmissionCheck of ours enforces budgets and approvals. The control plane supplies signals, records placements and shows them | Upstream owns the safety properties; we own only the decision. One seam, released and documented. Works without the hub on local signals. The open-source boundary is clean: controller and scorer open, signals hosted | Depends on a beta upstream feature and its pace (mitigated: we can contribute, and the dispatcher is small enough to re-home onto option (c) if MultiKueue fails us). One dispatcher per manager cluster (verify). The manager's quota must track the workers' capacity, which we render until upstream automates it                                                |

**Devil's advocate on (d).**

- _"Nominating one cluster throws away MultiKueue's race."_ The race is the
  fallback, not the plan. The controller nominates the best cluster and, after a
  declared patience (`nominate_next_after`), widens to the next. When it cannot
  run at all, MultiKueue's own Incremental behaviour is the degraded mode. So we
  never do worse than upstream.
- _"A management cluster is one more thing to run."_ It is small and CPU-only,
  and Kueue already runs there. The alternatives move the same responsibility into
  our hub, where a hub outage would stop placement for every customer
  ([section 11](#11-what-runs-where-and-the-hub-unreachable)). Where it runs is
  [decision 34](#15-open-owner-decisions).
- _"Kueue on every worker excludes teams without it."_ Yes, on purpose. Kueue is a
  managed add-on (§3 item 1): adopted where present, installed when absent
  and agreed. A second, Kueue-free dispatch path would double what we must prove
  correct ([decision 35](#15-open-owner-decisions)).
- _"Upstream may change the external dispatcher."_ It is a beta API on a 0.x
  project. We pin Kueue per release, test every bump on the k3d fleet
  ([section 14](#14-testing-without-gpus)), and take part upstream
  ([section 13.3](#133-upstream)).

**Recommendation: (d).** It puts the hard distributed-systems guarantees (quota
reservation, a single winner, garbage collection, requeue on worker loss) in
upstream code that already holds them, and keeps ours to the one thing nobody
else does: deciding well and saying why.

## 7. The placement brain

### 7.1 Eligibility first

A cluster is eligible for a unit only when every hard rule holds. A failure is
named in the explanation, never scored as a penalty.

| Rule                        | Holds when                                                                                                                                             | Source                                                                                                                                                               |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Tenant access               | The cluster is the org's own, registered and bound to this env's pool                                                                                  | `clusterregistry` (org-scoped), the env's bindings                                                                                                                   |
| Residency                   | The cluster's region is inside the declared residency, and so is every checkpoint replica target                                                       | Region from the inventory; the declaration                                                                                                                           |
| The atom fits contiguously  | Enough **healthy** hosts of the shape on one fabric domain (same `topology` block, placement group or superpod label), not merely enough GPUs in total | Inventory plus health; Kueue TAS topology levels where declared ([verify TAS label conventions](https://kueue.sigs.k8s.io/docs/concepts/topology_aware_scheduling/)) |
| Accelerator and runtime     | Product (`nvidia-h100-80gb`), driver and CUDA range, the orchestrator CRD (TrainJob, JobSet) and its version, the Kueue version the dispatcher needs   | Inventory (`pkg/mldiscover`, ml-workloads §3.2)                                                                                                                      |
| Recipe tested on this shape | A different world size or mesh than the template declares is allowed only when the recipe declares that resize as tested (§3 item 5)                   | `forge.Training` declaration                                                                                                                                         |
| Resumability, for a move    | A durable, fenced checkpoint is reachable from the cluster, in a format the recipe can load there (resharding if the mesh changes)                     | The training-run ledger                                                                                                                                              |
| Serving floor               | Placing the unit leaves the cluster's declared inference failover headroom intact ([§7.4](#74-the-serving-capacity-floor))                             | The declaration plus the inventory                                                                                                                                   |
| Budget and approval         | The launch is within its template's GPU-hour budget and the cluster's price; a production queue has its approval (decision 28)                         | The ledger, the price sheet, Changes approvals                                                                                                                       |
| Gates                       | A phase that consumes a checkpoint or model may run only when its declared gate passed (an eval gate, a promoted template release)                     | Ledger and release ledger                                                                                                                                            |
| Not cordoned                | The cluster is not cordoned by its owner or by the controller after repeated failures                                                                  | The declaration; controller state                                                                                                                                    |

### 7.2 The score: expected completion time plus total cost

Among eligible clusters, the controller picks the one with the lowest expected
cost to finish, with time priced at the run's declared value:

```
J(c) = V · ( Ŵ(c) + P̂(c) + Ŝ(c) + M̂(c) )            time until the first productive step
     + p(c) · n · R / T̂(c) · (1 + B̂(c))              accelerator cost of the remaining work
     + Ê(c)                                          egress for what must move

  Ŵ  forecast queue wait for this shape (a high quantile, not the mean)
  P̂  provisioning time when the cluster must scale up (autoscaler, DWS)
  Ŝ  startup: pull, init, checkpoint load and warm-up, measured per cluster
  M̂  staging: bytes not already there (checkpoint, data, weights) / measured throughput
  p  price per accelerator-hour on that cluster and capacity type
  n  accelerators; R the remaining work in samples or tokens
  T̂  measured throughput of THIS recipe on THIS hardware shape (samples/s per accelerator)
  B̂  expected recovery overhead: failure rate × (restart time + half the checkpoint interval)
  Ê  egress: bytes moved × $/GB on that path
  V  the declared value of an hour of delay (default: the run's own hourly accelerator price)
```

The terms map onto the owner's list: queue delay (Ŵ), provisioning (P̂), staging
(M̂; 4 TB at 10 Gb/s is about 53 minutes), measured throughput (T̂) and expected
recovery (B̂). Choices that keep the number honest:

- **Throughput is per recipe and per shape.** Sia's lesson is that one job's
  speed on an H100 against an A100 is not a constant across jobs. T̂ comes from
  the SDK's step series for this template on each product and node shape; with no
  history it falls back to a declared ratio and then to accelerator peak TFLOP/s,
  and the explanation says which.
- **Risk, not averages.** Ŵ uses a declared quantile (p75 by default), because
  a mean wait hides the tail that ruins a deadline.
- **A declared deadline changes the objective.** With
  `deadline` set, clusters that miss it at the quantile are dropped, and spot is
  preferred while the run is ahead of a uniform-progress line (SkyPilot's Can't
  Be Late policy; verify).
- **Hysteresis.** A running attempt is moved only when the gain exceeds the
  move's whole cost plus a margin, and at most `max_moves` per run per day. A
  placement that flaps is worse than a slow one.

### 7.3 The signals, and how the score degrades without them

The controller reads signals through one interface, `SignalProvider`
([§13.2](#132-the-boundary-open-scheduler-hosted-signals)). Some come from the
clusters themselves and are in the open-source controller. The rest are joined
per org by Reliant.

| Signal                                    | Feeds       | Local source (open, no Reliant)                                       | Hosted source (Reliant, per org)                                                                                  | Without it                                                          |
| ----------------------------------------- | ----------- | --------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| Free capacity and fragmentation per shape | Fit, Ŵ      | Worker Kueue ClusterQueue usage; node allocatable and topology labels | The inventory and capacity feed ([§10.1](#101-cluster-inventory-and-capacity))                                    | Declared order only, like MultiKueue's Incremental                  |
| Queue position and wait                   | Ŵ           | Kueue visibility API (`positionInClusterQueue`, beta since v0.9)      | A wait forecast per (cluster, shape, priority) learned from the ledger's admission history                        | Position × a fixed admit interval                                   |
| Provisioning time                         | P̂           | Autoscaler and ProvisioningRequest status                             | Measured per pool from node `Ready` times                                                                         | A declared constant per pool                                        |
| Startup breakdown                         | Ŝ           | —                                                                     | The restart-time stages of [reliant-for-ml.md §4.2](reliant-for-ml.md#42-the-restart-time-breakdown), per cluster | A declared constant                                                 |
| Checkpoint, weights and image locality    | M̂, Ê        | Image digests in node `status.images`; declared checkpoint URIs       | The ledger's checkpoints with tier, region and replication state; per-node weight caches                          | Assume a full copy                                                  |
| Dataset locality                          | M̂, Ê        | The run's dataset reference, digest and region (§3 item 6)            | The same, plus measured read throughput per cluster                                                               | Region match only                                                   |
| Throughput per recipe and shape           | T̂           | —                                                                     | SDK step series per template and shape                                                                            | A declared ratio, then peak TFLOP/s                                 |
| Health and bad-node history               | Fit, B̂      | Node conditions, NPD                                                  | Xid, ECC and NPD history per node incarnation and pool, joined across runs (ml-workloads drill-down 1)            | Current conditions only                                             |
| Failure and reclaim rates                 | B̂           | —                                                                     | MTBF per pool from attempt end reasons; spot reclaim rate per pool                                                | A declared rate, or the public spot advisor bucket                  |
| Price and egress                          | p, Ê        | The declared price sheet (ml-workloads decision 21)                   | The same, plus public cloud list prices for egress                                                                | Equal prices: minimise time only                                    |
| Budgets, entitlements, gates              | Eligibility | Declared budgets                                                      | Spend so far, the ML entitlement, eval gate and release state                                                     | Declared budgets enforced locally; gates assumed closed (fail safe) |

Three levels, each a strict superset of the one before, and the explanation
always states which one decided:

- **L0, declared:** the pool's order and affinities. No better than MultiKueue's
  Incremental dispatcher, and no worse.
- **L1, local:** what each worker says now (capacity, queue position, images on
  nodes). This is the open-source default, usable with no Reliant account.
- **L2, joined:** forecasts and measurements from the org's own history. This is
  the hosted product.

**Signals are per org, always.** A forecast for one org is learned only from that
org's ledger and telemetry. No org's data informs another's score, not as
anonymous priors either. The only shared inputs are public: cloud list prices and
published spot interruption buckets.

### 7.4 The serving capacity floor

Inference fails over by scaling up replicas in another region. If training has
taken that region's GPUs, failover has nowhere to go. So a pool declares a
**floor per cluster and shape**: accelerators that batch work may use only when
they are interruptible and give the capacity back within a declared time.

- Rendered as a Kueue cohort split: a serving ClusterQueue that holds the floor
  as nominal quota, lending it only to interruptible workloads, with preemption
  that reclaims it (verify the lending-limit and reclaim fields on the pinned
  Kueue).
- Checked at eligibility: a non-interruptible attempt is not placed where it
  would cut into the floor.
- Shown on the Fleet page as headroom against the floor, with an alert when the
  floor itself cannot be met.

### 7.5 Backfilling interruptible trials

Capacity under the floor, and capacity held for spares (decision 29), is not
wasted if interruptible work fills it. The controller prefers, for backfill, the
units cheapest to interrupt: **the one whose last durable checkpoint is the most
recent**, because preempting it loses the least. A trial declares itself
interruptible; the expected loss is `now − last durable checkpoint` plus its
restart time, read from the ledger. Sweeps are not in v1, so
backfill arrives with them, on a unit designed now.

### 7.6 Explanations, predicted and actual

Every decision is a record ([§10.3](#103-the-placement-ledger)) with:

- the unit, the policy level (L0/L1/L2) and the candidate set;
- each **ineligible** cluster with the rule that failed ("eu-west4: residency
  `us` excludes region europe-west4"; "us-east5: 61 healthy H100 hosts on fabric
  block b3, needs 64");
- each **eligible** cluster's terms (Ŵ, P̂, Ŝ, M̂, T̂, B̂, Ê) and J;
- the choice and its reason in one sentence: "us-central1-a: starts in ~6 min
  against ~2 h 10 min on us-east5; the 1.1 TB checkpoint is already replicated
  there; costs $310 more over the run's 9 remaining hours";
- **predicted values, then actual ones** as they arrive: wait, startup, staging,
  throughput, recovery. The difference is the controller's error, per term and
  per cluster. It is shown, and the L2 forecasts learn from it.

`forge ml place --explain <unit> --json` prints the same document from the CLI,
and the console's attempt page shows it as "Why here".

## 8. Failover of an attempt to another cluster

### 8.1 When

An attempt ends (spec 00 §2.4). The controller places the next attempt like any
other, with the move's costs in the score, so a move happens only when it wins:

```
move to c  ⇔  J(c) + margin  <  J(here)
  with  Ŵ(here) the forecast wait for re-admission where the attempt was,
        M̂(c) = (checkpoint bytes not yet replicated to c) / measured throughput,
        Ŝ(c) ≈ init + checkpoint load when the image and weights are pre-pulled
```

Worked example (ours): a 70B run with a 1 TB checkpoint, 10 Gb/s between regions.
With nothing replicated and nothing pulled, a move costs about 13 minutes of
staging, 5 of pull and 10 of restart, plus about $20: worth it only when the
local wait forecast exceeds roughly 30–40 minutes. With the checkpoint
replicated and the image pre-pulled it costs about 10 minutes, and a move wins
almost whenever capacity is gone.

Triggers are the attempt end reasons that mean **lost capacity**: `spot_reclaimed`,
`preempted_by_queue` with a re-admission forecast past the policy's patience,
`node_failure` with no spare, and two new additive reasons, **`cluster_lost`**
(the worker is unreachable past its timeout) and **`migrated`** (the controller
moved a waiting or degraded attempt). Hardware faults inside a healthy cluster go
to the in-cluster recovery tiers first ([§1.4](#14-most-failures-are-local)).

**Mode.** Like remediation (decision 32), failover across clusters defaults to
`recommend` when declared: the controller proposes the move, with its
explanation, in the Inbox. `act` must be declared
([decision 38](#15-open-owner-decisions)). Moving to another cluster spends
money and copies data across regions, so it is a team's stated choice.

### 8.2 Mechanics

1. **Detection.** MultiKueue requeues a lost worker's workloads after
   `workerLostTimeout` (default 15 minutes; verify). That is slow for a
   4,096-GPU run, so the controller also watches worker heartbeats and Kueue
   conditions, and can **evict** the manager-side Workload itself once a declared
   `cluster_lost_after` passes, recording the reason. (An open upstream bug, one
   ended watch marking a whole worker disconnected, #18115, is a reason to keep
   our own detection; verify on the pinned version.)
2. **Fence the old attempt** ([§8.4](#84-write-fencing)).
3. **Place** the next attempt with the controller's score; nominate the target.
4. **Stage**: replication has usually already copied the checkpoint; any delta
   is copied now, its time recorded as `staging`.
5. **Resume** from the newest durable, fenced checkpoint reachable there. The
   attempt records `resumed_from`, the new `cluster_id` and the previous
   attempt's end reason. Steps stay continuous (spec 00), and `steps_lost`
   counts the work since the checkpoint.
6. **Clean up**: when the old worker returns, MultiKueue's garbage collector
   removes the stale copies (KEP-693; verify), and the controller confirms that
   no pod of the fenced attempt still runs.

### 8.3 Checkpoint replication

A move is only fast if the checkpoint is already there. The declaration names
replica targets; the default is **none** (nothing crosses a region unless
declared):

- **Native first.** Object-store replication (GCS dual- and multi-region
  buckets, S3 Cross-Region Replication, Azure object replication) is preferred
  and only declared and checked by forge: never stand in for another tool.
- **A rendered copier otherwise.** A small Job per durable checkpoint using an
  adopted tool (rclone or the cloud's CLI; [decision 39](#15-open-owner-decisions)),
  copying the shards, then the manifest last.
- **Recorded.** Each checkpoint's replicas, with lag and bytes, are in the
  ledger, so M̂ is exact and "failover-ready in 2 clusters, 14 minutes behind"
  is a fact on the run page.
- **Residency is enforced at render.** A replica target outside the declared
  residency fails `forge env render`.
- **Cost is visible.** Replication egress and storage are shown as insurance,
  beside the badput they would save, like spares (reliant-for-ml §4.6).

### 8.4 Write fencing

After a partition, the old attempt may still be running on the worker the
manager can no longer see. It must not publish a checkpoint over its successor's
or record steps into its lineage.

- **Every attempt has an epoch**, `(run, phase, attempt n)`, and writes under its
  own prefix: `…/<phase>/attempt-<n>/step-<k>/`. Two attempts never write the same
  path.
- **A checkpoint is published only when every shard is durable**, by writing a
  manifest last. A checkpoint without a manifest does not exist to a resume.
- **Publishing is a ledger write that checks the epoch.** The control plane, or
  the controller's local journal when the hub is unreachable
  ([section 11](#11-what-runs-where-and-the-hub-unreachable)), accepts
  `checkpoint.durable` for attempt _n_ only while _n_ is the phase's current
  attempt. A fenced attempt's checkpoints are kept as orphans, never resumed from,
  and garbage-collected after a declared time.
- **The resume reads only published checkpoints**, so the successor cannot pick up
  a stale writer's state.
- **The SDK and the rendered environment carry the epoch** (`FORGE_ATTEMPT`), so a
  framework that writes its own checkpoints writes into the attempt's prefix.

### 8.5 Pre-pull at the target

A move is slow if the target pulls a 15 GB image and 140 GB of weights first.
Failover-ready clusters (those with replicated checkpoints) get the attempt's
**image digest and weight digests pre-pulled**: on declared spares as in
reliant-for-ml §4.7, or through `MLStack.pull_cache` (Spegel, Dragonfly or a
pull-through cache) where present. Digests, not tags, so the pulled bytes are
exactly what will run.

### 8.6 Resizing

A move to a cluster with fewer or different accelerators changes the world size
or the mesh. It is allowed only when the recipe declares that change as tested
(§3 item 5), the checkpoint format reshards (PyTorch DCP and Megatron
distributed checkpointing do; verify on the pinned versions), and the smoke run
exercises it ([reliant-for-ml.md §6.1](reliant-for-ml.md#61-smoke-training-per-pull-request)).
Otherwise the attempt waits or moves only to an identical shape.

## 9. Declaration in KCL

Additive and optional, as every ML type is: an env that declares none of it
renders byte-identically. The names are a sketch, not settled syntax.

```python
import forge

_mgmt = forge.ClusterTarget {cluster = "gke_acme_us-central1_mgmt", namespace = "ml", connected_cluster = "mgmt"}
_usc  = forge.ClusterTarget {cluster = "gke_acme_us-central1_gpu",  namespace = "ml", connected_cluster = "gpu-usc"}
_use  = forge.ClusterTarget {cluster = "gke_acme_us-east5_gpu",     namespace = "ml", connected_cluster = "gpu-use"}
_eu   = forge.ClusterTarget {cluster = "gke_acme_europe-west4_gpu", namespace = "ml", connected_cluster = "gpu-eu"}

cluster_pools = [forge.ClusterPool {
    name = "h100"
    manager = _mgmt                        # runs MultiKueue and the placement controller
    members = [
        forge.PoolMember {target = _usc, price = "h100-on-demand", fabric_label = "cloud.google.com/gce-topology-block"}
        forge.PoolMember {target = _use, price = "h100-spot", capacity_type = "spot"}
        forge.PoolMember {target = _eu,  price = "h100-reserved", region = "europe-west4"}
    ]
    serving_floor = [forge.ServingFloor {target = _usc, accelerators = 16}]
    placement = forge.PlacementPolicy {
        objective = "time_and_cost"        # J(c) above; or "time", "cost"
        value_per_hour = "auto"            # V: defaults to the run's own accelerator price
        wait_quantile = 0.75
        nominate = 1                       # clusters nominated at once
        nominate_next_after = "20m"        # widen when not admitted
        level = "joined"                   # declared | local | joined (L0/L1/L2)
    }
}]

trainings = [forge.Training {
    name = "pretrain-8b"
    orchestrator = "trainjob"
    pool = "h100"
    workers = 64
    dataset = forge.DatasetRef {uri = "gs://acme-data/fineweb-edu", snapshot = "2026-11-01", region = "us-central1"}
    placement = forge.TrainingPlacement {
        residency = ["us"]                 # a hard rule: regions, or region prefixes
        prefer = ["gpu-usc"]
        avoid = []
        budget = forge.Budget {gpu_hours_per_day = 4000}
        deadline = "2026-12-20"
        resize = []                        # world sizes this recipe has tested, e.g. [32, 64]
        failover = forge.CrossClusterFailover {
            mode = "recommend"             # off | recommend | act; default recommend when declared
            on = ["spot_reclaimed", "cluster_lost", "preempted_by_queue"]
            patience = "30m"               # wait for local re-admission before considering a move
            cluster_lost_after = "3m"
            max_moves_per_day = 3
            replicate_checkpoints_to = ["gpu-use"]   # within residency; native replication preferred
            prepull = True
        }
    }
    # resilience = forge.Resilience {...}   # reliant-for-ml §4.8, unchanged
}]
```

What forge renders from it, all into the existing bundle and delivered by the
hub's Flux:

- **On the manager:** Kueue with MultiKueue enabled, the AdmissionCheck,
  MultiKueueConfig and one MultiKueueCluster per member (referencing a credential
  Secret, never holding it; [section 12](#12-security-and-tenancy)), the manager's
  ClusterQueues with quotas derived from the members' last inventory (until
  upstream's capacity provider is stable; verify KEP-17401), Kueue's
  `multiKueue.dispatcherName` set to the controller, and the controller's
  Deployment, configuration and minimal Kubernetes permissions.
- **On every member:** Kueue (adopted or installed), the ClusterQueues and
  LocalQueues, the serving-floor cohort split, the orchestrator CRDs (Trainer,
  JobSet) when installed by us, the namespace, the checkpoint-store credentials by
  secret reference, and the copier when replication is not native.
- **Training jobs** go to the manager only, labelled `ml.reliant.dev/run`,
  `phase` and `attempt` (spec 00 §8). MultiKueue creates them on the winner.

A pool is a set of `ClusterTarget`s, so each member becomes a row in
`deploy_environment_cluster_bindings` through the existing declarative path, with
no new binding mechanism. `forge lint` gains rules (through the lint rollout table,
warning first): a pool member with no `connected_cluster`; replica targets outside
residency; `act` without a declared `patience`; a `resize` that the smoke run does
not exercise.

## 10. Control-plane changes

All additive, all gated by the ML entitlement (ml-workloads §4.1).

### 10.1 Cluster inventory and capacity

- **Facts on the registry row**: provider, region, zones and labels, derived from
  the cluster collector's inventory (ml-workloads §3.2, `pkg/mldiscover`) rather
  than typed in. A migration adds nullable columns to `deploy_tenant_clusters`
  (`forge:backward-compatible`).
- **A live heartbeat.** The collector's last report becomes `last_reported_at`,
  so `Connection` can say `connected` because the cluster spoke, not only
  because its token has not expired. It is additive beside today's derivation
  (`service.go:150-166`).
- **A capacity summary** per (cluster, accelerator shape, fabric block): total,
  allocatable, allocated, healthy, cordoned, and the Kueue usage per ClusterQueue
  and flavor. Latest values in Postgres, history in the tenant's ClickHouse.

### 10.2 A placement service

A new `PlacementService` in the ML service family, read-only to the console and
scoped per org:

| RPC                   | Caller                         | What                                                                                          |
| --------------------- | ------------------------------ | --------------------------------------------------------------------------------------------- |
| `GetPlacementSignals` | The controller (outbound pull) | The L2 signals for a pool: forecasts, measurements, locality, prices, budgets and gate states |
| `RecordPlacements`    | The controller                 | Batches of decisions and their actuals, idempotent by decision id; journal replay after a gap |
| `ExplainPlacement`    | Console, CLI, agents           | One decision document ([§7.6](#76-explanations-predicted-and-actual))                         |
| `ListPlacements`      | Console, CLI, agents           | Decisions by run, cluster, pool or time; the controller's error per term                      |

The controller pulls signals; nothing new dials into a customer's cluster. Its
token is per (org, manager cluster), minted and renewed through the existing
telemetry-token path (`internal/telemetrydelivery`), with a new `ml:place` scope.

### 10.3 The placement ledger

Decisions live beside the training-run ledger in control-plane Postgres, per org
(decision 25). A decision references the attempt it placed; the attempt gains
`cluster_id` and the two new end reasons. Inputs are stored as the evaluated
terms, never raw telemetry. Events join the stream of reliant-for-ml §5.1,
additively: `ml.placement.decided`, `ml.placement.widened`,
`ml.attempt.migration_proposed`, `ml.attempt.migrated`, `ml.cluster.lost`,
`ml.cluster.recovered`. Each is a typed trigger in the `forge` integration
manifest (decision 27), so a workflow can react to "run moved to another region".

### 10.4 What is not added

- No scheduler in the control plane. It neither picks clusters nor dispatches:
  it supplies signals and records what happened, as decision 24 keeps
  remediation out of it.
- No second delivery path. Templates, configuration and the controller arrive by
  hub Flux. Launches go to the manager cluster as a Kueue-managed CR, created
  the way ml-workloads §4 already proposes (as the connect ServiceAccount, with a
  render-derived grant), or by `forge ml run --context` with the user's own
  credentials.

## 11. What runs where, and the hub unreachable

```
 control plane (hub)                               management cluster (customer's)                     worker clusters
 ┌──────────────────────────────┐   signals (pull) ┌─────────────────────────────────────┐ Workload copies ┌───────────────────────┐
 │ inventory + capacity         │◀─────────────────│ placement controller (open source)  │────────────────▶│ Kueue (admits, gangs)  │
 │ training-run + placement     │─────────────────▶│  · eligibility + score              │                 │ Trainer / JobSet       │
 │   ledger                     │  decisions,      │  · nominatedClusterNames            │◀────status──────│ forge node agent +     │
 │ forecasts (per org)          │  actuals (push)  │  · local journal                    │                 │   cluster collector    │
 │ console, CLI, events, Inbox  │                  │ Kueue + MultiKueue (manager)        │                 │ checkpoint copier      │
 │ hub Flux ── bundles ─────────┼─────────────────▶│ AdmissionCheck (budgets, approvals) │                 └───────────────────────┘
 └──────────────────────────────┘                  └─────────────────────────────────────┘                       │ OTLP (outbound)
                ▲                                                                                                 │
                └─────────────────────────────────────────────────────────────────────────────────────────────────┘
```

| Component                      | Where                 | Why there                                                                         |
| ------------------------------ | --------------------- | --------------------------------------------------------------------------------- |
| Kueue per worker               | Each worker           | Admission, gang, preemption and topology belong next to the nodes                 |
| MultiKueue manager             | Management cluster    | Quota, copies and the single-winner guarantee in one place that sees every worker |
| Placement controller           | Management cluster    | It must act in seconds and keep acting when the hub cannot be reached             |
| AdmissionCheck (budgets)       | Management cluster    | Enforced before dispatch, from declared budgets plus spend the controller caches  |
| Signals, forecasts, ledger, UI | Control plane         | They need the org's history across clusters and runs                              |
| Templates and configuration    | Delivered by hub Flux | The existing declarative path                                                     |

**When the hub is unreachable:**

- Running attempts are unaffected: they are Kueue workloads in their workers.
- MultiKueue keeps dispatching, and **the controller keeps deciding**, on its
  cached L2 signals until their declared TTL (default 1 hour), then on L1. Each
  explanation records the level it used.
- Budgets keep being enforced locally from the last known spend plus the
  controller's own local accounting, conservatively: when unsure, refuse.
- Decisions, actuals and checkpoint publications are **journalled locally** and
  replayed, idempotently, when the hub returns. Fencing stays correct offline
  because the controller is the one writer of the current attempt epoch for its
  pool.
- New templates and configuration wait for delivery; launches against templates
  already delivered proceed (`forge ml run --context`).

**When the management cluster is unreachable:** workers keep running what they
admitted; nothing new is dispatched. The controller and the MultiKueue manager
run with leader election and two replicas (verify the Kueue manager's HA
guidance), and the management cluster should be a small CPU cluster that is not
itself a GPU pool member. A hosted manager is the alternative
([decision 34](#15-open-owner-decisions)).

**When a worker is unreachable:** its running attempts may still be running.
After `cluster_lost_after` the controller fences them, records `cluster_lost` and
places their successors under the failover policy ([section 8](#8-failover-of-an-attempt-to-another-cluster)).

## 12. Security and tenancy

- **One org's clusters only.** A pool's members must be registered clusters of
  the env's org; the registry's org-scoped reads (`clusterregistry/contract.go:7-11`)
  are the check, at render validation and again in the control plane. Signals,
  decisions and explanations are org-scoped, like every ML read.
- **Cross-org never.** No shared MultiKueue manager across orgs. A hosted manager,
  if chosen, is isolated per org (its own virtual control plane), because Kueue's
  ClusterQueues and MultiKueue objects are cluster-scoped. No org's history
  informs another org's forecasts.
- **Credentials stay where they are needed.** MultiKueueCluster credentials
  are scoped ServiceAccount tokens in the worker, stored as Secrets in the
  management cluster, minted by `forge ml pool join` from the owner's kube
  context, the way `forge cluster connect` mints the hub's. They never pass
  through KCL or the control plane. ClusterProfile with credential plugins is the
  path once it is stable (decision 40). The worker-side grant is the minimum
  MultiKueue needs (Workload and job kinds in the declared namespaces; verify
  the upstream list of Kubernetes permissions), and shown by `--dry-run`.
- **The controller's Kubernetes permissions** on the manager: read Workloads and ClusterQueues,
  patch `Workload.status` under the field manager Kueue requires (verify:
  `kueue-admission`, or the merge-patch gate), evict Workloads it fences, read
  the visibility API. Nothing on workers beyond what MultiKueue already holds.
- **Residency is defence in depth.** Refused at render (replica targets), filtered
  at eligibility, and enforced at the worker by an AdmissionCheck that rejects a
  Workload whose residency label does not match the cluster's region.
- **Spend is a security property** (reliant-for-ml §8). Budgets are enforced
  before dispatch; `act` failover needs an explicit declaration; every move is an
  activity event with its explanation; `ml:place` is a scope separate from
  `ml:launch` and remediation.
- **Explanations hold no secrets**: cluster names, regions, shapes and numbers,
  never credentials, URIs with tokens or sample data.
- **The controller image** is signed and carries an SBOM, delivered by digest
  like every forge-rendered component.

## 13. Open source

### 13.1 Which repository

| Option                                                               | For                                                                                                                                                                                                                                 | Against                                                                                                                             |
| -------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| **A new `reliant-labs/<name>` repository, Apache-2.0** (recommended) | Its users are Kueue users, most of whom will never run forge. Its own releases follow Kueue's cadence, not forge's. A clean history and governance to donate to a foundation later. No forge CLI semver or lint rollout bound to it | A second release train; forge must pin it                                                                                           |
| In forge (`pkg/placement` plus a controller under `cmd/`)            | One repository; the SDK precedent (decision 26)                                                                                                                                                                                     | Couples a Kubernetes controller to forge's CLI releases and v1 compatibility rules; makes "usable without forge" harder to believe  |
| Upstream into Kueue as a built-in dispatcher                         | The widest reach                                                                                                                                                                                                                    | The value is the signal interface and the scoring model, which upstream would reasonably want generic; slow; loses the product seam |

Recommendation: the new repository for the controller, the scorer, the
`SignalProvider` interface and its local providers, the explanation format and
the simulator. forge renders and pins it (`MLStack` gains a `placement`
component, adopt or install as every slot is), and its pure scoring package is
importable so `forge ml place --explain` and the control plane compute the same
thing, as `pkg/releasehealth` is shared today. The name is
[decision 37](#15-open-owner-decisions).

### 13.2 The boundary: open scheduler, hosted signals

| Open (Apache-2.0)                                                                                                                                                                   | Hosted (Reliant)                                                                                                 |
| ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| The MultiKueue external-dispatcher controller and the optional budget AdmissionCheck                                                                                                | `GetPlacementSignals`: forecasts and measurements learned per org from the ledger and telemetry                  |
| Eligibility rules, the scoring model, hysteresis and failover policy                                                                                                                | The training-run and placement ledgers, predicted against actual over time                                       |
| `SignalProvider`, a versioned gRPC and Go interface, and providers for Kueue visibility, node inventory and topology, Prometheus queries, a declared price sheet and declared rates | Bad-node history across runs and node incarnations; per-recipe, per-shape throughput; restart-time distributions |
| The local journal, fencing protocol and explanation format                                                                                                                          | The console's "Why here", events, Inbox proposals, triggers, gates and budgets joined with spend                 |
| A trace-replay simulator and the k3d multi-cluster test fleet                                                                                                                       | Entitlement, support                                                                                             |

The test of the boundary: **a team with Kueue and no Reliant account gets a
better dispatcher than Incremental** (L1), and a Reliant customer gets the same
controller deciding with its own history (L2). Anyone can write a
`SignalProvider`; ours is good because of the data behind it, not because the
interface is closed.

### 13.3 Upstream

Contribute, never fork. Asks worth a KEP or an issue, each useful to everyone:

- a dispatcher name per MultiKueueConfig rather than one per manager (verify that
  it is global today), so ours can coexist with the defaults;
- cordoning a worker cluster (open issue #19574; verify);
- richer worker capacity in `MultiKueueCluster` status, which any dispatcher could
  use;
- a per-workload or per-queue lost-worker timeout;
- removing the field-manager pitfall on `nominatedClusterNames` (verify the
  current guidance).

## 14. Testing without GPUs

The owner will not pay for GPUs to test (ml-workloads settled item 10). All of
it runs on CPU-only k3d:

- **A fleet**: one management cluster and three workers, each a k3d cluster with
  `topology.kubernetes.io/region` and fabric-block labels standing in for regions
  and superpods. control-plane's dev already runs a k3d pair
  (`k3d-control-plane-v2`, `k3d-cp-daemon-v2`); the fleet extends the same
  pattern.
- **Fake accelerators**: run-ai's fake-gpu-operator on each worker's nodes;
  **KWOK** for hundreds to thousands of fake GPU nodes per worker, to exercise
  fit, fragmentation and fabric contiguity at scale.
- **Real dispatch**: the pinned Kueue and MultiKueue, our controller as the
  external dispatcher, and a CPU PyTorch DDP job (gloo) under TrainJob or JobSet
  requesting fake GPUs, checkpointing to one MinIO per "region".
- **Failure injection**:
  - `docker pause` a worker's nodes or disconnect its network: `cluster_lost`,
    fencing, re-placement, garbage collection on return;
  - delete nodes labelled spot: `spot_reclaimed` and failover;
  - fill a worker's queue with KWOK workloads: wait forecasts and widening;
  - `tc` rate limits between MinIOs: staging time, and the break-even decision
    flipping as throughput changes;
  - block the controller's route to the control plane: L2 to L1 degradation, the
    local journal and its replay;
  - kill an attempt's pods mid-checkpoint: an unpublished checkpoint is never
    resumed from;
  - un-pause a fenced worker: its stale attempt cannot publish.
- **The scorer, deterministically**: unit tests over recorded signal snapshots
  with golden explanations; property tests that no placement leaves residency,
  breaks the serving floor, exceeds a budget, or splits an atom; a trace-replay
  simulator, on a fake clock, that replays admission histories and reports the
  regret against hindsight. No test sleeps on `workerLostTimeout`: timeouts are
  configuration the tests shrink.

What only hardware proves: real fabric contiguity and topology labels on cloud
GPU pools, real pull and staging throughput, real spot reclaim behaviour, and
throughput models on real shapes. Those come from a design partner under the
gated program (ml-workloads decision 12).

## 15. Open owner decisions

Numbering continues reliant-for-ml.md (23–32). Each gives the realistic options,
a recommendation and what it blocks.

| #   | Decision                                               | Options                                                                                                                                                                                                             | Recommendation                                                                                                                                                             | Blocks                 |
| --- | ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| 33  | Architecture                                           | (a) MultiKueue with our logic as an AdmissionCheck; (b) an Armada-style standalone scheduler; (c) our own hub-side scheduler over Flux; (d) hybrid: MultiKueue dispatch, our external dispatcher, hosted signals    | **(d)**. It matches §3 item 1, keeps the safety guarantees upstream, and leaves (c) as the pivot if MultiKueue fails us                                                    | Everything             |
| 34  | Where the MultiKueue manager and controller run        | (a) A management cluster the customer designates (small, CPU); (b) a Reliant-hosted manager per org, isolated in its own virtual control plane, like the other hosted add-ons; (c) (a) first, then (b)              | **(c)**. (a) needs no tenancy work, keeps placing when our hub is down and works for private clusters; (b) follows the owner's hosted add-ons direction once (a) is proven | Slice M2               |
| 35  | Workers without Kueue                                  | (a) Require Kueue on every pool member (adopted or installed); (b) also a direct-dispatch path of our own; (c) leave such clusters out of pools                                                                     | **(a)**. One dispatch path to prove correct; Kueue is a managed add-on already                                                                                             | Slice M2               |
| 36  | Repository and licence                                 | (a) A new `reliant-labs/<name>` repository, Apache-2.0; (b) inside forge; (c) upstream into Kueue only                                                                                                              | **(a)**, with forge rendering and pinning it and upstream asks filed as KEPs                                                                                               | Slice M2               |
| 37  | The project's name                                     | Candidates to check for trademark, Go module and container registry clashes (none checked yet)                                                                                                                      | The owner's choice; avoid "kueue" in the name                                                                                                                              | Slice M2 (public repo) |
| 38  | Default mode of cross-cluster failover                 | (a) `recommend` when declared, `act` stated explicitly; (b) `act` when declared                                                                                                                                     | **(a)**, consistent with decision 32: a move spends money and copies data across regions                                                                                   | Slice M4               |
| 39  | Who copies checkpoints between regions                 | (a) Native object-store replication where available, else a rendered copier Job using an adopted tool (rclone); (b) the training framework writes every replica; (c) only native replication                        | **(a)**. Native is cheapest and not ours; the copier covers cross-cloud and on-prem                                                                                        | Slice M4               |
| 40  | Inventory standard                                     | (a) Our registry only; (b) also emit SIG-Multicluster ClusterProfiles for each connected cluster once the API is stable; (c) adopt OCM                                                                              | **(b)**, when KEP-4322 leaves provisional; it is how MultiKueue, OCM and GKE's orchestrator consume clusters                                                               | Slice M3 (optional)    |
| 41  | Inference replica placement                            | (a) Declared per cluster, with the controller recommending where to add replicas as a proposed KCL diff; (b) a `forge.Spread` placement on a workload, rendered per member; (c) dynamic placement by the controller | **(a)** in v1, (b) when a design partner needs it. Serving is long-running and Flux-delivered; dynamic placement of it is a different product                              | Slice M6               |
| 42  | Low-communication cross-region training (DiLoCo-style) | (a) Defer: keep room for a multi-island job kind; (b) build it now                                                                                                                                                  | **(a)**. Research-grade, about 10–15B parameters in open runs, not a default in mainstream stacks                                                                          | Nothing now            |
| 43  | Pricing                                                | (a) Part of the ML entitlement and observability ingest, no separate charge; (b) per placed accelerator-hour; (c) the open controller free, L2 signals a paid tier                                                  | **(a)** while ML is by request, revisited with ml-workloads decision 11                                                                                                    | General availability   |
| 44  | Dogfood with no GPUs                                   | (a) k3d fleet only; (b) also run control-plane's nightly container tests as Kueue jobs dispatched across our two prod clusters; (c) a design partner only                                                           | **(a) and (c)**; (b) only if the owner wants real multi-cluster batch on our own clusters, because it puts our CI on a new dispatcher                                      | Slice M5               |

## 16. Rollout

Measurement before action, as in reliant-for-ml §11. Each slice ships alone and
is useful without the next, behind the ML entitlement.

| Slice                             | What lands                                                                                                                                                                                                                                  | Useful alone because                                                           |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| **M0. Inventory**                 | Region, zones, labels and heartbeat on registry rows from the collector (ml-workloads slice 1); the capacity summary per shape and fabric block; Fleet shows clusters by region with free capacity and health                               | "Where are my free H100s right now" across every connected cluster             |
| **M1. Observed placement**        | For teams already running MultiKueue: read `status.clusterName`, record each attempt's cluster and its dispatch latency; `forge.ClusterPool` renders Kueue and MultiKueue configuration (adopt or install); `forge ml pool join`            | Where every attempt ran, and how long dispatch took, before we decide anything |
| **M2. Controller, shadow**        | The open-source repository: eligibility, the L0/L1 scorer, explanations, the local journal. Kueue keeps its own dispatcher; ours computes what it would have nominated and records **predicted against actual**. `forge ml place --explain` | Proves, on the customer's own history, how much better the decisions would be  |
| **M3. Controller, acting; L2**    | The controller as the external dispatcher (`nominate = 1`, widening); the budget AdmissionCheck; `PlacementService`, the placement ledger, events and "Why here"; L2 forecasts; the serving floor                                           | Shorter waits and lower cost, each explained                                   |
| **M4. Cross-cluster failover**    | End reasons `cluster_lost` and `migrated`; fencing; checkpoint replication (native, else the copier); pre-pull at targets; break-even in the score; `recommend`, then `act`                                                                 | A lost region costs minutes instead of a stalled run                           |
| **M5. Evals and backfill**        | Eval jobs placed near their checkpoints; with sweeps (when they land), trial fan-out and backfill by checkpoint age                                                                                                                         | Idle capacity and spare headroom do useful, interruptible work                 |
| **M6. Inference recommendations** | Where to add replicas for latency and failover, as proposed KCL diffs (decision 41 (a))                                                                                                                                                     | Region coverage for serving without dynamic placement                          |
| Later                             | Multi-island jobs (decision 42); a hosted manager (decision 34 (b)); ClusterProfile emission (decision 40)                                                                                                                                  | —                                                                              |

It slots into reliant-for-ml's phases: M0 rides phase B (fleet and health), M1
rides C (scheduling and pod start), M2–M3 follow E1 (observed runs, which gives
the ledger they learn from), and M4 follows E5 (resilience actions), because a
cross-cluster move is a resilience action across a wider boundary.

**Dogfood.**

- Every slice is proven on the CPU-only k3d fleet in e2e with the failure
  injection of [section 14](#14-testing-without-gpus).
- The golden path of the owner's MVP milestone includes it: change a training
  recipe in a worktree, run the small e2e, **dispatch the full run to one of two
  clusters**, recover from an injected interruption, publish an immutable
  candidate, evaluate it against production in staging, and promote by Flux or
  hold with the evidence. That is M1 to M4 on two k3d workers.
- Before access widens, a design partner with clusters in two regions runs M2 in
  shadow mode: the first real measurement of decision quality.

## Appendix A: verification list

To check against the pinned versions before any of it becomes rendered
configuration, a rule or user-facing copy:

- Kueue: the external dispatcher (`multiKueue.dispatcherName`, `status.nominatedClusterNames`, its limit of 10, the field-manager requirement) and its release (v0.13); whether the dispatcher is global per manager; `workerLostTimeout` default and behaviour; garbage collection of stale copies after reconnect; issues #18115, #18466 and #19574; MultiKueue job kinds and the versions they arrived in (TrainJob especially); `managedBy` Kubernetes versions; KEP-17401's capacity provider; lending limits and reclaim for the serving floor; TAS topology labels; the visibility API; manager HA guidance; the worker-side Kubernetes permissions MultiKueue needs.
- Kubeflow Trainer v2: TrainJob's use of JobSet and its `managedBy` support under MultiKueue.
- Sia (SOSP'23) and Pollux (OSDI'21): the throughput-model and goodput claims, and Sia's licence.
- SkyPilot: licence, the multi-Kubernetes `allowed_contexts` behaviour, the failover loop, the NSDI'23 and NSDI'24 results.
- Armada: whether a gang may span executors; the scorer extension story.
- Karmada: the release shipping multiple pod templates; graduation date.
- ClusterProfile (KEP-4322) status, and MultiKueue's ClusterProfile gate.
- Egress list prices on AWS, GCP and Azure; H100 hourly prices; spot interruption buckets.
- Llama 3 interruption figures; DeepSeek-V3 node-limited routing; INTELLECT-1 and DiLoCo figures; torchft's DiLoCo support.
- PyTorch DCP and Megatron resharding on load, for tested resizes.
- Object-store replication options per cloud, and their consistency guarantees for a manifest written last.
