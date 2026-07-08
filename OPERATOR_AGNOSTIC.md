# Operators in the agnostic-core model

How does a controller-runtime **Operator** fit forge's target-agnostic
`forge.Service` core (`kcl/core.k`), given that the core's whole premise is a
workload description with *zero* Kubernetes vocabulary that projects onto k8s
today and compose/Nomad/ECS tomorrow? This is the last blocker to migrating
control-plane fully onto the agnostic core: its `workspace-controller` is a
`forge.Operator` (`kcl/schema.k:1785`), the one component still authored in the
legacy k8s-shaped vocabulary.

**Bottom line:** model an operator as an **agnostic `forge.Service` + a
cluster-scoped extension of the existing `k8s` escape hatch** (`K8sOverrides`
gains `cluster_rbac` and `crds`). Do **not** introduce a parallel top-level
`forge.Operator` core type. The legacy `Operator` schema is strictly *weaker*
than `Service`+hatch on a biting, concrete axis (it has no `ports`, which is
exactly why control-plane hand-writes a second `Service` manifest and hardcodes
`:9191` in three places), and a distinct core type would fork the one authoring
surface and re-duplicate every agnostic field. The escape hatch is precisely
the sanctioned vehicle for "the narrow, non-portable, k8s-only tail, made
explicit and localized" — and an operator's tail (cluster RBAC, CRDs, a Lease)
is exactly that.

---

## 1. What the code actually shows today

### 1.1 Two schema layers, two Bundle slots

Forge has **two** workload vocabularies living side by side:

- **Agnostic core** — `kcl/core.k`: `Service` (`core.k:222`), `EnvSource`
  (`core.k:38`), `EnvFrom` (`core.k:138`), `K8sOverrides` (`core.k:171`). Zero
  k8s nouns. Projected onto the demoted k8s shape by the adapter
  `_service_to_k8s` (`core.k:332`), *inside* the render pipeline.
- **Legacy k8s-shaped** — `kcl/schema.k`: `K8sWorkload` (the old `Service`,
  now adapter-internal plumbing, `schema.k` ~1640), `Operator`
  (`schema.k:1785`), `ClusterRBAC` (`schema.k:294`).

`Bundle` carries **both**: `workloads: [Service]` (agnostic, projected via
`_service_to_k8s`, `render.k:654` / `render.k:819`) *and* `operators:
[Operator]` (legacy typed, rendered via `_render_operator_manifests`,
`render.k:728`). So operators today never touch the agnostic path at all — they
are the one workload class that skipped the cutover.

### 1.2 The typed `Operator` — what it is and what it emits

```
schema Operator:                       # kcl/schema.k:1785
    name: str
    image: str
    image_tag?: str
    replicas: int = 1
    platform?: str
    crds: [str] = []                   # CRD *kind names* (not manifests)
    cluster_rbac: ClusterRBAC = ClusterRBAC {}
    leader_election: bool = True
    env_vars: [EnvVar] = []
    volumes?: [Volume]
    check:
        len(crds) > 0, "Operator.crds must list at least one CRD kind"
```

`_render_operator_manifests` (`render.k:728`) emits exactly two things:

1. `svc_lib.render_operator(...)` (`lib/services.k:511`) — a **Deployment**
   whose pod binds a ServiceAccount named after the operator, plus (when
   `leader_election`) a single `LEADER_ELECTION=true` env var
   (`lib/services.k:515`). **Note there is no `ports` field and no `Service`
   object** — an operator that watches the API server needs no inbound port, so
   the schema omits it.
2. `rbac_lib.render_cluster_rbac(...)` (`lib/rbac.k:88`) — a ServiceAccount +
   **ClusterRole** (default config-read rules + `cluster_rbac.rules`) +
   **ClusterRoleBinding**.

Two things the `Operator` schema does **not** do, that matter later:

