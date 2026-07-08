# Control-plane → agnostic `forge.Service` migration readiness

Read-only audit (2026-07-02). Scope: every workload and cluster-scoped resource in
`control-plane/deploy/kcl/**`, classified against the NOW-extended agnostic
`forge.Service` (`forge/kcl/core.k:350`) plus the landing operator support
(`k8s.operator{cluster_rbac, crds}` per `OPERATOR_AGNOSTIC.md`, proven by the
new fixture `forge/kcl/tests/positive_operator_service.k`). No code/KCL changed.

---

## 0. Decisive finding — control-plane's deploy is RED against live forge *today*

Control-plane consumes forge's KCL as a **live path dependency**, not a pinned
release:

```
# control-plane/deploy/kcl/kcl.mod:10
forge = { path = "../../../forge/kcl" }   # → the very forge checkout being reshaped
```

So the agnostic-core reshape has **already diverged control-plane's authoring
shape**. `kcl run staging/main.k -S manifests` currently **fails to compile**
(verified). Three break classes, all direct consequences of the reshape:

| # | Break | Evidence | Root cause |
|---|---|---|---|
| B1 | `Bundle.services` type mismatch | `stack.k:286` `expected [K8sWorkload], got [Service]` vs `forge/kcl/schema.k:2181` `services: [K8sWorkload]` | Builders (`lib/services.k:65,104,163,190,204`) return the **new agnostic** `forge.Service`, but `full_stack` places them in `Bundle.services` (now `[K8sWorkload]`). They belong in `Bundle.workloads: [Service]` (`schema.k:2175`). |
| B2 | `forge.Resources` field rename | `lib/resources.k:32-47` `Cannot add member 'cpu_request' … did you mean 'cpu_request_millicores'` | Agnostic `Resources` is now **neutral** (`cpu_request_millicores`/`memory_request_bytes`/`cpu_limit_millicores`/`memory_limit_bytes`); control-plane still writes k8s strings (`"100m"`, `"128Mi"`). |
| B3 | `deploy` / `env_vars` / `image_tag` gone from Service | `lib/builds.k:106` `Cannot add member 'deploy' to schema 'Service'`; every `full_stack` overlay sets `env_vars=`/`deploy=`/`image_tag=` (`stack.k:288-348`) | Agnostic `Service` has **no** `deploy` block, **no** `env_vars`, **no** `image_tag`. Env is `env: {str: EnvSource}`; scale/ports are flat `ports`/`replicas`/`resources`; build-only had a `deploy=BuildOnly` union. |

**Implication:** this is not optional cleanup. The migration is the **repair** that
restores a working render. Every row below is therefore "what the fix looks like,"
not "a nice-to-have."

---

## 1. Per-workload migration table

12 distinct workloads. "Target shape" assumes forge's landing operator support
(`k8s.operator`) is available.

