# Forge Composition — schemas on schemas, mixins, and lambdas to make control-plane's per-env deploy THIN

**Status:** design investigation (read-only; no code changed). Author target: forge maintainers + the control-plane dogfooding stack.

**The user's framing:** "schemas inheriting schemas inheriting schemas to push templates on top of templates." This doc takes that literally and grounds it in real control-plane services.

**The problem, verified.** control-plane's per-env deploy is FAT and repetitive:

| env | `main.k` lines | shape |
|---|---|---|
| `dev/main.k` | 1109 | hand-rolled `svc.X() \| { env_vars = env_merge(...) }` per service |
| `e2e/main.k` | 886 | same |
| `prod/main.k` | 414 | `full_stack(StackParams{ admin_env=env_merge(...), ... })` |
| `preprod/main.k` | 317 | same |
| `staging/main.k` | 316 | same |
| `dev-k8s/main.k` | 241 | same |

Two composition layers already exist and are good — `lib/services.k` (~236 lines of `*_base()` lambda builders returning the env-**invariant** slice of each `forge.Service`) and `lib/env.k` (~429 lines of role-bundle lambdas). But the seam is drawn in the wrong place: **per-service, env-invariant facts still live in every env's env block.** `OTEL_SERVICE_NAME=admin-server`, `PORT=8090`, `RUN_OPERATORS=false`, `LEADER_ELECTION_ID=workspace-controller`, the `WORKSPACE_CONTROLLER_URL` FQDN shape — these are properties of *the service*, not *the env*, yet they are re-stated in `_admin_env` / `_controller_env` / `_proxy_env` in staging **and** preprod **and** prod (and again, differently, in dev/e2e).

This doc shows how KCL's three composition primitives — **schema inheritance chains**, **mixins**, and **lambdas** — collapse that. The proof that it works is already in forge's own test tree: `schema WorkspaceProxy(forge.Service)` and `schema WorkspaceController(forge.Service)` (`kcl/tests/positive_user_model_inheritance.k`, `positive_operator_service.k`) render end-to-end today. control-plane's `deploy/kcl/` uses **zero** schema inheritance — that is the whole opportunity.

---

## 0. The KCL composition toolkit — what each primitive actually does

Grounded in the KCL v0.11 language (what forge pins) and verified against forge's fixtures.