- **It does not render the CRD manifest.** `crds` is only a *name list*. It
  drives `forge add crd` codegen and the Go `AddToScheme`/`SetupWithManager`
  wiring; the actual `CustomResourceDefinition` object is either the permissive
  stub from `lib/crd.k crd(...)` (`lib/crd.k:22`) or hand-supplied. Control-plane
  hand-supplies the full kubebuilder-generated CRD via
  `crd.workspace_crd()` in `additional_manifests` (`deploy/kcl/lib/stack.k:370`),
  *not* through `Operator.crds`.
- **`leader_election` is nominal in KCL.** It only emits an env var. The real
  leader election is unconditional in Go: `operatorkit.Run`
  (`pkg/appkit/operatorkit/operatorkit.go:133`) always runs the manager with a
  Lease, toggled by `LEADER_ELECTION_ID` / `LEADER_ELECTION_NAMESPACE` and
  in-cluster detection — it never reads a `LEADER_ELECTION` bool. This confirms
  the design call already made: **leader election is not a first-class field,
  it is an env default.**

### 1.3 `K8sOverrides` — the escape hatch as it stands

```
schema K8sOverrides:                   # kcl/core.k:171
    service_account?: str
    namespaced_rbac?: [{str: any}]     # NAMESPACE-scoped Role rules
    annotations?: {str: str}
    node_selector?: {str: str}
    tolerations?: [{str: any}]
    raw_manifests?: [any]              # Tier-2 floor: verbatim manifests
```

The adapter maps each field onto the demoted `K8sWorkload`
(`core.k:364-377`); `render_k8s_service` threads them into the Deployment. The
docstring is explicit that this is where "leader election, cluster RBAC, CRDs
stay on the k8s-native Operator/K8sWorkload; those don't port to compose"
(`core.k:174-177`) — i.e. the hatch was *designed anticipating* operators but
does not yet carry their fields. Crucially:

- `namespaced_rbac` is **NAMESPACE-scoped** — it appends rules to the service's
  generated `Role` (`lib/rbac.k:44`, `schema.k:1718-1729`). An operator needs
  **CLUSTER-scoped** `ClusterRole` rules. This is the gap the task flags.
- There is **no `cluster_rbac` and no `crds`** on `K8sOverrides`.

One subtlety that shapes the fix: `K8sCluster` (the deploy block) *already* has
an optional `cluster_rbac?: ClusterRBAC` field (`schema.k:806`), and it is
projected into the deploy-as-data JSON (`render.k:125`). **But the manifest
renderer never consumes it for a Service:** `_render_cluster_service`
(`render.k:718`) unconditionally calls `render_namespaced_rbac` — only
`_render_operator_manifests` calls `render_cluster_rbac`. So today a plain
Service *cannot* emit a ClusterRole even though a field exists to describe one.
The renderer, not just the schema, has to change.

### 1.4 The `workspace-controller` reality — messier than "an Operator"

Control-plane's operator is not a clean controller. It is a controller-runtime
manager **and** a Connect RPC server **and** a set of background goroutines, all
on the shared `control-plane` binary. The evidence:

- **It is a `forge.Operator`** (`deploy/kcl/lib/services.k:132`):
  `name="workspace-controller"`, `crds=["Workspace"]`,
  `cluster_rbac=_workspace_controller_rbac` (12 rules incl.
  `coordination.k8s.io/leases`, `deploy/kcl/lib/services.k:44-59`),
  `leader_election=True`.
- **It also serves an RPC on `:9191`.** The `WorkspaceControllerService`
  handler binds `PORT=9191` (`deploy/kcl/staging/main.k:145`), and admin-server
  dials `WORKSPACE_CONTROLLER_URL=...:9191` (`staging/main.k:109`). Because
  `Operator` has **no `ports`**, a *separate hand-written* `Service` manifest is
  bolted on: `infra.workspace_controller_svc(namespace)` in
  `additional_manifests` (`deploy/kcl/lib/stack.k:370`,
  `deploy/kcl/lib/infra.k:515`) — and `:9191` is hand-typed in *three* places
  (the port env, every `WORKSPACE_CONTROLLER_URL`, and the hand-written
  Service). `FORGE_KCL_IDIOMS.md:232` calls this out by name as a real drift the
  agnostic core would delete.
