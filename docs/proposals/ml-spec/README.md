# ML product spec (2026-11-26)

Durable spec for the ML redesign, written after the owner's review of the demo
("you're about 10% of the way there"). Owner input overrides anything here.

| File                      | What                                                                                                                                                                                                                    |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `00-object-model.md`      | Entities, hierarchy (project → experiment → run → phase → attempt → pod/rank → step; checkpoints, lineage), lifecycle states, attempt end reasons, mapping to k8s / forge / MLflow / W&B, storage, open questions O1–O6 |
| `01-metrics-inventory.md` | Index + neutral field set + cross-cutting rules; detail in `parts/01a`–`01d` (~700 rows)                                                                                                                                |
| `02-views.md`             | Every page and tab with wireframes, questions, metrics, interactions, empty states; decisions V1–V4                                                                                                                     |
| `03-competitive-bar.md`   | Synthesis; detail in `parts/03a` (trackers/registries/evals) and `parts/03b` (platforms/fleet/inference)                                                                                                                |
| `04-gap-vs-demo.md`       | Gap per view vs reliant#3693 / control-plane#3747, phased plan (A fixtures-first … G breadth), decisions G1–G4                                                                                                          |

Sources: forge `docs/proposals/ml-workloads.md` (main) and the observability addendum
(forge#2884, open). Confidence markers (V)/(K) are per fact in the part files.

Related proposals:

- [`docs/proposals/ml-workloads.md`](../ml-workloads.md): ML training and inference in
  your own clusters.
- [ML observability: where time and money go](../ml-workloads.md#ml-observability-where-time-and-money-go),
  the observability section of `ml-workloads.md` (forge#2884; open when this spec was
  written, merged since).
- [`docs/proposals/reliant-for-ml.md`](../reliant-for-ml.md): Reliant for ML, releases,
  testing, previews, observability and feedback loops for training and inference
  (forge#2888, open at the time of writing).

## Owner input log

- 2026-11-26 01:44Z: hierarchy CONFIRMED ("that's about right"): project → experiment → run → phase → attempt (wave) → pod/rank → step, checkpoints linking phases and producing model versions.
- 2026-11-26: owner will brain-dump in parallel; their input overrides research. Append it here verbatim as it arrives.
- Default decisions taken until the owner overrides (owner said "go for it all" / "both in parallel"): V1 nine sub-pages (Overview, Fleet, Setup, Queues, Recommendations, Runs, Evals, Models, Inference); V-renames Machines→Fleet, Stack→Setup; chart depth = strong defaults + compare, link out for custom reports; G1 reshape early-access proto now; G2 full demo on fixtures first; G3 B→C→D→E→F; G4 keep gate/access from reliant#3693 + cp#3747, redo pages and proto.
- 2026-11-26 01:51Z owner brain-dump (VERBATIM, overrides research):
  "the idea for the ML work is to mirror reliant, but for ML. ... we provide all the best practices around testing, releasing, security, observability, and more, so that you can just focus on business logic. and this type of peripherals make it easier to build the business logic by creating feedback loops and improving reliability. I'd like to do the same with ML. ie: we use flux for releases, should we do something like Kueue, or Ray for ML workloads? We tie releases into alerts to auto rollback, can we detect bad training, and improve things? We have scaffolding built into code, should we do that for pytorch, or other languages/frameworks? Everything in reliant is opt-in, so we'd need the same for ML workloads. We have ephemeral environments, security, testing.
  Some things we want for ML: 1. all the hardware stuff. more on DCGM, node exporter, node problem detector. 2. training runs with training id's to tie metrics to specific runs, training ranks, world size, mesh, dp, tp, pp, ep, cp, Kueue attributes and workloads; training and inference traces built deep into the workloads, and tied to dcgm stats, kubelet stats, scheduling and kueue (likely through trace spans); training step stats, phase stats, token stats, training loss, mfu, grad norm, learning rate; Memory/cpu/gpu alloc/util; Tflops, lm_loss, grad_norm, timers, sampling, queries, **goodput**, model vitals, model insights. Can we create the platform that allows and enables (because we can combine with reliant agentic workflows itself), self improving reinforcement learnings. Scheduling, histograms, continuous profiling, more, auto alerts, etc. Maybe some of this uses existing nav tabs, maybe not. thoughts?"
  Note: Reliant does NOT auto-rollback today (north-star N1 = roll forward; failed rollouts keep the previous release serving; a regressed release halts promotion). Training auto-remediation (rewind to last good checkpoint, cordon bad node + new attempt) is a different, safer case.
  Existing console nav (reliant frontends/web/src/components/Layout/sidebar/navConfig): Overview, Deployments, Previews, Observability (Dashboards, Analytics, Speed insights), Tests, Security, Inbox, Agents, Workflows, Runs (= workflow runs — name collision with ML runs), Sandboxes, Connectors, Library, Auth, Configure, Usage & billing.
- 2026-11-26 02:20Z owner decisions:
  - Nav: owner asked "will these be new flavors/mechanisms under the hood, or reuse existing primitives like OCI, Flux?" Answer given: reuse (OCI registry + digests for weights/checkpoints/eval sets, Flux delivery, release ledger + obs check, OTel/ClickHouse obs pipeline, Changes, alerting/Inbox, workflows, previews). Genuinely new: a training-run ledger (run/phase/attempt records, like releases), the Python SDK, Kueue/JobSet as adopted upstream (not ours). So HYBRID is the working default unless the owner objects.
  - Python: instrumentation SDK first, PyTorch supported (HF Trainer, Lightning, torchtitan, Megatron, DeepSpeed callbacks) + `ml/instrument` skill; `forge scaffold training` later. SETTLED.
  - Bad training: opt-in auto-remediation declared in KCL. SETTLED. Plus, owner emphasis: at large scale (thousands of GPUs, all collaborating), fast checkpoint / replace / resume is the priority — downtime costs millions. Design for minimal time-to-resume: fast failure detection, hot spares, fast checkpoint, node replacement without full restart, restart-time breakdown, goodput.
  - Self-improving RL: build the FEEDBACK-LOOP HOOKS first (events, APIs, CLI, triggers). Users plug in the forge CLI however they like; Reliant workflows are one harness on top. SETTLED.
