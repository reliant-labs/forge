---
name: ingress
description: Gateway API ingress — schemas, gateway topology, route attachment, TLS via cert-manager, provider matrix.
---

# Ingress (Gateway API)

Forge's ingress story is Kubernetes Gateway API — first-class `Gateway`,
`HTTPRoute`, and `GRPCRoute` resources rendered alongside the typed
entities. There is no `Ingress` resource, no per-controller annotations,
and no `forge dev port-forward` (deleted in favor of this model).

> **Experimental feature.** Ingress is gated behind
> `features.experimental.ingress: true` in `forge.yaml`. The Gateway
> codegen, `forge dev cluster up` Traefik install, the
> cert-manager wiring, and `forge dev urls` are all off until the flag
> is on. The scaffold still emits `deploy/kcl/<env>/ingress.k` so a
> flag flip activates everything with no rescaffold. We treat the
> cross-provider matrix (k3d / GKE / EKS) as experimental until enough
> projects have shipped through it without provider-specific patches.

## Mental model

- **KCL is the source of truth.** Gateways and routes are declared as
  typed KCL values in `deploy/kcl/ingress.k`; per-env overrides live in
  `deploy/kcl/<env>/ingress.k`. `forge generate` never overwrites them.
- **One project-wide topology, per-env overrides.** The base file
  declares the gateway shape (listeners, ports, protocols). Each env
  re-exports the base and overlays only what differs (hostname, TLS,
  ports). KCL `|` merge handles the diff cleanly.
- **Routes attach by name.** `HTTPRoute.gateway` + `HTTPRoute.listener`
  reference a `Gateway.name` + `GatewayListener.name`. No `parentRefs`
  to hand-write; forge fills them in at render time.
- **The GatewayClass is cluster-scoped, not in the Bundle.** Dev:
  `forge dev cluster up` installs Traefik + applies the `traefik`
  GatewayClass. Cloud: the user enables the cloud provider's Gateway
  controller and references its class by name.
- **No route, no host access.** Services without an `HTTPRoute` /
  `GRPCRoute` are cluster-internal only. This is intentional — see
  "The no-port-forward contract" below.

## When to reach for this skill

- Declaring or changing the project's ingress topology
  (`deploy/kcl/ingress.k`).
- Attaching a new service to an existing gateway/listener.
- Swapping listeners + adding TLS for a staging/prod cutover.
- Multi-worktree dev where two namespaces share a host port and route
  by path prefix.
- Debugging "I can't reach my service from the host" / "the route
  applied but traffic 404s" / "cert-manager isn't issuing".

## Schema quick-ref

All schemas live in `kcl/schema.k` (~lines 159-302). All are imported
from `forge` in user files.

### `Gateway`

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `name` | str | required | Unique per Bundle. Referenced by routes. |
| `listeners` | [GatewayListener] | required | >=1; names + ports unique within the gateway. |
| `gateway_class_name` | str | `"traefik"` | Names the cluster's GatewayClass. |
| `host` | str | `""` | Default hostname for routes attached to this gateway. Per-route `host:` wins. |
| `tls` | GatewayTLS | _(unset)_ | Required for any HTTPS listener on the gateway. |
| `raw_policy` | str | `""` | Verbatim YAML escape hatch — see below. |

Use multiple `Gateway`s when you want separate domain/cert/policy
isolation: typically `public`, `internal`, `webhooks`.

### `GatewayListener`

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `name` | str | required | Referenced by routes (Gateway API `sectionName`). |
| `port` | int | required | 1..65535. Unique within the gateway. |
| `protocol` | "HTTP" \| "HTTPS" \| "H2C" | required | See "HTTP vs gRPC vs H2C". |
| `path_prefix` | str | `""` | Prepended to every route on this listener. Must start with `/` when set. |

### `GatewayTLS`

| Field | Type | Notes |
|-------|------|-------|
| `cert_issuer` | str | cert-manager `ClusterIssuer` name (e.g. `letsencrypt-prod`). |
| `secret_name` | str | Secret cert-manager populates with the issued cert. |