- **The CRD is hand-supplied**, not from `Operator.crds`: the full
  kubebuilder `workspaces.reliant.dev` CRD rides `additional_manifests` via
  `crd.workspace_crd()` (`deploy/kcl/lib/stack.k:370`).
- **RBAC is cross-wired.** `admin-server` runs the *same* control-plane binary
  with `RUN_OPERATORS=false` (`staging/main.k:107`) but still needs the
  in-process manager's cluster read perms for cache-sync; control-plane
  *rebinds* forge's rendered `workspace-controller-clusterrole` to the
  admin-server SA (`deploy/kcl/lib/rbac.k:188-235`) rather than duplicate the
  rule set.
- **Runtime is controller-runtime.** `RunOperators`
  (`internal/app/lifecycle.go:64`) → `lifecyclekit.Run` →
  `operatorkit.Run` builds a manager (kubeconfig resolution with graceful
  no-cluster degrade, Lease leader election, scheme registration, health
  probes; `operatorkit.go:133-172`). The reconciler embeds
  `forge/pkg/controller.Reconciler[T]` and owns the pod/PVC/namespace/netpol
  lifecycle (`internal/operators/workspace/doc.go`).

The takeaway from 1.4: the "operator" is **90% an ordinary workload**
(image, env, a listening port, resources, replicas, volumes) **+ a thin
k8s-only tail** (cluster RBAC, a CRD, a Lease). The current typed `Operator`
models the tail well but the *workload* poorly — most conspicuously it can't
express the port the thing actually listens on.

---

## 2. Q1 — Service + escape hatch, not a distinct workload type

**Recommendation: model an operator as `forge.Service` whose `k8s` block
carries the cluster-scoped tail (`cluster_rbac`, `crds`), with leader election
as an env default.** Retire `bundle.operators` / the typed `Operator` once
migrated.

Argument:

1. **An operator is overwhelmingly a normal Service.** Its portable fields —
   `image`, `command`, `env`, `ports`, `replicas`, `resources`, `build`,
   `volumes` — are *identical* facts to any Service and are already fully
   modeled by `forge.Service` (`core.k:274-285`). Nothing about "watches the API
   server" changes what those fields mean.

2. **`Service`+hatch is strictly *more* expressive than `Operator` on a field
   that bites today.** `Operator` has no `ports`; the workspace-controller
   *listens on 9191*, so control-plane hand-writes a `Service` object and
   triplicates the port literal (§1.4). A `forge.Service` has `ports` natively
   — the container port, the `Service` object, and every dependent's resolved
   URL all derive from one core fact. Migrating onto Service **deletes**
   `infra.workspace_controller_svc` and two hand-typed `:9191`s. The distinct
   type is not just redundant, it is a regression we already paid for.

3. **A distinct `forge.Operator` core type re-forks the core.** It would have
   to re-declare `image`/`command`/`env`/`ports`/`resources`/`build`/`volumes`/
   `replicas` — every agnostic field — or inherit them, re-introducing the exact
   k8s-vs-agnostic split at the *type* level that the whole cutover collapsed
   into one authoring surface (`FORGE_KCL_IDIOMS.md:123-137`). The legacy
   `Operator` *is* that duplication, and it already drifted (missing `ports`).
   Two workload types is two things to keep in sync forever.

4. **The escape hatch is the *designed* home for this.** `K8sOverrides`
   literally names "leader election, cluster RBAC, CRDs ... don't port to
   compose" as its rationale (`core.k:174-177`), and `FORGE_KCL_IDIOMS.md:456-489`
   (§9.3) already sketches `workspace_controller = forge.Service { ... k8s =
   K8sOverrides {...} }`. Non-portability made **explicit, localized, and
   lintable** is the model working as intended — an operator *is* non-portable,
   so it *should* carry a `k8s` block, and compose *should* skip it (§5).

The only honest tension — "an operator is *so* k8s-bound that the k8s block is
load-bearing to its identity, not a 20% tail" — is real and addressed in the
devil's-advocate section (§7). It argues for making the tail **typed and
first-class within the hatch**, not for a parallel top-level type.

---

## 3. Q2 — What is escape-hatch tail vs. what is normal Service

