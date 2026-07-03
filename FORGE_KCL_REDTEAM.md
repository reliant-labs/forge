# FORGE_KCL_REDTEAM.md — adversarial review of the agnostic-core reshape

Scope: the reshape of forge's KCL into a **target-agnostic workload spec** (`forge.Service`
in `kcl/core.k`) with per-target adapters (`_service_to_k8s`, `_service_to_compose`), read
against what **control-plane actually ships** (`deploy/kcl/**`). Read-only; ruthless by request.
Every claim is grounded `file:line`. Verdicts are steelmanned, not hedged.

Acid test posed: *can the agnostic model express EVERYTHING control-plane really ships?*
**Answer: no — not close. The real deploy is majority escape-hatch/raw-manifest, the agnostic
path has zero production adoption, and the one shipped adapter (k8s) silently drops probes and
securityContext.**

---

## The headline in one paragraph

`forge.Service` cleanly models the easy 40% of a workload (name/image/command/args/env/
env_from/ports/replicas/resources/volumes). But control-plane's real deploy is dominated by
things the core cannot express and shunts to `raw_manifests` / `additional_manifests`:
**every NetworkPolicy (17), every PDB, the LimitRange, the GKE HealthCheckPolicy, the CRD, the
StorageClass, all cluster infra (nats/temporal/temporal-ui/litellm as raw Deployments), the
operator's own Service, and every non-default securityContext.** Worse, two capabilities that
*every* production Deployment needs — **health probes** and a **securityContext override** — are
absent from the core AND from the k8s adapter's rendered output (the `HealthCheck` and
`security_context` schemas exist but are **dead code** the manifest renderer never reads). And no
control-plane environment uses the agnostic path at all — it is **dogfood-fixture-only**. The
"target-agnostic" framing is aspirational: today this is an opinionated k8s generator with a
compose *sketch*, and as a workload spec it is strictly weaker than Score.dev, which it cites as
lineage.

---

## Prioritized findings (severity-ranked)

### S1 — CRITICAL

#### S1-1. Health probes do not exist — in the core OR in the rendered k8s output. `HealthCheck` is dead code.
- The agnostic `Service` (`kcl/core.k:222-285`) has **no** health/liveness/readiness/startup field.
- The legacy `HealthCheck` schema exists (`kcl/schema.k:305-325`) and `K8sCluster.health_check`
  is declared (`kcl/schema.k:785`) and projected into the JSON contract (`kcl/render.k:119`) —
  **but the k8s manifest renderer never emits a probe.** `render_k8s_service`
  (`kcl/lib/services.k:323-444`) builds the container with `ports/resources/env/envFrom/
  securityContext/volumeMounts` and **no `livenessProbe`/`readinessProbe`/`startupProbe`**.
  Confirmed: `grep -c Probe kcl/render.k` → `0`; no probe in `kcl/lib/services.k`.
- Consequence: **every forge-rendered Deployment ships with no probes.** control-plane's own app
  services (`admin-server`, `workspace-proxy`, `reliant-api-server`, `daemon-gateway`) get pod-level
  liveness/readiness only by accident of the GKE `HealthCheckPolicy` (LB-level, `deploy/kcl/lib/infra.k:570-609`)
  — a wedged-but-alive pod is never restarted, and traffic is not gated during boot on non-GKE
  targets. control-plane's infra that *does* need probes (`nats`, `temporal`, `litellm`,
  `deploy/kcl/lib/infra.k:136-145,267-276,469-482`) is hand-written raw *because forge can't render them*.
- **Fix:** put probes in the core (`Service.health: HealthCheck` with http/tcp/exec + startup),
  render them in `render_k8s_service`, and either delete or wire the orphaned
  `K8sCluster.health_check`. Non-negotiable for anything called a "workload spec." Score models
  this as first-class `containers[].livenessProbe`/`readinessProbe`.

#### S1-2. Coverage: the majority of control-plane's real deploy is NOT core-expressible.
Classifying every k8s feature cp ships (evidence from `deploy/kcl/**`):

**Core-expressible** (falls out of `forge.Service`): image, command/args, env (value/secret/config),
env_from, ports, replicas, resources, empty_dir/secret/configMap volume mounts, per-service SA name,
image_pull_secrets. — `admin-server`/`workspace-proxy`/`reliant-*`/`daemon-gateway` bodies.

