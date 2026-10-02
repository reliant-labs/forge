# Disk hygiene in forge — design (2026-10-01)

Companion research: `disk-hygiene-gc-knobs.md` (each GC knob, verified against
the versions in use) and `disk-hygiene-forge-inventory.md` (every resource forge
creates, and what reclaims it).

## Context

A developer Mac running forge's dev stack went from comfortable to 97% full
(70 GiB free of 1.8 TiB). Measured consumers: the local k3d registry (55 GiB),
two k3d nodes' containerd stores (40 GiB), an inactive buildx builder (27 GiB),
Go linker scratch orphaned in `$TMPDIR` (15 GiB), forge's own leaked test
fixtures (~5 GiB), and the cross-repo source cache (9 GiB). Docker Desktop's VM
disk is a 1 TiB sparse file, so every percent-of-disk threshold (kubelet image
GC at 85%, BuildKit's default budget) sees a near-empty disk and never fires.

## Starting point: the storage-lifecycle commits

The first three commits on this branch (`internal/storage/` + `forge storage …`)
implement most of the machinery:

- Machine policy `~/Library/Application Support/forge/storage.json`
  (`internal/storage/policy.go`): absolute budgets — host reserve 20 GiB,
  BuildKit 20 GiB / 168h unused, kubelet `imageMaximumGCAge` 168h, registry
  14d + newest 5, log budget 1 GiB.
- `forge storage {check,status,policy,gc,daemon,install,register,configure-nodes,worktrees}`
  (`internal/cli/storage.go`, `storage_schedule.go` for launchd/systemd).
- Host-reserve admission check before every build lane (`checkBuildStorageFn`).
- New k3d clusters get a kubelet drop-in mount (`addClusterStorageArgs`,
  called from `createK3dCluster`).
- BuildKit prune per registered builder with `--max-used-space` + `until=`.
- Registry retention: protected set from pods/controllers/jobs in all registered
  contexts + docker containers + release-ledger pins; complete filesystem
  manifest inventory so untagged index children are retained; DELETE via a
  loopback helper with delete enabled; offline GC; no `--delete-untagged`.
- Rotated-log expiry; tierguard fixture cleanup; cross-process flock.

## Why it never ran (the core defect)

Activation is imperative: `register` + `install` must be run by hand, in order,
and never were. The policy file on the affected machine had no registries,
contexts or projects. A protection that must be switched on is a protection that
is off.
Philosophy: declarative, converge, working defaults.

## Gaps measured today, and the decisions

### G1 — Zero-touch activation (converge, don't register)

forge already knows, at `forge env up` / `forge env deploy` / `forge build`:
the env's declared `forge.Cluster`s (contexts), the local registry container
(from k3d config `registries.use` / `ensureConfigRegistries`), its host aliases
(the `registries.config` mirror keys), the repositories it pushes, and the
project's `.forge/releases` ledger. **Every one of those touch points converges
the machine policy** (idempotent upsert, under the storage lock, never removing
entries another project added). Registration of a registry requires the full
set (container + aliases + contexts + repos); partial facts are recorded only
when complete. `register` remains as the manual escape hatch.

Scheduling: `install` stays explicit (it writes a LaunchAgent — outward-facing),
BUT `forge env up` (and `forge doctor`) print a one-line notice when the policy
has registries and no schedule is installed, naming `forge storage install`.
Additionally, an **opportunistic pass**: at the end of a successful
`forge env up`, if the last completed GC (recorded timestamp in the policy dir)
is older than 24h, run the NON-disruptive layers in the background with a
short budget: builder prune, rotated logs, temp-dir sweep. Never registry GC
(that stops the registry) and never node restarts from an opportunistic pass.

### G2 — CRD / arbitrary-resource image references

The protected set reads only core workload kinds. `workspaces.reliant.dev`
CRs reference `workspace-base` tags; they survived today only by age.
Decision: protect by **scanning every listable namespaced+cluster resource for
image-shaped strings**, not by a kind allowlist: `kubectl api-resources
--verbs=list -o name`, then `kubectl get <all> -A -o json`, and collect every
JSON string value that parses as an image reference whose host matches a
registered alias (or a bare `@sha256:` digest). Fail closed: any list error
aborts deletion (current behaviour for workloads). Exclude only Events/
Secrets (noise / never reference images we can't see elsewhere — Secrets are
skipped for safety of not reading them). Unit-test with a fake CR.

### G3 — Untagged manifests under reused tags (~25 GB today)

`:dev` is re-pushed hundreds of times; each push orphans the prior manifest.
They are never reclaimed because "untagged" can also mean "index child / OCI
artifact". The branch already builds the complete reference graph (every
revision, walked from every retained root). Decision: an untagged manifest that
is **(a)** not reachable from any retained root (tag, pin, protected digest,
index child of a retained index), **(b)** not referenced by any protected
digest, and **(c)** older than the registry TTL by its revision-link mtime, is
eligible. This is graph-safe GC, distinct from distribution's unsafe
`--delete-untagged`. DELETE goes through the API by digest like tagged ones.