| Operator concern | Agnostic analog? | Home |
|---|---|---|
| `image`, `command`, `args` | Yes — same as any workload | `Service` core (`core.k:274-277`) |
| `env` / `env_from` | Yes — `{str: EnvSource}` map + bulk import | `Service` core (`core.k:278-279`) |
| **`ports` (the `:9191` RPC listener)** | **Yes** — plain ints; container port + Service + dependents' URLs derive from it | `Service` core (`core.k:280`). Fixes the §1.4 drift. |
| `replicas`, `resources` | Yes | `Service` core (`core.k:281-282`) |
| `build` (shared cobra binary) | Yes — build union, target-agnostic | `Service` core (`core.k:283`) |
| `volumes` (daemon kubeconfig, scratch dirs) | Yes — agnostic `Volume` | `Service` core (`core.k:284`) |
| `node_selector` / `tolerations` (kata pool) | No | `k8s` hatch — already there (`core.k:218-219`) |
| `service_account` (out-of-band SA) | No | `k8s` hatch — already there (`core.k:215`) |
| **`cluster_rbac` (ClusterRole across namespaces)** | **No** — k8s cluster-scoped authz; `namespaced_rbac` is Role-only | `k8s` hatch — **needs adding** |
| **`crds` (owned CRD kinds)** | **No** — CRDs are a k8s API-extension concept; also the codegen/`AddToScheme` bridge | `k8s` hatch — **needs adding** |
| The CRD *manifest* itself | No | `k8s.raw_manifests` (as control-plane already does via `additional_manifests`), or a forge-rendered stub |
| Leader-election Lease | No (behavior + one RBAC rule) | **env default** (`LEADER_ELECTION_ID`, already how Go works) + a `leases` rule inside `cluster_rbac` |
| The controller-runtime manager (watch/cache/informers) | No — pure runtime | **Go**, gated by `RUN_OPERATORS`; not a KCL concern beyond "this workload has cluster_rbac+crds" |

The dividing line is clean: **everything a compose adapter could plausibly
render is core; everything that presupposes a Kubernetes API server (cluster
authz, CRDs, a Lease, informers) is the k8s-only tail.**

---

## 4. Q3 — The `K8sOverrides` additions (cluster_rbac + crds)

Two new optional fields, plus the *renderer* change without which the schema is
inert (§1.3).

### 4.1 Schema shape

```kcl
schema K8sOverrides:                   # kcl/core.k:171 — additions marked NEW
    service_account?: str
    namespaced_rbac?: [{str: any}]     # NAMESPACE-scoped Role rules (Service-tier)
    cluster_rbac?:    ClusterRBAC      # NEW — CLUSTER-scoped ClusterRole rules (operator-tier)
    crds?:            [str]            # NEW — CRD *kind names* this workload owns
    annotations?: {str: str}
    node_selector?: {str: str}
    tolerations?: [{str: any}]
    raw_manifests?: [any]
```

Notes on the shape:

- **`cluster_rbac` reuses the existing `ClusterRBAC` schema** (`schema.k:294`) —
  same `rules: [{apiGroups,resources,verbs}]` dicts control-plane already writes
  (`deploy/kcl/lib/services.k:44`). No new RBAC vocabulary. It sits *beside*
  `namespaced_rbac` so the two tiers stay legible: namespaced = Role, cluster =
  ClusterRole. Forge keeps auto-adding the default config-read rule
  (`lib/rbac.k:29`), so authors supply only CRD/lease/coordination rules.
- **`crds` is a name list**, mirroring `Operator.crds` (`schema.k:1801`) — it is
  the bridge to `forge add crd` / Go `AddToScheme`, **not** the CRD manifest.
  The manifest keeps riding `raw_manifests` (control-plane's full kubebuilder
  CRD) or, for greenfield, a forge-rendered `lib/crd.k crd(...)` stub. Keep the
  `len(crds) > 0` invariant *only* if a workload declares `cluster_rbac`
  intending to be an operator — better as a lint nudge than a hard `check`, so
  an ordinary Service that just needs cluster read (rare) isn't forced to invent
  a CRD.