**Escape-hatch-only** (`Service.k8s`, `kcl/core.k:171-220`): node_selector, tolerations,
pod annotations, out-of-band service_account, extra namespaced RBAC.

**NOT expressible — must become `raw_manifests`/`additional_manifests`** (the headline set):
| Capability | Evidence (control-plane) | Count |
|---|---|---|
| NetworkPolicy (per-component ingress, GFE carve-outs, egress, ipBlock, matchExpressions) | `deploy/kcl/lib/netpol.k:28-358` | 13 + 4 |
| PodDisruptionBudget | `deploy/kcl/lib/pdb.k:48-64` ("forge has NO native PodDisruptionBudget concept") | 5 (prod) |
| LimitRange | `deploy/kcl/lib/limits.k:53-84` | 1 |
| GKE HealthCheckPolicy | `deploy/kcl/lib/infra.k:570-609`, `deploy/kcl/prod/main.k:248-252` | 3 |
| CustomResourceDefinition | `deploy/kcl/lib/crd.k:11-171` | 1 |
| StorageClass | `deploy/daemon-cluster/storageclass.yaml` | 1 |
| Cluster infra as raw Deployment+Service (multi-role, probes, fsGroup, root) | `deploy/kcl/lib/infra.k` nats/temporal/temporal-ui/litellm | 4 |
| Operator's own Service (`:9191`) | `deploy/kcl/lib/infra.k:515-533` (Operator emits no Service) | 1 |
| Cross-cluster ClusterRole beyond `namespaced_rbac` | `deploy/kcl/lib/rbac.k:29-296` | several |
| Namespace PSS-baseline patch | `deploy/kcl/lib/infra.k:43-57` | per-env |
| RuntimeClassName **on a pod** | (see S2-5b) | — |

- **The ratio is the finding.** control-plane needed ~10 bespoke library modules
  (`netpol.k`, `pdb.k`, `limits.k`, `infra.k`, `crd.k`, `rbac.k`, `nats.k`, `resources.k`, `ports.k`,
  `builds.k`) to fill forge gaps. A "target-agnostic workload spec" whose flagship consumer routes
  the majority of its manifests around the spec is not yet a spec; it's a convenience layer with a
  large `raw_manifests` bypass.
- **Fix / decision:** pick the coverage you will own as first-class (probes, securityContext, PDB,
  NetworkPolicy, StatefulSet — the recurring cp raw-manifest set) vs. the coverage you consciously
  leave to `raw_manifests`. State it. Right now the boundary is "whatever forge didn't get to,"
  which the IDIOMS doc itself admits will "churn" (`FORGE_KCL_IDIOMS.md:542`).

#### S1-3. securityContext is unmodelable, and the one existing knob is dead — this is *why* cp's infra can't be `forge.Service`s.
- The k8s adapter hardcodes a restricted-PSA securityContext (`kcl/lib/services.k:149-164,383,403`):
  `runAsNonRoot`, `runAsUser=65532`, `readOnlyRootFilesystem=true`, `drop:[ALL]`, no `fsGroup`.
- `K8sCluster.security_context: SecurityContext` exists (`kcl/schema.k:610-620,786`) and is
  projected to JSON (`kcl/render.k:120`) but **`render_k8s_service` never reads it** — always emits
  the hardcoded default. So even the legacy override is non-functional in the manifest path.
- The agnostic `Service` + `K8sOverrides` have **no securityContext field at all** (`kcl/core.k:215-220`).
- Concrete blast radius in control-plane: `temporal-auto-setup` needs **root**
  (`deploy/kcl/lib/infra.k:195-262`, no pod securityContext); `nats` needs `runAsUser=1000` +
  **`fsGroup=1000`** to write JetStream (`deploy/kcl/lib/infra.k:86-92`); `litellm`/`temporal` need
  PSS **baseline** not restricted (`deploy/kcl/lib/infra.k:43-57`). None can be a `forge.Service`
  — which is exactly why they are 400 lines of raw dicts. The controller-created workspace pods go
  further: `Privileged`, custom `Capabilities`, `FSGroup`, `RunAsUser`
  (`internal/operators/workspace/workspace_controller.go:1504,1528,1549-1553,1688-1692`).
- The only "escape" is `raw_manifests`, which replaces the **entire** Deployment — forfeiting every
  adapter benefit (image resolution, env projection, volume rendering) for the sake of one field.
