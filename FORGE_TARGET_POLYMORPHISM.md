# Forge target polymorphism — one agnostic `Service`, many render targets

**Status:** design proposal (read-only investigation; no code changed). Author target:
forge maintainers + the control-plane dogfooding stack.

**No backwards-compat constraint on rendered output.** The bar is *correct* multi-target
projection of ONE authored `forge.Service`, not byte-identity with today's manifests.

---

## 0. The problem, stated precisely (verified)

The agnostic-core reshape (`FORGE_KCL_IDIOMS.md`) collapsed the old multi-mode workload
into ONE target-agnostic schema — `forge.Service` (`forge/kcl/core.k:410`) — with **no
`deploy` field**. It has exactly ONE adapter wired into render: `_service_to_k8s`
(`core.k:581`), invoked unconditionally on every `Bundle.workloads` entry by both render
entrypoints:

```kcl
# forge/kcl/render.k:660  (JSON contract `render`)
"services": [_render_service(s) for s in bundle.services] \
    + [_render_service(_service_to_k8s(w, bundle.cluster_target)) for w in bundle.workloads]

# forge/kcl/render.k:846  (k8s manifests `render_manifests`)
_projected = [_service_to_k8s(w, _ct) for w in bundle.workloads]
```

So an agnostic `Service` is **hardwired to k8s**. `_service_to_compose` (`core.k:731`) is a
proof sketch with **no render entrypoint** ("compose is not a deploy target here"). There is
**no host adapter at all**.

Meanwhile the multi-mode `deploy` union still exists — but only on the **demoted, legacy**
`K8sWorkload` (`schema.k:1711`):

```kcl
deploy?: HostDeploy | K8sCluster | External | Compose | BuildOnly
```

Render dispatches *that* carrier on `deploy.type` (`render.k:163`) into host / cluster /
external / compose / build-only.

### Why control-plane's shared builders break

`control-plane/deploy/kcl/lib/services.k` builders (`admin_server_base()` etc.) were reshaped
to return the **new agnostic** `forge.Service`. But every consumer overlays a *target* onto
the builder's result:

| Consumer | Overlay applied to the shared builder | Result |
|---|---|---|
| `dev/main.k:705` (host) | `svc.admin_server_base() \| { deploy = forge.HostDeploy{runner="air", …} }` | **compile error** — `Cannot add member 'deploy' to schema 'Service'` |
| `dev/main.k:687` (compose) | `forge.Service { name="dev-infra", deploy = forge.Compose{service="dev-infra"} }` | same — `deploy` not on `Service` |
| `lib/stack.k:288` (cluster) | `svc.admin_server_base() \| { env_vars = …, deploy = cluster.deploy \| {…} }` | same — `deploy` + `env_vars` not on `Service` |