### 4.2 Adapter + renderer wiring (the load-bearing half)

1. **Adapter** (`_service_to_k8s`, `core.k:364`): project the two new fields
   onto the demoted `K8sWorkload`. `cluster_rbac` → a new
   `K8sWorkload.cluster_rbac?: ClusterRBAC` field (or overlay onto the
   already-existing `_deploy.cluster_rbac`, `schema.k:806`); `crds` →
   `K8sWorkload.crds` (drives deploy-as-data → Go codegen).

2. **Renderer** (`_render_cluster_service`, `render.k:718`): branch on
   presence. When the workload carries `cluster_rbac`, emit
   `rbac_lib.render_cluster_rbac(...)` (ServiceAccount + ClusterRole +
   ClusterRoleBinding, `lib/rbac.k:88`) **instead of / in addition to**
   `render_namespaced_rbac`. This is the single behavioral change that makes an
   agnostic Service able to produce a ClusterRole — today only
   `_render_operator_manifests` can (`render.k:730`).

3. **Leader election**: no field. The pod already gets its Lease from
   `operatorkit.Run` at runtime; the KCL side supplies `LEADER_ELECTION_ID` as a
   normal `env` entry (control-plane already does, `staging/main.k:138`) and a
   `coordination.k8s.io/leases` rule inside `cluster_rbac` (already rule #12,
   `services.k:57`).

Net: `render_operator` + `_render_operator_manifests` + `bundle.operators` +
the `Operator` schema all become removable once callers move to
`workloads`. The functionality folds into the one Service path.

---

## 5. Q4 — The compose-rejection story

An operator has no compose analog: there is no API server to watch, no Lease, no
ClusterRole, no CRD. The agnostic-core design already gives the right answer
**for free** — a compose adapter simply **never reads `svc.k8s`**
(`core.k:411-417`, `_service_to_compose`). Because the escape hatch is
*target-namespaced*, the k8s-only tail structurally cannot bleed into a non-k8s
target. So the portable half of an operator's Service (image/env/ports/…)
*would* render to compose; the k8s tail is silently dropped, which is correct
for a plain Service but **dangerously silent for an operator** — a compose
"operator" that starts but watches nothing and reconciles nothing is worse than
a clear failure.

Recommendation — **make the rejection loud, at two levels:**

- **Lint/`forge audit` (primary):** a Service whose `k8s` block sets `crds` (or
  `cluster_rbac` with a `leases`/CRD rule) is, by definition, a controller. When
  a compose target is requested, `forge lint`/`forge audit` should **surface a
  finding**: "workload `X` is a Kubernetes operator (declares `k8s.crds`); it
  has no docker-compose representation and will be **skipped**." This matches the
  §4-style "lint can flag it" nudge (`FORGE_KCL_IDIOMS.md:452`) and the
  additive-finding contract the audit-json consumers already expect.
- **Adapter (belt-and-suspenders):** `_service_to_compose` should **skip**
  (not error on) such a workload — omit it from the compose stack entirely
  rather than emit a half-workload that looks alive. Skip + a visible warning,
  not a hard build failure, because a mixed stack (some services compose-able,
  one operator not) should still bring up its compose-able half for local dev.

The distinction `k8s.crds`-is-set gives forge a **precise, typed predicate** for
"this is an operator" — which is another argument for putting `crds` in the
schema rather than leaving operator-ness implicit. (A distinct `forge.Operator`
type would give the same predicate, but at the cost analyzed in §2/§7.)

---

## 6. Q5 — Migrating control-plane's `workspace-controller`

Concrete path, smallest-diff-first. Nothing here requires touching the Go
reconciler — it's all KCL + a modest forge schema/renderer change.

**Forge work required (prerequisite):**

1. Add `cluster_rbac?: ClusterRBAC` and `crds?: [str]` to `K8sOverrides`
   (`core.k:171`).
2. Project both in `_service_to_k8s` (`core.k:364`) onto `K8sWorkload`.
3. Teach `_render_cluster_service` (`render.k:718`) to emit `render_cluster_rbac`
   when the workload carries `cluster_rbac` (§4.2).