- **Fix:** a typed securityContext override (pod `fsGroup`/`runAsUser`/`runAsGroup`; container
  `capabilities.add`/`readOnlyRootFilesystem`/`privileged`), rendered by the adapter, and read the
  dead field. This is the single change that would let the infra stack fold into the model.

---

### S2 — SERIOUS

#### S2-4. env-as-a-map + sort-by-key silently breaks k8s `$(VAR)` interpolation ordering (and is a migration regression).
- `env_project` emits entries **sorted by key** (`kcl/core.k:105-120`, `sorted([...])`), and the
  test enshrines it (`kcl/tests/positive_env_project.k:53-56` asserts `["ALPHA","MIKE","ZULU"]`).
- k8s `env` supports dependent expansion: `VALUE: "$(EARLIER_VAR)/x"` resolves **only** against
  entries that appear *earlier in the list*. Alphabetical order does not respect data dependencies:
  `A="$(Z)"` renders with `A` before `Z`, so `$(Z)` is left **unexpanded** (literal `$(Z)`). This is
  silent — no error, wrong value at runtime.
- It is also a **regression on migration**: the legacy `env_merge` preserves author order
  (`kcl/base.k:156-163`, last-occurrence position retained), so a service that today relies on
  `$(VAR)` order and moves to the map form breaks. The core doc even celebrates that "k8s env order
  is not semantically meaningful" (`kcl/core.k:93-95`) — which is **false** for `$(VAR)` references.
- control-plane does not use `$(VAR)` today (all literals/refs, per enumeration), so this is latent
  — but a "workload spec" that can't represent ordered env is a real ceiling and a trap for the
  next consumer.
- **Fix:** either preserve insertion order (and accept that map iteration order in KCL must be made
  deterministic another way), or detect `$(VAR)` refs and topologically order / reject cycles, or at
  minimum lint+document that `$(VAR)` is unsupported. Do not ship silent reordering as "not
  semantically meaningful."

#### S2-5. No Downward API (`fieldRef`/`resourceFieldRef`) — not even in the escape hatch.
- `EnvSource` channels are exactly `value | from_secret | from_config` (`kcl/core.k:63-76`); the k8s
  projection renders only `value`/`secretKeyRef`/`configMapKeyRef` (`kcl/lib/services.k:108-132`).
- There is **no** channel for `valueFrom.fieldRef` (`POD_NAME=metadata.name`, `POD_IP=status.podIP`,
  `POD_NAMESPACE=metadata.namespace`, `NODE_NAME=spec.nodeName`) or `resourceFieldRef`
  (limits/requests-derived env). `K8sOverrides` has no `env` either, so it isn't even reachable via
  the hatch — only by replacing the whole Deployment with `raw_manifests`.
- Real controllers, OTel resource detection, and pod-aware logging need these. control-plane
  doesn't today (confirmed no `fieldRef` in repo), but this is a hard wall for the "many real
  controllers" use case OPERATOR_AGNOSTIC.md is trying to serve.
- **(S2-5b) RuntimeClassName on a pod** is similarly unreachable from a `forge.Service`:
  `render_k8s_service` emits no `runtimeClassName`; `K8sOverrides` has node_selector/tolerations but
  **not** runtimeClassName (`kcl/core.k:215-220`). cp gets away with it only because kata/gvisor
  pods are **controller-created in Go** (`internal/operators/workspace/workspace_controller.go:1795-2113`),
  not forge-rendered. A forge-managed kata service would be stuck.
- **Fix:** add a `from_field`/`from_resource` channel to `EnvSource`; add `runtime_class` to
  `K8sOverrides`. Both are cheap, additive, and unblock the controller/observability story.

#### S2-6. Single-container assumption — no sidecars, initContainers, or ephemeral containers except by whole-Deployment replacement.
- `Service` has one `image` (`kcl/core.k:275`); the adapter builds a one-element `containers` list
  (`kcl/lib/services.k:384-407`). No initContainers, no sidecars, no ephemeral/debug containers.
- control-plane's controller-created pods use `InitContainers` + a setup container with its own
  securityContext (`internal/operators/workspace/workspace_controller.go:1542,1766-1783`) — not
  forge-rendered, but proof the shape is needed. Sidecars are now table-stakes (service mesh, OTel
  collector, cloud-sql-proxy, secrets agents).
