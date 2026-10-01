# Disk hygiene — GC knobs per layer (researched 2026-10-01)

Companion to `disk-hygiene-briefing.md`. Durable; do not delete.
Premise (settled): Docker Desktop's VM disk is a 1 TiB sparse file, so every
percent-of-disk threshold is useless. We need **absolute-byte or age** limits.

## Versions verified on this machine (read-only probes)

| Component | Version | How verified |
|---|---|---|
| k3s node | `rancher/k3s:v1.36.3-k3s1` (`k3s version v1.36.3+k3s1`) | `docker exec k3d-control-plane-v2-server-0 k3s --version` |
| k3d CLI | v5.9.0 (registry container was created by k3d v5.8.1 per label) | `k3d version`, `docker inspect` labels |
| Docker Engine | 29.8.0, Docker Desktop, overlay2, root `/var/lib/docker` | `docker version`, `docker info` |
| buildx | v0.37.1; BuildKit v0.33.0 in `default`/`desktop-linux` (docker driver) | `docker buildx version`, `docker buildx ls` |
| Registry | `docker.io/library/registry:2` → **distribution 2.8.3** | `docker exec k3d-control-plane-registry registry --version` |
| Docker Desktop VM disk | `DiskSizeMiB: 1048576`, `DiskTRIM: true`; VM sees `/dev/vda1 1007G, 149G used` | `settings-store.json`; `docker run --privileged --pid=host alpine nsenter -t 1 -m df` |

---

## 1. kubelet image GC on k3s 1.36

### 1a. `imageMaximumGCAge`
**Verdict: exists, GA (stable) since Kubernetes 1.35, default ON; config-file field only — there is no `--image-maximum-gc-age` CLI flag.**
Feature gate `ImageMaximumGCAge`: alpha 1.29, beta (default on) 1.30–1.34, stable 1.35+.
The k3s node already renders `imageMaximumGCAge: 0s` (= disabled) and
`imageMinimumGCAge: 2m0s` into its defaults drop-in (observed in
`/var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00-k3s-defaults.conf`).
Note `k3s kubelet --help` prints nothing in a k3d node (no output), so flag
discovery has to come from upstream docs; upstream deprecates GC flags in
favor of the config file and never added one for max age.
```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
imageMaximumGCAge: 72h
```
Caveats: age is "time unused", tracked **in kubelet memory** — a kubelet
restart (= k3d node container restart, `k3d cluster stop/start`, Docker
Desktop restart) resets the clock, so laptops that restart daily may never
reach a 72h threshold. Pick something like 24h–48h, or pair with an explicit prune.
Sources: https://github.com/kubernetes/website/blob/main/content/en/docs/reference/command-line-tools-reference/feature-gates/ImageMaximumGCAge.md ,
https://kubernetes.io/docs/concepts/architecture/garbage-collection/#image-maximum-age-gc

