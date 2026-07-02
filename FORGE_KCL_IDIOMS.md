# Forge KCL Idioms — a target-agnostic workload spec, k8s as the first adapter

**Status:** design proposal (read-only review; no code changed). Author target: forge
maintainers + the control-plane dogfooding stack.

**No backwards-compat constraint.** This proposal reshapes the rendered output. The bar is
that conversion is **correct** (same workload semantics), *not* byte-identical to today's
manifests. Roadmap gates validate behavior, not diffs.

---

## The thesis

A forge KCL app description should be a **target-agnostic workload spec**. Kubernetes is not
*the* output — it is the **first adapter**. The same core ("this service, this image, these
env values, it depends on admin-server, it needs this secret") should render to k8s
manifests today and to a docker-compose stack (or Nomad, ECS, Fly, …) tomorrow, with **zero
k8s vocabulary in the thing the author writes.** This is the Score.dev / Acorn lineage: one
workload spec, many *resolvers*.

forge is already halfway here and doesn't name it:

- `Service.deploy` is a union — `HostDeploy | K8sCluster | External | Compose | BuildOnly`
  (`kcl/schema.k:1615`). **`Compose` already exists** (`kcl/schema.k:815`, with `env_file` /
  `service` fields) — a second target is scaffolded, just unrendered.
- The AppConfig cutover (`config_schema.k`) is **already an agnostic core**: a typed config
  the Go code reads, with no k8s in it. `appConfigEnvVars` (`config_projection.k`) is
  **already a k8s adapter** — the projection of that core into k8s `env:`.

The pain in control-plane's six envs — `forge.env_merge(...)` over 15–33 hand-typed
`EnvVar`s per service, cross-service URLs (`http://<svc>.<ns>.svc.cluster.local:<port>`)
hand-typed and duplicated ×5 — is not a KCL-ergonomics problem. **It is k8s-adapter detail
leaking into the core the author writes.** Fix the seam and the pain dissolves; get a second
target for free.

---

## 0. Ground truth (verified files)

| Fact | Evidence |
|---|---|
| `Service.deploy` is already a multi-target union; `Compose` is already a member | `kcl/schema.k:1615` (`deploy?: HostDeploy \| K8sCluster \| External \| Compose \| BuildOnly`); `kcl/schema.k:815` (`schema Compose` with `env_file`, `service`) |
| No compose *renderer* exists yet — the target is declared, not resolved | no compose projection in `internal/` |
| `EnvVar` is k8s-shaped (multi-channel: `value` \| `secret_ref+key` \| `config_map_ref+key`) and sits on the *core* `Service.env_vars` | `kcl/schema.k:70` (`schema EnvVar`), `:1614` (`env_vars: [EnvVar]` on `Service`) |
| `env_merge` exists only to dedup a *list*; the render layer *also* has a hidden `_dedup_env_vars` | `kcl/base.k:121–163` (header: *"KCL list `+` CONCATENATES … silently rescued by a HIDDEN `_dedup_env_vars`"*) |
| Cross-service URLs are hand-typed FQDN strings, duplicated ×5 cluster envs | `WORKSPACE_CONTROLLER_URL …:9191`, `RELIANT_API_URL …:9090` identical in staging/preprod/prod/e2e/dev-k8s `main.k` |
| forge *validates* hand-typed FQDNs but never *synthesizes* them | `deploy_namespace_check.go` regex-matches `*.<ns>.svc.cluster.local`; namespace is a per-env KCL literal |
| The service graph (names + ports) is fully known at gen time | `components_gen.json`; `lib/ports.k` (`admin_ports=[8090]`, `proxy_ports=[8080]`, `reliant_api_ports=[9090]`, `daemon_gateway_ports=[9190]`) |
| AppConfig core / k8s projection split already exists | `config_schema.k` (`schema AppConfig`, Tier-1 forge-owned) vs `config_projection.k` (`appConfigEnvVars` → `[EnvVar]`, Tier-1) |
| The k8s-only escape hatch is already in use | control-plane `extra_manifests` = PDBs (`lib/pdb.k`), `LimitRange` (`lib/limits.k`), GKE `HealthCheckPolicy` (`lib/infra.k`), netpol (`lib/netpol.k`) |
| Config codegen ownership tiers | `config_native_emit.go`: `config_schema.k`+`config_projection.k` = `writeForgeOwned` (Tier-1); `<env>/config.k` = `writeUserScaffoldIfAbsent` (user-owned) |
| Lint framework | `internal/cli/lint/lint.go` (flag → `collect…Findings` → `format…`, warnings-only, `--json`); `lint_config_deps.go` is the closest analog |

---

## 1. Taxonomy — split by CORE vs ADAPTER

Four kinds of config, classified by **who owns the shape**: the agnostic core (what the
author writes, target-independent) or a specific adapter (how one target realizes it).

| Kind | Real control-plane example | CORE owns | ADAPTER owns |
|---|---|---|---|
| **App config** | `LOG_LEVEL=warn`, `GITHUB_CLIENT_ID`, `APP_URL`, `IDLE_TIMEOUT` | typed `AppConfig` value (proto) | k8s: `ConfigMap` + `configMapKeyRef` env. compose: inline `environment:` map |
| **Deployment topology** | `RELIANT_API_URL`, `PROXY_AUTHZ_URL`, `WORKSPACE_CONTROLLER_URL` | *the dependency* ("depends on admin-server") | k8s: resolve to `<name>.<ns>.svc.cluster.local:<port>`. compose: resolve to `<name>:<port>` |
| **Identity / ports** | `OTEL_SERVICE_NAME`, `PORT` | service name + port number | k8s: `Service.spec.ports`, container `containerPort`. compose: `ports:` / `expose:` |
| **Secrets** | `LITELLM_MASTER_KEY`, `STRIPE_SECRET_KEY` | *abstract ref*: secret name + key | k8s: `secretKeyRef`. compose: `env_file` / compose `secrets:` |

**The through-line:** the core says *what* ("I am workspace-proxy on port 8080, I depend on
admin-server, I read `LITELLM_MASTER_KEY` from secret `control-plane-litellm/master-key`").
The adapter decides *how* to wire each of those on its target. Today control-plane's `main.k`
writes the *how* (k8s FQDNs, `secretKeyRef`, `env:` lists) by hand — that is the leak.

---

## 2. The CORE ↔ ADAPTER seam

### 2.1 The minimal agnostic core schema

Zero k8s vocabulary. This is what the author writes.

```kcl
# CORE — target-agnostic workload spec. No Deployment, no FQDN, no secretKeyRef.
schema Workload:
    name: str
    image: str
    command: [str] = []
    # env is a MAP keyed by name; the value is a target-neutral union.
    env: {str: EnvSource} = {}
    # typed app-config the process reads (the AppConfig cutover — already core).
    config?: AppConfig
    # abstract dependencies: "I need to reach these workloads". NO address here.
    depends_on: [Dependency] = []
    # abstract secret needs: name+key, not a k8s secretKeyRef.
    secrets: [SecretRef] = []
    ports: [Port] = []
    resources?: Resources          # requests/limits as plain numbers
    replicas: int = 1

schema EnvSource:
    # exactly one channel — but these are SEMANTIC, not k8s wire shapes.
    value?: str                    # a literal
    from_config?: str              # a field of `config` (AppConfig)
    from_secret?: SecretRef        # {secret, key}

schema Dependency:
    workload: str                  # "admin-server"
    port?: int                     # which port (defaults to the dep's first)
    # the env var the resolved address is injected as, e.g. "PROXY_AUTHZ_URL"
    as_env?: str

schema SecretRef:
    secret: str                    # "control-plane-litellm"
    key: str                       # "master-key"

schema Port:
    number: int
    name?: str
```

### 2.2 What lives in core vs the k8s adapter — every current primitive mapped

| Today's forge KCL primitive | Nature | Belongs in |
|---|---|---|
| `EnvVar` (`value`/`secret_ref`/`config_map_ref`) | k8s `env:` entry shape | **k8s adapter** — core uses `{str: EnvSource}` |
| `env_merge` + hidden `_dedup_env_vars` | dedup of a *list* (see §3) | **k8s adapter** — core map never needs it |
| `ConfigMap` / `config_map_ref` | k8s object + ref | **k8s adapter** — core has typed `AppConfig` |
| `RenderedSecretKey` / `secret_ref` / `secretKeyRef` | k8s secret wiring | **k8s adapter** — core has `SecretRef {secret, key}` |
| hand-typed `<svc>.<ns>.svc.cluster.local:<port>` | k8s DNS | **k8s adapter** endpoint resolver — core has `Dependency` |
| `Service` (k8s Service object), `Gateway`/`HTTPRoute`/ingress | k8s networking objects | **k8s adapter** — core has `ports` + external-exposure intent |
| PDB, `LimitRange`, `HealthCheckPolicy`, `NetworkPolicy` (control-plane `extra_manifests`) | k8s-only knobs | **k8s adapter escape hatch** (see §9) — no core analog |
| `AppConfig` (`config_schema.k`) | typed config the Go reads | **CORE** (already is) |
| `appConfigEnvVars` (`config_projection.k`) | AppConfig → `[EnvVar]` | **k8s adapter** (already is — just not named as such) |
| `Workload.name`/`image`/`command`/`build`/`ports`/`resources`/`replicas` | what the thing *is* | **CORE** (mostly already on `Service`) |

The seam is not hypothetical: it runs straight through primitives forge already ships. The
AppConfig cutover already sits exactly on the correct side of it.

---

## 3. Centerpiece — env-as-a-LIST is a k8s artifact, so `env_merge` belongs in the adapter

`forge.env_merge` is the clearest tell that adapter detail has leaked into the core. Its own
header (`kcl/base.k:121`) is the confession:

```kcl
# kcl/base.k:156 — name-keyed, last-wins dedup over two EnvVar lists
env_merge = lambda base: [EnvVar], overrides: [EnvVar] -> [EnvVar] {
    _all = base + overrides
    [ _all[i] for i in range(len(_all))
      if len([1 for j in range(i + 1, len(_all)) if _all[j].name == _all[i].name]) == 0 ]
}
```

It exists *only* because "KCL list `+` CONCATENATES — it does NOT de-duplicate," so
`APP_ENV + [EnvVar{name="LOG_LEVEL"}]` emits `LOG_LEVEL` twice and kubectl server-side apply
rejects it. And the render layer *already* runs a hidden `_dedup_env_vars` downstream — a
second copy of the same dedup.

**Why does env need dedup at all?** Because it is modeled as a *list*. And it is modeled as a
list **because k8s `env:` is a list** (`[]EnvVar`). That is the entire reason. Look at the
two targets side by side:

| Target | Native env shape | Dedup needed? |
|---|---|---|
| Kubernetes | `env:` is a **list** `[{name, value/valueFrom}]` | **Yes** — duplicate names are a hard error |
| docker-compose | `environment:` is a **map** `{NAME: value}` | **No** — a map can't hold two of a key |

env-as-a-list is not fundamental to a workload — it is a **k8s wire format**. The correct
core is a **map**: `env: {str: EnvSource}`. A map:

- makes duplicate keys **structurally impossible** (the footgun `env_merge` guards against
  cannot occur);
- merges with **native KCL `|`** (`base | {overrides}`, last-wins) — no O(n²) comprehension;
- renders trivially to *both* targets — to a k8s `env:` list (adapter walks the map, emits
  `[]EnvVar`, applies the dedup that is now honestly the adapter's job) and to a compose
  `environment:` map (adapter emits it near-verbatim, no dedup).

```kcl
# CORE authoring — native merge, terse, footgun impossible:
_env = _shared_env | { "LOG_LEVEL" = {value = "debug"} }
```

### The directive: MOVE `env_merge` out of `kcl/`, into the k8s adapter

`env_merge` and `_dedup_env_vars` are **k8s-adapter internals**: the map→`[]EnvVar`
projection is where list-dedup legitimately lives. They should leave the shared `kcl/`
surface entirely. The core author never sees a list, never types `env_merge`, and a compose
adapter never links against dedup code it doesn't need. Compose-vs-k8s is the *proof* the map
is the correct core: only one of the two targets needs the list, so the list cannot be
fundamental.

---

## 4. Dependencies & topology — core declares, each adapter resolves the address

Today (`prod/main.k:140`) the author hand-writes a k8s FQDN:

```kcl
forge.EnvVar {name = "PROXY_AUTHZ_URL", value = "http://admin-server." + _namespace + ".svc.cluster.local:8090"}
```

That string is *three* k8s facts (DNS suffix, namespace, port) the author should never
handle. In the core it is one abstract dependency:

```kcl
# CORE
depends_on = [Dependency {workload = "admin-server", port = 8090, as_env = "PROXY_AUTHZ_URL"}]
```

Each adapter owns an **endpoint resolver** that turns a `Dependency` into an address for its
target:

```kcl
# k8s adapter — the resolver the first draft called "topology_gen.k". Now correctly
# scoped: it is the K8S ADAPTER's job, not a core primitive.
resolve_k8s = lambda d: Dependency, ns: str -> str {
    "http://" + d.workload + "." + ns + ".svc.cluster.local:" + str(d.port)
}

# compose adapter — same Dependency, different address.
resolve_compose = lambda d: Dependency -> str {
    "http://" + d.workload + ":" + str(d.port)
}
```

Same core `depends_on = [{workload="admin-server", port=8090, as_env="PROXY_AUTHZ_URL"}]`
→ k8s injects `PROXY_AUTHZ_URL=http://admin-server.control-plane-prod.svc.cluster.local:8090`
→ compose injects `PROXY_AUTHZ_URL=http://admin-server:8090`. The ×5 duplication is deleted
because the adapter resolver is generated once per target and the core states the dependency
once per service.

> **Real gap this surfaces:** `workspace-controller` is a `forge.Operator` with no
> `deploy.ports`, so its `:9191` is hand-typed in *both* `_controller_env` and every
> `WORKSPACE_CONTROLLER_URL`. In the core model, the controller's `ports: [{number=9191}]`
> is a core fact; both the container port and every dependent's resolved URL derive from it.
> Non-`Workload` infra (nats:4222, temporal:7233, litellm:4000) that forge doesn't model as
> a graph node stays a literal until registered as a core `Workload` or external resource —
> see §11.

---

## 5. Secrets — core names the ref abstractly; each adapter encodes it

Core (`Workload.secrets` / `EnvSource.from_secret`): a name + key, nothing more.

```kcl
# CORE
env = { "LITELLM_MASTER_KEY" = {from_secret = SecretRef {secret = "control-plane-litellm", key = "master-key"}} }
```

Adapters encode that abstract ref for their target:

- **k8s adapter** → `env: [{name: LITELLM_MASTER_KEY, valueFrom: {secretKeyRef: {name: control-plane-litellm, key: master-key}}}]` (exactly today's `secret_ref`/`secret_key` projection).
- **compose adapter** → an `env_file:` entry, or a compose top-level `secrets:` mount, resolving `control-plane-litellm/master-key`.

This is the same split the AppConfig cutover already draws: `config.<env>.yaml`'s
`${NAME#KEY}` override names an abstract secret; `appConfigEnvVars` (k8s adapter) turns it
into `secretKeyRef`. Nothing about naming a secret is k8s-specific; only the *encoding* is.

---

## 6. Config — the AppConfig cutover is already agnostic core (not wasted)

This is the load-bearing "not wasted" claim. The in-flight cutover splits **cleanly** along
the seam with no rework:

- `config_schema.k` (`schema AppConfig`, Tier-1) = **agnostic core**. A typed config the Go
  code reads; no k8s in it. It IS `Workload.config`.
- `deploy/kcl/<env>/config.k` (`AppConfig { ... }`, user-owned) = **core per-env values**.
- `config_projection.k` `appConfigEnvVars(...)` (Tier-1) = **the k8s adapter's config
  projection** — AppConfig → `ConfigMap` + `configMapKeyRef`/`secretKeyRef` `[]EnvVar`. A
  compose adapter writes a *different* projection of the *same* `AppConfig` into an inline
  `environment:` map.

So the cutover is not something to route around — it is the first correctly-factored piece
of the core↔adapter architecture. This proposal generalizes its shape (typed core +
per-target projection) to the rest of the workload spec.

---

## 7. Proof of agnosticism — one real service, two targets

Take prod **workspace-proxy** (`prod/main.k:126`). In the agnostic core it is:

```kcl
# CORE — no k8s, no compose, no FQDN, no secretKeyRef, no env list.
workspace_proxy = Workload {
    name = "workspace-proxy"
    image = "control-plane"
    command = ["./workspace_proxy"]
    ports = [Port {number = 8080}]
    config = app_config                       # shared AppConfig (log level, etc.)
    env = {
        "OTEL_SERVICE_NAME" = {value = "workspace-proxy"}   # (adapter could derive from name)
        "APP_URL"           = {value = "https://app.reliantlabs.io"}
        "PROXY_AUTHZ_MODE"  = {value = "rpc"}
        "SUPABASE_URL"      = {value = "https://dash.reliantlabs.io"}
        "NATS_PASSWORD"     = {from_secret = SecretRef {secret = "control-plane-nats", key = "password"}}
    }
    depends_on = [
        Dependency {workload = "admin-server", port = 8090, as_env = "PROXY_AUTHZ_URL"}
    ]
}
```

**k8s adapter renders:**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: workspace-proxy, namespace: control-plane-prod}
spec:
  template:
    spec:
      containers:
        - name: workspace-proxy
          image: ghcr.io/reliant-labs/control-plane:<tag>
          command: ["./workspace_proxy"]
          ports: [{containerPort: 8080}]
          env:                                   # ← MAP projected to LIST + deduped (adapter job)
            - {name: OTEL_SERVICE_NAME, value: workspace-proxy}
            - {name: APP_URL, value: "https://app.reliantlabs.io"}
            - {name: PROXY_AUTHZ_MODE, value: rpc}
            - {name: SUPABASE_URL, value: "https://dash.reliantlabs.io"}
            - name: NATS_PASSWORD                # ← from_secret → secretKeyRef
              valueFrom: {secretKeyRef: {name: control-plane-nats, key: password}}
            - {name: PROXY_AUTHZ_URL,            # ← Dependency → FQDN:port
               value: "http://admin-server.control-plane-prod.svc.cluster.local:8090"}
---
apiVersion: v1
kind: Service
metadata: {name: workspace-proxy, namespace: control-plane-prod}
spec: {selector: {app: workspace-proxy}, ports: [{port: 8080, targetPort: 8080}]}
```

**compose adapter renders the SAME core:**

```yaml
services:
  workspace-proxy:
    image: ghcr.io/reliant-labs/control-plane:<tag>
    command: ["./workspace_proxy"]
    ports: ["8080:8080"]
    environment:                                 # ← MAP stays a MAP, no dedup pass
      OTEL_SERVICE_NAME: workspace-proxy
      APP_URL: "https://app.reliantlabs.io"
      PROXY_AUTHZ_MODE: rpc
      SUPABASE_URL: "https://dash.reliantlabs.io"
      PROXY_AUTHZ_URL: "http://admin-server:8090"   # ← Dependency → service:port
    env_file: [./secrets/control-plane-nats.env]     # ← from_secret → env_file (NATS_PASSWORD)
    depends_on: [admin-server]                        # ← Dependency also drives startup order
```

The three interesting projections, side by side:

| Core construct | k8s adapter | compose adapter |
|---|---|---|
| `env: {str: EnvSource}` (map) | `env:` **list** + dedup | `environment:` **map**, verbatim |
| `Dependency{admin-server, 8090}` | `admin-server.<ns>.svc.cluster.local:8090` | `admin-server:8090` + `depends_on:` ordering |
| `from_secret{control-plane-nats, password}` | `secretKeyRef` | `env_file:` mount |

One authored core; every target-specific decision made by the adapter.

---

## 8. How forge structures this

**KCL module layout**

```
kcl/
  core.k                  # Tier-1 forge-owned: Workload, EnvSource, Dependency, SecretRef, Port, AppConfig
  adapters/
    k8s.k                 # Tier-1: core → Deployment/Service/ConfigMap/Secret; map→[]EnvVar+dedup;
                          #         endpoint resolver (FQDN:port); secretKeyRef; env_merge lives HERE now
    compose.k             # Tier-1: core → compose services; map→environment; service:port; env_file
deploy/kcl/<env>/
  app.k                   # USER-OWNED: the core Workload descriptions (the app, once)
  config.k                # USER-OWNED: per-env AppConfig values (the cutover — unchanged)
  main.k                  # USER-OWNED: pick target + per-env instance → forge.render_for(target, app)
```

| Artifact | Tier | Rationale |
|---|---|---|
| `core.k` (Workload/EnvSource/Dependency/SecretRef), `config_schema.k` | **Tier-1 regenerated** | The agnostic vocabulary; proto-derived where applicable |
| `adapters/k8s.k`, `adapters/compose.k` (projections, endpoint resolvers, `env_merge`, `appConfigEnvVars`) | **Tier-1 regenerated** | Pure functions of core → target; forge owns every target |
| `deploy/kcl/<env>/app.k` (the core Workload descriptions) | **Scaffold-once, user-owned** | forge scaffolds the skeleton from `components_gen.json`; the *wiring intent* (who depends on whom, which secret) is app knowledge forge can't infer, and must never be clobbered |
| `deploy/kcl/<env>/config.k` | **User-owned** (already) | per-env values |

Rendering is `forge.render_for(target, workloads, env)` — the target selects the adapter.
`components_gen.json` already gives forge the names/ports to scaffold `app.k` skeletons and
to generate the endpoint resolver.

---

## 9. Two audiences, both first-class — the delightful 80% and the empowered power user

The design axis here is **not** "cheap now vs expensive later." It is: a **delightful happy
path for the 80%** *and* **an escape hatch that never disempowers the power user**. Both are
**permanent, first-class goals** — neither is a cost to be deferred. The 20% path is
*engineered*, just with different ergonomics than the 80%.

### 9.1 Core vs escape is decided by AUDIENCE COVERAGE, not by minimizing surface

For each field the question is **not** "does adding this cost core surface?" It is: **"is this
common enough that the 80% deserve happy-path treatment?"** If yes → promote to agnostic core
and make it *delightful* (typed, defaulted, cross-target). If it's genuinely power-user-only →
it lives in the escape hatch, made as good as reasonably possible. The cost of the core
surface does **not** enter the decision.

By that test, most of what *looks* k8s-specific is actually core — it has a direct
cross-target analog and the 80% hit it constantly:

| Core field | k8s projection | compose projection |
|---|---|---|
| `resources` (requests/limits) | container `resources` | `deploy.resources` |
| `replicas` | `Deployment.spec.replicas` | `deploy.replicas` |
| `volumes` / mounts | `volumes` + `volumeMounts` | top-level `volumes:` + service `volumes:` |
| `health` (liveness/readiness) | `livenessProbe`/`readinessProbe` | `healthcheck:` |
| `restart` intent | (Deployment default) | `restart:` |

So the design is a **RICH CORE + a genuinely-good escape hatch.** The escape-hatch tail —
knobs only the power user reaches for — is real and permanent: `PodDisruptionBudget`, `HPA`
behavior, node affinity / `nodeSelector`+tolerations (control-plane's kata pinning),
topology-spread, `securityContext`, raw annotations, GKE `HealthCheckPolicy`. Those users are
a first-class audience; the hatch must be *good for them*, not a grudging dump.

### 9.2 Two options for the hatch — and the decision

Two shapes were weighed for how a workload expresses that narrow tail:

- **Option A — post-render merge:** `my_app = forge.render(svc) | { overrides }`. The
  override is `|`-merged onto the *already-rendered k8s object*.
- **Option B — namespaced block on the service:** `svc = forge.Service { ... k8s = { ... } }`.
  The **adapter** reads its own namespace (`svc.k8s`) and applies the override during render.

**Decision: B, not A.** Reasons:

1. **A resurfaces the env-as-list problem inside the override.** Post-render k8s is
   list-shaped (`spec.template.spec.containers` is a LIST). To patch one field of a
   volumeMount, `|` onto that path *replaces the whole list* — so you must reconstruct the
   entire `containers` array to change one value. The exact list-surgery §3 just deleted, now
   back in the override.
2. **A erodes the single core at the call site.** `forge.render(svc) | {...}` splits the app
   into per-target render *expressions* — `render_k8s(svc) | {}` here, `render_compose(svc) |
   {}` elsewhere — so there is no longer one canonical agnostic app object; the seam leaks
   into every call site.
3. **B keeps ONE canonical app object** with per-target annotations. Because the *adapter*
   applies `svc.k8s`, it can do a **structural, by-name merge** — splice a volumeMount into
   the container *named X*, not blind-replace a positional list. And B makes non-portability
   **visible and lintable**: a service carrying a `.k8s` block is explicitly non-portable to
   compose, so `forge lint` can flag it (see §4-style nudge).
4. **B's impurity is contained** — namespaced (`svc.k8s` / `svc.compose`), optional, and a
   service *without* the block is fully agnostic. A does none of this.

### 9.3 The escape hatch is TIERED, not binary — a good experience for the power user

"Escape hatch" must **not** mean "fall off a cliff into untyped YAML." The `svc.k8s` block is
**tiered**: typed, discoverable, structured per-target knobs for the common power-user needs,
with `raw_manifests` as the **ultimate floor** beneath — reached only for genuinely exotic
things nothing else covers.

```kcl
# Option B (chosen): ONE core object; the power-user tail lives in a target-namespaced,
# TIERED block — typed knobs first, raw floor last.
workspace_controller = forge.Service {
    name = "workspace-controller"
    # ── the delightful 80%: rich agnostic core ──
    # env map, depends_on, resources, replicas, volumes, health — typed, defaulted, portable
    k8s = K8sOverrides {                           # ← Tier 1: TYPED, discoverable power-user knobs
        affinity = {...}
        node_selector = {"reliant.dev/runtime" = "kata"}
        tolerations = [{key = "reliant.dev/runtime", value = "kata", effect = "NoSchedule"}]
        pod_disruption_budget = {min_available = 1}
        hpa = {min = 2, max = 10, target_cpu = 70}
        security_context = {run_as_user = 1000, fs_group = 1000}
        annotations = {...}
        # ── Tier 2: the ULTIMATE floor — verbatim manifests for the genuinely exotic ──
        raw_manifests = [_healthcheck_policy]      # today's extra_manifests, target-scoped
    }
    # no `compose` block → fully portable to compose
}
```

`K8sOverrides` is a real, typed schema (KCL checks the fields; an editor/LLM can discover
them). `raw_manifests` is always available so **no k8s capability is ever unreachable** — but
it is the *last* resort, not the *only* resort. The power user gets typed knobs for the common
80% of *their* needs and an unbounded floor for the rest. That is an engineered 20% path, not a
cliff.

### 9.4 The real enabler — keyed maps all the way down

B's clean structural merge is a **consequence** of one design choice: the adapter keeps
collections as **KEYED MAPS internally** — containers-by-name, volumes-by-name, env-by-name —
until the *final* YAML projection flattens them to lists. This is the *same principle* as
env-as-map (§3), generalized. Solve keyed intermediates once and **every** deep override
merges structurally by name instead of clobbering a positional list; A-vs-B then almost
resolves itself, because on a keyed intermediate the override is a clean `|` on the right
sub-map. Keyed-intermediates is the load-bearing primitive; the escape-hatch ergonomics fall
out of it.

### 9.5 What we reject / keep

- **REJECT Kustomize-style overlay *files*** (a separate patch-yaml per env). That is the
  sharded-config smell this whole doc fights, and it punts merge semantics to
  strategic-merge-patch rules forge doesn't control.
- **KEEP option A's post-render `|`** available — but *only* as the last-ditch "patch the
  literal rendered YAML" hatch (the `k8s.raw` list above / a final `| {...}`), never the
  primary override path.

---

## 10. Phased roadmap (no backwards-compat — gates are correctness, not diff-identity)

| Phase | Change | Correctness gate (NOT byte-identity) |
|---|---|---|
| **0** | Adopt the AppConfig cutover as core: `main.k` consumes `config.k` + a projection, retire legacy `config_gen.k`/`APP_ENV`. | Each env boots; the process reads the same *values* (spot-check env in a running pod), regardless of `env:` ordering/shape changes. |
| **1** | Introduce the core `Workload` + `{str: EnvSource}` map + `Dependency`; move `env_merge`/`_dedup_env_vars`/endpoint-resolver into `adapters/k8s.k`; delete hand-typed FQDNs. | Rendered k8s manifests are *semantically* equivalent: same containers, same resolved env values, deps reachable. Reviewed by behavior, not `git diff`. |
| **2** | Ship `adapters/compose.k`; render one env (dev-k8s or a new `dev-compose`) to compose from the same core. | The compose stack comes up and passes the same smoke/e2e flow the k8s dev env does. |
| **3** | Rich core (`resources`/`replicas`/`volumes`/`health`) + keyed-map intermediates + the narrow `svc.k8s` block (option B); migrate control-plane's PDB/kata/HealthCheckPolicy into `svc.k8s`. | k8s render unchanged in behavior; a service with no `.k8s` block renders to compose too. |

Phase 0+1 alone delete the two biggest drift sources (per-env `APP_ENV` divergence; ×5
hand-typed cross-service URLs) and are now *unconstrained by fidelity* — we can pick the
cleanest rendered shape, not the one that happens to match today's bytes.

---

## 11. Devil's advocate

The first draft's biggest objection — **EnvVar-insertion-order / byte-identical fidelity** —
is **gone**: no backwards-compat, so reshaping output is allowed and the order-churn risk
evaporates. New risks, argued honestly:

**Leaky abstraction — not every k8s knob is compose-able.** Real, but §9 re-anchors it on the
80/20 empowerment axis: `resources`/`replicas`/`volumes`/`health`/`restart` are things the 80%
hit constantly and have compose analogs, so they get **delightful core** treatment. The
power-user tail (PDB, HPA, affinity, topology spread, `securityContext`, annotations,
`HealthCheckPolicy`) is served by the **tiered §9.3 `svc.k8s` block** — typed knobs first, raw
floor last — adapter-applied so it merges structurally by name (not list-clobber) and stays
visible/lintable. Neither audience is shortchanged: the 80% get a happy path, the power user
gets an engineered 20% path.

**Core-vs-escape boundary will churn.** A field may move between core and `svc.k8s` as the
picture sharpens. That is fine — the deciding question is stable even if the answer moves: **"is
this common enough that the 80% deserve happy-path treatment?"** Promote to core when the answer
is yes (a real cross-target analog + broad use); keep it in the tiered hatch when it's
power-user-only. The boundary moving *is* the design working — it is re-answering an
audience-coverage question, not paying down a cost.

**`svc.k8s: {str: any}` is untyped — LLM deep-path typos.** A free-form block invites
`tolerationss:` / wrong-nesting bugs an LLM won't catch. Mitigation: **type the common
overrides** (`node_selector`, `tolerations`, `pod_disruption_budget`, `annotations`) as real
schema fields so KCL checks them; leave only the genuine long-tail as `raw`. And because forge
renders k8s *through Go*, the raw block can optionally be **validated against the k8s OpenAPI
schema at render time** — a typo becomes a render error, not a silently-dropped field.

**B still admits k8s into the model.** True — `svc.k8s` is k8s in the app description.
**Accept it.** The goal was never *zero* coupling; it is coupling that is **explicit, minimal,
namespaced, and visible** — the opposite of today's k8s-detail smeared through every
`env_merge` and hand-typed FQDN. A `svc.k8s` block you can see and lint is the honest form of
a dependency that genuinely exists.

**Lowest-common-denominator core loses k8s power.** The danger is real if the core is defined
as "the intersection of all targets." It is instead "the workload semantics common enough that
the 80% deserve a happy path" (a superset of the intersection), with the power-user tail in the
tiered hatch — never unreachable. `depends_on` is the test case: it means *more* on k8s (drives
DNS + could drive NetworkPolicy) than on compose (startup order), and that's fine — the
*adapter* decides how much to make of a core fact. The core is not weaker; it is *unopinionated
about wire format*, and no k8s power is lost because `svc.k8s` (down to `raw_manifests`) is
always there.

**Is the adapter seam justified only if compose actually ships?** No — and this is the key
reframe. The justification is **not** a bet on a second target arriving. The agnostic core **is
the 80% happy path**, and it is **correct on its own merits regardless of whether a second
adapter ever ships**: env-as-a-map kills `env_merge` + the duplicate-name footgun, abstract
`depends_on` kills the ×5 hand-typed FQDN drift, abstract secrets stop leaking `secretKeyRef`
into the author's hands — all wins *within the k8s adapter alone*. Compose is the **proof the
core is honest** (if the same core can't render to a second target, the "agnostic" claim was a
lie), not the payoff that justifies the work. So the compose adapter earns its place as a
correctness check on the seam, and the core earns its place as the happy path — independent of
each other.

---

## Summary — revised architecture

- **The seam:** a forge KCL app is a **target-agnostic workload spec**; k8s is the **first
  adapter**, docker-compose the proof-of-agnosticism second. Core says *what*; each adapter
  decides *how*. forge is already halfway (`Service.deploy` union incl. `Compose`; the
  AppConfig core / `appConfigEnvVars`-projection split).
- **The core schema:** `Workload{name, image, command, env: {str: EnvSource}, config:
  AppConfig, depends_on: [Dependency], secrets: [SecretRef], ports, resources, replicas}` —
  **zero k8s vocabulary**. Env is a **map** (union-valued), deps are **abstract**, secrets are
  **abstract name+key**.
- **env_merge moves, not demotes:** env-as-a-list is a *k8s wire format*; `env_merge` + the
  hidden `_dedup_env_vars` are **k8s-adapter internals** and leave the shared `kcl/` surface.
  compose (native env *map*, no dedup) is the proof the map is the correct core.
- **topology → adapter resolver:** the core `Dependency` carries no address; each adapter
  resolves it (k8s `<name>.<ns>.svc.cluster.local:<port>`, compose `<name>:<port>`). Deletes
  the ×5 hand-typed FQDN duplication.
- **compose proof:** one real core `workspace-proxy` renders to k8s Deployment+Service AND a
  compose service, with env-map / abstract-deps / abstract-secrets each projecting differently
  per target (§7).
- **Two first-class audiences:** a **delightful happy path for the 80%** (rich agnostic core)
  and an **escape hatch that never disempowers the power user** — both permanent goals, not a
  cost/timing trade-off. Core-vs-escape is decided by **audience coverage** ("is this common
  enough that the 80% deserve a happy path?"), not by minimizing surface. The hatch is
  **tiered**: typed `svc.k8s` knobs (affinity/PDB/HPA/securityContext/annotations) for common
  power-user needs, `raw_manifests` as the ultimate floor. Override shape = **option B**
  (adapter-applied namespaced block), not post-render `|`, enabled by keeping collections as
  **keyed maps** until final projection.
- **What changed vs the first draft:** (1) fidelity/byte-identity constraint **dropped**;
  gates are now behavioral. (2) `topology_gen.k` is no longer a "core primitive" — it is **the
  k8s adapter's endpoint resolver**. (3) `env_merge` is not "demoted" — it is **relocated into
  the k8s adapter**, out of `kcl/`. (4) new **core↔adapter seam**, **compose adapter sketch**,
  and **tiered option-B escape hatch** (keyed-map enabler). (5) the AppConfig cutover is
  reframed as *already-correct core*. (6) escape-hatch/justification re-anchored on the
  80%-happy-path + power-user-empowerment axis — **not** YAGNI/cost-timing.
- **Highest-leverage recommendation:** build the **rich agnostic core + k8s adapter** — it is
  the 80% happy path and it is correct on its own merits (map-env kills `env_merge` and the
  duplicate-name footgun; abstract `depends_on` kills the ×5 FQDN drift), independent of any
  second target. Land the **compose adapter** as the proof the core is honest, and the
  **tiered `svc.k8s` hatch** so the power user is never boxed in.
