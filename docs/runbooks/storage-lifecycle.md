# Local storage lifecycle

`forge storage` bounds rebuildable development state. Persistent volumes and
workspaces have separate lifecycles: registry layers, containerd images, BuildKit
cache and application data can all live in Docker volumes, so volume age is not
an indication that a volume is safe to remove.

## Defaults

| Storage                                                                                                  | Policy                                                                                                                                                                                               |
| -------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Physical host filesystem                                                                                 | Refuse a build below 20 GiB available                                                                                                                                                                |
| BuildKit                                                                                                 | Remove cache unused for 7 days toward 20 GiB per registered local builder                                                                                                                            |
| k3d node images                                                                                          | Kubelet removes unused images after 7 days; pressure GC at 80%, down to 70%                                                                                                                          |
| Node container logs                                                                                      | 10 MiB × 3 files per container                                                                                                                                                                       |
| Registered local registry repositories                                                                   | Keep 14 days and newest 5 distinct digests; protect workloads, release pins and stable aliases                                                                                                       |
| Forge dev logs (`forge env up` foreground tee)                                                           | Rotate at 50 MiB (`FORGE_LOG_ROTATE_BYTES`; `0` disables); the current stream keeps its path                                                                                                         |
| Rotated Forge logs                                                                                       | Keep newest 5 per stream; expire after 7 days or toward 1 GiB per project/environment                                                                                                                |
| Cross-repo source cache (`<UserCacheDir>/forge/sources`)                                                 | Evict clones unused for 14 days beyond the newest 2 per repository (`source_cache_unused`, `source_cache_keep`)                                                                                      |
| Temp scratch (`$TMPDIR`)                                                                                 | Remove allowlisted toolchain/test scratch, and orphaned Go `t.TempDir()` roots (`Test…<digits>/` holding only `001`, `002`… dirs), idle 24h and open by no process; never anything with git metadata |
| Host images tagged for a dead local registry                                                             | Untag `localhost:<port>`, `registry.localhost:<port>` and `k3d-*:<port>` images when no running registry backs that alias and no container (running or stopped) uses the image                       |
| Shared Go build cache (`go env GOCACHE`) and golangci-lint cache                                         | Remove entries unused for 48h (`go_cache_unused`), then oldest-first toward 60 GiB (`go_cache_gib`); never an entry touched in the last 2h; top-level files are never touched                        |
| goimports/gopls module index (`<UserCacheDir>/goimports`)                                                | Keep the generation each `index-name-*` link names, anything touched in the last 24h; remove older generations                                                                                       |
| Orphaned private Go caches (`.gocache-*`, `*-gocache`, `$WORKTREE/.gocache`, `/tmp/scratchpad/gocache*`) | Remove a recognised build/module cache idle 24h that no live process environment names and no process holds open; never the shared cache                                                             |
| Worktrees                                                                                                | Idle ≥ 24h, unlocked, unused, clean, ignored files all rebuildable, HEAD pushed; removal needs `worktree_reap` (see Worktrees)                                                                       |
| Database/PVC/workspace volumes                                                                           | Never removed by storage GC                                                                                                                                                                          |

The cache and log budgets are targets: recent/in-use cache and the diagnostic
floor can exceed them. No budget overrides release or workload protection. The
host-space check uses the host filesystem, not the much larger sparse disk that
Docker Desktop exposes to Linux. Build checks also cover output paths, the OS
temporary directory, and explicitly configured `GOTMPDIR`, `GOCACHE` and
`GOMODCACHE` paths. Add a custom Docker data disk or cache mount configured
through a tool-specific config file to `host_paths`.

## Go caches