4. (Optional, later) retire `Operator` / `render_operator` /
   `_render_operator_manifests` / `bundle.operators` once no caller uses them.
5. (Optional) `forge audit` finding for "operator on a compose target" (§5).

**Control-plane migration (once forge ships 1-3):**

1. Replace `workspace_controller_base` (`deploy/kcl/lib/services.k:132`,
   returning `forge.Operator`) with a `forge.Service` builder:

   ```kcl
   workspace_controller_base = lambda image: str -> forge.Service {
       forge.Service {
           name  = "workspace-controller"
           image = image
           command = ["./control-plane", "workspace"]   # the operator subcommand
           build = forge.GoBuild { cmd = "./cmd/control-plane", output_name = "control-plane" }
           ports = [9191]                                # ← was impossible on Operator
           k8s = forge.K8sOverrides {
               cluster_rbac = _workspace_controller_rbac  # unchanged 12-rule set
               crds = ["Workspace"]
               # leader election: LEADER_ELECTION_ID rides env_vars, as today
           }
       }
   }
   ```

2. In `full_stack` (`deploy/kcl/lib/stack.k:246`): move `_controller` from
   `operators = [_controller]` (`stack.k:350`) into the `services = [...]` list
   (`stack.k:286`), overlaying `env_vars`, `volumes`, and per-env
   `deploy`/`replicas` exactly as the other services do.

3. **Delete `infra.workspace_controller_svc`** from `additional_manifests`
   (`stack.k:370`, `infra.k:515`) — the `ports = [9191]` now generates the
   `Service` object. The two remaining `:9191` literals collapse to derivations
   of the single core `ports` fact.

