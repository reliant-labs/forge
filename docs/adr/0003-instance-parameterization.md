# 3. Declarative instance-parameterization (N parallel dev stacks)

- Status: accepted
- Date: 2026-06-27
- Deciders: project authors

## Context and Problem Statement

Developers want to run N dev stacks in parallel — one per git worktree — on
shared local infrastructure (one k3d cluster, one Postgres, one NATS, one
temporal). Each stack needs its own namespace, its own host-port block, and
its own slice of the shared infra (a per-instance DB, a per-instance NATS
subject prefix) so the worktrees don't collide.

Two forge-side gaps blocked this:

1. **No instance identity.** Nothing told KCL "this stack is worktree _foo_,
   index 2" so it could derive a namespace suffix and a port offset.

2. **`forge up` and `forge deploy` rendered DIFFERENT ports.** `up` armed a
   persisted port store (`kclplugin.UsePortStore` → `.forge/ports-<env>.json`)
   so `resolve_port` returned stable ports across runs. `deploy` did **not** —
   it re-probed the preferred ports on every render, so a port that drifted
   under `up` (because its preferred was busy) was re-resolved differently
   under `deploy`. control-plane worked around this by hand-pinning
   `reliant-api` to 3091. With per-instance port blocks the drift would
   multiply.

## Decision

Two render-time primitives, both forge-owned, both pushing identity INTO KCL
(KCL stays pure and composes deterministically). The default (no instance)
renders **byte-identical** to before.

### Primitive 1 — instance identity → KCL options

`internal/instance` resolves an instance identity once per command and pushes
it into KCL as options, threaded through the same `-D key=value` seam forge
already uses for `option("namespace")` / `option("image_tag")`:

```
option("instance")        -> str  (the sanitized name; ""  for the default)
option("instance_index")  -> int  (a stable small int;   0   for the default)
```

- **Resolution order:** `--instance=<name>` flag → else the git **worktree**
  directory basename (only for a _linked_ worktree, not the primary checkout)
  → else the current git **branch** → else `""` (the default stack).
- **Sanitization:** lowercased, reduced to DNS-safe `[a-z0-9-]`, collapsed
  dash runs, no leading/trailing dash, bounded to 24 chars. An all-symbol
  input that sanitizes to `""` falls back to the default.
- **Index registry** `.forge/instances.json` `{name: index}`: the next free
  index (≥ 1; 0 is reserved for the default so a named worktree never displaces
  the default's block) is assigned on first use and persisted, stable
  thereafter. The default needs no registry entry and takes no lock.
- **Quoting:** `instance` is a QUOTED KCL string literal so an all-digit name
  stays `str` (the same coercion fix as `image_tag`); `instance_index` is a
  bare int.
- **No instance ⇒ no `-D` args**, so `option("instance")` is `None`
  (KCL default `""`) and `option("instance_index")` is `None` (default `0`).

### Primitive 2 — the port store is the SINGLE SOURCE OF TRUTH

`kclplugin.UsePortStore(path)` is now called on **both** the `up` AND the
`deploy` render path (via the shared `cli.activateInstance` helper), with the
**same** instance-scoped path. Consequences:

- `resolve_port(role, preferred)` allocates a port ONCE (availability-checked),
  persists it, and every subsequent render — `up` OR `deploy` — just **reads**
  it back. A given `(env, instance, role)` resolves the **identical** port
  under both commands, forever. **The up-vs-deploy drift is dead; the
  reliant-api hand-pin can be removed.**
- The store is **instance-scoped**: `.forge/ports-<env>[-<instance>].json`.
  The default instance keeps the historical `.forge/ports-<env>.json` path
  (byte-identical dev loop); a named instance gets its own file, so its port
  block never collides with another worktree's.

### Global lock primitive

`internal/instance` guards the index registry **and** the per-instance port
store allocation under a single advisory file lock
(`.forge/instances.lock`, `flock(2)`, blocking). The concurrent first-`up` of
two worktrees serializes through it, so they cannot race to the same index or
the same port block. The lock auto-releases on process exit (no stale-lock
cleanup, unlike an `O_EXCL` marker file). One lock, not two, so the ordering
can never deadlock. The default instance takes no lock.

## The KCL-side contract (for the consuming project)

forge pushes the options; KCL composes them, deterministically and purely:

```python
_sfx = "-" + option("instance") if option("instance") else ""

# Names — pure string interpolation:
namespace      = "control-plane-dev" + _sfx
db_name        = "controlplane" + _sfx.replace("-", "_")   # per-instance DB
nats_prefix    = option("instance")                         # subject/account prefix
temporal_ns    = option("instance")                         # per-instance namespace

# Ports — derive the per-instance PREFERRED port from instance_index, then
# hand it to resolve_port (which stabilizes it in the instance-scoped store):
_idx           = option("instance_index")
gateway_http   = forge.resolve_port("gateway-http", 28080 + _idx * 100)
gateway_grpc   = forge.resolve_port("gateway-grpc", 29190 + _idx * 100)
reliant_api    = forge.resolve_port("reliant-api",  3091  + _idx * 100)
```

`base + index*100` gives each instance a disjoint 100-port block; the
instance-scoped store keeps that block stable across runs and identical under
`up` and `deploy`. Pure `base + index*100` math (no `resolve_port`) is also
valid when the ports never need to dodge a busy port.

**Topology:** shared k3d cluster(s); per-instance NAMESPACE + host-port block.
Pre-map a host-port RANGE at cluster-create (e.g. `28080..28080+N*100`) so a
new instance needs no cluster recreate.

## Consequences

- **Default path unchanged.** No `--instance`, plain single-checkout repo ⇒ no
  options emitted, historical port-store path, no registry, no lock. Byte-
  identical renders.
- **`forge up --instance=foo` and `forge deploy --instance=foo` must match.**
  They share the store + the options, so they render identically — but the
  caller must pass the same `--instance` (or rely on the same worktree/branch
  auto-resolution) for both.
- **Files forge now owns** (all under `.forge/`, gitignored, machine-local):
  `instances.json` (index registry), `instances.lock` (global lock),
  `ports-<env>[-<instance>].json` (per-instance port store).
- General win for ALL forge users, instance or not: `up` and `deploy` can no
  longer disagree on a port.
