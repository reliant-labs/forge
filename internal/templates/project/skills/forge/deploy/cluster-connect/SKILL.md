---
name: cluster-connect
description: Connect a Kubernetes cluster you operate to the control plane so environments can deploy into it — the one auth (a scoped ServiceAccount token on every cluster), the two identities and the grant forge applies, choosing the address the platform dials, disconnect, and the KCL binding.
---

# Connecting a cluster you operate

`forge cluster connect` registers a cluster **by address**. Nothing is
installed in it beyond a ServiceAccount, its token and the RBAC the platform's
apply needs, nothing about the cluster changes, and the cluster keeps existing
after you disconnect.

```bash
forge cluster connect prod-us --context gke_acme_us-central1_prod --env prod
```

forge reads the API server address and CA from your kubectl context, applies
the in-cluster grant through that same context, mints a ServiceAccount token
there, and uploads it to the control plane **write-only**. There is no cloud
IAM step and no second command.

**Declarative and idempotent by name.** Re-running `connect` with the same name
UPDATES the cluster rather than colliding, so fixing a rotated endpoint is the
same command again, and the command is safe to put in a script.

`--env` names **which control plane** to talk to, from that env's
`forge.ControlPlane` declaration. Your organization comes from the credential
and is never sent.

## One auth, on every cluster

GKE, EKS, AKS, VKE, k3s, bare metal: every cluster connects the same way, with
a ServiceAccount token forge mints in it. There is no `--auth` flag. One
mechanism means the platform protects every customer — and itself, since the
control plane's own clusters connect exactly like yours — with the same code.

The cost is a bearer token at rest on the platform, and it is bounded twice:
by the RBAC below, and by `disconnect`, which revokes it.

## Which address the platform dials

The address registered is **your context's own `server`**. The platform dials
it from a pod in its own cluster, so pick the context whose server that pod
can reach:

| Your cluster | Context to connect through |
|---|---|
| Public API endpoint | the one `get-credentials` writes by default |
| GKE private endpoint, platform on the same VPC | `gcloud container clusters get-credentials <c> --internal-ip` |
| Anything else | a context whose `server` is the reachable address |

A kubeconfig's credential — on GKE an `exec` plugin that runs on your machine —
is never read. Only the address and the CA are.

## What forge grants, and the two identities

Two identities are in play, and the grant is wrong without either.

1. **The connect identity** — `forge-system/forge-connect`, the ServiceAccount
   forge mints. It authenticates the TLS connection, and that is all it can
   do: its ClusterRole carries one rule, `impersonate` on the ServiceAccount
   named `reliant-deploy-tenant`.
2. **The impersonated identity** — the one every apply runs as. The
   platform's kustomize-controller runs with
   `--default-service-account=reliant-deploy-tenant`, so Flux impersonates
   `system:serviceaccount:flux-<your org>:reliant-deploy-tenant` on **your**
   cluster — a namespace that does not exist there and does not need to.

forge applies, server-side, through your context:

```
Namespace          forge-system
ServiceAccount     forge-system/forge-connect
Secret             forge-system/forge-connect-token   (kubernetes.io/service-account-token)
ClusterRole        forge-connect-<name>          impersonate serviceaccounts/reliant-deploy-tenant
ClusterRoleBinding forge-connect-<name>          -> forge-system/forge-connect
```

### The token, and why it is a Secret rather than a TokenRequest

The token is **sent write-only and never printed** — not in the summary, not on
failure, not in `--dry-run`. The control plane has no field to return it in.

A long-lived Secret token, not a bounded `TokenRequest`, and the trade-off is
worth stating: nothing rotates a bounded token on a schedule, so a cluster
nobody re-connects for 90 days would stop deploying, and the failure arrives as
an authentication error during an unrelated release. The Secret token has no
such deadline, it is revocable by deleting it (which `disconnect` does), and its
blast radius is bounded by the RBAC above — which is the control that actually
matters. When rotation infrastructure exists, this is the one decision that
changes.

## Binding an environment to it

Registering a cluster does not point anything at it. An environment names it:

```python
_prod = forge.ClusterTarget {
    cluster = "gke_acme_us-central1_prod"   # the kubectl context: how YOUR machine reaches it
    namespace = "acme-prod"
    connected_cluster = "prod-us"           # the registered name: how the PLATFORM reaches it
}
```

Two fields for the same physical cluster, through two different authorities.
`cluster` is a kubectl context — a local fact that differs between laptops and
does not exist in CI. `connected_cluster` is an org-level resource holding the
address, CA and credential the platform deploys with. A control-plane-backed env
targeting a real cluster needs both.

forge sends these as `cluster_bindings` on `EnsureEnvironment`, resolving each
name to its id. **Unset on a non-k3d cluster in an env that declares a control
plane is refused at render**, naming `forge cluster connect` — because the
alternative is a deploy that is accepted and never lands, surfacing later as a
Flux credential error two layers from the missing field.

A k3d cluster needs none of this: forge applies to it directly and the control
plane never dials it.

## Disconnect

```bash
forge cluster disconnect prod-us --env prod --context gke_acme_us-central1_prod
```

Two halves, in this order: the control plane revokes the credential first, then
forge deletes the ServiceAccount, token Secret and RBAC it created. A token
revoked server-side is already powerless, so a cleanup that then fails leaves
litter rather than a live credential.

**Refused while any live environment targets the cluster** — that check is the
control plane's, which is why this is not a local delete.

Deleted by exact name, and a missing object is not an error, so a disconnect run
twice converges. The `forge-system` **namespace is left in place**: it is shared
by construction and deleting a namespace deletes whatever else is in it.

Without `--context`, forge deregisters the cluster and tells you which objects
to remove by hand.

## Checking what you would send

`--dry-run` prints the request and every object that would be applied, and
contacts nothing:

```bash
forge cluster connect prod-us --context gke_acme_us-central1_prod --dry-run
```

## Refusals you may hit

| Message | Why |
|---|---|
| `whose host is a loopback or bind address` | The hub dials from a **pod**, where `127.0.0.1` is that pod. The deploy would fail as "connection refused" against a healthy cluster. Connect by an address reachable from outside the cluster. |
| `https only` | Any other scheme carries the credential in clear text. |
| `carries no certificate authority` | forge will not connect a cluster whose API server it cannot verify, and never falls back to skipping verification. Re-run your provider's `get-credentials`. |
| `unknown flag: --auth` | There is one auth now; drop the flag. |
| `resolve the credential's organization` | forge asks the control plane which org your credential acts for — it composes the hub namespace in the impersonated username, so the RBAC would be wrong without it (in the quiet way, authorizing a user that never appears). Authenticate (signed in to Reliant: nothing to do — `reliant forge` / an agent shell uses your session; standalone: `forge login`; CI: the env's `token_env`) and re-run; the token needs `deploy:read`. |