- Only escape is `raw_manifests` — again, forfeiting the whole adapter.
- **Fix:** this is the expensive-to-retrofit one. Score models `containers: {name: {...}}` (a map)
  precisely so sidecars/init are first-class. If forge intends `Service` to be *the* spec, decide
  the plural-container shape **now** — bolting a second container on later is a breaking reshape of
  the exact schema everything will have adopted.

#### S2-7. Workload types: agnostic path is Deployment-only. No StatefulSet, no DaemonSet.
- `Bundle.workloads: [Service]` (`kcl/schema.k:2145`) projects **only** to a `K8sCluster` Deployment
  via `_service_to_k8s` (`kcl/core.k:332-379`) → `render_k8s_service` (Deployment+Service). There is
  no discriminator for StatefulSet/DaemonSet/Job.
- Jobs/CronJobs and Operators exist but are **legacy, non-agnostic** schemas
  (`kcl/schema.k` `CronJob`, `Operator`; `Bundle.cronjobs`/`operators`), so they are outside the
  "target-agnostic" claim entirely.
- No StatefulSet anywhere in forge (`grep StatefulSet kcl/` → only a docstring). A stateful workload
  (any embedded DB, NATS JetStream with stable identity + `volumeClaimTemplates`, ordered rollout)
  **cannot** be an agnostic workload — which is again why cp's nats/temporal are raw Deployments
  (and would want StatefulSets in a real cluster).
- **Fix:** decide whether the agnostic core owns a `workload_type`/`stateful` discriminator (Score
  leans on `resources` provisioners for stateful backing stores instead — see S-verdict). Either
  way, "Deployment-only" must be stated, not implied.

#### S2-8. Service exposure is impoverished and split across two models.
- `render_k8s_service` always emits a bare **ClusterIP** Service with no `type`
  (`kcl/lib/services.k:415-441`) — no LoadBalancer/NodePort/headless(`clusterIP:None`, required for
  StatefulSet). `ports: [int]` (`kcl/core.k:280`) cannot express "expose externally," protocol
  (UDP), or named/headless semantics.
- External reachability lives in **separate** top-level schemas (`Gateway`/`HTTPRoute`/`GRPCRoute`,
  `kcl/schema.k:476-608`) that are **not part of `Service`** — so an "agnostic workload" cannot
  declare its own reachability; you author it twice, in two models, wired by string name
  (`HTTPRoute.service`). That is precisely the coupling the IDIOMS "core says WHAT" thesis
  (`FORGE_KCL_IDIOMS.md:68-71`) claims to avoid.
- Operators emit **no Service at all** (`render_operator`, `kcl/lib/services.k:511-551`), so cp
  hand-writes one for the controller's `:9191` RPC (`deploy/kcl/lib/infra.k:515-533`) — the exact
  `§1.4` gap OPERATOR_AGNOSTIC.md admits.
- **Fix:** give `Service` an exposure declaration (internal/headless/external + protocol) that the
  adapter lowers to Service type (and, for external, wires a route). Score's `service.ports` +
  `resources` split is a cleaner precedent than "ports here, Gateway over there."

#### S2-9. Migration debt: the agnostic path has ZERO production adoption — it is dogfood-only, atop a full parallel legacy stack.
- No control-plane env uses `Bundle.workloads`. All use `Bundle.services` (legacy `K8sWorkload`):
  `deploy/kcl/prod/main.k:393`, `preprod/main.k:290`, `staging/main.k:293` (`services = _bundle.services + [...]`).
  Every service is authored with legacy `env_vars: [EnvVar]` + `forge.env_merge`
  (`deploy/kcl/prod/main.k:109-192`, `deploy/kcl/lib/env.k`), not the map form.
- The only consumer of the agnostic `Service` is a **forge fixture**
  (`kcl/tests/positive_workspace_proxy_dogfood.k`) that *re-expresses* one prod service; it touches
  no control-plane code (its own header says so, lines 1-9).
- So forge now carries **two of everything**: `Service` vs `K8sWorkload`, `EnvSource` vs `EnvVar`,
  `EnvFrom` vs `EnvFromSource`, env-map vs `env_vars`+`env_merge`+hidden `_dedup_env_vars`. `core.k`
  header explicitly blesses indefinite coexistence ("Both shapes coexist on the forge surface,"
  `kcl/core.k:31-33`). The reshape added a second model and validated it against a mock, not the
  fleet.