Do not set a private `GOCACHE`/`GOMODCACHE` per task and do not run `go clean
-cache` or `go clean -modcache`. Without `-trimpath` Go keys every main-module
package by its absolute directory, so each worktree builds its own copy of the
shared cache; a private cache per task is a cold 5-14 GB copy nobody reuses, and
`go clean -cache` under running builds produces `link: cannot open file
…/go-build/…-d`. Age-based trim is the safe mechanism: Go bumps an entry's mtime
on use once it is an hour stale, so an entry older than 2h is not in use by a
live build. The installed schedule (`forge storage install`) runs the bounded
non-disruptive pass (`forge storage auto-gc`) HOURLY, because agent load adds
~40 GB/h of cache entries and a daily trim cannot hold a 60 GiB budget; the
full pass, with registry downtime, stays daily at 03:30. Both take the
maintenance lock, so they never overlap. `forge env up` additionally starts
auto-gc at most once per 24h. `forge storage gc`/`auto-gc` apply it; `forge storage status` reports the shared cache
size. Orphan reaping fails closed: if the process list or the open-file listing
cannot be read, private caches are kept. The detached auto-gc pass has a 15
minute budget so a first trim of a very large cache can make progress; a pass
cut off by the deadline stops cleanly and resumes next time. Dry-run
(`forge storage gc`) prints each candidate and its bytes.

Direct Go, frontend, shell and Docker build lanes recheck capacity before starting.
`forge env up` checks before startup and host launches. `forge storage check
[path...]` exposes the same check for custom build/watch commands. New scaffolded
Air configs prefix every rebuild with `forge storage check ./tmp &&`; add this
prefix to existing/custom Air configs to guard their internal rebuild loop.
These are admission checks, not disk reservations: concurrent builds and one
large in-progress build can still consume the reserve.

On a CI runner (`CI` or `GITHUB_ACTIONS` set) a shortfall is printed as a
warning and the build proceeds. The reserve guards a persistent machine against
accumulated caches; an ephemeral runner's disk is discarded with the job, and a
stock GitHub runner starts below the 20 GiB default. A disk that cannot be read
still refuses. Set `FORGE_STORAGE_ENFORCE_RESERVE=1` on a persistent self-hosted
runner to keep the refusal.

## Set up a machine

There is nothing to register by hand. forge converges the machine policy from
facts it already holds, every time it touches them:

- `forge env up` / `forge env deploy` reconcile each declared `forge.Cluster`
  and record its context, the local registry its k3d config `registries.use`s,
  every host alias in that config's mirror keys, and the project's
  `.forge/releases` ledger pins. Warm clusters converge too, not only fresh
  creates.
- `forge build` records the local repositories it pushes and the Docker builder
  it actually uses (`BUILDX_BUILDER`, or `default`).

Convergence is additive and idempotent: it never removes an entry another
project contributed, and a registry becomes a cleanup target only once its
container, aliases, contexts and at least one repository are all known.
Projects under the system temp directory are never registered.

Each `forge storage gc` starts by pruning entries that no longer exist. A
project is dropped when its directory is gone. A cluster is dropped only when
both of these hold: its context is missing from kubeconfig, and docker shows no
container (running or stopped) labelled with it. A context that exists but is
unreachable, or a cluster whose nodes are only stopped, stays registered, and
registry cleanup keeps refusing on it. If either source cannot be read, nothing
is pruned. A registry entry is dropped when `docker ps -a` cleanly reports that
its container no longer exists (an e2e cluster's registry dies with the
cluster); an unreadable answer keeps it. A preview reports the pruning but does
not write it.

At the end of a successful `forge env up`, if the opportunistic pass has not
been attempted in 24 hours, forge starts the non-disruptive layers in the
background and returns immediately: rotated logs, builder cache, the temp sweep
and the source cache. The pass runs as a detached `forge storage auto-gc`
process. Its log is `logs/auto-gc.log`, next to the policy. The whole pass,
including each layer's `lsof` snapshot and its walk over entries, is bounded at
2 minutes. A layer cut off by that limit removes nothing further, and the rest is
picked up by the next pass. It never runs registry GC or restarts nodes from
that path. Set `FORGE_STORAGE_AUTO=0` to turn it off. It records each attempt,
failures included, in `last-auto-gc.json` next to the policy. That record is only
a rate limit: a machine where the pass cannot succeed is not retried on every
`up`.

Every applied full pass (`forge storage gc --apply`, and the scheduled job)
records its outcome in `last-full-gc.json`: when it ran, whether it succeeded,
and which layers failed. Only the full pass runs registry retention, so this
record is how forge knows whether that retention is working. If registries are
registered and the last full pass failed, is more than 48 hours old, or never
completed, `forge doctor` warns and `forge env up` prints one line saying so.

