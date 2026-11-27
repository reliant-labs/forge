# 01 — Metrics inventory

**Status:** product spec draft, 2026-11-26. The inventory is long, so it is split into four
part files. This file is the index, the shared conventions and the cross-cutting rules.
Every part uses the same columns: **Name** (exact metric/field/event) · **Source**
(exporter/component + version notes) · **Type & unit** · **Granularity/labels** ·
**Default on?** · **Why it matters** · **Used by view** (02-views) · **When absent**
(detection + recommendation id + one-clause fix). Each part ends with Derived metrics,
Gotchas and Sources.

| Part                                                                               | Covers                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | Rows |
| ---------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---- |
| [`parts/01a-hardware-node.md`](parts/01a-hardware-node.md)                         | DCGM full field set (utilisation, profiling pipes, memory/BAR1, power/energy/limits, thermals incl. HBM, clocks, all clock-event bits + violation counters, ECC SBE/DBE vol/agg, retired pages, row remap, Xid, PCIe, NVLink/NVSwitch/fabric, device info, labels, `dcgmi diag` r1–r4); Xid table (13 … 154) with action; node-problem-detector monitors/conditions/events + GPU/fabric custom checks (EKS `AcceleratedHardwareReady`, GKE, AKS, IB, Fabric Manager, GPU count); node-exporter incl. InfiniBand/RoCE; kubelet/cAdvisor/CRI; kubelet tracing spans; device plugin; GFD labels; DRA ResourceSlice; AMD/Gaudi/Neuron/TPU mapping; bad-GPU score                   | ~170 |
| [`parts/01b-scheduling-objects-cost.md`](parts/01b-scheduling-objects-cost.md)     | Kueue (~70 metrics, Workload conditions/reasons, TAS, MultiKueue, ProvisioningRequest/DWS); kube-scheduler + `FailedScheduling` classifier; Volcano/KAI/Run:ai/coscheduling; cluster-autoscaler, Karpenter, Spegel/Dragonfly; pod conditions, container reasons, exit codes, events; pod-start stages S0–S10 with formulas; orchestrator status for Job, JobSet, Trainer v2, PyTorchJob/MPIJob, KubeRay, Volcano, LWS, KServe, Gateway API Inference Extension; rank↔pod join keys; GPU-hour accounting, price models, OpenCost, goodput/badput                                                                                                                               | ~165 |
| [`parts/01c-training-traces-profiling.md`](parts/01c-training-traces-profiling.md) | Training metrics by family with the key each of 8 stacks logs (HF/Accelerate, Lightning, Megatron/NeMo, DeepSpeed, torchtitan, Composer, Levanter, MaxText); lifecycle phases (pretrain → midtrain → SFT → DPO/KTO → PPO/GRPO → distill → QAT → eval) and attempt semantics; trace span tree and sampling; NCCL debug/profiler plugin/RAS; PyTorch Flight Recorder; layered hang detection; profiling tool matrix (Kineto, HTA, Nsight Systems/Compute, memory snapshots, py-spy, eBPF, JAX) with overhead, size and on-demand trigger in k8s; W&B/MLflow system-metric keys; **the proposed `ml.train.*` / `ml.rl.*` / `ml.checkpoint.*` OTel conventions (~45 instruments)** | ~185 |
| [`parts/01d-inference-evals.md`](parts/01d-inference-evals.md)                     | vLLM V1 (every `vllm:` metric on main, incl. waiting-by-reason, admission rejections, cached tokens by source, KV block lifetimes, opt-in MFU), SGLang, TGI, Triton + TensorRT-LLM, NIM/Dynamo, KServe/Knative/llm-d/GIE/LWS, Ray Serve; neutral SLI mapping; OTel GenAI conventions (now in `semantic-conventions-genai`, all Development); request trace design with ingest-time child spans; evals data model, 15 framework output formats, benchmarks by category, eval statistics, online evals; model registry metadata                                                                                                                                                  | ~180 |

## Confidence

Researchers marked each fact (V) verified against an upstream source this session or (K)
from knowledge. Verified upstream this session: dcgm-exporter `default-counters.csv` and
`dcp-metrics-included.csv`; NVIDIA's Xid guide (79 and the 154 rule only; the catalog is now a
spreadsheet); NPD kernel monitor config; Kueue `metrics.go` and Workload conditions;
JobSet fields/labels; vLLM, SGLang, TGI, Triton metric sources; OTel GenAI semconv;
TRL GRPO/DPO keys; MLflow GPU system-metric keys and REST API; PyTorch Flight Recorder;
verl MoE metrics. **Everything (K) must be checked before it becomes an adapter rule or
user-facing copy** — notably the Xid rows 121–150, kubelet tracing span names, Trainer v2
status fields, Megatron/torchtitan/Composer key names and W&B system keys.

