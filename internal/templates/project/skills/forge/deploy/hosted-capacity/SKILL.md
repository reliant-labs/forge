---
name: hosted-capacity
description: The hosted capacity pre-flight — what `forge env deploy` and `forge env build --push` check with the control plane before building, what each refusal code means, and the Unimplemented fallback.
---

# Hosted capacity pre-flight

`forge env deploy <env>` (every form, including `--plan-only` and
`forge env deploy <env> <release>`) and `forge env build <env> --push` /
`--release` on an env with hosted workloads ask the control plane
(`DeployService.CheckDeployCapacity`, read-only) whether the org may run what
the deploy would run — summed CPU, memory and storage, workloads, databases,
builds, static sites — BEFORE anything is built, pushed or recorded. A refusal
exits non-zero with the reason, the fix, and demand vs. the plan's ceiling.
The server enforces the same decision at record/promote; the pre-flight only
makes it fail in seconds instead of after a build.

| Code | Meaning | Fix |
|---|---|---|
| `NO_COMPUTE_PLAN` | Compute (workloads, jobs, databases, builds) with no active compute plan | Subscribe to a compute plan in billing settings |
| `STATIC_FREE_TIER_EXCEEDED` | No plan; static-only deploy beyond the free tier (1 site, ~1 GiB) | Remove a site, or subscribe |
| `EXCEEDS_CEILING` | The plan's CPU/memory/storage ceiling is below the demand | Reduce resources or upgrade |
| `SPEND_CAP_REACHED` | The org's spend cap is reached | Raise the cap |
| `PLAN_UNKNOWN` | The plan could not be determined | Retry; it is never read as "no plan" |

A control plane that predates the check answers Unimplemented: forge prints a
one-line WARNING ("capacity could not be pre-checked") and continues. A
network or auth failure fails the deploy.