| # | Workload | Authored today (file:line) | Target shape | Gaps / notes |
|---|---|---|---|---|
| 1 | **admin-server** | `lib/services.k:65` `forge.Service` + `stack.k:288` overlay (`env_vars`,`deploy`) | **Agnostic Service now.** `image`/`command`/`build`(GoBuild) already core; `deploy→ports=[8090]+replicas+resources`; `env_vars→env:{str:EnvSource}`; leases RBAC → `k8s.namespaced_rbac` | Clean. Needs the env-map rewrite (§3) + neutral Resources (B2). Its leases `Role` (`rbac.k:152`) folds to `k8s.namespaced_rbac`, **but** it also binds the `default` SA (`rbac.k:181`) — see §4 note. |
| 2 | **workspace-proxy** | `lib/services.k:104` `forge.Service` + `stack.k:300` overlay | **Agnostic Service now.** `ports=[8080]`, `volumes` (kubeconfig, `stack.k:308`), `env`; per-env `proxy_cluster` still handled by `deploy`-cluster grouping | Cleanest of all — matches `tests/positive_workspace_proxy_dogfood.k` exactly. **Recommended first proof (§7).** |
| 3 | **reliant-api-server** | `lib/services.k:163` + `stack.k:317` overlay; `image_tag` pin `stack.k:325` | **Agnostic Service** EXCEPT `image_tag`. `ShellBuild`, scratch `volumes` (`services.k:154`), `env` all map | **BLOCKER G1:** per-service `image_tag` pin has no agnostic-Service field (`core.k` has none; only `K8sWorkload.image_tag` `schema.k:1702`). No-op build + env-wide tag fan ⇒ `ImagePullBackOff` (`stack.k:86-96`). |
| 4 | **reliant-temporal-worker** | `lib/services.k:190` + `stack.k:328` | **Agnostic Service** EXCEPT `image_tag` | Same G1. |
| 5 | **daemon-gateway** | `lib/services.k:204` + `stack.k:340` | **Agnostic Service** EXCEPT `image_tag` + per-env `image`/`command`/`build` overlays | Same G1. Overlays (`stack.k:347`) are ordinary field merges — fine. |
| 6 | **workspace-controller** | `lib/services.k:132` `forge.Operator`; hand-written Service `infra.k:515`; LimitRange `limits.k:53`; PDB `prod/main.k:262` | **Agnostic Service + `k8s.operator`.** `ports=[9191]` ⇒ **deletes** `workspace_controller_svc` + collapses the 3 `:9191` literals; `resources` on the Service ⇒ **deletes** the LimitRange; `k8s.operator{cluster_rbac=_workspace_controller_rbac.rules, crds=["Workspace"]}`; `volumes` (daemon kubeconfig + ghcr-pull) | Needs `command=["./control-plane","workspace"]` explicit (Operator had none). **Fidelity check:** the `k8s.operator` ClusterRole is named `workspace-controller-clusterrole` (fixture asserts it, line 124) — matches the admin-server rebind's hard-coded name (`rbac.k:235`). CRD manifest stays raw (row 13). |
| 7 | **workspace-base** (build-only) | `lib/builds.k:95` `forge.Service{ deploy=None }` | **NOT expressible on agnostic Service.** | **BLOCKER G2:** agnostic `Service` has no `deploy` union, so no `BuildOnly` (`schema.k:1387,1670`). Must stay a `K8sWorkload` with `deploy=BuildOnly` in `Bundle.services`, or forge needs a build-only flag on `Service`. |
| 8 | **control-plane-migrate** (one-shot Job) | `stack.k:355` `forge.CronJob{schedule=""}` in `Bundle.cronjobs` | **Stays `forge.CronJob`.** No agnostic Job/CronJob workload type exists (`core.k` models Deployments only; `CronJob` is legacy k8s-shaped `schema.k:1888`). | Not a blocker — `Bundle.cronjobs` (`schema.k:2184`) remains. But it never becomes "agnostic." |
| 9 | **nats** | raw Deployment+Service `infra.k:68` (in `additional_manifests`) | **Foldable to agnostic Service now.** `security=SecurityOverrides{run_as_user=1000, run_as_group=1000, fs_group=1000}`, `health=HealthProbe{http_path="/healthz", port=8222}`, `ports=[4222,8222]`, `env`(from_secret), `resources` | Fully folds. Or leave raw (see §5). |
| 10 | **temporal** | raw `infra.k:178` | **Foldable with caveats.** `security{run_as_non_root=False, read_only_root_fs=False}`, `health{tcp=True, port=7233}`, `ports=[7233,9090]`, `env` | Caveat: distinct liveness/readiness **timings** (`infra.k:267-276`) collapse to one `HealthProbe` (80/20 loss). Caveat: forge's default `run_as_user=65532` can't be **unset** to "image default" — only overridden to a value. |
| 11 | **temporal-ui** | raw `infra.k:299` | **BLOCKED from folding.** | **G3:** `enableServiceLinks=False` (`infra.k:332`) is load-bearing (else crash) and unmodeled anywhere (`core.k`/`K8sOverrides`). Stays raw. |
| 12 | **litellm** | raw `infra.k:402` | **BLOCKED from folding.** | **G4:** `startupProbe` w/ `failureThreshold=36` (`infra.k:469`) is load-bearing (90-120s boot) and `HealthProbe` explicitly excludes startup (`core.k:346`). Stays raw. `security.read_only_root_fs=False` would work; the probe is the blocker. |

