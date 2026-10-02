# Local storage lifecycle

`forge storage` bounds rebuildable development state. Persistent volumes and
workspaces have separate lifecycles: registry layers, containerd images, BuildKit
cache and application data can all live in Docker volumes, so volume age is not
an indication that a volume is safe to remove.

## Defaults

| Storage                                                  | Policy                                                                                                                                                                                               |
| -------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Physical host filesystem                                 | Refuse a build below 20 GiB available                                                                                                                                                                |
| BuildKit                                                 | Remove cache unused for 7 days toward 20 GiB per registered local builder                                                                                                                            |
| k3d node images                                          | Kubelet removes unused images after 7 days; pressure GC at 80%, down to 70%                                                                                                                          |
| Node container logs                                      | 10 MiB × 3 files per container                                                                                                                                                                       |
| Registered local registry repositories                   | Keep 14 days and newest 5 distinct digests; protect workloads, release pins and stable aliases                                                                                                       |
| Forge dev logs (`forge env up` foreground tee)           | Rotate at 50 MiB (`FORGE_LOG_ROTATE_BYTES`; `0` disables); the current stream keeps its path                                                                                                         |
| Rotated Forge logs                                       | Keep newest 5 per stream; expire after 7 days or toward 1 GiB per project/environment                                                                                                                |
| Cross-repo source cache (`<UserCacheDir>/forge/sources`) | Evict clones unused for 14 days beyond the newest 2 per repository (`source_cache_unused`, `source_cache_keep`)                                                                                      |
| Temp scratch (`$TMPDIR`)                                 | Remove allowlisted toolchain/test scratch, and orphaned Go `t.TempDir()` roots (`Test…<digits>/` holding only `001`, `002`… dirs), idle 24h and open by no process; never anything with git metadata |
| Worktrees                                                | Explicit command only; clean, merged, directory and commit older than 30 days                                                                                                                        |
| Database/PVC/workspace volumes                           | Never removed by storage GC                                                                                                                                                                          |

The cache and log budgets are targets: recent/in-use cache and the diagnostic
floor can exceed them. No budget overrides release or workload protection. The
host-space check uses the host filesystem, not the much larger sparse disk that
Docker Desktop exposes to Linux. Build checks also cover output paths, the OS
temporary directory, and explicitly configured `GOTMPDIR`, `GOCACHE` and
`GOMODCACHE` paths. Add a custom Docker data disk or cache mount configured
through a tool-specific config file to `host_paths`.

Direct Go, frontend, shell and Docker build lanes recheck capacity before starting.
`forge env up` checks before startup and host launches. `forge storage check
[path...]` exposes the same check for custom build/watch commands. New scaffolded
Air configs prefix every rebuild with `forge storage check ./tmp &&`; add this
prefix to existing/custom Air configs to guard their internal rebuild loop.
These are admission checks, not disk reservations: concurrent builds and one
large in-progress build can still consume the reserve.

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

At the end of a successful `forge env up`, if no maintenance pass has run in
24 hours, forge runs the non-disruptive layers inline under a 2-minute budget:
rotated logs, builder cache, the temp sweep and the source cache. It never runs
registry GC or restarts nodes from that path. The attempt is stamped in
`last-gc.json` next to the policy even when it fails, so a machine where it
cannot succeed is not retried on every `up`.

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

```sh
git fetch origin
forge storage worktrees --repo /path/to/repo --base origin/main
forge storage worktrees --repo /path/to/repo --base origin/main --apply
```

This preserves the primary/current worktree, locked trees, unmerged commits,
dirty trees and untracked source files. Git performs removal without `--force`;
branches remain. Ignored build outputs in a removed tree are rebuildable. Review
the preview and avoid running this while someone is using an old clean tree.
Worktree removal is intentionally excluded from the daily cache job.

## Recovery and rollout

A hard kill or Docker outage can leave a `-forge-retention`/`-forge-gc` container.
Inspect and stop it before restarting the public registry; never run filesystem
GC concurrently with a registry writer. Normal cancellation gets a separate
cleanup timeout. A stale helper blocks the next pass instead of overlapping GC.

Disable any older ad hoc registry-retention launch agent before installing the
Forge job, and migrate its repository/context allowlist and pins. Two independent
maintenance programs do not share Forge's lock. Persistent workspace, database,
and unmerged worktree cleanup requires its own explicit teardown decision.