| Primitive | Syntax | What it composes | Bound at | Type identity |
|---|---|---|---|---|
| **Inheritance** | `schema Sub(Base):` | Base fields + defaults + checks; Sub overrides defaults, adds fields/checks | **definition** time | `Sub` instance **IS-A** `Base` — satisfies `lambda x: Base` |
| **Mixin** | `mixin M for Proto` + `schema S(B): mixin [M]` | injects **derived attributes** + **check blocks** into the schema, orthogonal to the inheritance chain; multiple allowed | **definition** time | mixin is not a type; `S` stays `S` |
| **Protocol** | `protocol Proto:` | the field contract a mixin depends on (mixin can read `Proto`'s fields) | — | structural |
| **Union / merge** | `base \| { overlay }` | right-merge over an **instance**: keep base fields, apply overlay on top | **runtime** (per use) | result keeps base's schema type |
| **`:` vs `=` in an overlay** | `\| { env: {...} }` vs `\| { env = {...} }` | `:` **unions into** a dict/schema field (base keys survive); `=` **replaces** it wholesale | runtime | — |
| **Lambda → schema** | `lambda p: P -> forge.Service { ... }` | a parameterized factory: computes a schema instance from args | call time | returns the declared type |
| **`check`** | `check: <expr>, "msg"` | invariant enforced at construction (fires **before** any `\|` overlay) | construction | — |

The two axes that matter:

- **Inheritance and mixins are DEFINITION-time** (compile-time templates): they fix a *type* and its *defaults*/*invariants*. Discoverable, checkable, named. But inheritance is **single** (no `schema C(A, B)`), and defaults are largely static.
- **`|` and lambdas are RUNTIME** (per-instance composition): they layer computed, per-env values. Flexible and value-dependent, but the overlay (a `{str:}` dict) is **untyped** — KCL won't check the keys you put in `| { ... }`.

The design principle that falls out: **put value-INDEPENDENT structure in the inheritance chain + mixins (definition time); put value-DEPENDENT deltas in lambdas + `|` (runtime).** Nearly every mistake in a "clever" layering is putting a per-env *value* into a schema default (forces `option()` gymnastics) or putting env-*invariant* structure into a per-env `|` overlay (the repetition we're killing).

---

## 1. Inheritance CHAINS — `forge.Service` → `CpService` → `AdminServer` → per-env instance

The user's "schemas inheriting schemas inheriting schemas." Three real levels, each adding what is invariant AT THAT LEVEL.

### Level 0 — `forge.Service` (forge-owned, agnostic core)

Already exists (`kcl/core.k`): `name`, `image`, `command`, `env: {str: EnvSource}`, `ports`, `replicas`, `resources`, `build`, `volumes`, `health`, `security`, `k8s`. Zero k8s vocabulary. **Env is a MAP** — native `|` merge, no `env_merge`, duplicate keys structurally impossible.

### Level 1 — `CpService(forge.Service)`: the control-plane conventions

Every control-plane *backend binary* (admin-server, workspace-proxy, workspace-controller) shares: the `control-plane` image, the shared-binary Go build target, and a boot invariant (must name itself to OTel; runs plaintext behind the edge). None of that is env-specific → it belongs on a base **schema**, stated ONCE.

```kcl
# lib/cp.k  (forge-consumer, control-plane-owned)
import forge

schema CpService(forge.Service):
    """Control-plane backend convention layer. Every cp binary is the ONE
    shared control-plane image + cmd/control-plane build; every one must
    name itself to OTel and terminates TLS at the edge (DISABLE_TLS)."""
    image = "control-plane"
    build = forge.GoBuild { cmd = "./cmd/control-plane", output_name = "control-plane" }
    # env DEFAULT that every subtype inherits and can union onto.
    env = {
        "DISABLE_TLS" = {value = "true"}
    }
    check:
        # OTEL_SERVICE_NAME is a per-SERVICE fact — enforce every subtype set it.
        "OTEL_SERVICE_NAME" in env, "CpService requires env.OTEL_SERVICE_NAME"
```

### Level 2 — `AdminServer(CpService)`: the per-service, env-INVARIANT identity

This is the layer that today is **scattered across every env's `_admin_env`.** `OTEL_SERVICE_NAME=admin-server`, `RUN_OPERATORS=false`, `PORT=8090`, `command`, `ports` are the SAME in staging, preprod, prod, dev-k8s. Pull them onto the schema:

```kcl
# lib/cp.k
schema AdminServer(CpService):
    """admin-server — the no-filter catch-all backend. Everything here is
    identical in staging/preprod/prod/dev-k8s; only domains/CORS differ per env."""
    name = "admin-server"
    command = ["./control-plane", "server"]
    ports = [8090]
    env = {
        "OTEL_SERVICE_NAME" = {value = "admin-server"}
        "RUN_OPERATORS"     = {value = "false"}
        "PORT"              = {value = "8090"}
    }

schema WorkspaceProxy(CpService):
    name = "workspace-proxy"
    command = ["./workspace_proxy"]
    build = forge.GoBuild { cmd = "./cmd/workspace_proxy", output_name = "workspace_proxy" }  # overrides CpService default
    ports = [8080]
    env = {
        "OTEL_SERVICE_NAME" = {value = "workspace-proxy"}
        "PORT"              = {value = "8080"}
    }

# Operators are just Services with a k8s.operator tail (proven in
# positive_operator_service.k) — so the SAME chain covers the controller.
schema WorkspaceController(CpService):
    name = "workspace-controller"
    command = ["./control-plane", "workspace"]
    ports = [9191]                          # generates BOTH the containerPort AND the :9191 Service
    env = {
        "OTEL_SERVICE_NAME"  = {value = "workspace-controller"}
        "LEADER_ELECTION_ID" = {value = "workspace-controller"}
        "PORT"               = {value = "9191"}
        "METRICS_BIND_ADDRESS"      = {value = ":8080"}
        "HEALTH_PROBE_BIND_ADDRESS" = {value = ":8081"}
        "IDLE_TIMEOUT"       = {value = "30m"}
    }
    k8s = forge.K8sOverrides {
        operator = forge.OperatorSpec { cluster_rbac = _workspace_controller_rbac, crds = ["Workspace"] }
    }
```

### Level 3 — the per-env INSTANCE: only the deltas, via `|` with `:`-merge

```kcl
# prod/main.k  — the ONLY admin-server lines that survive
_admin = AdminServer {} | {
    env: cross.supabase(prod_supabase) | cross.litellm(_ns) | cross.llm_key | {
        "APP_URL"               = {value = "https://app.reliantlabs.io"}
        "CORS_ORIGINS"          = {value = "https://app.reliantlabs.io,https://reliant-prod.web.app"}
        "ALLOWED_REDIRECT_HOSTS"= {value = "app.reliantlabs.io,reliant-prod.web.app"}
        "GATEWAY_URL"           = {value = "https://gateway.reliantapi.com"}
        "WORKSPACE_CONTROLLER_URL" = {value = cross.k8s_url("workspace-controller", _ns, 9191)}
        "RELIANT_API_URL"          = {value = cross.k8s_url("reliant-api-server", _ns, 9090)}
        "DAEMON_IMAGE"          = {value = forge.image_ref_for(_registry, "workspace-base", _image_tag)}
        "DEPLOY_ENV"            = {value = "prod"}
    }
    replicas = 2
    resources = _admin_res
}
```

The `:` after `env` is **load-bearing** and proven in `positive_user_model_inheritance.k`: it unions the delta INTO the inherited env (so `OTEL_SERVICE_NAME`, `RUN_OPERATORS`, `PORT`, `DISABLE_TLS` from Levels 1-2 all survive) and adds the prod-only keys. An `=` there would DESTROY the inherited env.

### Where inheritance BEATS `|` — and where `|` wins

| Use inheritance (`schema Sub(Base)`) when… | Use `\|` instance-merge when… |
|---|---|
| the fact is **env-invariant** (name, image, ports, command, `OTEL_SERVICE_NAME`, `RUN_OPERATORS`) — state it once, get a **named type** and a `check` | the value is **per-env** (domains, CORS, namespace-derived URLs, replicas, resources) |
| you want KCL to **enforce an invariant** across all instances (`check: "OTEL_SERVICE_NAME" in env`) | you're applying a computed overlay whose keys vary run-to-run |
| discoverability matters — an LLM/editor sees `AdminServer` and its typed defaults | you're layering a cross-cutting fragment (see mixins/lambdas below) |

**Be honest about the single-inheritance ceiling.** KCL has no `schema C(A, B)`. So a chain is a *line*, not a lattice: `CpService → AdminServer` is fine, but you **cannot** also inherit a `LitellmClient` base and a `DaemonClusterAware` base into `AdminServer`. The moment a concern is *orthogonal* to the "what service am I" axis, inheritance is the wrong tool — that is exactly what mixins and lambdas are for (§2, §3). Don't try to model "reads LiteLLM" or "in the daemon-split" as a base class; you'll run out of inheritance slots at N=1.

---

## 2. MIXINS — cross-cutting concerns, orthogonal to the inheritance chain

The inheritance chain answers "**what** service is this." Cross-cutting concerns answer "**what capabilities** does it wire" — LiteLLM client, daemon-cluster split, restricted-PSA scratch volumes. These cut ACROSS the service axis (admin-server AND controller read LiteLLM; proxy AND controller are in the prod daemon-split) and there are several, so they cannot ride single inheritance. This is the "templates on top of templates" the user wants.

### 2a. What mixins are genuinely good at: invariant + structural defaults

A mixin injects **derived attributes** and **check blocks** into a schema at definition time, bound to a `protocol` contract. The clean win is value-independent structure. Example — the reliant scratch-volume + `DATA_DIR` concern (today hand-repeated in `lib/services.k` `_reliant_scratch_volumes` + `lib/env.k`):

```kcl
# lib/mixins.k
protocol HasEnv:
    env: {str: forge.EnvSource}

# A mixin that BOTH declares the scratch volumes AND asserts the writable-path
# invariant every reliant binary needs under readOnlyRootFilesystem.
mixin ReliantScratch for HasEnv:
    volumes: [forge.Volume] = [
        forge.Volume { name = "data",   mount_path = "/data" }
        forge.Volume { name = "config", mount_path = "/home/nonroot/.config" }
    ]
    check:
        # DATA_DIR must point at the writable mount or the pod dies at boot.
        env["DATA_DIR"].value == "/data" if "DATA_DIR" in env else True, \
            "ReliantScratch: DATA_DIR must be /data (the writable empty_dir)"

schema ReliantBinary(forge.Service):
    mixin [ReliantScratch]
    image = "reliant"
    build = forge.ShellBuild { cmd = "true  # sibling-repo binary, built out-of-band" }

schema ReliantApiServer(ReliantBinary):
    name = "reliant-api-server"
    ports = [9090]
```

Here the mixin adds `volumes` (a non-colliding derived default) and a `check` — both value-independent — to every `ReliantBinary` regardless of where it sits in the chain. `mixin [A, B, C]` composes several such concerns.

### 2b. The honest limitation: mixins are a POOR fit for "add these env keys"

The tempting design is `SupabaseAuthEnv` / `DaemonClusterEnv` / `InfraEnv` mixins that each inject a bundle of `env` entries. **This does not compose cleanly, for two reasons:**

1. **Same-field collision.** A mixin sets an *attribute default*. If `SupabaseAuthEnv` and `NatsEnv` and `LitellmEnv` all declare `env = {...}`, they are three defaults for the SAME field — KCL does not union them into one map; the schema's merge resolves to one. You do **not** get `supabase ∪ nats ∪ litellm`. (Contrast `|` on instances, which *does* union — §1 Level 3.)
2. **Value dependence.** These bundles need per-env VALUES — `namespace` (for the in-cluster FQDN), the Supabase URL, the daemon-link JSON. A mixin computes only from the schema's own declared fields, so to make `NatsEnv` work you'd have to add a `namespace: str` field to the schema and thread the env through `option()`/params anyway. You've paid inheritance-definition cost for a runtime value.

So: **mixins for value-independent invariants + structural defaults (2a). Lambdas + `|` for the value-dependent env bundles (2b → §3).** A mixin `check` is, however, an excellent *guardrail* over bundles a lambda built — e.g. a mixin that asserts "if `PROXY_AUTHZ_MODE=rpc` then `PROXY_AUTHZ_URL` is set," catching a half-wired daemon-split at render time.

---

## 3. LAMBDAS — the value-dependent composition engine

Lambdas are where the per-env cross-cutting bundles and the specialization live. control-plane's `lib/env.k` is *already* this, at 429 lines — this section modernizes it onto the env-MAP shape (killing `env_merge`) and adds the two missing pieces: cross-cutting **fragment builders** and an **endpoint resolver**.

### 3a. Fragment builders — one bundle, composed with native `|`

```kcl
# lib/cross.k  — cross-cutting env-map FRAGMENTS. Each returns {str: EnvSource};
# compose with native map-merge `|`. Replaces lib/env.k's env_merge lambdas.
import forge

supabase = lambda url: str, issuer: str -> {str: forge.EnvSource} {
    {
        "SUPABASE_URL"        = {value = url}
        "SUPABASE_JWT_ISSUER" = {value = issuer}
    }
}

nats = lambda ns: str -> {str: forge.EnvSource} {
    {
        "NATS_URL"      = {value = "nats://nats." + ns + ".svc.cluster.local:4222"}
        "NATS_USER"     = {from_secret = {name = "control-plane-nats", key = "user"}}
        "NATS_PASSWORD" = {from_secret = {name = "control-plane-nats", key = "password"}}
    }
}

litellm = lambda ns: str -> {str: forge.EnvSource} {
    {
        "LITELLM_URL"        = {value = "http://litellm." + ns + ".svc.cluster.local:4000"}
        "LITELLM_MASTER_KEY" = {from_secret = {name = "control-plane-litellm", key = "master-key"}}
    }
}

# daemon-cluster split (prod: proxy + controller). Derives from the ClusterClient.
daemon_cluster = lambda link -> {str: forge.EnvSource} {
    {
        "DAEMON_CLUSTER_ID" = {value = link.id}
        "CLUSTER_CONFIGS"   = {value = link.config_json}
    }
}

llm_key = {
    "LLM_KEY_ENCRYPTION_KEY" = {from_secret = {name = "control-plane-secrets", key = "llm_key_encryption_key"}}
}
```

Composition at the call site is native map-union — no helper, footgun impossible:

```kcl
env: cross.supabase(url, iss) | cross.nats(_ns) | cross.litellm(_ns) | cross.llm_key | { <deltas> }
```

Compare to today's `forge.env_merge(forge.env_merge(app_env, _db_url_only), [ ... ])` nesting. The map makes `env_merge` (and forge's hidden `_dedup_env_vars`) unnecessary — see `kcl/core.k`'s header for why the map is the correct core.

### 3b. The endpoint resolver — kills the ×5 hand-typed FQDN

The single highest-value lambda. Today `http://<svc>.<ns>.svc.cluster.local:<port>` is hand-typed and duplicated across staging/preprod/prod/e2e/dev-k8s (`WORKSPACE_CONTROLLER_URL …:9191`, `RELIANT_API_URL …:9090`, `PROXY_AUTHZ_URL …:8090`). Three k8s facts (DNS suffix, namespace, port) the author should never handle:

```kcl
# lib/cross.k
k8s_url = lambda svc: str, ns: str, port: int -> str {
    "http://" + svc + "." + ns + ".svc.cluster.local:" + str(port)
}
```

`cross.k8s_url("admin-server", _ns, 8090)` at every dependent site. When the controller's port (9191) is a *core fact* on `WorkspaceController.ports` (§1 Level 2), even the port literal derives from the schema instead of being triplicated.

### 3c. The specializer — "(base service, env facts) → env-specialized Service"

The top-level composition lambda the user asked about: take a per-service schema instance, the env name, and the namespace, and return the fully env-specialized `forge.Service` by layering the right cross-cutting fragments. This is `full_stack` refactored to consume the inheritance chain:

```kcl
# lib/cp.k
specialize = lambda base: forge.Service, ns: str, deltas: {str: forge.EnvSource},
                    fragments: {str: forge.EnvSource} -> forge.Service {
    base | { env: fragments | deltas }
}

# call site (prod admin-server), fragments assembled from §3a:
_admin = cp.specialize(
    AdminServer {}, _ns,
    { "APP_URL" = {value = "https://app.reliantlabs.io"}, ... },        # deltas
    cross.supabase(prod_sb) | cross.litellm(_ns) | cross.llm_key,       # fragments
) | { replicas = 2, resources = _admin_res }
```

**Lambda vs schema — the deciding rule.** A *schema* when the thing is a **named type with defaults + invariants you want checked** (a service identity, a convention layer). A *lambda* when the thing is a **pure function of inputs** (a fragment computed from `ns`; a URL from `svc/ns/port`; an instance specialized from env facts). Schemas give you type identity and `check`; lambdas give you parameterization. Neither subsumes the other — the good design uses schemas for the *nouns* (services) and lambdas for the *verbs* (resolve, specialize, build-fragment).

---

## 4. Target file layout + a real BEFORE → AFTER

### Target layout

```
deploy/kcl/
  lib/
    cp.k        # NEW: CpService → AdminServer/WorkspaceProxy/WorkspaceController chain
                #      + ReliantBinary → ReliantApiServer/... ; the `specialize` lambda
    cross.k     # NEW: cross-cutting env-map FRAGMENTS (supabase/nats/litellm/daemon_cluster)
                #      + the k8s_url endpoint resolver  (replaces most of lib/env.k)
    mixins.k    # NEW: ReliantScratch + guardrail checks (value-independent concerns)
    stack.k     # SLIMMED: full_stack consumes the chain; StackParams loses the six
                #          *_env params (services carry their own identity now)
    services.k  # DELETED (folded into cp.k schemas) — ~236 lines
    env.k       # SLIMMED to the frontend VITE_*/NEXT_PUBLIC_* builders (~150 lines);
                #          the role-bundle lambdas become cross.k fragments
    resources.k / ports.k / builds.k / netpol.k / ... # unchanged
  prod/main.k   # THIN: cluster coords + per-env deltas + the 3-4 prod-only concerns
  staging/main.k / preprod/main.k / dev-k8s/main.k  # THIN
  dev/main.k    # THIN(ner): host-mode port/instance math stays (it's genuinely per-env),
                #            but every env block collapses to deltas
```

### BEFORE (prod/main.k `_admin_env`, today — 14 lines of env, on top of 3 shared-bundle lambda calls)

```kcl
_admin_env = forge.env_merge(_shared_env + _litellm_env + _deploy_env + _llm_key_env, [
    forge.EnvVar {name = "OTEL_SERVICE_NAME", value = "admin-server"}
    forge.EnvVar {name = "RUN_OPERATORS", value = "false"}
    forge.EnvVar {name = "PORT", value = "8090"}
    forge.EnvVar {name = "WORKSPACE_CONTROLLER_URL", value = "http://workspace-controller." + _namespace + ".svc.cluster.local:9191"}
    forge.EnvVar {name = "RELIANT_API_URL", value = "http://reliant-api-server." + _namespace + ".svc.cluster.local:9090"}
    forge.EnvVar {name = "WORKSPACE_BASE_DOMAIN", value = "workspaces.reliantapi.com"}
    forge.EnvVar {name = "APP_URL", value = "https://app.reliantlabs.io"}
    forge.EnvVar {name = "CORS_ORIGINS", value = "https://app.reliantlabs.io,https://reliant-prod.web.app"}
    forge.EnvVar {name = "ALLOWED_REDIRECT_HOSTS", value = "app.reliantlabs.io,reliant-prod.web.app"}
    forge.EnvVar {name = "GATEWAY_URL", value = "https://gateway.reliantapi.com"}
    forge.EnvVar {name = "DAEMON_IMAGE", value = forge.image_ref_for(_registry, "workspace-base", _image_tag)}
] + env.prod_admin_env_tail())
```

`OTEL_SERVICE_NAME`, `RUN_OPERATORS`, `PORT` here are **byte-identical** in staging/preprod/dev-k8s. The two FQDNs are hand-typed in all five. That is the drift surface.

### AFTER

```kcl
_admin = AdminServer {} | {
    env: cross.supabase(_supabase_url, _supabase_jwt_issuer) | cross.litellm(_ns) | cross.llm_key | {
        # ── genuinely prod-specific: domains + cross-service deps ──
        "WORKSPACE_CONTROLLER_URL" = {value = cross.k8s_url("workspace-controller", _ns, 9191)}
        "RELIANT_API_URL"          = {value = cross.k8s_url("reliant-api-server", _ns, 9090)}
        "WORKSPACE_BASE_DOMAIN"    = {value = "workspaces.reliantapi.com"}
        "APP_URL"                  = {value = "https://app.reliantlabs.io"}
        "CORS_ORIGINS"             = {value = "https://app.reliantlabs.io,https://reliant-prod.web.app"}
        "ALLOWED_REDIRECT_HOSTS"   = {value = "app.reliantlabs.io,reliant-prod.web.app"}
        "GATEWAY_URL"              = {value = "https://gateway.reliantapi.com"}
        "DAEMON_IMAGE"             = {value = forge.image_ref_for(_registry, "workspace-base", _image_tag)}
        "DEPLOY_ENV"               = {value = "prod"}
    }
    replicas = 2
    resources = _admin_res
}
```

`OTEL_SERVICE_NAME`/`RUN_OPERATORS`/`PORT`/`DISABLE_TLS` are GONE from every env — they live once on `AdminServer`/`CpService`. The FQDNs are resolver calls. What remains is exactly the prod-specific truth.

### Quantified reduction

- **Env-invariant per-service keys** (`OTEL_SERVICE_NAME`, `PORT`, `RUN_OPERATORS`, `LEADER_ELECTION_ID`, `METRICS_/HEALTH_BIND`, `IDLE_TIMEOUT`, `DISABLE_TLS`): ~4-6 per service × 3 backend services × 5 cluster envs ≈ **60-90 restated EnvVar lines deleted**, replaced by ~15 lines of schema defaults stated ONCE in `lib/cp.k`.
- **Hand-typed FQDNs**: 3 distinct URLs (`WORKSPACE_CONTROLLER_URL`, `RELIANT_API_URL`, `PROXY_AUTHZ_URL`) × up to 5 envs ≈ **~12 hand-typed cluster-DNS strings** → resolver calls; the DNS-suffix/namespace/port knowledge lives in one 3-line lambda.
- **`lib/services.k` (236 lines)** folds into `lib/cp.k` schemas at ~roughly half the size (the builders' long "what's invariant" comments become the schema itself), and it now ALSO absorbs the per-service env identity that was in the env blocks.
- **`env_merge` / `forge.DB_ENV`-filtering / `_dedup` scaffolding**: removed at every call site (map-union replaces it).
- Net: the three cloud `main.k` env sections (~85 lines each in prod, similar in staging/preprod) drop to the **per-env delta keys only** — roughly a **40-55% cut** in each cluster env's authored env surface, and the deleted lines are precisely the drift-prone duplicated ones.

`dev/main.k` (1109) is a special case: much of its bulk is the parallel-dev host-port/NATS-account math and long rationale comments that are *genuinely* per-env (not duplication). The chain still removes its per-service `OTEL_SERVICE_NAME`/`PORT`/`DISABLE_TLS` repetition, but expect a smaller *percentage* win there than in the cloud envs — its fat is mostly irreducible host-mode logic, not restated facts.

---

## 5. Devil's advocate — how deep before "clever, not simple"?

**The failure mode is real.** A four-level chain plus three mixins plus a specialize-lambda can be HARDER to read than the explicit `env_merge` list it replaced — because to know what `_admin.env` actually contains you now trace `forge.Service` defaults → `CpService` defaults + check → `AdminServer` defaults → the `specialize` fragments → the `:` overlay. Five hops for one env var. The current code, for all its repetition, is *locally legible*: the whole admin env is in one list you can read top-to-bottom.

**Guidance on depth:**

- **Three inheritance levels is the ceiling.** `forge.Service → CpService → AdminServer → instance`. That is one forge-owned agnostic level, one org-convention level, one service-identity level, then values. A *fourth* schema level (e.g. `ProdAdminServer(AdminServer)`) is almost always wrong — that is what the `|` instance overlay is for. If you're tempted to make an env a subclass, stop: envs are *values*, not *types*.
- **Mixins: cap at 2-3 per schema, and only value-independent ones.** `mixin [ReliantScratch]` is fine. `mixin [ReliantScratch, NatsAware, LitellmAware, DaemonSplit]` where each injects env is the anti-pattern §2b warns about — it won't even union correctly, and it hides where env comes from. Prefer explicit `cross.*(...) | ...` at the call site: it's longer but you can *see* the fragments.
- **A cross-cutting concern belongs in a lambda-fragment, not a mixin or a base class, the moment it (a) needs a per-env value or (b) coexists with other concerns on the same field.** Both are true of every env bundle here. So the env bundles stay lambdas (as `lib/env.k` already has them) — the refactor is mostly "convert to map shape + add the endpoint resolver + pull the *invariant* keys up into schemas," NOT "turn everything into mixins."
- **Keep the overlay visible.** The virtue of `AdminServer {} | { env: {...deltas...} }` is that the *deltas* — the thing that actually differs prod-from-staging — are spelled out locally. Don't hide the deltas behind another lambda; hide only the *invariant* and the *cross-cutting* parts. The reader should see, at the env call site, exactly and only what makes this env this env.

**Lambda vs schema, restated as the simplicity test:** if you can't name a `check` you'd want on it, it's probably a lambda, not a schema. Services have invariants (`OTEL_SERVICE_NAME` must be set, ports must be positive) → schemas. Env fragments are just data computed from `ns` → lambdas. Reaching for a schema where a lambda suffices adds a type name and a definition-time binding for no checkable invariant — clever, not simple.

**The strongest counter-argument to this whole doc:** control-plane is a *single application* with *six envs*, not a platform with hundreds of tenants. The inheritance/mixin machinery pays off at scale (many services × many envs); at N=6-service × 6-env the `env_merge` lists are verbose but not *complex*. The honest scope: adopt **Levels 1-2 of the chain** (kills the per-service invariant repetition — clear win, low cleverness) and the **endpoint resolver** (kills the FQDN drift — highest value, near-zero cleverness). Treat mixins and a deep specialize-lambda as *optional* — reach for them only if a concern actually proliferates. Don't build the lattice before the duplication demands it.

---

## Summary

- **The seam is misplaced, not missing.** control-plane already composes with lambda builders (`lib/services.k`) and role-bundle lambdas (`lib/env.k`), but env-**invariant** per-service facts (`OTEL_SERVICE_NAME`, `PORT`, `RUN_OPERATORS`, the FQDN shape) still live in every env's env block. That is the duplication.
- **Inheritance chain (`forge.Service → CpService → AdminServer → instance`)** moves per-service *identity + invariants* to a named, checked type stated once. Proven renderable today by forge's own `positive_user_model_inheritance.k` / `positive_operator_service.k`. Ceiling: single inheritance, three levels; envs are values (`|`), never subclasses.
- **Mixins** are for value-INDEPENDENT structure + `check` guardrails (`ReliantScratch`), NOT for env-key bundles — same-field defaults don't union and env bundles need per-env values.
- **Lambdas** carry the value-dependent work: cross-cutting env *fragments* composed with native map-`|`, the *endpoint resolver*, and the *specialize* factory.
- **Thin main.k:** each cluster env's env section drops to the per-env delta keys — a ~40-55% cut in authored env surface for the cloud envs, and the deleted lines are exactly the drift-prone duplicated ones.

**Single highest-leverage composition primitive:** the **`cross.k8s_url(svc, ns, port)` endpoint-resolver lambda** (§3b). It is three lines, needs none of the inheritance/mixin machinery, and deletes the most dangerous duplication in the tree — the ×5 hand-typed `<svc>.<ns>.svc.cluster.local:<port>` FQDNs that forge *validates but never synthesizes*. It's the cheapest change with the biggest correctness payoff, and it's the on-ramp: once dependencies are resolver calls, promoting service identity into the `CpService/AdminServer` inheritance chain (§1) is the natural follow-on.