### G4 — kubelet GC on every forge-created cluster

`addClusterStorageArgs` covers `createK3dCluster` (declarative + `forge cluster
up`). The `forge env deploy` fallback (`deploy.go` ~L2649/2667 — raw
`exec.Command("k3d","cluster","create",...)`) bypasses it. Route both through
`createK3dCluster` (or apply `addClusterStorageArgs`). Also: the standalone
registry created by `k3dRegistryCreate` gets `--delete-enabled` (k3d v5.9
flag, verified) so API deletes work without the helper dance for new
registries (keep the helper path for existing ones).

### G5 — Test/temp leaks (forge's own suites)

- `forge-e2e-bin-*` (`internal/cli/scaffold_e2e_test.go` ~L428),
  `forge-skill-validate-*` (`internal/templates/skills_validation_test.go`
  ~L166), `forge-kcl-module-test-*` (`internal/templates/kcl_module_test.go`
  ~L74): sync.Once temp dirs never removed → `TestMain` removes them
  (`m.Run()` then RemoveAll; keep-on-failure is fine if it prints the path).
  Read-only files (go mod cache inside) need chmod-walk before RemoveAll.
- `embedded_postgres_log*`: upstream leaks one temp file per start
  (`fergusstrange/embedded-postgres` logging.go). pgtest + hostinfra: remove
  it after Stop (find via the library's logger field if exported, else sweep
  `$TMPDIR/embedded_postgres_log*` older than 1h at pgtest boot — pgtest
  already has a reaper, extend it).
- `go-link-*` (15 GB): Go linker scratch from killed links. Not forge code, but
  forge's sweep can reclaim it: `forge storage gc` adds a **temp sweep** layer:
  `$TMPDIR` entries matching a fixed allowlist of known scratch prefixes
  (`go-link-`, `go-build`, `embedded_postgres_log`, `forge-*`, `tierguard-`),
  older than 24h, not open by any process (one `lsof` snapshot), skipping
  anything containing a `.git` FILE or dir? — NO: tierguard trees contain a
  `.git` dir from `git init` inside a test fixture; allow `.git` dirs only for
  the `tierguard-` / `forge-` prefixes, never for unknown names.

### G6 — Dev log growth during a long `forge env up`

Logs are truncated per `up` but unbounded during one. Add a size-capped
rotating sink in the up tee (`internal/cli/up.go` `procRegistry.start`, both
background and foreground branches): rotate at 50 MiB to
`<svc>.<RFC3339-ish>.log` (the name shape `logs.go`'s `rotatedLog` regexp
already expires), keep the stream path stable so `grep $L/*.log` keeps working.
The background branch hands the fd to the child, so a rotating writer there
needs a pipe+copier goroutine instead of the raw file — do it only for the
foreground/tee path if the background path can't be done without changing
process supervision; document the remaining gap.

### G7 — Visibility: `forge doctor` disk check

New doctor check "Disk": host free bytes (statfs on project dir + TMPDIR),
Docker data allocated (Docker Desktop: `du` of Docker.raw if present), builder
cache size, registry volume size, policy activation state (registries
registered? schedule installed? last GC age). WARN under 50 GiB free or when
the schedule is absent with registries registered; FAIL under the host reserve.
Model on `internal/doctor/orphanedcluster.go` (probe struct with fakeable fns).

## Explicit non-goals (this wave)

- Switching the local registry to zot (research recommends it; it is the right
  long-term answer — online GC + declarative retention, no write outage — but it
  changes registry ownership and k3d integration. Separate design.)
- Moving dev to a stable per-worktree moving tag (viable — deploy pins by digest
  — but it changes build-state/tag semantics. G3 reclaims the same bytes.)
- Sweeping dangling compose volumes (may be other projects' databases; doctor
  reports them, never deletes).
- Worktree deletion beyond the branch's explicit `storage worktrees` command.
- Reclaiming a dead worktree's dev-stack state beyond its port block
  (`devstack.Prune`): its k8s namespace and per-worktree databases outlive it
  today. Two traps for whoever builds it: the mtime of `.git/worktrees/<name>`
  is not an activity signal (any `git status` touches it), and a worktree whose
  repo was deleted is invisible to `git worktree list` — find them by their
  `.git` file instead.