Forge auto-emits a `cert-manager.io/v1` `Certificate` resource for any
gateway that declares `tls` — pointed at `cert_issuer`, filling
`secret_name`. The user does not hand-write Certificates.

### `HTTPRoute` / `GRPCRoute`

| Field | Type | Default | Notes |
|-------|------|---------|-------|
| `name` | str | required | Unique per Bundle. |
| `gateway` | str | required | Target `Gateway.name`. |
| `listener` | str | required | Target `GatewayListener.name`. |
| `service` | str | required | Backend `Service.name` in the same env. |
| `port` | int | required | Backend service port. |
| `host` | str | `""` | Overrides the gateway's default `host` for this route. |
| `path` | str | `""` | Match path. Full path = `<listener.path_prefix>/<path>`. Empty = listener prefix is the only match. |
| `raw_policy` | str | `""` | Verbatim YAML escape hatch. |

`HTTPRoute` targets `HTTP` or `HTTPS` listeners. `GRPCRoute` targets
`H2C` or `HTTPS` — gRPC over HTTP/1.1 isn't a thing.

### Routing to frontends and webhooks

A route's `service:` field names any Kubernetes `Service` in the env
namespace — it doesn't have to map to a forge.yaml `services:` entry.
Anything that scaffolds a `Service` works: entries under `services:`,
`frontends:`, and `webhooks:` (declared on a parent service) are all
valid backends. K8s resolves the name; the forge.yaml block that owns
the workload is invisible at route-resolution time. `forge audit`
treats all three as known.

Common prod shape — frontend on the public gateway, API on the same
gateway under a path, webhooks on a separate gateway with its own TLS:

```python
forge.HTTPRoute {name = "web", gateway = "public", listener = "https", service = "web", port = 3000}
forge.HTTPRoute {name = "api", gateway = "public", listener = "https", service = "api", port = 8080, path = "/api"}
forge.HTTPRoute {name = "stripe", gateway = "webhooks", listener = "https", service = "stripe", port = 8080}
```

## HTTP vs gRPC vs H2C

| Protocol | Use for | TLS | Where |
|----------|---------|-----|-------|
| `HTTP` | REST, gRPC-web, anything HTTP/1.1 or HTTP/2 over plaintext | none | dev, internal cluster traffic |
| `HTTPS` | All HTTP + native gRPC in production | TLS-terminated at the gateway | prod (any env with cert-manager) |
| `H2C` | Native gRPC in dev (HTTP/2 cleartext) | none | dev only — flip to `HTTPS` in prod overrides |

In dev a single `public` Gateway typically declares both an `HTTP`
listener for REST + gRPC-web and an `H2C` listener for native gRPC.
In prod both fold into one `HTTPS` listener on :443.

## The base topology

`deploy/kcl/ingress.k` (scaffolded by `forge new --kind service`):

```python
import forge

PUBLIC = forge.Gateway {
    name = "public"
    listeners = [
        forge.GatewayListener {name = "http", port = 18080, protocol = "HTTP"}
        forge.GatewayListener {name = "grpc", port = 19190, protocol = "H2C"}
    ]
}

GATEWAYS: [forge.Gateway] = [PUBLIC]
HTTP_ROUTES: [forge.HTTPRoute] = [
    forge.HTTPRoute {
        name = "my-api"
        gateway = "public"
        listener = "http"
        service = "my-api"
        port = 8080
    }
]
GRPC_ROUTES: [forge.GRPCRoute] = []
```

Ports `18080` / `19190` are the dev host-side ports — `forge dev
cluster up` derives `deploy/k3d-ports.yaml` from the listeners and
merges it into `deploy/k3d.yaml` so the host can hit the listener.
It also renders the vendored Traefik install with a matching
`--entrypoints.<name>.address=:<port>` arg per listener; Traefik
v3.2's kubernetesgateway provider needs the static entrypoint
declared at install time. **Adding or removing a listener requires
re-running `forge dev cluster up`** to install the new entrypoints
(idempotent — the Traefik Deployment restarts with the new args).