- **Risk:** coexistence-forever is the likely outcome. The IDIOMS roadmap is "no backwards compat"
  (`FORGE_KCL_IDIOMS.md:513`) yet the code ships *with* backwards compat and no migration. The
  dogfood hides that operators, cronjobs, infra, netpol, pdb, probes, and securityContext all still
  require the legacy/raw path — i.e. the happy-path proof elides ~60% of a real bundle.
- **Fix:** migrate ONE real env end-to-end (whole bundle, not one service) or explicitly freeze the
  agnostic path as experimental. A green fixture is not adoption.

---

### S3 — MODERATE

#### S3-10. LLM-usability: concept-count is high and the `:` vs `=` env footgun is real.
- To author one service an LLM must compose: (a) `schema WorkspaceProxy(forge.Service)` inheritance,
  (b) schema-level field defaults, (c) a base *instance* (`workspace_base`), (d) per-env `|` override,
  (e) inside that, `env = base.env | {…}` (**merge**) vs `env = {…}` (**replace**),
  (f) the `EnvSource` one-of channel rules, (g) the tiered `k8s` hatch, (h) `appConfigEnvMap(...) | {…}`
  for config. That's three layers of "where does this default come from" (schema default vs base
  instance vs per-env) — the dogfood uses all three (`positive_workspace_proxy_dogfood.k:36-116`).
- **The sharp footgun:** `env` is a map, so an override that writes `env = { … }` instead of
  `env = base.env | { … }` **silently drops all inherited env** (the base secret/config channels
  vanish with no error). An LLM regenerating a service block is highly likely to do this. The map
  design eliminates the *duplicate-key* footgun (`kcl/core.k:9-33`) but introduces a
  *dropped-inheritance* footgun that is harder to detect (no crash, missing env at runtime).
- Untyped hatch fields (`namespaced_rbac: [{str:any}]`, `tolerations: [any]`, `raw_manifests: [any]`,
  `kcl/core.k:216-220`) get **no** KCL checking — deep-path typos render silently. The doc concedes
  this (`FORGE_KCL_IDIOMS.md:551-555`) but the mitigation (k8s-OpenAPI validation at render) is
  unbuilt.
- **Fix:** consider making env-override *additive by default* (or lint a bare `env =` in an override
  as suspicious); type the common hatch fields; ship the OpenAPI render-time check before this is
  the recommended authoring path.

#### S3-11. Operator-agnostic proposal is under-baked relative to control-plane's real operator.
- OPERATOR_AGNOSTIC.md proposes `Service + k8s.operator{cluster_rbac, crds}` and leaves the shape
  (flat vs typed sub-block) **OPEN** (`OPERATOR_AGNOSTIC.md:446-451`), leans on a runtime
  `RUN_OPERATORS` gate + leader-election-as-env-convention with no KCL declaration of intent, and
  defers retiring the legacy `Operator` (`OPERATOR_AGNOSTIC.md:355`).
- It does not address that cp's `workspace-controller` needs, beyond cluster_rbac+crds: **a Service**
  (`:9191`, `deploy/kcl/lib/infra.k:515`), a **LimitRange** (`deploy/kcl/lib/limits.k`), a **PDB**
  (`deploy/kcl/prod/main.k:262`), and cross-cluster kubeconfig **secret volumes**
  (`deploy/kcl/prod/main.k:215`). Folding the operator into `Service` without also giving `Service`
  an exposure model (S2-8) and PDB (S1-2) just relocates the gaps.
- **Fix:** resolve the exposure + PDB gaps first; then operator = Service + hatch is coherent.
  Until then it's a rename that inherits every unsolved Service gap.