### 1b. How k3s accepts kubelet config
**Verdict: both work. k3s runs kubelet with `--config-dir=/var/lib/rancher/k3s/agent/etc/kubelet.conf.d` (observed in the node's process table). `--kubelet-arg=config=<file>` is copied to `10-cli-config.conf`, `--kubelet-arg=config-dir=<dir>` to `20-cli-config-dir/`, inside that managed dir; `00-k3s-defaults.conf` is regenerated each start, later files override.**
Writing your own file directly into `kubelet.conf.d/` also works but that dir is
on the k3s data volume and is k3s-managed; prefer the flag.
Source: https://github.com/k3s-io/k3s/blob/main/pkg/daemons/agent/agent.go (`extractConfigArgs`, `writeKubeletConfig`);
k3s docs "Kubelet configuration files": https://docs.k3s.io/installation/configuration (config file / drop-ins).

### 1c. Passing through k3d `Simple` config
**Verdict: mount the file into every node and point `--kubelet-arg=config=` at it, with node filters covering servers and agents.**
```yaml
apiVersion: k3d.io/v1alpha5
kind: Simple
volumes:
  - volume: /abs/host/path/.forge/k3d/kubelet-gc.yaml:/etc/forge/kubelet-gc.yaml
    nodeFilters: [server:*, agent:*]
options:
  k3s:
    extraArgs:
      - arg: --kubelet-arg=config=/etc/forge/kubelet-gc.yaml
        nodeFilters: [server:*, agent:*]
```
Caveats: `extraArgs` are baked into the node container `Cmd` at creation
(observed `Config.Cmd` = `server --disable=… --cluster-cidr=…`), so this only
applies to **newly created** clusters — existing clusters need recreate. Host path
must be absolute and exist before `k3d cluster create`; on Docker Desktop the path
must be under a file-shared dir. Alternative without a host file: k3d `files:`
(v5.5+) to inject content directly, which forge could render from KCL.
Pattern in forge: `spliceK3dCIDRs` (`internal/cli/dev_cluster_ingress.go`).
Source: https://k3d.io/stable/usage/configfile/

### 1d. Does age GC delete images used by stopped-but-present containers?
**Verdict: No.** The kubelet image manager only frees images not referenced
by any container the runtime reports (`ListContainers` includes exited
containers); exited pod containers keep their image "in use" until the kubelet's
container GC removes them (MinAge/MaxPerPodContainer). So images of current
pods (running or crash-looped) are safe; images of deleted pods become
eligible after `imageMaximumGCAge` of non-use. Also `imageMinimumGCAge` (2m) is
a floor. Pinned images (sandbox/pause) are never removed.
Source: kubelet `pkg/kubelet/images/image_gc_manager.go` (`detectImages` marks images used by `runtime.GetPods(ctx, true)` — all pods incl. exited containers): https://github.com/kubernetes/kubernetes/blob/master/pkg/kubelet/images/image_gc_manager.go

### 1e. Byte-absolute knob?
**Verdict: confirmed none.** Only `imageGCHighThresholdPercent` /
`imageGCLowThresholdPercent` (percent of imagefs) and `evictionHard`
(`imagefs.available` accepts an absolute quantity like `50Gi`, but that is
*eviction pressure* — it evicts pods and then triggers image GC; on a 1 TiB
VM, "available < X Gi" would only fire when the Mac is already full, and evicting
pods is not a hygiene tool). Observed k3s defaults: `evictionHard imagefs.available: 5%`.
Source: https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/

### 1f. `crictl rmi --prune` on a live node
**Verdict: safe for running workloads; it removes every image not used by an existing container (running OR exited) — it does NOT keep "recent" images, so the next pod start re-pulls.**
Help text (observed): `--prune, -q  Remove all unused images`. Also observed
warning: `crictl rmi <tag>` removes the whole image with ALL its tags.
Kubernetes docs discourage external GC tools because they race the kubelet;
the practical risk is a pod being scheduled between check and delete →
`ImagePullBackOff` until re-pulled from the local registry (cheap here, since
`localhost:5051` is local — unless the registry was pruned too, see 3).
Prefer `imageMaximumGCAge`; use `crictl rmi --prune` only as an explicit
"forge clean" action, never on a timer.
```sh
docker exec k3d-<cluster>-server-0 crictl rmi --prune
```
Source: `crictl rmi --help` in node; https://github.com/kubernetes-sigs/cri-tools/blob/master/docs/crictl.md

---

## 2. BuildKit / buildx GC

### 2a. `docker buildx prune` flags (buildx 0.37.1, verified)
**Verdict: absolute-byte flags exist: `--max-used-space`, `--reserved-space`, `--min-free-space`, plus `--filter until=<dur>`. `--keep-storage` is gone from help (deprecated alias of `--reserved-space`).**
```sh
docker buildx prune -f --builder <name> --filter until=168h --max-used-space 10GB
```
`--min-free-space` is measured against the builder's fs (the 1 TiB VM) → useless here.
Source: `docker buildx prune --help`; https://docs.docker.com/reference/cli/docker/buildx/prune/

### 2b. Docker driver (default / desktop-linux) — daemon.json `builder.gc`
**Verdict: configured in `~/.docker/daemon.json` (Docker Desktop: Settings → Docker Engine); this machine already has `defaultKeepStorage: "20GB"`, enabled.** Effective: the 0.5 GB default-builder cache is bounded. Policies can use `reservedSpace`/`maxUsedSpace`/`minFreeSpace`/`keepDuration`/`filter`. Engine-wide; forge must NOT edit it (user-owned, restart required). Doctor can *read* it via `docker info`? — not exposed; read the file only.
```json
{"builder":{"gc":{"enabled":true,"defaultKeepStorage":"20GB",
  "policy":[{"keepDuration":["168h"],"maxUsedSpace":"20GB"},{"all":true,"maxUsedSpace":"20GB"}]}}}
```
Caveat: defaults if no config are `reservedSpace` = min(10%, 10GB), `maxUsedSpace` = min(60%, 100GB), `minFreeSpace` 20GB — the 100GB cap is absolute but far too high.
Source: https://docs.docker.com/build/cache/garbage-collection/

### 2c. docker-container driver — `buildkitd.toml`
**Verdict: per-builder GC lives in buildkitd.toml `[worker.oci]` + `[[worker.oci.gcpolicy]]`; a CLI CAN set it at creation with `docker buildx create --buildkitd-config <file>` (flag verified).** Config is copied into the builder container at create; changing it needs `buildx rm` + recreate.
```toml
[worker.oci]
  gc = true
  maxUsedSpace = "10GB"
  reservedSpace = "2GB"
  [[worker.oci.gcpolicy]]
    keepDuration = "168h"
    maxUsedSpace = "5GB"
  [[worker.oci.gcpolicy]]
    all = true
    maxUsedSpace = "10GB"
```
Caveat: GC only runs while the builder container is running; an inactive builder (like `relbuild`, 27 GB) never shrinks — only `buildx prune`/`buildx rm` frees it. Forge does not currently create builders.
Source: https://github.com/moby/buildkit/blob/v0.33.0/docs/buildkitd.toml.md

### 2d. Per-invocation option?
**Verdict: none. `docker buildx build` has no GC/cache-size flag; GC is a builder/daemon property. Forge must either own a builder (create with `--buildkitd-config`) or run `docker buildx prune --max-used-space … --filter until=…` after builds (cheap, idempotent, targets only the builder forge used).**

---

## 3. distribution / registry GC

### 3a. Deleting manifests via API
**Verdict: `storage.delete.enabled: true` is required (DELETE returns 405 `UNSUPPORTED` otherwise). Settable via env `REGISTRY_STORAGE_DELETE_ENABLED=true`. k3d `registry create` exposes `--delete-enabled` (verified in k3d v5.9.0 help) and `-v/--volume` (to mount a config.yml), but has NO `--env` and NO `--label`; in a cluster config `registries.create` likewise has no env.** The running registry's config.yml (observed) has no `delete` section → deletes currently impossible without recreate. Recreating keeps data if the volume is reused (`-v <vol>:/var/lib/registry`).
Tag deletion granularity: distribution deletes a *manifest by digest*, which drops every tag pointing at it.
Sources: https://distribution.github.io/distribution/about/configuration/#delete ; `k3d registry create --help`.

### 3b. `garbage-collect --delete-untagged` dangers
**Verdict: on 2.8.3 (what runs here) `--delete-untagged` is UNSAFE with multi-arch/OCI image indexes — it deletes untagged child manifests referenced only by a tagged index (issue #3178), corrupting multi-platform images. Fixed in distribution v3 (PR #4285). Concurrent pushes are unsafe: blobs uploaded but not yet linked by a manifest are swept.**
Docs: "ensure that the registry is in read-only mode or not running at all."
Read-only mode (`REGISTRY_STORAGE_MAINTENANCE_READONLY='{"enabled":true}'` / `storage.maintenance.readonly.enabled`) is a **config** setting → requires a registry restart to toggle on and again to toggle off; it saves nothing over "stop, run GC in a one-shot container on the same volume, start". Either way there is a write outage. The existing control-plane script (stop + offline GC) is the correct pattern for distribution.
```sh
docker stop k3d-control-plane-registry
docker run --rm -v <vol>:/var/lib/registry registry:2 garbage-collect /etc/docker/registry/config.yml  # no -m on 2.x if any image is multi-arch
docker start k3d-control-plane-registry
```
Sources: https://distribution.github.io/distribution/about/garbage-collection/ ;
https://github.com/distribution/distribution/issues/3178 ; https://github.com/distribution/distribution/pull/4285

### 3c. Registry v3
**Verdict: v3 (`registry:3`, distribution 3.x) fixes the index handling in `--delete-untagged` and supports OCI properly; GC is still offline mark-and-sweep with the same read-only/stop requirement; no retention policies. Here: `registry:2` = 2.8.3. k3d's default image is still `registry:2`; forge can pass `--image registry:3`.** Caveat: v3 config file path/env names largely unchanged but verify before switching; storage layout is compatible.

### 3d. Alternatives
- **crane / regctl**: `crane delete <ref>` / `regctl tag rm <ref>` (regctl uses the OCI tag-delete API, falling back to a dummy-manifest trick) — both still require `delete.enabled` on distribution and still need offline GC to free blobs. Useful as libraries (go-containerregistry is already the natural Go dep) for "list tags, delete old", not a GC solution.
  Sources: https://github.com/google/go-containerregistry/blob/main/cmd/crane/doc/crane_delete.md , https://github.com/regclient/regclient/blob/main/docs/regctl.md
- **zot** (`ghcr.io/project-zot/zot-linux-<arch>`): built-in online GC (`gc`, `gcDelay`, `gcInterval`) and **declarative retention** (`storage.retention.policies` with `keepTags` by `mostRecentlyPushedCount`, `pushedWithin`, `pulledWithin`, regex patterns; `deleteUntagged`), referrers-aware, no stop needed.
  Source: https://zotregistry.dev/latest/articles/retention/ , https://github.com/project-zot/zot/blob/main/examples/config-retention.json
- **Assessment: zot wins for forge's local registry.** It matches forge's "declarative, converge" philosophy: forge renders one JSON from KCL and the registry enforces itself continuously, without forge owning a stop/GC/start state machine, without the 2.8.3 index bug, and without a write outage. Costs: k3d's `registry create` assumes distribution (`--image` can swap it, but k3d also writes a distribution-style config and expects port 5000) — forge would create the registry container itself (forge already runs "standalone registry ensure" in `cluster_registry.go`) and wire it via k3d `registries.use`/`registries.config` mirrors. Retention can't see "image used by a pod in cluster X", so keep a generous `mostRecentlyPushedCount` + `pulledWithin` (kubelet pulls count as pulls, which naturally protects images in use after a re-pull) or keep forge's protect-list pass via deletes. Migration: push-through or re-build; old volume can be dropped after.

---

## 4. Docker Desktop

### 4a. Does deleting data inside the VM release host disk?
**Verdict: yes on current Docker Desktop for Mac with `DiskTRIM: true` (observed): the VM's ext4 discards and the sparse Docker.raw is hole-punched, typically within minutes, not instantly.** Measure with `du -h ~/Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw` (allocated), not `ls` (apparent 1 TiB). Caveat: discard is batched (periodic fstrim), so freed space may lag.
Source: https://docs.docker.com/desktop/troubleshoot-and-support/faqs/macfaqs/ (disk space / Docker.raw)

### 4b. Supported reclaim trigger
**Verdict: `docker run --privileged --pid=host docker/desktop-reclaim-space` is the documented legacy trigger (runs fstrim in the VM); with TRIM enabled it is normally unnecessary. Equivalent read-only-safe form: `docker run --rm --privileged --pid=host alpine nsenter -t 1 -m -- fstrim -av`.** Restarting Docker Desktop also trims.

### 4c. Can a CLI read size/free?
**Verdict: VM fs size/free yes, via `df` in the VM (`docker run --rm --privileged --pid=host alpine nsenter -t 1 -m -- df -B1 /var/lib/docker`, verified: 1007G/149G). Unprivileged `docker run --rm -v /var/lib/docker:/x alpine df` is NOT valid (mounts aren't the data root). `docker info` exposes neither size nor free; `docker system df` exposes only Docker-object usage. The cap is only in `~/Library/Group Containers/group.com.docker/settings-store.json` (`DiskSizeMiB`).** The decisive number for hygiene is **host** free space (`statfs` on the Mac volume, `syscall.Statfs` in Go) plus Docker.raw allocated bytes — forge can read both directly.

### 4d. OrbStack / colima
- **OrbStack**: dynamic disk that grows and **shrinks automatically** (free space returned to macOS); no cap to configure. Percent thresholds are relative to a large virtual size → same problem. Source: https://docs.orbstack.dev/docker/
- **colima**: fixed-size disk set by `colima start --disk <GiB>` (default 100 GiB); recent versions (Lima, sparse disk) can reclaim via fstrim but not reliably automatic — older versions never shrink. A 100 GiB VM disk actually makes percent-based GC work-ish. Source: https://github.com/abiosoft/colima/blob/main/docs/FAQ.md
- **Implication**: forge's limits must be absolute/age and must not assume Docker Desktop paths; detect runtime via `docker info --format '{{.OperatingSystem}}'` ("Docker Desktop", "OrbStack", colima reports Ubuntu/"colima" context name).

---

## 5. Docker resource labels

**Verdict: yes — `docker ... --filter label=forge.project=<x>` works on containers, volumes, networks, images; forge can label most things it creates, with gaps on k3d.**

| Resource | Custom label support | Flag |
|---|---|---|
| k3d node containers | yes (Docker labels) | `k3d cluster create --runtime-label 'dev.forge.project=cp@server:*;agent:*;loadbalancer'` (verified in help); config `options.runtime.labels` |
| k3s node (k8s) labels | yes but these are **Kubernetes** labels, not Docker — not visible to `docker --filter` | `--k3s-node-label` |
| k3d cluster volumes/network | no custom labels; k3d stamps `app=k3d`, `k3d.cluster=<name>` | filter on those |
| k3d registry (`k3d registry create`) | **no `--label`** (verified); k3d stamps `k3d.role=registry` etc. (observed) | forge creating the registry itself with `docker run --label` closes this gap |
| Compose projects | yes, automatic `com.docker.compose.project=<name>`; plus `labels:` per service/volume/network | |
| Images | yes | `docker build --label` / `docker buildx build --label` (verified); also `LABEL` in Dockerfile. Survives push → pulled into k3d containerd (visible via `crictl inspecti`) |
| buildx builders | no Docker labels on builder metadata; the builder container is `buildx_buildkit_<name>0` | naming convention |
| Registry blobs/tags | n/a — use image config labels + tag naming | |

Caveat: label filter finds only resources created after forge starts labeling; existing ones need k3d/compose labels.
Source: https://docs.docker.com/engine/manage-resources/labels/ ; https://k3d.io/stable/usage/configfile/

---

## Recommended mechanism per layer

| Layer | Mechanism | Absolute/age | Needs restart/recreate |
|---|---|---|---|
| k3d node containerd images | `imageMaximumGCAge: 48h` via mounted KubeletConfiguration + `--kubelet-arg=config=` (all nodes); explicit `crictl rmi --prune` only from an on-demand `forge clean` | age | cluster recreate |
| BuildKit (docker driver) | after-build `docker buildx prune -f --filter until=168h --max-used-space <N>GB --builder <used>`; doctor reads daemon.json `builder.gc` and warns, never edits | bytes + age | none |
| BuildKit (docker-container, if forge ever owns a builder) | `docker buildx create --buildkitd-config` with `maxUsedSpace` + `keepDuration` policies | bytes + age | builder recreate |
| Local registry | **switch to zot** with declarative `retention.policies` + online GC, rendered from KCL; interim: distribution with `--delete-enabled`, forge tag-retention via go-containerregistry, then stop → offline `garbage-collect` (no `-m` on 2.8.3) → start, or move to `registry:3` | count + age | registry recreate |
| Docker Desktop VM | rely on TRIM (verify `DiskTRIM`), doctor measures host free (`statfs`) + Docker.raw allocated bytes; offer `fstrim` via privileged nsenter | n/a | none |
| Attribution | label everything forge creates: `--runtime-label` on k3d, `--label` on images, `docker run --label` on forge-owned registry, compose labels | n/a | new resources only |