4. **Keep** `crd.workspace_crd()` in `additional_manifests` (`stack.k:370`) —
   `k8s.crds` is only the name/codegen bridge; the full kubebuilder CRD stays a
   raw manifest (equivalently movable to `k8s.raw_manifests` on the controller
   Service so it routes to the controller's cluster by ownership).

5. **RBAC rebind unchanged** — `admin_server_operator_rolebinding`
   (`deploy/kcl/lib/rbac.k:221`) rebinds `workspace-controller-clusterrole` to
   the admin-server SA. `render_cluster_rbac` names the ClusterRole
   `${name}-clusterrole` (`lib/rbac.k:101`) identically whether the source is an
   `Operator` or a `Service` with `k8s.cluster_rbac`, so the rebind's assumed
   name (`rbac.k:235`) stays valid. **This is the single most important
   fidelity check for the migration.**

6. Verify `render_manifests` output is semantically equivalent: same Deployment,
   same ClusterRole/Binding, same SA, plus a *new* generated `Service` object
   replacing the hand-written one. Gate on behavior (controller reconciles,
   admin-server reaches `:9191`), not byte-identity — consistent with
   `FORGE_KCL_IDIOMS.md:513` phase-3.

Order note (per user memory on config-projection ownership): the `crds`/
`cluster_rbac` fields are proto-free pure-KCL schema additions, so this does
**not** touch `config_projection_gen.go` or the config tests — orthogonal to the
concurrent config work.

---

## 7. Devil's advocate

**7.1 "Operator = Service + k8s block" breaks down where the tail is not a
tail.** The escape hatch's whole justification is that it holds a *narrow,
non-portable 20%* while the core holds the portable 80%
(`FORGE_KCL_IDIOMS.md:399-422`). For an operator that's arguably inverted: the
cluster RBAC + CRD + manager *is the point of the workload*; strip the `k8s`
block and you don't have a degraded-but-running service, you have a binary that
does nothing. Calling that load-bearing 50% a "20% tail" is a category
smell — the hatch was framed for PDBs and HealthCheckPolicies (cosmetic k8s
niceties), not for the workload's reason to exist.

**7.2 A distinct `forge.Operator` reads better and can enforce invariants.**
`forge.Operator` announces intent ("this is a controller") at the type level; it
can `check` that `crds` is non-empty and `cluster_rbac` is present; it gives
forge one clean, unambiguous hook for `forge add crd`, `AddToScheme`/
`SetupWithManager` scaffolding, and the compose-skip predicate — no "sniff
whether `k8s.crds` is set" heuristic. The typed `Operator` we have *does* model
the controller's essence cleanly (its only real defect is the missing `ports`).

**7.3 Rebuttal — why Service+typed-hatch still wins.** The dispositive fact is
**the shared workload surface**. The workspace-controller is not a pure
controller: it is *also* an RPC server on `:9191`, runs the same cobra binary as
admin-server, and admin-server runs the *same* manager in-process with
`RUN_OPERATORS=false`. "Operator" and "Service" are not disjoint categories
here — they are two facets of one binary. A parallel top-level type forces a
false either/or (which is precisely why control-plane had to hand-bolt a
`Service` onto its `Operator`). Every argument in 7.2 is satisfiable **inside
the Service model** without forking the core:

- *Legibility/intent*: put the operator bits in a **typed** hatch sub-schema.
  Instead of loose `cluster_rbac`/`crds` on `K8sOverrides`, group them:
  `k8s.operator?: OperatorSpec { crds: [str], cluster_rbac: ClusterRBAC }`. The
  presence of `k8s.operator` is the unambiguous "this is a controller"
  predicate — as legible as a distinct type, with a place to `check(len(crds) >
  0)` — while `image`/`ports`/`env` stay on the one Service. This is the
  recommended refinement of §4 if the flat two-field shape feels too implicit.
- *Codegen hook*: `k8s.operator.crds` is exactly as good a hook for `forge add
  crd`/`AddToScheme` as `Operator.crds` is.
- *Compose predicate*: `k8s.operator != Undefined` (§5).

So the right resolution is **not** a parallel workload type and **not** a bag of
loose fields, but **one authoring surface (`forge.Service`) with a typed,
first-class, cluster-scoped operator sub-block inside the k8s hatch.** That
keeps the single core, preserves every invariant and codegen hook a distinct
type would offer, and — uniquely — models the real control-plane component,
which is a Service and an operator at once. The legacy `forge.Operator` is the
cautionary example: a "clean" distinct type that couldn't express the port its
own reference implementation listens on.

---

## 8. Recommendation summary

- **Shape:** operator = `forge.Service` + a cluster-scoped extension of the
  `k8s` escape hatch. **Not** a parallel `forge.Operator` core type.
- **Hatch additions:** `K8sOverrides.cluster_rbac?: ClusterRBAC` (reuse the
  existing schema) and `K8sOverrides.crds?: [str]` (name list → codegen). If a
  cleaner intent-signal is wanted, group them as a typed
  `k8s.operator?: OperatorSpec { crds, cluster_rbac }` sub-block (§7.3) — the
  preferred final shape.
- **Renderer (the non-optional half):** `_render_cluster_service` must emit
  `render_cluster_rbac` when the workload carries `cluster_rbac` — today only
  `_render_operator_manifests` does. Schema alone is inert.
- **Leader election:** stays an env default (`LEADER_ELECTION_ID` + a `leases`
  rule), never a first-class field — consistent with how `operatorkit.Run`
  already behaves.
- **Compose:** the target-namespaced hatch means compose already ignores the
  tail; make the rejection *loud* — a `forge audit` finding + adapter **skip**
  (not a hard error) for any workload declaring operator bits.
- **Control-plane migration:** small, Go-untouched. Swap the `forge.Operator`
  builder for a `forge.Service` with `ports=[9191]` + `k8s.cluster_rbac`/`crds`,
  move it from `bundle.operators` into `services`, delete the hand-written
  `workspace_controller_svc` Service and two `:9191` literals, keep the CRD
  manifest and the ClusterRole-name-stable RBAC rebind. Fidelity check that
  bites: the rebind assumes the ClusterRole is named
  `workspace-controller-clusterrole` — `render_cluster_rbac` preserves that name
  from a Service exactly as from an Operator.
- **Payoff:** deletes the drift `FORGE_KCL_IDIOMS.md:232` names, collapses two
  workload vocabularies to one, and lets the *last* control-plane component join
  the agnostic core.
```