Registry GC, which briefly stops the registry, runs only from the scheduled job
or an explicit `forge storage gc --apply`. Install the schedule once per machine:

```sh
forge storage gc --dry-run   # review the plan
forge storage install        # daily at 03:30
```

`forge env up` and `forge doctor` say so when registries are registered but no
schedule is installed.

`forge storage register` remains the manual escape hatch: importing an old
project's ledger (`--ledger`), adopting a builder forge never used
(`--builder relbuild`), or pinning an image (`--pin`, a full reference or a
canonical digest). Release cuts pin their digests before the ledger entry is
published. Pins remain until an operator edits the policy; a failed release cut
may leave a harmless extra pin.

The default policy is `forge/storage.json` inside the OS user configuration
directory (`~/Library/Application Support` on macOS, `$XDG_CONFIG_HOME` or
`~/.config` on Linux). `FORGE_STORAGE_POLICY` overrides it for all build hooks;
`--policy` overrides a particular storage command. JSON is strict: unknown fields
are errors. Use `forge storage register --builder relbuild` to adopt another
local builder. An unavailable builder fails its own cleanup; independent healthy
builders and registries still receive maintenance.
Remote Docker contexts/builders are refused. Registration pins the Docker context
so a later context switch cannot redirect scheduled deletion elsewhere.

`install` saves a stable executable and schedules daily maintenance at 03:30 using
launchd or a systemd user timer. It does not depend on a worktree surviving. Run
it again after upgrading Forge. Linux users who need maintenance while logged
out must configure user-service lingering. `storage daemon --interval 24h` is the
portable foreground alternative. Logs are in `~/Library/Logs/forge-storage` on
macOS or the systemd journal on Linux.

