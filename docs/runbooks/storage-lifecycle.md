# Local storage lifecycle

`forge storage` bounds rebuildable development state. Persistent volumes and
workspaces have separate lifecycles: registry layers, containerd images, BuildKit
cache and application data can all live in Docker volumes, so volume age is not
an indication that a volume is safe to remove.

## Defaults

| Storage | Policy |
| --- | --- |
| Physical host filesystem | Refuse a build below 20 GiB available |
| BuildKit | Remove cache unused for 7 days toward 20 GiB per registered local builder |
| k3d node images | Kubelet removes unused images after 7 days; pressure GC at 80%, down to 70% |
| Node container logs | 10 MiB × 3 files per container |
| Registered local registry repositories | Keep 14 days and newest 5 distinct digests; protect workloads, release pins and stable aliases |
| Rotated Forge logs | Keep newest 5 per stream; expire after 7 days or toward 1 GiB per project/environment |
| Worktrees | Explicit command only; clean, merged, directory and commit older than 30 days |
| Database/PVC/workspace volumes | Never removed by storage GC |

The cache and log budgets are targets: recent/in-use cache and the diagnostic
floor can exceed them. No budget overrides release or workload protection. The
host-space check uses the host filesystem, not the much larger sparse disk that
Docker Desktop exposes to Linux. Add a custom Docker data disk to `host_paths`.

## Set up a machine

```sh
forge storage policy
forge storage register --context k3d-control-plane --context k3d-daemon \
  --repository localhost:5051/control-plane \
  --repository localhost:5051/workspace-base
forge storage gc --dry-run
forge storage install
```

Registration imports `.forge/releases/*.json` from the current project;
`--ledger` selects another ledger directory. Builds register their declared local
repositories, local clusters and project log directory automatically. Release
cuts pin their digests before the ledger entry is published. Import old projects'
ledgers when enabling cleanup for an existing shared registry. `--pin` accepts a
full image reference or a canonical digest. Pins remain until an operator edits
the policy; a failed release cut may leave a harmless extra pin.

The default policy is `forge/storage.json` inside the OS user configuration
directory (`~/Library/Application Support` on macOS, `$XDG_CONFIG_HOME` or
`~/.config` on Linux). `FORGE_STORAGE_POLICY` overrides it for all build hooks;
`--policy` overrides a particular storage command. JSON is strict: unknown fields
are errors. Edit `builders` to include custom builders, such as `relbuild`.
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
queryable, including scaled-to-zero controllers and ReplicaSets. Failure to read
a protected set prevents deletion. Only explicitly registered repositories are
eligible. Stable aliases (`dev`, `e2e`, `main`, `latest`, `stable`, exact semver)
are retained. Index children and untagged manifests remain protected; GC never
uses `--delete-untagged`. Arbitrary remote registries, customized registry
containers, and application volumes are outside this local implementation.

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
