---
name: cluster-connect
description: Connect a Kubernetes cluster you operate to the control plane so environments can deploy into it — the two auth modes, the one-time IAM grant, the token fallback, disconnect, and the KCL binding.
---

# Connecting a cluster you operate

`forge cluster connect` registers a cluster **by address**. Nothing is
installed in it beyond the RBAC the platform's apply needs, nothing about the
cluster changes, and the cluster keeps existing after you disconnect.

```bash
forge cluster connect prod-us --context gke_acme_us-central1_prod --env prod
```

forge reads the API server address and CA from your kubectl context, tells the
control plane, applies the in-cluster grant through that same context, and
prints the one-time cloud IAM grant you run yourself.

**Declarative and idempotent by name.** Re-running `connect` with the same name
UPDATES the cluster rather than colliding, so fixing a rotated endpoint is the
same command again, and the command is safe to put in a script.

`--env` names **which control plane** to talk to, from that env's
`forge.ControlPlane` declaration. Your organization comes from the credential
and is never sent.

## Two auth modes, which are the two ends of a trade-off

| `--auth` | What authenticates | When |
|---|---|---|
| `gcp` | The hub's own GCP identity, through Flux `kubeConfig.configMapRef`. **No secret crosses the boundary.** | GKE |
| `token` | An RBAC-scoped ServiceAccount token forge mints in your cluster. | **Any** Kubernetes cluster |

`--auth auto` (the default) picks `gcp` for a `gke_<project>_<location>_<cluster>`
context and `token` for everything else.

**The token path is a real answer, not a degraded one.** It is how a cluster
with no cloud identity to trust — Vultr VKE, bare metal, k3s — becomes a target
at all, and **EKS and AKS clusters work through it today**. It is strictly
worse than workload identity in one specific way (a replayable bearer token
exists at rest), and that cost is bounded by the RBAC forge applies and by
`disconnect` revoking it.

There is deliberately no `aws` or `azure` mode. Flux supports both providers
and the control plane reserves the enum tags, but neither path is built or
tested — and an untested path that *looks* supported is worse than an absent
one: it gets chosen, fails inside a cloud client, and reports a problem with
your cluster.

## The one-time grant: two identities, and the half people miss

This is the part most likely to be misread. Getting it half right produces a
cluster that connects fine and refuses every apply.

1. **The connect identity** authenticates the TLS connection to your API
   server. On `gcp` that is the hub's GCP service account, which the connect
   response hands back (`HubIdentity`) because it is a fact about the platform
   you have no way to know.
2. **The impersonated identity** authorizes the apply once connected. The hub's
   kustomize-controller runs with `--default-service-account=reliant-deploy-tenant`,
   so Flux impersonates
   `system:serviceaccount:flux-<org>:reliant-deploy-tenant` on **your** cluster
   — a namespace that does not exist there and does not need to.

So **granting the hub's cloud identity read access is not sufficient.** forge
applies the in-cluster half for you: a ClusterRole/ClusterRoleBinding
(`forge-connect-<name>`) carrying the bootstrap writes the platform makes
(Namespace, ServiceAccount, Role, RoleBinding) plus permission to impersonate
exactly that one username, pinned by `resourceNames`.

The cloud half forge **cannot** do — it is an IAM write in your project, with
credentials forge does not hold — so it prints it:

```
gcloud projects add-iam-policy-binding acme \
  --member=serviceAccount:control-plane@acme.iam.gserviceaccount.com \
  --role=roles/container.clusterViewer
```

A control plane with no GCP identity (normal in dev) makes forge say so rather
than print a grant with a blank principal in it, which is how someone runs a
binding that silently grants nothing. Use `--auth token` against a dev cluster.

On the `token` path there is **no cloud grant at all**: forge applied the RBAC
and minted the credential.

## The token, and why it is a Secret rather than a TokenRequest

forge creates, server-side-applied in your cluster:

```
Namespace       forge-system
ServiceAccount  forge-system/forge-connect
Secret          forge-system/forge-connect-token   (kubernetes.io/service-account-token)
ClusterRole     forge-connect-<name>  + its ClusterRoleBinding
```

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
| `carries no certificate authority` | (token path; on gcp the CA comes from GKE's describe) forge will not connect a cluster whose API server it cannot verify, and never falls back to skipping verification. Re-run your provider's `get-credentials`. |
| `--auth gcp needs a GKE context` | Workload identity is GKE-only here. Use `--auth token`. |
| `resolve the credential's organization` | forge asks the control plane which org your credential acts for — it composes the hub namespace in the impersonated username, so the RBAC would be wrong without it (in the quiet way, authorizing a user that never appears). Authenticate (signed in to Reliant: nothing to do — `reliant forge` / an agent shell uses your session; standalone: `forge login`; CI: the env's `token_env`) and re-run; the token needs `deploy:read`. |
