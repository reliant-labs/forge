## V1 of `docs/adr/env-verbs.md`: one local verb

Implements ADR task V1, with the owner's amendment: the local verb **keeps the
name `forge env up`** rather than becoming `env dev`.

### (a) `forge run` folded into `forge env up`

`forge run` was a thin alias over the **same `runUp`** that `forge env up <env>`
calls — same render, same port-conflict guard, same non-TTY detach, same
per-service logs. Its one distinct feature was dev-server passthrough, which
now lives on the env verb:

```
forge env up dev -- --host 0.0.0.0     # reaches Vite/Next as `npm run dev -- --host 0.0.0.0`
```

The two spellings had already drifted, which is the actual argument for
deleting one: the alias took the environment as an `--env` **flag** defaulting
to `dev`, where `env up` takes it as a **required positional**. "Which env is
running?" had two different answers depending on which spelling you typed.

`forge run` is **deleted**, not aliased, with a removalguard entry.

### (b) `env up` refuses a non-local env

Before any work is done:

```
<env> is not a local environment — use 'forge env deploy <env>'
```

Locality is decided from the env's **own declaration** — its rendered runtimes
and cluster targets — never from machine state, so the verdict does not depend
on what happens to be installed or reachable. An env is non-local if it has:

| binding | why |
|---|---|
| a `forge.OnHosted` workload, database or frontend | the control plane runs it |
| a `forge.OnCluster` workload, or an env-wide `cluster_target`, whose context is not a recognized local one | it is someone else's cluster |
| a frontend bound to a bucket or Firebase | it ships to a CDN |

Local clusters are recognized with the existing `isLocalCluster` (k3d / kind /
docker-desktop / minikube / rancher-desktop / colima / orbstack) — the same
predicate that already gates plaintext Secret projection, so the two cannot
disagree. An **undeclared** cluster counts as non-local, deliberately: a
binding forge cannot place must not be assumed to be the harmless case.

**Where the gate sits matters.** It runs immediately after the render and
before the build, the apply, or any process start. Previously a hosted `env up`
got as far as a docker build and a `kubectl apply` against whatever context was
current, then died on something incidental — a missing cluster, an unreachable
registry — which reads as a broken machine rather than the wrong command.

This is the mirror image of the existing `refuseLocalEnvDeploy`, which stops
`env deploy` on an env that runs entirely here. Together the two verbs are
**total**, and whichever one you reach for names the other when it is wrong.
`TestEnvUpRefusalIsTotalWithDeployRefusal` pins that pair.

### Tests

New `internal/cli/env_up_locality_test.go`:

- **refusal** for a hosted env and for a remote cluster, each asserting the
  ADR's exact first line as a prefix (written out literally in the test, not
  built from the helper, so a reworded message fails instead of agreeing with
  itself) and that the message names the offending binding;
- **acceptance** for ten local shapes — host processes, compose, k3d, kind,
  docker-desktop, build-only, a frontend dev server, the scaffolded dev env,
  an empty env, a nil render. A gate that rejects the dev env would be worse
  than no gate, so this half is the one that keeps it honest;
- remote `cluster_target` with no cluster-bound workload, hosted database, and
  shipped frontend as separate cases.

`internal/cli/up_passthrough_test.go` (was `run_test.go`) moves the passthrough
coverage onto `env up`, including a case pinning that **no `--env` flag** comes
back, and that a second bare positional is now a usage error instead of being
silently swallowed.

### The sweep

The removalguard entry forced ~120 references across every surface forge ships
— Go comments, skills that ship downstream, project templates, docs, README,
Makefile, Taskfile and the scaffolded `.gitignore`. Two narrow allowances keep
text that documents the removal rather than referring to it: the ADR's own
Decision table, and the record of the collision that named `forge ci run`
(`ci_run.go` now says in the same breath that the colliding command is gone and
the name does not move back).

`forge project new`'s next-steps block and the `forge` skill's quickstart now
print `forge env up dev`, so the first command a new project is told to run
exists.

### Gates

- `go test -short ./...` — 108 packages, exit 0
- `go test -count=1 ./internal/cli/` — exit 0 (171s)
- `forge lint --no-fix` — exit 0, 12 gating linters

Tests fail before the change and pass after: the locality tests do not compile
against `main` (no `classifyEnvLocality`), and the five command-reference
suites — `TestCapabilities_EnumeratesTheRealTree`,
`TestNewNextStepsArePasteable`, `TestNewNextStepsResolveHelper`,
`TestSkillTemplatesReferOnlyToRealCommands`,
`TestSkillsForgeCommandReferencesExist` — each failed on the deleted command
until its emitting source was fixed.

### Note on the ADR

`docs/adr/env-verbs.md` is updated in this PR per the owner's instruction: the
table row and task V1 now say `env up`, and the title is
`forge env {up,build,deploy,status}`. The Problem section still describes the
pre-ADR state, including `forge run`, which is the point of it.

Siblings V3 (`env deploy`) and V4 (`env status`) touch disjoint files; the only
shared file is `internal/cli/env.go`, which this PR does not modify (`env up`
was already registered).
