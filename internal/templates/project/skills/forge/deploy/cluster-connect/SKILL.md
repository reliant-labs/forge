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

## A binding is one env on one cluster, with its own identity and a derived grant

`connect` takes `--env`, and that env's binding on this cluster gets **its own
identity** — nothing is shared across envs or clusters:

| Auth | The binding's identity |
|---|---|
| `gcp` | A Google service account the control plane allocates **for this binding** (the connect response returns it). GKE RBAC accepts it as a `User` subject by email. |
| `token` | A ServiceAccount forge mints for this binding (`forge-system/forge-connect-<cluster>-<env>`), and its token. |

The apply runs **as that identity**: no impersonation and no bootstrap writes.
The only objects connect writes in your cluster are the grant itself (plus the
token identity's ServiceAccount and Secret, and the delivery grant if asked).
If the control plane returns no per-binding identity, forge binds nothing and
says so — it never falls back to a shared one.

### You choose where; the render chooses what

`--apply-scope` is the owner's decision. The rules inside it are derived from
what the env's render applies **on this cluster**: one rule per (apiGroup,
resource), nothing else. A release that adds a kind is **Forbidden by your
cluster's own access check** until you re-run connect — a tripwire, not a policy
engine.

| `--apply-scope` | Grant |
|---|---|
| `namespace:<ns>[,<ns>…]` (the default, over the namespaces the render targets) | A **Role + RoleBinding in each namespace**. A rendered `Namespace` object is a name-pinned ClusterRole (`get`, `patch`, `update`; never `create` or `delete`), a RoleBinding to a ClusterRole needs `bind` on exactly that ClusterRole. **Refused** when the render has cluster-scoped kinds (CRDs, ClusterRoles…) or objects outside the list. |
| `cluster` | A **ClusterRole + ClusterRoleBinding** derived from the render, plus `bind`/`escalate` on RBAC when the render carries RBAC kinds. **This is cluster-admin equivalent**: the identity can grant itself anything. forge prints that every time. |

Every rule set also holds `get/list/watch` on the ReplicaSets and Pods of any
workload kind in the render, which Flux's health check reads.

### Review before you widen: `--dry-run`

```bash
forge cluster connect prod-us --context ... --env prod --apply-scope cluster --dry-run
forge cluster connect prod-us --context ... --env prod --dry-run --live-from live.yaml   # offline
```

`--dry-run` contacts no control plane and writes nothing. It renders the env,
reads the binding's live RBAC (read-only, through `--context`, or from a capture
with `--live-from`; the selector to capture with is printed), and prints
`+` new, `~` changed, `-` removed (a re-run prunes it), `=` unchanged — one line
per rule — then **the permissions this run newly grants**, then the exact
objects it would apply. On gcp pass `--binding-gsa` to review the real subject.

A re-run prunes this binding's grant objects the new plan no longer names, so
narrowing the scope narrows the cluster. It also removes the legacy
pre-binding grant (the shared identity with impersonate and bootstrap writes).

### The cloud half (gcp)

forge cannot run an IAM write in your project, so it prints it:

```
gcloud projects add-iam-policy-binding acme \
  --member=serviceAccount:<this binding's GSA> \
  --role=roles/container.clusterViewer
```

On the `token` path there is **no cloud grant at all**.

## The token, and why it is a Secret rather than a TokenRequest

forge creates, server-side-applied in your cluster, per binding:

```
Namespace       forge-system                          (shared, left in place)
ServiceAccount  forge-system/forge-connect-<cluster>-<env>
Secret          forge-system/forge-connect-<cluster>-<env>-token   (kubernetes.io/service-account-token)
```

The token is **sent write-only and never printed** — not in the summary, not on
failure, not in `--dry-run`. A long-lived Secret token, not a bounded
`TokenRequest`: nothing rotates a bounded token, so a cluster nobody re-connects
for 90 days would stop deploying. It is revocable by deleting it (disconnect
does), and its blast radius is the grant above.

## Secret delivery: opt-in, named, least privilege

Some Secrets are not deployed with a bundle but supplied and renewed by the
control plane (a `forge.ControlPlaneSecret`). Connect grants write access to
exactly those and nothing else, only when you name them:

```bash
forge cluster connect prod-us --context ... --env prod \
  --delivery-secret control-plane-prod/reliant-otel-auth \
  [--delivery-subject user:delivery@acme.iam.gserviceaccount.com]
```

- Per declared namespace, a **Role** (never a ClusterRole, whose `resourceNames`
  would apply in every namespace) allows `get/update/patch/delete` pinned to the
  named Secrets, plus `create`.
- RBAC cannot pin `create` to a name, so a **ValidatingAdmissionPolicy**
  (fail-closed, matched by exact username) admits writes from the delivery
  identities only for the declared `namespace/name` pairs, and only as type
  `Opaque` — otherwise a `service-account-token` Secret would mint a credential.
- `--delivery-subject` adds a second, dedicated identity beside the connect one.
- The policy is applied in its own step before the Roles, so a cluster without
  ValidatingAdmissionPolicy (Kubernetes < 1.30) fails before the create grant lands.
- Re-running connect re-converges it and **only adds**: a narrower re-run
  replaces the policy at once but leaves old delivery Roles for you to prune.
  `disconnect` removes all of it. `--dry-run` prints every object.

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
forge deletes everything it created for this cluster, **found by label** (every
binding's Roles, ClusterRoles and bindings, each token ServiceAccount and
Secret, and the delivery policy). A token revoked server-side is already
powerless, so a cleanup that then fails leaves litter rather than a live
credential.

**Refused while any live environment targets the cluster** — that check is the
control plane's, which is why this is not a local delete.

A missing object is not an error, so a disconnect run twice converges. The
`forge-system` **namespace is left in place**: it is shared by construction and
deleting a namespace deletes whatever else is in it.

Without `--context`, forge deregisters the cluster and tells you which objects
to remove by hand.

## Checking what you would send

See "Review before you widen" above: `--dry-run` prints the RBAC diff and every
object that would be applied.

## Refusals you may hit

| Message | Why |
|---|---|
| `whose host is a loopback or bind address` | The hub dials from a **pod**, where `127.0.0.1` is that pod. The deploy would fail as "connection refused" against a healthy cluster. Connect by an address reachable from outside the cluster. |
| `https only` | Any other scheme carries the credential in clear text. |
| `carries no certificate authority` | (token path; on gcp the CA comes from GKE's describe) forge will not connect a cluster whose API server it cannot verify, and never falls back to skipping verification. Re-run your provider's `get-credentials`. |
| `the env's render does not fit namespace scope` | The render has cluster-scoped kinds or objects outside your namespaces. Widen the list, or choose `--apply-scope cluster` knowing it is cluster-admin equivalent. |
| `applies nothing to this cluster` | The env's KCL does not bind this context. Set `ClusterTarget.cluster` / `connected_cluster` first. |
| `--auth gcp needs a GKE context` | Workload identity is GKE-only here. Use `--auth token`. |
| `resolve the credential's organization` | forge checks the credential against the control plane before it writes anything to your cluster. Authenticate (signed in to Reliant: nothing to do — `reliant forge` / an agent shell uses your session; standalone: `forge login`; CI: the env's `token_env`) and re-run; the token needs `deploy:read`. |

A Secret the control plane writes and renews INTO a connected cluster (the OTLP ingest token) is declared with `forge.ControlPlaneSecret`: `deploy/control-plane-secret`.