**Score:** all **5 app Services (1-5) + the operator (6)** are agnostic-Service-
expressible today — the entire application tier — with only two forge gaps
biting (G1 `image_tag`, and the shared config prerequisite §3). The 4 infra
pods (9-12) plus the Job (8) and build-only (7) do **not need** to become
Services: they keep their existing `additional_manifests` / `cronjobs` /
`services`(K8sWorkload) homes, all of which the model retains. Two infra pods
(nats, temporal) *can* fold as a bonus; two (temporal-ui, litellm) cannot.

---

## 2. Cluster-scoped & policy resources (the raw tail)

None of these are workloads; they ride `Bundle.additional_manifests` /
`extra_manifests` today and **stay there** — the agnostic model has no first-class
type for any of them. This is the RedTeam headline and it holds: the *majority*
of control-plane's rendered objects are Bundle-level raw.

| Resource | Owner / builder | Target home | Migratable? |
|---|---|---|---|
| Workspace **CRD** | `crd.workspace_crd()` `crd.k:11` (via `stack.k:370`) | `Bundle.additional_manifests` (or `k8s.raw_manifests` on the controller Service) | Stays raw. `k8s.operator.crds` is only the codegen/name bridge, not the manifest. |
| **NetworkPolicy** ×13 | `netpol.control_plane_policies` `netpol.k:28` | `additional_manifests` | Stays raw — no NetworkPolicy in the core. Single largest category. |
| Per-workspace **NetworkPolicy** ×4 | `netpol.workspace_namespace_policies` `netpol.k:285` | Applied by the **Go controller** at runtime, not the bundle | Out of scope (runtime). |
| **PodDisruptionBudget** ×5 (prod) | `pdb.pod_disruption_budget` `pdb.k:48` | `extra_manifests` (`prod/main.k:257`) | Stays raw — no PDB in the core. |
| **LimitRange** ×1 (prod) | `limits.control_plane_limitrange` `limits.k:53` | `extra_manifests` | **DELETED** once row-6 controller sets `resources` directly — its sole reason to exist is the Operator's missing `resources` (`limits.k:12`). |
| GKE **HealthCheckPolicy** ×3 (preprod/prod) | `infra.healthcheck_policy` `infra.k:570` | `extra_manifests` (`prod/main.k:248`) | Stays raw (GKE-only, CRD). |
| Namespace **PSS baseline patch** | `infra.namespace_baseline_patch` `infra.k:43` | `additional_manifests` | Stays raw. |
| **workspace-proxy ClusterRBAC** (SA+ClusterRole+CRB) | `rbac.workspace_proxy_rbac` `rbac.k:247` | Stays raw, OR fold into `k8s.operator.cluster_rbac` on the proxy Service (it only *reads* cluster-wide — a "read-only operator") | Foldable but low-value; raw is fine. |
| **admin-server operator ClusterRoleBinding** (rebind of `workspace-controller-clusterrole` → admin SA) | `rbac.admin_server_operator_rolebinding` `rbac.k:221` | Stays raw — a **cross-workload** rebind no single Service owns | Stays raw. Depends on row-6 preserving the ClusterRole name (fidelity check). |
| **admin-server leases** Role+RoleBinding | `rbac.admin_server_leases_rbac` `rbac.k:152` | Role → `k8s.namespaced_rbac` on admin Service; binding to `default` SA → raw | Partial (see §4). |
| **StorageClass** / kata **RuntimeClass** | `deploy/daemon-cluster/*.yaml` | Out-of-band YAML, **not in the KCL bundle** | Out of scope. (forge *does* model `Bundle.runtime_classes` `render.k:668` if ever wanted.) |
| **Gateways / HTTPRoutes / GRPCRoutes** | `ingress.k` / `prod/ingress.k` (`Bundle.gateways/http_routes/grpc_routes`) | Unchanged — separate top-level Bundle schemas | Not part of Service; no migration. |
| **Frontends** (reliant-web, admin-web) | `Bundle.frontends` `prod/main.k:293` | Unchanged | No migration. |
| **helm_charts** (cert-manager, envoy) | `platform.helm_charts` | Unchanged | No migration. |

---

## 3. Config-wiring prerequisite (must precede any Service env rewrite)

The agnostic `Service.env` is a **map** `{str: EnvSource}`; config lowers through
the generated `appConfigEnvMap` as `from_config` entries. Control-plane's entire
env pipeline is the **opposite** shape:

- `deploy/kcl/*/config_gen.k` is `forge generate` output but still the **old
  `[forge.EnvVar]` shape** — `APP_ENV: [forge.EnvVar]` of `config_map_ref`
  entries (`prod/config_gen.k:10`), not a map/`appConfigEnvMap`.
- `lib/env.k` is **~430 lines of `[forge.EnvVar]` + `forge.env_merge`** — every
  role bundle (`shared_env`, `litellm_env`, `reliant_base_env`, the prod tails,
  …) returns `[EnvVar]` and composes with `env_merge` (`env.k:75-331`).
- `full_stack` threads those lists as `env_vars=` overlays (`stack.k:288-348`).

**Prerequisite:** run `forge generate` so config projects as the map-shaped
`appConfigEnvMap` the agnostic path consumes (the guard was unblocked — "Finding
A" — but generate has **not** been run, so `config_gen.k` is still the legacy
list). Then `lib/env.k`'s helpers must be rewritten to build `{str: EnvSource}`
and compose with native `|` (last-wins), dropping `env_merge`/`env_project`
round-trips. This is the **single largest mechanical lift** in the migration and
gates rows 1-6 — no Service's `env` can be authored until the config map exists.

`forge.env_project` (`core.k:124`) converts map→`[EnvVar]` but there is **no**
reverse; a stopgap that keeps `env.k` as `[EnvVar]` and projects backward does
not exist, so the pipeline genuinely has to move to maps.

---

## 4. Bundle-level questions

- **Jobs / CronJobs:** no agnostic workload type. The migrate Job stays
  `Bundle.cronjobs` (legacy `CronJob`, `schema.k:1888`). Not a blocker, but a
  "full agnostic" bundle is impossible until forge adds a Job/`workload_type`
  discriminator. **Open forge question.**
- **Gateways / ingress:** already separate top-level Bundle schemas
  (`gateways`/`http_routes`/`grpc_routes`). They are **not** part of `Service`
  and need no migration — but note the RedTeam critique (exposure is authored in
  a second model, wired by string name) is unresolved. Not a blocker.
- **Bundle-wide raw manifests:** `Bundle.additional_manifests` (`schema.k:2201`)
  **survives** and is where all of §2's policy/infra tail lives — so "there is no
  Bundle-level raw slot" is **false**; the slot control-plane already uses
  remains. `k8s.raw_manifests` (per-service, `core.k:241`) is the *additional*
  service-owned floor. Standalone infra (nats/temporal, owned by no app service)
  correctly stays in `additional_manifests`, not `k8s.raw_manifests`.
- **build-only:** G2 — a build-only artifact (workspace-base) has no agnostic
  representation. Either keep one `K8sWorkload{deploy=BuildOnly}` in
  `Bundle.services` (the model retains `services: [K8sWorkload]` for exactly this
  interop, `schema.k:2179-2181`) or add a build-only flag to `Service`.
- **admin-server `default`-SA binding:** `rbac.admin_server_leases_rbac` and the
  operator rebind bind BOTH `admin-server` **and** `default` SAs (`rbac.k:181`,
  `rbac.k:239`) because forge's `render_k8s_service` historically didn't set
  `serviceAccountName`. The operator fixture shows `k8s.operator` **does** bind
  the pod SA (`positive_operator_service.k:110`) — confirm the plain-Service path
  now sets `serviceAccountName` too; if it does, the `default`-SA half of these
  bindings becomes dead weight (a cleanup, not a blocker).

---

## 5. Per-env migration order

`full_stack` is **shared** by every cluster env (`stack.k:246`), so rewriting it +
`lib/services.k` + `lib/env.k` + `lib/resources.k` migrates the app-service
*shape* for **all envs at once**. "First env" therefore means "first env to
validate render + deploy," and the throwaway envs are the validation grounds
before any live cluster.