## Neutral field set (the schema the UI speaks)

Views and rules use neutral names; vendor/exporter names live only in the versioned
adapter data (ml-workloads decision 3). Groups, each mapped per part file:

| Group                | Neutral fields (examples)                                                                                                                                                           | Primary source      |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------- |
| `accel.util`         | `busy_ratio` (coarse), `engine_active`, `sm_active`, `sm_occupancy`, `tensor_active`, `fp16/fp32/fp64_active`, `dram_active`, `enc/dec_util`                                        | DCGM (01a §1.1–1.2) |
| `accel.memory`       | `used_bytes`, `free_bytes`, `reserved_bytes`, `total_bytes`, `bar1_used_bytes`                                                                                                      | 01a §1.3            |
| `accel.power`        | `power_watts`, `power_limit_watts`, `enforced_limit_watts`, `energy_joules_total`                                                                                                   | 01a §1.4            |
| `accel.thermal`      | `gpu_temp_c`, `memory_temp_c`, `slowdown_temp_c`, `shutdown_temp_c`                                                                                                                 | 01a §1.5            |
| `accel.clock`        | `sm_clock_mhz`, `mem_clock_mhz`, `max_sm_clock_mhz`, `throttle_seconds_total{reason}`                                                                                               | 01a §1.6–1.7        |
| `accel.reliability`  | `ecc_sbe_{volatile,aggregate}`, `ecc_dbe_{…}`, `retired_pages{sbe,dbe,pending}`, `remapped_rows{correctable,uncorrectable}`, `row_remap_{pending,failure}`, `xid_last`, `xid_event` | 01a §1.8, §2        |
| `accel.interconnect` | `pcie_{tx,rx}_bytes`, `pcie_replay_total`, `pcie_link_{gen,width}`, `nvlink_{tx,rx}_bytes`, `nvlink_errors_total{kind}`, `nvlink_links_down`                                        | 01a §1.9–1.10       |
| `node.health`        | NPD conditions/events, kubelet/runtime restarts, IB port state/errors, device plugin unhealthy                                                                                      | 01a §3–4            |
| `sched`              | queue wait, admission, preemption, quota/usage/borrowing, pod-start stages                                                                                                          | 01b                 |
| `train`              | `ml.train.step*`, `ml.train.tokens`, `ml.train.samples`, `ml.checkpoint.*` (forge names); loss, lr, grad norm, MFU, memory, RL and the rest as user-named step scalars              | 01c §6              |
| `serve`              | queue wait/depth, TTFT, ITL, TPOT, e2e, prefill/decode, tokens, batch, KV util, preemptions, prefix hit, spec acceptance, errors by reason, per adapter                             | 01d §1.7            |
| `eval`               | score, stderr/CI, n, per slice, delta vs baseline, significance, judge/human agreement, cost                                                                                        | 01d §4              |
| `cost`               | GPU-hours by category, $ by subject, $/1M tokens, goodput                                                                                                                           | 01b §5              |

## Cross-cutting rules

1. **Two DCGM naming schemes.** dcgm-exporter `main` renamed metrics with unit suffixes
   (`DCGM_FI_DEV_GPU_UTIL_RATIO`, `…_BOARD_POWER_WATTS`, `…_THROTTLE_SECONDS_TOTAL`, plus
   `DCGM_EXP_*`); the 3.3/4.x exporters most clusters run emit legacy names in %, MiB, mJ,
   µs. The adapter maps both and normalises units. DCGM 3.3 renamed throttle reasons to
   clock-event reasons (field 112 unchanged).
2. **Xid is a last-value gauge.** `DCGM_FI_DEV_XID_ERRORS` holds the last code; the
   _change_ is the event. Kernel-log `NVRM: Xid` lines (collected by the node agent's
   `filelog`) are the authoritative event stream; NPD turns them into conditions.
3. **Counters vs gauges.** ECC aggregate, energy, violation time, NVLink/PCIe error
   totals are counters (alert on increase/rate). Utilisation, temperature, KV % are gauges.
4. **"True" utilisation is `sm_active` / `engine_active`**, not `GPU_UTIL` (busy-kernel
   time, overstates). Under MIG, `GPU_UTIL` is unavailable; use per-instance profiling
   fields. Under time-slicing, per-pod attribution is not meaningful: say so.
