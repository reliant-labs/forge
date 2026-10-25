---
name: deploy/openbao
description: Running a self-run secret store with forge.OpenBao — what the component renders, how its clients authenticate (JWT only), the ordering of an ADOPTION of a live store (ship every consumer's JWT flip in the same release), the HA node spread, and the failure windows each step costs. Use when you declare Bundle.openbao, when you replace a hand-run OpenBao with the component, when you plan a rollout that moves a store's clients, or when two store pods land on one node.
---

# `forge.OpenBao`: the store and its clients

```python
openbao = forge.OpenBao {}      # Raft x3, TLS, static seal, audit on, JWT auth
```

One component per cluster. It renders the server (a StatefulSet for Raft, a
Deployment for Postgres storage), its Services, certificate and CA ConfigMap, a
PodDisruptionBudget, a pre-rollout `widen` Job and a post-rollout `converge` Job.
Bootstrap values (seal key, database password, `operator init` output) are
`deploy/openbao-bootstrap`. Publishing it to other clusters is
`deploy/openbao-public`. This skill is the part that decides whether a rollout
hurts.

## Clients authenticate with JWT, and only JWT

The component converges one JWT auth mount per cluster (`jwt-<cluster id>`) and a
login role per principal: `consumer-<name>` for each entry of `consumers`, the
converge role, the writer, and one per environment a control plane declares. A
client logs in with a projected service-account token whose audience is the
store's (`audience` in the access document), verified against the cluster's
issuer.

It does **not** render Kubernetes auth or AppRole. The server pods mount no
service-account token, and the `system:auth-delegator` binding a Kubernetes-auth
login needs is not rendered.

## Adopting a live store: ship the clients' JWT flips in the SAME release

Replacing a hand-run OpenBao with the component keeps its name, namespace and
storage. What changes is how clients get in. **At the cutover every client still
on Kubernetes or AppRole auth loses access**:

- a token it already holds works until its TTL (about 15 minutes), then every new
  login is refused (`403`);
- a client with no cached token is locked out at once.

And every release restarts every workload (`service.version` rides the pod
template). So a flip shipped in a LATER release costs another full roll, and the
client stays down for the whole gap between the two. The SC7 prod adoption did it
one flip per release: the controller's tenant reads failed for about 14 minutes
and the identity reconciler's sweeps for about 27.

**The rule: one release carries the adoption AND every consumer's flip.**

Why that is safe, by construction: the secret ordering is

```
CRDs -> ESO -> OpenBao (widen, rollout, converge) -> stores -> ExternalSecrets -> workloads
```

The component's `widen` and `converge` Jobs run, and are awaited, BEFORE any
workload of the release is applied. So when a flipped client's new pod starts, its
JWT mount and its role already exist. Nothing has to be triggered by hand.

For each consumer, in the same release:

1. Declare it in `openbao.consumers` with exact grants (its role is created by the
   `converge` Job).
2. Point its login at the JWT mount and role, with the store's audience.
3. Mount the audience-bound projected token and the CA ConfigMap
   (`tls.ca_configmap`, published into the namespace by the converge Job) into its
   pod, and make the client read the token file on every login.
4. Remove the old Kubernetes-auth or AppRole configuration, so the client cannot
   fall back to a path that no longer exists.

A client you cannot flip in this release stays down until you do. Say which ones
before you start, and decide whether that is acceptable.

### The first converge needs a break-glass token

The self mount and the `openbao-converge` role do not exist in a store that was
never converged by the component, so the first converge cannot log in by JWT. It
needs one root token minted from the recovery keys
(`OPENBAO_BREAK_GLASS_TOKEN`), supplied for that run; the script revokes it at the
end. After it, JWT works and any credential only the old setup used can go.

### What forge tells you

`forge env deploy <env>` raises an advisory before it applies anything (the
`store_adoption` preflight finding, never blocking) when the component is about to
replace a Deployment or StatefulSet of the same name that forge did not install
(no `forge.dev/component-owner` label). It repeats the rule above. A store forge
already owns, and a `--target` deploy that does not install the store, do not
raise it.

### Other leftovers

The previous store's PodDisruptionBudget, ingress NetworkPolicy, RBAC and
auth-delegator ClusterRoleBinding are pruned by the owned-object prune once the
rollout is healthy, if forge stamped them (`deploy/plan`). Objects applied before
the stamp existed carry no label: label them once, or delete them by hand. Two
PodDisruptionBudgets over one set of pods block every node drain, so check.

### Measured cost of each step (rehearsal)

| Step | Cost |
|---|---|
| store gains a plaintext transitional port, then clients move to it (two deploys) | 0 failed requests |
| clients moved to the new port in the SAME deploy that adds it | 95 s, every client |
| the adoption itself | about 6 s per client (a standby's `307` to the TLS address) |
| Kubernetes-auth clients, until flipped | every new login `403` |
| a flip in the adoption release | 0 |
| a flip in a later release | the whole gap |

## HA: replicas never share a node

With `replicas >= 2` the component renders a hard topology spread
(`kubernetes.io/hostname`, `maxSkew: 1`, `whenUnsatisfiable: DoNotSchedule`), so a
node loss cannot take a Raft quorum, or the active node and its standby, with it.
A single-node cluster still schedules (one domain has no skew). Zone spread
(`topology.kubernetes.io/zone`) is best effort (`ScheduleAnyway`). A preference
was not enough: both prod pods landed on one node.

Relax it only with a reason, which warns on every render:

```python
openbao = forge.OpenBao {
    ha = forge.OpenBaoHA {
        node_spread = "preferred"
        node_spread_reason = "two-node staging pool; replicas share a node during upgrades"
    }
}
```

`node_spread_reason` without `"preferred"`, and `"preferred"` without a reason, are
both refused. A cluster with fewer schedulable nodes than `replicas` leaves the
surplus pods Pending under the default: size the pool, or relax it on purpose.

## Converge retries

The converge Job retries a connection-level failure (no HTTP answer: a node drain,
a pod restart behind the Service, a proxy hiccup) with bounded backoff, and fails
fast on a real API error, quoting the store's actual answer. It never replays
`sys/init` or a mount enable after an ambiguous failure. A refused credential is a
verdict (exit 64); an unreachable store is not, and is retried by the Job.