| Order | Env | Why | Risk |
|---|---|---|---|
| 1 | **dev-k8s** | Local k3d, single-cluster, `render_infra=True`, replicas=1, no PDB/HealthCheckPolicy/daemon-split (`dev-k8s/main.k:161-215`). It is the **explicit fidelity anchor** of `full_stack` ("renders BYTE-IDENTICAL to the hand-written dev-k8s bundle", `stack.k:28`). Exercises Service+operator+infra+Job+CRD+netpol end-to-end. | Lowest — not serving users. |
| 2 | **e2e** | Throwaway 2-cluster k3d (`proxy_cluster`, daemon split, real builds). Proves the cross-cluster grouping + operator on the split topology. | Low — CI/throwaway. |
| 3 | **staging** | Simplest **cloud** env: Vultr, `gke=False` netpol, no daemon-split, replicas=1, no PDB/HealthCheckPolicy (`staging/main.k`). First live-cluster proof. | Medium. |
| 4 | **preprod** | GKE, in-cluster daemons, HealthCheckPolicies, replicas=1 (`preprod/main.k`). Adds the GKE GFE netpol + HealthCheckPolicy raw tail. | Medium. |
| 5 | **prod** | Most complex: daemon-cluster split, PDBs, LimitRange, replicas=2, dial-out (`prod/main.k`). Last. The LimitRange deletion (§2) lands here. | Highest. |

---

## 6. Prioritized forge gaps blocking a FULL migration

Ranked by how much they bite the **application tier** (rows 1-6) vs. the raw tail.

1. **G1 — per-service `image_tag` on `Service`.** Blocks reliant-api/worker/
   daemon-gateway (rows 3-5): out-of-band images on a pinned tag, env-wide tag
   fan ⇒ `ImagePullBackOff`. Cheap additive field; highest bite (3 of 5 app
   Services). *Alternatively* a per-service registry/tag override.
2. **G2 — build-only on `Service`.** Blocks workspace-base (row 7). Add a
   build-only marker (mirror the `K8sWorkload{deploy=BuildOnly}` semantics) or
   accept the one-`K8sWorkload` interop escape.
3. **Config map-shape prerequisite (§3).** Not strictly a "forge gap" (it's
   `forge generate` + a control-plane env.k rewrite), but it **gates rows 1-6**
   and is the biggest single lift. Run generate first.

Secondary (block only the *optional* infra fold, not the app tier — raw homes
remain, so these are ceilings, not blockers):

4. **G3 — `enableServiceLinks`** (temporal-ui) — unmodeled; RedTeam S4-13.
5. **G4 — startupProbe / distinct liveness-readiness timings** (litellm,
   temporal) — `HealthProbe` is single-probe 80/20 by design (`core.k:346`).
6. **No agnostic Job/CronJob/StatefulSet workload type** — the migrate Job and any
   future stateful infra can never be "agnostic"; they stay legacy schemas.
7. **`SecurityOverrides` can override but not *unset*** forge's `run_as_user=65532`
   default to "image default" (bites temporal/temporal-ui/litellm folds).
8. **NetworkPolicy / PDB / LimitRange / CRD / HealthCheckPolicy** remain raw —
   the model owns none. This keeps control-plane's deploy *majority-raw* forever
   unless forge adopts first-class policy types. Not a migration blocker (the
   raw slot works), but it is the ceiling on "fully agnostic."

---

## 7. Recommendation

- **First env:** **dev-k8s** — local, lowest blast radius, and the declared
  `full_stack` fidelity anchor. Validate the full bundle render (Service ×5 +
  operator + infra + Job + CRD + netpol) there, then e2e, then staging→preprod→prod.
- **First service (the proof):** **workspace-proxy** — standalone binary, smallest
  env surface, one volume, and forge **already ships a worked example** of it on
  the agnostic Service (`kcl/tests/positive_workspace_proxy_dogfood.k`). Author it
  as `forge.Service` in `Bundle.workloads`, leave the other four in
  `Bundle.services` transiently (both lists render — `render.k:837` + `render.k`
  services path), and diff the manifest output.
- **Do first, unblocks everything:** run `forge generate` for the map-shaped
  config (§3), and land forge **G1 (`image_tag`)** so the reliant siblings can
  move in the same pass. G2 (build-only) can lag behind a one-`K8sWorkload`
  escape.
- **Fidelity gate that bites:** the operator ClusterRole must stay named
  `workspace-controller-clusterrole` (fixture line 124 ⇄ rebind `rbac.k:235`).
  Verify before deleting `infra.workspace_controller_svc` and the LimitRange.