## Per-env override patterns

Each env's `deploy/kcl/<env>/ingress.k` re-exports the base; `main.k`
wires the lists into the Bundle:

```python
# deploy/kcl/prod/main.k
import deploy.kcl.prod.ingress as ing

_bundle = forge.Bundle {
    # ... services, operators, ...
    gateways = ing.GATEWAYS
    http_routes = ing.HTTP_ROUTES
    grpc_routes = ing.GRPC_ROUTES
}
```

### Worked example: dev (plaintext) → prod (HTTPS + cert-manager)

```python
# deploy/kcl/prod/ingress.k
import deploy.kcl.ingress as base
import forge

PROD_PUBLIC = base.PUBLIC | {
    host = "api.example.com"
    tls = forge.GatewayTLS {
        cert_issuer = "letsencrypt-prod"
        secret_name = "my-api-tls"
    }
    listeners = [
        forge.GatewayListener {name = "http", port = 80, protocol = "HTTP"}
        forge.GatewayListener {name = "grpc", port = 443, protocol = "HTTPS"}
    ]
}

GATEWAYS = [PROD_PUBLIC]
HTTP_ROUTES = base.HTTP_ROUTES
GRPC_ROUTES = base.GRPC_ROUTES
```

The KCL `|` merge replaces only the listed fields; route lists stay
identical because the routes attach by name, and the `public` name is
preserved.

## Multi-worktree dev: `path_prefix`

Two worktrees of the same project on one workstation collide on the
host port. Solution: each worktree's `dev/ingress.k` overlays a
distinct `path_prefix` on the listeners:

```python
import deploy.kcl.ingress as base

WORKTREE_PUBLIC = base.PUBLIC | {
    listeners = [l | {path_prefix = "/my-api-dev"} for l in base.PUBLIC.listeners]
}

GATEWAYS = [WORKTREE_PUBLIC]
HTTP_ROUTES = base.HTTP_ROUTES
GRPC_ROUTES = base.GRPC_ROUTES
```

Now `http://localhost:18080/my-api-dev/...` reaches this worktree's
routes; the other worktree uses a different prefix. The route's `path`
match still applies under the prefix.

## cert-manager dependency

Required for any gateway with `tls`. Forge does not install
cert-manager — it's a cluster-side prerequisite. The cluster operator
installs cert-manager + creates one or more `ClusterIssuer`s
(typically `letsencrypt-staging`, `letsencrypt-prod`). Forge's
`GatewayTLS.cert_issuer` names one of them.

`forge doctor` checks for cert-manager + the named `ClusterIssuer`
when a gateway with `tls` is declared.

## Provider matrix

