# ADR: four environment verbs — `forge env {up,build,deploy,status} <env>`

Status: **approved by owner 2026-10-01**. Implementation tasks V1–V6 below.

## Problem

Getting code somewhere had ~8 overlapping entry points: `forge run`, `forge env up`,
`forge build [--push] [--release]`, `forge release cut`, `forge env promote`,
`forge env deploy`, and six read verbs (`env status`, `env verify`, `env wait`,
`env rollout`, `env topology`, `env history`). Several are literal duplicates
(`run` ≡ `env up`; `release cut` ≡ the tail of `build --release`; `env rollout` ≡
`env wait --timeout 0`) and the rest are views of one question.

## Decision

Everything that targets an environment is `forge env <verb> <env>`, like
`kubectl`/`terraform workspace`. Four verbs carry the whole lifecycle:

| Verb | Meaning | Absorbs (deleted) |
|---|---|---|
| `forge env up <env> [-- <dev-server flags>]` | Run the env on this machine: build, local apply, host processes, frontends. **Refuses** an env with any workload bound to a non-local runtime (hosted, a remote cluster) with: `<env> is not a local environment — use 'forge env deploy <env>'`. | `forge run` |
| `forge env build <env> [--release vX] [--push]` | Build the env's artifacts. `--release vX` implies push and records an immutable release (the noun "release" stays: an immutable digest set). Without `--release`, builds (and `--push` pushes) only. | `forge build --push`, `forge build --release`, `forge release cut` |
| `forge env deploy <env> [vX \| --from <src-env> [--from-promotion id]]` | Make `<env>` run release vX: record the promotion (CAS, `--expect-current`, `--supersede`), have it applied (hosted: the control-plane converger; self-managed: client-side render+apply in the same command), then **wait for health by default** (`--no-wait`, `--timeout`, `--fail-fast`). With no version on a self-managed env, renders the env's current binding (today's spec-change deploy). `--gate` records evidence. | `forge env promote`, `forge env deploy`'s old meaning, `promote --wait/--deploy` |
| `forge env status <env> [--wait] [--history] [--json]` | One view: bound release, rollout phase per workload, health, verify (running digests vs binding), gates, ledger freshness. `--wait` blocks until the rollout settles (the old `env wait`; exit codes unchanged). `--history` pages promotions. Without `<env>`: all envs (old `topology`). | `env status`, `env verify`, `env wait`, `env rollout`, `env topology`, `env history` |

Unchanged: `forge env down|render|smoke|list|new|config|options|secrets|ps|devstack`,
top-level `forge build` (compile-only, never pushes; a local check),
`forge gate record|list`, `forge ci run`, `forge release verify` (artifact
existence; not env-scoped), `forge release where`.

Exit codes, JSON envelope, run-id flags and every refusal (CAS → 3, in-flight → 4,
source_moved → 3, wait outcomes 1/5/6, unreachable 2) are **unchanged** — they move
with the code into the new verbs. Hosted/self-managed parity: `deploy` always means
"record + apply + (wait)"; who applies is an implementation detail.

Pre-1.0: the absorbed commands are **deleted**, not aliased. A removalguard entry
pins each deleted spelling. Docs, skills, the scaffold, generated CI and
`forge env deploy`'s existing help all move to the new verbs in the same wave.

## Tasks (disjoint ownership)

- **V1 `env up`** — fold `forge run` into the existing `forge env up` (the `--` dev-server passthrough moves onto it); non-local refusal + test; delete `forge run`. The local verb KEEPS the name `env up`: it is the spelling already in every doc, skill and scaffolded script, and renaming it to `env dev` would have churned all of them to say the same thing.
- **V2 `env build`** — move build `--push/--release` and `release cut` into `env build`; top-level `forge build` becomes compile-only (refuses `--push/--release` with a pointer); delete `release cut`.
- **V3 `env deploy`** — fold `promote.go`+`promote_*.go` into `deploy`: version/`--from` ⇒ promote path; wait-by-default; self-managed client-side apply after the ledger write; no version ⇒ current spec-change deploy. Delete `env promote`.
- **V4 `env status`** — merge status/verify/wait/rollout/topology/history into one command with `--wait`/`--history`; delete the others.
- **V5 docs/skills/scaffold** — every skill, doc, template, CHANGELOG, help text; removalguard entries for all deleted spellings. Runs after V1–V4 land (or in parallel, rebasing).
- **V6 scaffolded CI** (was F6) — `release.yml` + vendored `forge-promote` action on the new verbs: `forge env build <env> --release`, `forge env deploy staging`, `forge env deploy prod --from staging`, `forge gate record`, `forge ci run`.