#### S3-12. KCL substrate defeats type-checking exactly at the adapter boundary.
- Cross-package schema identity forces the render lib to type its lambda params as `any` and do
  **dynamic field access** on the most safety-critical code — the projection to manifests
  (`kcl/lib/services.k:10-19`, "we type the lambda parameters as `any` … field access here is
  dynamic"). A misspelled field in `render_k8s_service` is a runtime render surprise, not a checked
  error.
- The v0.11 lambda-body limitation forces `_X = X` alias hacks throughout the adapter
  (`kcl/core.k:80,294-295`). These are papercuts individually but they cluster at the seam where
  correctness matters most, and they raise the cost/novelty of the substrate for both maintainers
  and LLMs (KCL is far less represented in training data than YAML/Helm/Kustomize/cdk8s).
- Steelman: KCL *is* a real language (schemas, inheritance, checks, lambdas) and buys genuine
  load-time validation (registry-required `kcl/schema.k:744`, platform-required
  `kcl/schema.k:2253-2256`) that static YAML specs can't. That value is real and argues for keeping
  KCL as the *authoring* layer — but not for the `any`-typed adapter internals.
- **Fix:** if the adapter must consume `any`, add explicit render-time `check`s / typed re-validation
  at the boundary so field typos fail loudly.

---

### S4 — MINOR / NITS

- **S4-13.** `terminationGracePeriodSeconds`, `lifecycle` hooks (preStop/postStart),
  `enableServiceLinks` (cp uses it, `deploy/kcl/lib/infra.k:332`), `imagePullPolicy` override, and
  `serviceAccountName: default` opt-out are all unmodeled; each is raw-manifest-only.
- **S4-14.** HPA / affinity / anti-affinity / topologySpreadConstraints are entirely absent (correct
  as escape-hatch-only for now, but note StatefulSet + anti-affinity is the standard HA story cp will
  eventually want).
- **S4-15.** `EnvFrom` (bulk import) is in the core (`kcl/core.k:138-169`) and the dogfood, but **no
  real cp service uses it** — it's speculative surface added ahead of a consumer.
- **S4-16.** `_service_to_compose` (`kcl/core.k:433-491`) lowers `from_config` to `${VAR}` shell
  interpolation and `from_secret` to a whole-secret `env_file` — a *different value* than the k8s
  path for the same abstract ref. The "same abstract ref, two encodings" claim is honest, but the
  compose encoding silently changes semantics (an env-file dumps *all* keys of the secret, not the
  one named). As a proof-of-agnosticism it actually surfaces that the abstraction leaks.

---

## What we're missing — capabilities that don't fit the model at all

Ranked by how often control-plane actually hits them:

1. **Probes** (S1-1) — needed by every Deployment; today: absent everywhere.
2. **securityContext deltas** (S1-3) — fsGroup/runAsUser/caps/root/PSS-baseline; blocks folding all
   cluster infra into the model.
3. **NetworkPolicy** (17 in cp) — no model; the single largest raw-manifest category.
4. **PodDisruptionBudget** (5 in prod) — no model; pdb.k exists solely because forge has none.
5. **Multi-container / initContainers / sidecars** — whole-Deployment-replacement only.
6. **StatefulSet / headless Service / volumeClaimTemplates** — no stateful workload can be agnostic.
7. **Downward API env + runtimeClassName-on-pod** — controllers, observability, kata.
8. **LimitRange, CRD, StorageClass, HealthCheckPolicy, namespace PSS patch** — all raw.
9. **Ordered/dependent env (`$(VAR)`)** — silently mis-ordered by sort (S2-4).
10. **Operator's own Service** and **exposure type** (ClusterIP-only).

The through-line: forge models the *pod's container* well and the *pod/workload envelope* poorly,
and models *cluster-scoped policy* (netpol/pdb/limits/rbac-beyond-namespaced) not at all.

---

## Are we building the right thing? — the Score/alternatives verdict

**Score.dev is, verbatim, a target-agnostic container-workload spec that renders to k8s
(`score-k8s`) and compose (`score-compose`)** — the exact thing this reshape is building, and the
lineage the docs cite (`FORGE_KCL_IDIOMS.md:18-19`), alongside Acorn. Measured against it, forge's
core is **strictly a subset today**, and it has already walked into the wall Score's model solves:

| Concern | Score | forge agnostic `Service` |
|---|---|---|
| Multiple containers (sidecar/init) | `containers: {name: {...}}` map — first-class | one `image` only (S2-6) |
| Probes | `livenessProbe`/`readinessProbe` first-class | none (S1-1) |
| Resource dependencies (db, queue, dns, route) | `resources: {}` graph + provisioners | **deferred as YAGNI** (`depends_on` deferred; nats/temporal/litellm stay hand-typed literals, `FORGE_KCL_IDIOMS.md:235-238`) |
| Service ports / exposure | `service.ports` | `ports:[int]`, exposure split into separate Gateway schemas (S2-8) |
| Variables/interpolation | explicit `${...}` with defined resolution | sort-by-key breaks `$(VAR)` (S2-4) |
| Second target actually ships | yes (compose, k8s, humanitec) | compose is an unshipped **sketch** (`kcl/core.k:381-491`) |

So on the *agnostic-spec* axis, forge is reinventing a **narrower** Score, and the very feature
Score centers — the `resources` provisioner graph — is the one forge punted on and is now paying
for with hand-typed `nats:4222`/`temporal:7233`/`litellm:4000` literals.

**But bespoke is not obviously wrong — here's the honest steelman of why forge shouldn't just adopt Score:**
- **KCL composition beats static YAML.** Score files are flat YAML with `${}` placeholders; forge
  gets schema inheritance, `|` merges, lambdas, and **load-time invariants** (registry/platform
  required, one-of env channels, gateway listener uniqueness) that catch errors Score can't
  (`kcl/schema.k:744,2253`, `kcl/core.k:67-76`). For an LLM-authored, guardrailed product that is
  real moat.
- **forge owns the whole pipeline** (build union, dev loop, CI, secrets provider, deploy/rollback).
  Score is *spec-only* — adopting it still leaves you writing the k8s/compose implementations forge
  already has. You'd trade a data-model you control for one you don't and still keep all the plumbing.
- **The tiered escape hatch (`raw_manifests` floor) + render-through-Go** is a stronger extension
  story than Score's `x-`/patch-file approach — nothing is ever unreachable.
- **The proto-config → env projection** (`internal/codegen/config_projection_gen.go`) is genuinely
  nice and has no Score equivalent.

**Verdict.** The *bespoke KCL authoring layer* is defensible and worth keeping — it's a good
opinionated k8s generator with real validation and a clean escape hatch. The *"target-agnostic
workload spec"* **framing is oversold** and, taken literally, is a strictly-weaker Score with one
unshipped adapter and zero production adoption. Two coherent ways forward, pick one:

- **(A) Mean it.** Commit to agnostic: **steal Score's data model** — plural `containers` (map),
  first-class probes, and the `resources` provisioner graph (kill the hand-typed infra literals) —
  and **actually ship compose + migrate a real cp env**. Then the ambition is earned.
- **(B) Drop the framing.** Own "opinionated k8s + escape hatch," delete the compose sketch and the
  agnostic marketing, and spend the saved complexity budget on the **k8s coverage cp needs today**
  (probes, securityContext, PDB, NetworkPolicy, StatefulSet) instead of a second target no one uses.

What you should **not** do is stay in the middle: a half-agnostic core that is weaker than Score
*and* doesn't cover k8s, carrying two parallel schema stacks forever. That is the current trajectory.

---

## Top 5 — fix before going further

1. **Probes in the core, and render them.** Add `Service.health` (http/tcp/exec + startup);
   emit `livenessProbe`/`readinessProbe`/`startupProbe` in `render_k8s_service`; delete or wire the
   dead `HealthCheck`/`K8sCluster.health_check`. Nothing called a workload spec ships without this.
   (S1-1)
2. **securityContext override, read the dead field.** Typed pod (`fsGroup`/`runAsUser`) + container
   (`capabilities`/`readOnlyRootFilesystem`/`privileged`) overrides in `K8sOverrides`, rendered by
   the adapter. This alone lets nats/temporal/litellm fold into the model instead of 400 raw lines.
   (S1-3)
3. **Decide the multi-container + workload-type shape NOW.** Sidecars/init and StatefulSet are
   breaking retrofits after adoption. Adopt Score's plural `containers` map and a `workload_type`
   (or `resources`-graph) discriminator before more services onboard. (S2-6, S2-7)
4. **Fix env ordering or forbid `$(VAR)`.** Stop silently sorting dependent env. Preserve insertion
   order, or topologically order refs, or lint/reject `$(VAR)`. Document it either way. (S2-4)
5. **Migrate one real control-plane env end-to-end — or freeze the agnostic path as experimental.**
   The dogfood fixture proves the happy path and hides that operators/cronjobs/infra/netpol/pdb/
   probes/securityContext all still need the legacy+raw path. Prove the *whole bundle*, or stop
   calling it the authoring surface. (S2-9)

Cheap adjacent wins: add a Downward-API `from_field` channel + `runtime_class` to the hatch (S2-5);
make env-override additive-by-default or lint bare `env =` in overrides (S3-10).