Registry maintenance briefly stops pulls and pushes, replans behind a temporary
loopback-only registry, deletes eligible manifests, runs offline GC, then
restarts the public registry. All connected k3d clusters must be registered and
queryable. The protected set is every image reference found in ANY listable
resource in those clusters, custom resources included (a `Workspace` CR's image
is protected exactly like a Deployment's); only Secrets, Events and aggregated
metrics are skipped. Failure to list any resource prevents deletion. Only
explicitly registered repositories are eligible. Stable aliases (`dev`, `e2e`,
`main`, `latest`, `stable`, exact semver) are retained.

### Registries created before `--delete-enabled`

A registry created before forge passed `--delete-enabled` to k3d refuses
manifest DELETE, so retention could never reclaim anything from it. `forge
storage gc` detects this (no `storage.delete.enabled` in its `config.yml`, no
`REGISTRY_STORAGE_DELETE_ENABLED`) and, inside the maintenance window it
already takes for offline GC, rewrites `config.yml` in the stopped container
(`docker cp`). The outage therefore happens once, not on every pass. After the
restart forge sends a DELETE for a manifest that cannot exist and requires a
404 rather than 405. It is idempotent: a delete-enabled registry is never
touched, and a preview only announces that the migration would run. The edit
lives in the container's writable layer, so it survives restarts but not a
`k3d registry delete`; recreate through forge, which enables delete. If the
edit fails the pass still reclaims through its own delete-enabled helper and
the next pass retries. The volume, labels and networks are never touched.

### Stale host images

Images tagged for a local registry that no longer exists (for example
`localhost:5061/workspace-base`) are untagged by both the full and the
non-disruptive passes; `forge storage gc --dry-run` lists them. A `localhost`
or `registry.localhost` alias is live while a running container publishes that
host port or a registered running registry owns the alias; a `k3d-<name>` alias
is live while that container runs. An image any container still uses is kept.

Untagged manifests are reclaimed by forge's own graph walk, never by
distribution's `--delete-untagged` (unsafe with image indexes on 2.x). An
untagged manifest is eligible only when it is older than the retention window
AND unreachable from every retained root: retained tags, pins, protected
digests, and the children of any retained index. That reclaims the history a
re-pushed tag such as `:dev` leaves behind while keeping index children and OCI
artifacts. Any unreadable manifest aborts the whole plan. Registries forge
creates are created with `--delete-enabled`. Arbitrary remote registries,
customized registry containers, and application volumes are outside this local
implementation.

## Existing clusters

New k3d clusters receive a persistent kubelet configuration mount. K3s 1.32+ is
required for kubelet drop-ins. Existing nodes need a deliberate restart:

```sh
forge storage status
forge storage configure-nodes
forge storage configure-nodes --apply
```

Nodes restart sequentially and must report the configured age before proceeding.
This interrupts a single-node development cluster; it does not delete its data.
Kubelet's unused-image clock resets on restart, so frequent restarts can postpone
age-based collection. Pressure GC is complementary, not a substitute for host
space monitoring.

## Worktrees

Agents create worktrees anywhere (`~/src/rl-agent-worktrees`, `/tmp`,
`~/.reliant/worktrees`, `.claude`), so the reaper does not scan the disk. git
knows every worktree of a repository wherever it lives: the layer runs
`git worktree list --porcelain` for each repository in the policy's `repos`
plus the git toplevel of each registered project. `repos` is filled by the
same converge touch points as `projects` (`forge build`, the cluster phase): the
project's own repository, its local `docker.build_contexts` and its `go.mod`
`replace` directories, so a control-plane build also covers `../reliant` and
`../forge`.

A non-main worktree is **removable** only if all hold:

- not `locked` (the ownership contract: an owner that wants its worktrees left
  alone runs `git worktree lock`) and not prunable;
- idle at least 24h: the newest of the directory mtime and the admin
  directory's `index`, `HEAD` and `logs/HEAD` (activity, not commit age);
- not in use: one `lsof` snapshot covers all candidates and the layer **fails
  closed** if it cannot be taken; forge's own working directory is also kept;
- `git status --porcelain --untracked-files=normal` is empty;
- every **ignored** path is on the rebuildable allowlist (`worktree_rebuildable`;
  default `node_modules`, `dist`, `bin`, `.next`, `.turbo`, `coverage`,
  `.gocache`, `*.tsbuildinfo`, `kcl.mod.lock`, `go.work`, `.forge/logs`, …).
  `data/`, `.env*`, `.forge/hostinfra/`, `*.db`, `*.sqlite` and `secrets/` are
  never rebuildable, even if a policy lists them;
- pushed: HEAD is reachable from a remote-tracking ref, or is an ancestor of
  `origin/HEAD` (else `origin/main`). A squash-merged branch whose remote branch
  was deleted is therefore held, deliberately.

Removal is `git worktree remove` without `--force`; if git refuses the tree is
kept and reported. Branches are never deleted, and `git worktree prune` drops
only registrations whose directory is gone.

The layer is part of `forge storage gc` and the background auto-gc, but it only
**previews** until the policy sets `"worktree_reap": true`. `forge storage
status` lists held worktrees by reason (locked, recently active, in use, dirty,
unrecognized ignored data, unpushed) so a person can act on what the reaper
will not touch. The explicit command runs the same classifier over one repo:

```sh
forge storage worktrees --repo /path/to/repo            # preview
forge storage worktrees --repo /path/to/repo --apply
```

## Recovery and rollout

Before forge stops a registry for GC, it records a marker beside the policy
(`registry-maintenance/<container>.stopped`). After the pass, forge restarts the
registry and checks that it is running and answering, retrying for up to two
minutes. If it still is not serving, the pass fails and names the registry. A
pass that was killed partway leaves the marker, and the next `forge storage gc`
(a preview is enough) restarts the registry. A registry that someone stopped by
hand has no marker, so forge leaves it alone.

A hard kill or Docker outage can leave a `-forge-retention`/`-forge-gc` container.
The registry is never restarted while one is still running; never run filesystem
GC concurrently with a registry writer. Normal cancellation gets a separate
cleanup timeout. A stale helper blocks the next pass instead of overlapping GC.
Maintenance containers mount the registry's data volume by name, so removing one
can never remove the volume.

Disable any older ad hoc registry-retention launch agent before installing the
Forge job, and migrate its repository/context allowlist and pins. Two independent
maintenance programs do not share Forge's lock. Persistent workspace, database,
and unmerged worktree cleanup requires its own explicit teardown decision.