| Provider | `gateway_class_name` | How it gets installed |
|----------|----------------------|----------------------|
| Traefik (dev default) | `traefik` | `forge dev cluster up` applies vendored manifests + the `traefik` GatewayClass. Versions pinned in `internal/templates/ingress/traefik/VERSION`. |
| GKE Gateway | `gke-l7-global-external-managed` (or env-specific) | Cluster operator enables the GKE Gateway controller on the cluster. Forge only references the class. |
| AWS Gateway API Controller | depends on install | Cluster operator installs the [AWS Gateway API Controller](https://github.com/aws/aws-application-networking-k8s). Forge only references the class. |
| Istio (roadmap, v1.x) | `istio` | Cluster operator installs Istio with Gateway API enabled. Same shape. |

In all cases the schema is identical — only `gateway_class_name`
changes per env. The cloud Gateway controllers are user-installed, not
forge-installed, because they're cluster-wide infrastructure that
predates any single forge project.

## `raw_policy` escape hatch

`Gateway.raw_policy`, `HTTPRoute.raw_policy`, and `GRPCRoute.raw_policy`
are emitted verbatim alongside the resource. Use them for
provider-specific policy CRDs:

- Traefik `Middleware` (rate limit, basic auth, IP allowlist).
- Envoy Gateway `BackendTrafficPolicy` (retry, circuit breaking).
- GKE `GCPBackendPolicy` (Cloud Armor, IAP).
- Anything Gateway API standard doesn't model yet.

```python
forge.HTTPRoute {
    name = "billing"
    gateway = "public"
    listener = "http"
    service = "billing"
    port = 8080
    raw_policy = """
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: billing-ratelimit
spec:
  rateLimit:
    average: 100
    burst: 200
"""
}
```

The contents are not validated by KCL — that's by design. Prefer
typed fields when forge offers them; reach for `raw_policy` only when
nothing typed fits.

## The no-port-forward contract

Pre-v1 forge shipped `forge dev port-forward` to expose cluster
services on host ports. That command was removed when Gateway API
landed. The contract is now:

- **A service with an HTTPRoute/GRPCRoute is reachable from the host**
  via the gateway listener's port.
- **A service without a route is cluster-internal only.** This is
  intentional — if you can't reach it, it shouldn't be reachable.
- **Stateful workloads (Postgres shells, debugger sessions) use
  `kubectl port-forward` directly.** Don't wire those through a
  gateway; they're one-off operator tasks, not application traffic.

`forge up` no longer starts port-forwards and logs a hint to run
`forge dev urls`.

## Common commands

```bash
forge dev cluster up        # Install GatewayClass + Traefik (dev only, feature-gated).
forge dev urls              # Print ingress URL table grouped by gateway/listener.
forge dev urls --json       # Machine-readable for scripts / CI.
forge dev status            # Includes an "Ingress URLs:" section.
forge audit                 # Cross-checks routes <-> services (errors on unknown refs).
forge audit --json          # Same, machine-readable.
forge doctor                # Verifies GatewayClass + (when tls declared) ClusterIssuer.
```

## Feature gate

Ingress is feature-gated in `forge.yaml`:

```yaml
features:
  ingress: true
```

- `service` kind projects: default **on**.
- `cli` / `library` kind projects: default **off**.

Off → no ingress scaffolding, no `forge dev cluster up` GatewayClass
install, no audit cross-check. Flip it on later by setting the flag
and running `forge generate` (the scaffolded `deploy/kcl/ingress.k`
templates are copy-once; they will not appear retroactively — copy
from the templates if needed).

## Common pitfalls

- **Unknown gateway or listener name in a route.** `forge audit` flags
  it as an error. Check `Gateway.name` + `GatewayListener.name` match
  exactly (case-sensitive).
- **Missing GatewayClass.** Pods stuck in `Pending`, no LB endpoint.
  `forge doctor` reports it; in dev, re-run `forge dev cluster up`.
- **Missing ClusterIssuer when `tls` is set.** Certificate stuck in
  `Issuing` forever. `forge doctor` reports it; cluster operator must
  install cert-manager + create the issuer.
- **Port collision between gateways.** Two `Gateway`s with overlapping
  listener ports on the same cluster will fight for the LB. KCL won't
  catch this (it's cross-gateway); `forge audit` does.
- **gRPC route on an HTTP listener.** Native gRPC needs H2C or HTTPS.
  The KCL check passes (any protocol is technically allowed in the
  schema) but traffic 502s. Use H2C in dev, HTTPS in prod.
- **`path` without leading slash.** KCL check catches this — fix the
  literal.
- **Editing `*_gen.k` files by hand.** Ingress files (`ingress.k`,
  `<env>/ingress.k`) are user-owned and stable across `forge generate`.
  Don't confuse them with the `*_gen.k` regeneration outputs.

## Rules

- One `deploy/kcl/ingress.k` per project; per-env files re-export with
  `|` overlays. Don't duplicate the base topology across envs.
- Don't hand-write Gateway API YAML — go through the KCL schemas.
- Don't add `Ingress` resources — Gateway API only.
- Don't reinstate `forge dev port-forward` — use ingress + listener
  ports for app traffic, `kubectl port-forward` for one-off admin.
- `raw_policy` is a last resort; reach for typed fields first.
- TLS goes through cert-manager `ClusterIssuer`; forge auto-emits the
  `Certificate` — don't hand-write it.