The shared builder is *meant* to be the single env-invariant slice of admin-server
(`name`/`image`/`command`/`build`), reused by dev (host) AND prod (cluster). The reshape made
`Service` mono-target (k8s), so the one builder can no longer serve the host env — the exact
`CONTROL_PLANE_MIGRATION.md` break class **B3** ("`deploy`/`env_vars`/`image_tag` gone from
Service").

**The fix is a target-selection seam: keep the builder pure-agnostic, and let the ENV select
which adapter each workload renders through.** That is this document.

---

## 1. Target selection — WHERE does an env say "host" vs "k8s" vs "compose"?

### 1.1 The requirement dev imposes: target is PER-WORKLOAD, not per-env

The tempting minimal primitive is `Bundle.target: "k8s" | "host" | "compose"` — one target
per env. **It is insufficient**, because control-plane's `dev` env is irreducibly *mixed*,
and for permanent, documented reasons (`dev/main.k:1-31`, `:716`, `:906`):

| dev workload | Target | Why it can't move (verified from dev/main.k) |
|---|---|---|
| admin-server | **host** | fast edit→air-rebuild loop (`:689`) |
| reliant-api-server, reliant-temporal-worker | **host** | sibling `go run ./cmd/reliant` from `../reliant` (`:799`,`:826`) |
| workspace-proxy | **k8s** | must dial workspace pod IPs `10.42/16`, unroutable from host on macOS (`:716`) |
| daemon-gateway | **k8s** | same pod-IP dial constraint (`:838`) |
| workspace-controller | **k8s** | controller-runtime needs the kubelet-projected SA token, only present in-pod (`:906`) |
| dev-infra (postgres/nats/temporal/litellm) | **compose** | docker-compose infra aggregator (`:687`) |

Three targets, one env, and none of the placements are accidental. So the primitive must
resolve target **per workload**, with an env-level default for the common case (prod/staging/
preprod/e2e are 100% k8s).

### 1.2 The primitive: a target-namespaced marker block, defaulted by `Bundle.target`

Two viable shapes were weighed:

- **(A) `Bundle.workload_targets: {str: str}`** — a name→target map on the bundle. Keeps the
  Service 100% pure, but reintroduces a **stringly-typed cross-reference** (the same smell as
  the ingress `listener = "controller"` string-join in `dev/main.k:207`): a typo in a service
  name silently mis-targets, and the target lives *away* from the host-only knobs it needs.

- **(B) a target-namespaced sub-block on the Service, whose PRESENCE selects the adapter** —
  `svc.host` (HostOverrides) / `svc.compose` (ComposeOverrides), mirroring the existing
  `svc.k8s` escape hatch and the established **presence-predicate idiom** forge already uses:
  `svc.k8s.operator != Undefined` is what marks a Service an operator (`core.k:649`). A
  Service with a `host` block renders through the host adapter *and* that same block carries
  the host-only knobs (runner/air/working_dir) the adapter needs — no second lookup, no name
  cross-ref.

**Choose (B), defaulted by a scalar `Bundle.target`.** The scalar handles "whole env → one
target" (a future `dev-compose`, or the 100%-k8s cloud envs — they set nothing and get k8s);
the per-service block handles the mixed env AND co-locates the knobs. This is the direct
generalization of how `svc.k8s` already works: target-namespaced, optional, and a Service
with **no** target block is fully portable and follows `Bundle.target`.

```kcl
# forge/kcl/schema.k — Bundle (additive)
schema Bundle:
    workloads: [Service] = []
    # NEW — env-wide DEFAULT target for every workload that declares no
    # per-service target block. "k8s" preserves today's behavior exactly
    # (every cloud env + dev-k8s set nothing → all workloads render to k8s).
    target: str = "k8s"          # "k8s" | "host" | "compose"
    cluster_target?: ClusterTarget
    ...
    check:
        target in ["k8s", "host", "compose"], "Bundle.target must be k8s | host | compose"
        # RELAXED: cluster_target is required only if SOME workload resolves to
        # k8s (the old check demanded it for ANY workload — see §1.4).
```

### 1.3 Dispatch — one resolver, three adapters

The render layer gains a single per-workload resolver that picks the adapter. Every adapter
produces the **same carrier type** the pipeline already consumes — `K8sWorkload` — so nothing
downstream changes (the JSON contract and Go CLI already dispatch a `K8sWorkload` on its
`deploy.type`, `render.k:163`):

```kcl
# forge/kcl/core.k — the ONE dispatch point (replaces the hardcoded _service_to_k8s calls)
_workload_target = lambda svc: Service, default: str -> str {
    "host"    if svc.host    != Undefined else \
    "compose" if svc.compose != Undefined else \
    default
}

_project_workload = lambda svc: Service, bundle: Bundle -> K8sWorkload {
    _t = _workload_target(svc, bundle.target)
    _service_to_host(svc)                          if _t == "host" else \
    _service_to_compose_workload(svc, bundle.cluster_target) if _t == "compose" else \
    _service_to_k8s(svc, bundle.cluster_target)
}
```

Both render entrypoints change from a hardcoded `_service_to_k8s(w, ct)` to
`_project_workload(w, bundle)`:

```kcl
# render.k:660  (JSON contract) — was: _service_to_k8s(w, bundle.cluster_target)
"services": [_render_service(s) for s in bundle.services] \
    + [_render_service(_project_workload(w, bundle)) for w in bundle.workloads]

# render_manifests: host/compose workloads are NOT k8s manifests, so they drop
# out of the manifest stream exactly like today's `deploy.type == "cluster"` filter.
_projected = [_project_workload(w, bundle) for w in bundle.workloads]
_cluster_services = [s for s in bundle.services if s.deploy and s.deploy.type == "cluster"] \
    + [w for w in _projected if w.deploy.type == "cluster"]
```

A host-target workload projects to `K8sWorkload{deploy = HostDeploy}`, a compose-target one to
`K8sWorkload{deploy = Compose}` — precisely the shapes the legacy path and the Go up/deploy
loop already handle. **The multi-target machine already exists on the `K8sWorkload` carrier;
this proposal just re-connects the agnostic front-end to it via a target selector.**

### 1.4 The Bundle check that must relax

The current guard (`schema.k:2246`, "Agnostic `workloads` are cluster-only today") **forbids**
exactly this. It must become: `cluster_target` is required iff at least one workload resolves
to the k8s target. Host-only / compose-only envs need no `cluster_target` — dev still declares
one because its proxy/controller/gateway are k8s.

---

## 2. The HOST adapter — `_service_to_host` + a `svc.host` sub-block

### 2.1 What host mode needs that the agnostic `Service` lacks

Projecting `Service` (name/image/command/env-map/ports/build) to the existing `HostDeploy`
(`schema.k:637`) surfaces exactly three host-only facts the portable core deliberately does
NOT model — read straight off dev's real usage:

| Host-only fact | dev/main.k evidence | Why it's host-only |
|---|---|---|
| **runner** (`air`/`go-run`/`binary`/`delve`) | admin-server `runner="air"` (`:707`); reliant `runner="go-run"` (`:815`) | k8s/compose have no live-reload runner concept |
| **air_config** | `.air.admin-server.toml` (`:708`) | only meaningful for `runner="air"` |
| **working_dir** | `../reliant` for the sibling binaries (`:816`,`:834`) | cross-repo source root; no analogue in a container image |

Plus one **command divergence**: the host command is often *not* the container command —
reliant-api's k8s command is `["reliant","server","api"]` but its host command is
`["go","run","./cmd/reliant","server","api"]` (`:802`). admin-server and workspace-proxy, by
contrast, share their command across host and cluster. So the host command override must be
*optional* — absent → reuse the agnostic `svc.command`.

Everything else host mode needs, the core already has: `env` (the `{str: EnvSource}` map, host
coordinates), `build` (the no-op ShellBuild / GoBuild), and **secrets** — which come from the
`Bundle.secret_provider` (`DotenvSecrets`), *not* a per-service `secrets_file`, exactly as dev
already works (`dev/main.k:1054-1060`). So the host adapter sets **no** `secrets_file`.

### 2.2 The `svc.host` sub-block (HostOverrides) — the host escape hatch

Symmetric to `K8sOverrides` (`core.k:238`): a target-namespaced, minimal block of host-only
knobs. Its **presence** is the "render me to host" signal (§1.2).

```kcl
# forge/kcl/core.k — additive, sibling to K8sOverrides
schema HostOverrides:
    """The HOST adapter's escape hatch — a target-namespaced block on an
    agnostic `Service` (`svc.host`) carrying the narrow tail of host-run
    knobs the portable core does NOT model (live-reload runner, Air config,
    a sibling-repo working directory, an optional host-only command).

    PRESENCE of this block selects the host adapter for this Service
    (mirrors `svc.k8s.operator != Undefined` marking an operator). A Service
    with no `host` block follows `Bundle.target`. Applied BY the adapter
    (`_service_to_host`), never as a post-render merge — the projection is a
    structural, by-name splice onto the demoted K8sWorkload+HostDeploy carrier."""
    runner: str = "go-run"          # go-run | air | binary | delve (== HostDeploy.runner)
    air_config?: str                # only when runner == "air"
    working_dir?: str               # cross-repo source root (sibling binaries)
    command?: [str]                 # host command OVERRIDE; unset => svc.command

    check:
        runner in ["go-run", "air", "binary", "delve"], \
            "HostOverrides.runner must be go-run | air | binary | delve"
        air_config == Undefined or runner == "air", \
            "HostOverrides.air_config is only valid when runner = 'air'"
```

Add one field to `Service` (`core.k:509`, next to `k8s?: K8sOverrides`):

```kcl
    host?: HostOverrides            # presence → host target (see §1.2)
    compose?: ComposeOverrides      # presence → compose target (see §3)
```

### 2.3 `_service_to_host` — the adapter

Projects one agnostic `Service` onto the demoted `K8sWorkload{deploy = HostDeploy}` carrier.
It reuses `env_project` (`core.k:124`) verbatim — the SAME map→`[EnvVar]` projection the k8s
adapter uses — so there is zero new env machinery.

```kcl
# forge/kcl/core.k — the HOST adapter (sibling to _service_to_k8s)
_service_to_host = lambda svc: Service -> K8sWorkload {
    _h = svc.host                     # guaranteed set (presence selected this adapter)
    # Host command override wins (go run ...) else the agnostic container command.
    _command = _h.command if _h.command else (svc.command if svc.command else [])
    _K8sWorkloadForAdapter {
        name  = svc.name
        image = svc.image             # inert for go-run/air (built from source), kept for `binary`
        command = _command
        # env MAP -> [EnvVar], reusing the k8s projection. from_field (downward
        # API) has no host analogue and is dropped by env_project's consumer the
        # same way compose drops it — a host process has no pod to read.
        env_vars = env_project(svc.env)
        if svc.build:
            build = svc.build         # the no-op ShellBuild / GoBuild rides through
        deploy = HostDeploy {
            runner = _h.runner
            if _h.air_config:
                air_config = _h.air_config
            if _h.working_dir:
                working_dir = _h.working_dir
            # NO secrets_file: secrets come from Bundle.secret_provider (DotenvSecrets),
            # injected into host services by the CLI — the model dev already uses.
            # NO env_vars restatement beyond the projected map.
        }
    }
}
```

That is the entire host adapter. It is smaller than `_service_to_k8s` because host mode has no
Service object, no RBAC, no volumes-as-k8s, no securityContext — the portable core's k8s tail
simply isn't consulted.

---

## 3. Wiring COMPOSE — and the dev-infra category error

The prompt asks two distinct questions here; they have **different** answers.

### 3.1 The compose RENDER target (the `_service_to_compose` sketch)

`_service_to_compose` (`core.k:731`) already renders an agnostic `Service` to a valid
docker-compose service *dict* (image/env-map/ports/volumes/health/resources). Wiring it into
render is mechanical, mirroring §1.3: a `_service_to_compose_workload` variant that returns the
`K8sWorkload{deploy = Compose}` carrier (so the JSON contract stays uniform), and a new
`render_compose(bundle)` entrypoint that walks `bundle.workloads` where the resolved target is
`compose`, calls the sketch, and assembles a `{"services": {name: dict}}` compose file. A
`ComposeOverrides` block (compose-only knobs: `compose_file`, `env_file`) mirrors
`HostOverrides`.

**But control-plane needs none of this today.** No control-plane env runs its Go app *under*
compose — dev runs the app on the *host*. So wiring the compose render adapter is proving the
seam is honest (per the idioms thesis), **not** a control-plane requirement. It should land
when a real `dev-compose` env exists, not as part of unbreaking the shared builders.

### 3.2 dev-infra is NOT a workload — it's an infra prerequisite

`forge.Service{name="dev-infra", deploy=Compose{service="dev-infra"}}` (`dev/main.k:687`) is a
**category error** waiting to happen if forced into the agnostic model. It is not a workload
forge builds or injects env into — it declares *"bring up this pre-existing docker-compose
service (postgres+nats+temporal+litellm) before deploy"* (`docker compose up -d dev-infra`,
via the compose file's `depends_on` graph). It has:

- no image forge builds (it's an aggregator over third-party images),
- no env forge injects (each infra service owns its own),
- no ports/replicas/health forge renders.

It is the compose analogue of `Bundle.helm_charts` (bring up a platform dep) and
`Bundle.clusters` (ensure a cluster exists) — a **prerequisite**, not a rendered workload.

**Recommendation:** dev-infra stays OUT of `Bundle.workloads`. Either keep it exactly as it is
today — a legacy `K8sWorkload{deploy=Compose}` in `Bundle.services` (the carrier the model
explicitly retains for "the non-cluster deploy modes until those are folded", `schema.k:2199`)
— or promote it to a dedicated, honest `Bundle.compose_deps: [ComposeDep]` list keyed by
compose-service name. Do **not** contort it into an agnostic `Service`; the agnostic Service
models a workload forge owns, and dev-infra is a dependency forge merely *starts*.

This is a real, load-bearing finding: **the compose adapter (render my app to compose) and
dev-infra (start this compose dep) are different concerns and must not share a primitive.**

---

## 4. The payoff — ONE builder, host in dev + k8s in cluster

The acceptance criterion: the SAME `svc.admin_server_base()` renders to host in `dev` and to
k8s in a cluster env, with the shared-builder break gone.

### 4.1 The shared builder — pure agnostic, UNCHANGED

`control-plane/deploy/kcl/lib/services.k:65` already returns a pure agnostic `Service`. Under
this proposal it **needs no change** — it carries only the env-invariant slice, and never a
target:

```kcl
admin_server_base = lambda -> forge.Service {
    forge.Service {
        name = "admin-server"
        image = "control-plane"
        command = ["./control-plane", "server"]
        build = forge.GoBuild { cmd = "./cmd/control-plane", output_name = "control-plane" }
    }
}
```

### 4.2 BEFORE (today — both callers fail to compile)

```kcl
# dev/main.k:705  → ERROR: Cannot add member 'deploy' to schema 'Service'
svc.admin_server_base() | {
    deploy = forge.HostDeploy {
        runner = "air"
        air_config = ".air.admin-server.toml"
        env_vars = _cp_host_env
    }
}

# lib/stack.k:288  → ERROR: no 'deploy', no 'env_vars' on Service
svc.admin_server_base() | {
    env_vars = p.admin_env
    deploy = p.cluster.deploy | { replicas = 1, ports = p.admin_ports } | p.admin_deploy
}
```

### 4.3 AFTER — the same builder, target chosen by the env

```kcl
# ── dev/main.k (HOST) ────────────────────────────────────────────────
# env is the agnostic map, host coordinates (localhost:5434, etc.).
# The `host` block's PRESENCE selects the host adapter AND carries air.
_admin_host = svc.admin_server_base() | {
    env  = _cp_host_env_map                      # {str: EnvSource}
    host = forge.HostOverrides {
        runner = "air"
        air_config = ".air.admin-server.toml"
    }
}

forge.Bundle {
    target = "k8s"                               # default: proxy/controller/gateway
    cluster_target = _devhost_k8s
    workloads = [
        _admin_host,                             # → HOST (has `host` block)
        svc.workspace_proxy_base()   | { env = _proxy_env_map,      ports = [8080] },  # → k8s
        svc.daemon_gateway_base()    | { env = _gw_env_map,         ports = [9190,8081], build = ... },  # → k8s
        svc.reliant_api_server_base()| { env = _reliant_api_map,
            host = forge.HostOverrides { runner="go-run", working_dir="../reliant",
                                         command=["go","run","./cmd/reliant","server","api"] } },  # → HOST
        # ... reliant-temporal-worker likewise host
    ]
    operators = [ svc.workspace_controller_base("control-plane") | { env = _controller_env_map } ]  # → k8s
    # dev-infra stays a legacy compose DEP, not a workload (§3.2):
    services = [ forge.K8sWorkload { name="dev-infra", deploy=forge.Compose{service="dev-infra"} } ]
    secret_provider = forge.DotenvSecrets { path = ".env.dev.secrets" }
}
```

```kcl
# ── prod/main.k via lib/stack.k (CLUSTER) — SAME builder, no host block ──
svc.admin_server_base() | {
    env      = p.admin_env_map                   # {str: EnvSource}, in-cluster DNS
    ports    = p.admin_ports                     # [8090]
    replicas = 2
    resources = _admin_resources
}
# Bundle.target defaults to "k8s"; no per-service host/compose block → k8s adapter.
```

`svc.admin_server_base()` is authored ONCE. In `dev` it acquires a `host` block and renders to
a `HostDeploy` (air, `.air.admin-server.toml`, host env, secrets from the DotenvSecrets
provider). In `prod` it acquires ports/replicas/resources and renders to a Deployment+Service.
**The `| { deploy = … }` break is gone** because the builder carries no `deploy` and the target
is a bundle-level concern resolved by the presence of `svc.host`.

Note the two remaining migration prerequisites this doc does **not** solve (they are
orthogonal, tracked in `CONTROL_PLANE_MIGRATION.md`): the env must become map-shaped
(`env: {str: EnvSource}`, §3 there — the `_cp_host_env_map` above), and G1 (per-service
`image_tag`) — both already have `Service`-level answers landing (`Service.image_tag`,
`core.k:495`) or in flight. Target polymorphism is the third leg and the one this doc adds.

---

## 5. Devil's advocate — is this the right amount of generalization?

**"Multi-target-per-service over-generalizes (simpler-is-better)."** The idioms doc's own model
is `render_for(target)` — ONE target per render. Per-service target is strictly more machinery.
*Rebuttal:* dev is **empirically, irreducibly mixed** (§1.1) — three targets, six workloads,
every placement forced by a real constraint (macOS pod-IP routing, the controller SA token, the
edit-loop). A single-target render simply cannot express dev. The per-service block is not
speculative generality; it is the minimum that models the env forge already ships. What *would*
be over-generalization is wiring the **compose render adapter** for control-plane — §3.1 shows
control-plane never renders its app to compose, so that stays a seam-honesty proof, not a
control-plane feature. The proposal is deliberately asymmetric: build the host adapter (dev
needs it now), defer the compose render adapter (no env needs it).

**"Host mode is a dev-loop concern, not a deployment-topology concern — keep it out of the
render model entirely."** The strongest counter-proposal: leave `dev` on legacy
`K8sWorkload{deploy=HostDeploy}` and DON'T make the shared builders agnostic — host-vs-cluster
is about *how you iterate*, not *what the workload is*. *Rebuttal, with the honest cost:* if dev
stays legacy, the shared builder cannot return BOTH an agnostic `Service` (for the cloud envs)
and a legacy `K8sWorkload` (for dev). So dev would have to **fork** its five service
declarations — re-stating admin-server's `image`/`command`/`build` locally — which is *exactly*
the drift `lib/services.k` + `lib/stack.k` were created to kill (~1100 lines deleted, per
memory). For admin-server and workspace-proxy the invariant slice (`name`/`image`/`command`/
`build`) is genuinely identical host-vs-cluster, so a fork would re-open real drift (dev's
admin image/command silently diverging from prod's). **That is the case FOR the host adapter.**

But the rebuttal has a sharp edge worth stating: for the **reliant siblings**, the shared slice
is thin — their host command (`go run ./cmd/reliant …`) and cluster command
(`["reliant",…]`) already **diverge** (`services.k:27`), so the builder only shares
`name`/`build`/`volumes`. There, the host adapter earns less. So the honest scope is: **the
host adapter is clearly worth it for admin-server + workspace-proxy** (fat invariant slice,
same command), and marginal-but-consistent for the siblings (thin slice, but keeping them on
ONE model beats a special-case fork).

**"The presence-predicate is implicit magic."** `svc.host != Undefined → host target` is not
obvious at a glance. *Rebuttal:* it is the **same idiom already shipped** — `svc.k8s.operator
!= Undefined` marks an operator (`core.k:649`), and forge's whole escape-hatch design is
presence-namespaced blocks. Consistency beats a novel `workload_targets` map, and the
`Bundle.target` scalar keeps the common (all-one-target) case explicit and greppable. A
`check` rejecting a Service that sets *both* `host` and `compose` closes the ambiguity.

**"dev-infra proves the model is leaky."** *Rebuttal:* the opposite — §3.2 shows the model
stays clean precisely by REFUSING to model dev-infra as a workload. A prerequisite is not a
workload; conflating them would be the leak. Keeping dev-infra as a `compose_dep` is the model
holding its line.

---

## Verdict — does this make the control-plane migration SIMPLE?

**Yes, for the piece it addresses — and it is the piece that currently makes the migration
impossible to even compile.** The three break rows in `CONTROL_PLANE_MIGRATION.md §0` are: B1
(wrong Bundle list — mechanical), B2 (neutral `Resources` — mechanical), and **B3 (`deploy`
gone from `Service`)** — B3 is *this* problem, and it is the one with no mechanical fix, because
it is a **missing capability**, not a rename. Target polymorphism supplies the capability:

- The shared builders (`lib/services.k`) stay pure-agnostic and **unchanged** — one source of
  truth for admin-server/proxy/gateway/controller across ALL five envs, host and cluster alike.
  The drift `lib/stack.k` + `lib/services.k` deleted stays deleted.
- Each env's `main.k` overlays a target where it diverges — a `host` block in dev, ports/
  replicas/resources in cluster — which is *already* where per-env divergence lives.
- The cloud envs (prod/staging/preprod/e2e/dev-k8s) set nothing new: `Bundle.target` defaults
  to `"k8s"`, no `host` blocks, byte-identical target behavior to today.

It is **not** a total migration solution — it is orthogonal to the map-shaped-env prerequisite
(§4.3) and G1 (`image_tag`), which remain. But it is the load-bearing third leg, and it is
small: one `Bundle.target` field + one check-relaxation, one `HostOverrides` schema + one
`Service.host` field, one `_service_to_host` adapter (~15 lines, reusing `env_project`), and a
one-line dispatch change at two render call-sites. No renderer rewrite — the multi-target
machinery already exists on the `K8sWorkload` carrier; this just re-connects the agnostic
front-end to it.

### The single highest-leverage primitive

**`_service_to_host` + the target-namespaced `svc.host` (HostOverrides) presence-marker.**
It is the one primitive that directly unbreaks the shared builder: it lets a pure agnostic
`Service` — authored once in `lib/services.k` — render to a host `HostDeploy` when the ENV
attaches a `host` block (dev) and to a k8s Deployment when it does not (cluster), with the
target chosen off the builder, not baked into it. Everything else (the `Bundle.target` default,
the compose render adapter, the dev-infra `compose_dep`) is either a trivial default or a
deferrable second target. The host adapter is what turns "one agnostic definition, many
targets" from a k8s-only claim into a shipping reality for the env — `dev` — that needs it most.