5. **Profiling counters conflict.** DCGM profiling (DCP) and Nsight/CUPTI cannot both hold
   the performance counters; a profiling capture must pause DCP on that node or the UI
   must warn (01c gotchas).
6. **Attribution.** Device→pod needs the exporter's Kubernetes mode
   (`pod`/`namespace`/`container` labels via the pod-resources socket); device→rank needs
   rank↔pod join keys (01b §4.10) plus the pod's device list.
7. **Cardinality.** Per-device fields ~30 × device; per-rank only on the two per-rank
   training instruments; closed attribute sets (`ml.step.phase`, `ml.checkpoint.op`); NVLink totals by default, per-link only on the GPU detail
   page window. Fleet panels are distributions and top-N (50-series cap).
8. **Events expire** from the API server after ~1h; only a watch makes pod-start and Kueue
   timelines durable: the generic, opt-in Kubernetes inventory and events capability on
   forge's existing collector (forge#2998).
9. **Attribute naming.** The step-breakdown attribute is `ml.step.phase` (closed set
   data_wait/forward/backward/optimizer/comm_exposed/checkpoint/eval/other). The lifecycle
   phase of the object model is not an attribute: it is joined from the run ledger by run
   and attempt. The draft's `ml.phase` (forge#2884) stays as a deprecated alias of
   `ml.step.phase`. Ingestion normalizes legacy `ml.phase` to `ml.step.phase`;
   if both are present, `ml.step.phase` wins. Queries and dashboards apply the
   same fallback to stored legacy data before grouping, so old SDK emissions and
   historical series remain in phase breakdowns and longitudinal comparisons.
   Each observation is counted once, never once per alias. Keep this compatibility
   path throughout v1; retirement requires an owner-approved future major and a
   migration for legacy emitters and stored data. See the mapping below.
10. **Absent ≠ zero.** Every row's "When absent" id is a finding in the Coverage
    category; views show "not collected" with that finding, never a zero line.

## Attribute mapping

Narrowed (owner direction "simpler is better", 2026-11-26). Only run, attempt, step and
checkpoint get `ml.*` names, because OTel has none for them, with one temporary
exception: client token totals retain the ML-owned contract below until the
replacement GenAI metrics are verified. Everything else uses an
existing OTel name (checked 2026-11-26 against open-telemetry/semantic-conventions main and
the new open-telemetry/semantic-conventions-genai repository, where the GenAI conventions
now live; all of them are development stability).

`ml.*` names forge keeps:

| Concept    | Name                                                                                                                                                                                                                                                                                                                                                    | Notes                                                                |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| Run        | `ml.run.id` (resource)                                                                                                                                                                                                                                                                                                                                  | the ledger's run id                                                  |
| Attempt    | `ml.attempt` (resource, int, unique within the run); `ml.world_size` (resource); `ml.rank` only on per-rank instruments                                                                                                                                                                                                                                 | the phase is joined from the ledger by run and attempt, no attribute |
| Step       | `ml.train.step`, `ml.train.steps.total` (gauges); `ml.train.step.duration` (histogram, attribute `ml.step.phase`, closed set data_wait, forward, backward, optimizer, comm_exposed, checkpoint, eval, other; `ml.phase` is its deprecated alias); `ml.train.rank.step.duration`; `ml.train.tokens`, `ml.train.samples` (counters); span `ml.train.step` |                                                                      |
| Checkpoint | `ml.checkpoint.duration` (attribute `ml.checkpoint.op`), `ml.checkpoint.bytes`, `ml.checkpoint.step` (gauge, last durable); spans `ml.checkpoint.save` and `ml.checkpoint.load`                                                                                                                                                                         |                                                                      |

Everything else, existing names:

| Concept                      | Use                                                                                                                                                                                                                                                                                                                                                                                   | Stability   |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------- |
| Training template / workload | `service.name` (forge sets it per workload); `k8s.job.name`                                                                                                                                                                                                                                                                                                                           | stable      |
| Release                      | `service.version` (forge already stamps it: `kcl/render.k` `_otel_resource_attributes`)                                                                                                                                                                                                                                                                                               | stable      |
| Model identity (serving)     | `gen_ai.request.model`, `gen_ai.response.model`; the digest is not an attribute: join `service.version` and the model name to the release ledger's `oci` artifact                                                                                                                                                                                                                     | development |
| Provider                     | `gen_ai.provider.name` (`gen_ai.system` is deprecated)                                                                                                                                                                                                                                                                                                                                | development |
| Serving latency              | `gen_ai.server.request.duration`, `gen_ai.server.time_to_first_token`, `gen_ai.server.time_per_output_token`; `gen_ai.operation.name`                                                                                                                                                                                                                                                 | development |
| Client tokens                | Mapping deferred: `ml.client.tokens` (Counter, unit `{token}`, attribute `gen_ai.token.type` = input or output); replacement GenAI names and units must be verified before adoption                                                                                                                                                                                                                                          | development |
| Cluster objects              | `k8s.cluster.name`, `k8s.node.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.container.name`, `k8s.job.name`                                                                                                                                                                                                                                                                       | stable      |
| Host                         | `host.name`, `host.id`                                                                                                                                                                                                                                                                                                                                                                | development |
| GPU device                   | attributes `hw.id`, `hw.type = gpu`, `hw.vendor`, `hw.model`, `hw.serial_number`, `hw.driver_version`, `hw.firmware_version`, `hw.gpu.task`; metrics `hw.gpu.utilization`, `hw.gpu.memory.usage`, `hw.gpu.memory.limit`, `hw.gpu.memory.utilization`, `hw.gpu.io`, `hw.gpu.errors`, `hw.gpu.status`; power and energy through generic `hw.power` and `hw.energy` with `hw.type = gpu` | development |
| Not in OTel                  | per-GPU temperature, a k8s device attribute, a `gpu.*` namespace, a model digest or version, anything for training                                                                                                                                                                                                                                                                    | —           |

The client-token row is the supported ML-owned emission and query contract, not
an OTel mapping. Keep emitting and querying it until a replacement is verified
against a pinned upstream revision and its migration/query behavior is specified;
this inventory does not authorize switching to candidate GenAI token series.
Count input and output once per completed operation, including cached input and reasoning
output in their respective totals. Queries use `ml.client.tokens`; never add
legacy and replacement SDK series for the same operation. Legacy
`gen_ai.client.token.usage` histograms remain ingestible: their sum, split by
`gen_ai.token.type`, supplies token totals, not their observation count. The
candidate replacement names in 01d §2 are not an emission or query contract until
verified against a pinned revision of `semantic-conventions-genai`, including
instrument types and units; adopting them must retain this ML-owned path.

Vendor series with no OTel equivalent (DCGM profiling fields, temperature, XID) keep the
vendor's own name through the accelerator adapter (ml-workloads decision 3).

Dropped `ml.*` names, and where each went:

| Dropped                                                                                       | Now                                                                                                            |
| --------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `ml.training.name`                                                                            | `service.name`                                                                                                 |
| `ml.model.name`, `ml.model.digest`                                                            | `gen_ai.request.model` plus the release ledger through `service.version`                                       |
| `ml.phase.key`, `ml.phase.kind`                                                               | the run ledger                                                                                                 |
| `ml.framework*`, `ml.nccl.version`, `ml.parallel.*`, `ml.model.params`                        | run params in the ledger (MLflow-style params); the mesh (dp, tp, pp, ep, cp) is a run param, not a tag        |
| `ml.train.loss`, `lr`, `grad.norm`, `mfu`, `flops`, memory, MoE, `ml.rl.*`, `ml.train.custom` | step-keyed scalars named by the user (MLflow-compatible metrics) in the step-keyed table, not OTel conventions |
| `ml.model.load` span                                                                          | the serving runtime's own startup signals and the readiness condition                                          |
| `ml.eval.score`                                                                               | eval results in the ledger                                                                                     |

Activity-stream event types (`ml.run.launched`, `ml.attempt.ended`, `ml.checkpoint.durable`,
`ml.eval.regressed`, `ml.model.promoted`, …) are activity verbs, not OTel attributes, and
are unchanged.

## Recommendation ids referenced by the parts

The parts introduce ~60 `when absent` and detection ids beyond ml-workloads §5 and the
#2884 addendum (e.g. `dcgm-field-off-<group>`, `gpu-util-misleading`,
`idle-allocated-gpu`, `kueue-local-queue-metrics-off`, `checkpoint-interval-too-long`,
`flight-recorder-off`, `nccl-ras-off`, `inference-tracing-off`, `eval-no-ci`). A
consolidated catalogue (id, category, rule, impact unit, fix, slice) is the next work item
for the redesign; it should be generated from the parts so it does not drift.
