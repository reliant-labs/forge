# 3. Declarative parallel-dev-stack primitives (N stacks, one per worktree)

- Status: accepted
- Date: 2026-06-27
- Deciders: project authors

> **v2 (this document).** Supersedes the v1 "instance"/"instance_index"
> design. v1 exposed a single resolved `option("instance")` plus a
> user-visible `option("instance_index")` and an `--instance` flag. Feedback
> killed all three: *"instance is too generic — supply worktree and branch
> and let the user decide which they want to use"*; *"what the fuck is
> instance index? prefer keys over indexes… it should be hidden from
> users… if it's port allocation memoize it in the forge allocate_port()
> func."* v2 exposes the RAW git facts and hides the index inside a memoized
> port allocator. The default (primary checkout) still renders
> byte-identically to a stack with no dev-stack parameterization.

## Context and Problem Statement

Developers want to run N dev stacks in parallel — one per git worktree — on
shared local infrastructure (one k3d cluster, one Postgres, one NATS, one
temporal). Each stack needs its own namespace, its own host-port block, and
its own slice of the shared infra (a per-stack DB, a per-stack NATS subject
prefix) so the worktrees don't collide.

Two forge-side gaps blocked this:

1. **No stack identity in KCL.** Nothing told KCL "this checkout is worktree
   _foo_ on branch _bar_" so it could derive a namespace suffix and a port
   offset.

2. **`forge up` and `forge deploy` rendered DIFFERENT ports.** `up` armed a
   persisted port store so `resolve_port` returned stable ports across runs;
   `deploy` re-probed the preferred ports on every render, so a port that
   drifted under `up` was re-resolved differently under `deploy`.

## Decision

Two render-time primitives, both forge-owned, both pushing facts INTO KCL
(KCL stays pure and composes deterministically). The default (primary
checkout, no parallel stack) renders **byte-identical** to before.

### Primitive 1 — raw git facts as KCL options

`internal/devstack` resolves the git facts once per command and pushes them
into KCL as options, threaded through the same `-D key=value` seam forge
already uses for `option("namespace")` / `option("image_tag")`. There is
**no resolved "instance" and no user-visible index** — forge supplies two
raw facts and the KCL author decides which to key on:

```
option("worktree")  -> str   the LINKED-worktree directory basename, or
                             ""  on the PRIMARY checkout (any branch).
option("branch")    -> str   the current git branch, sanitized DNS-safe,
                             always (reported on the primary checkout too).
```

- **`worktree` resolution:** the worktree directory basename, but ONLY for a
  LINKED git worktree (a `git worktree add`'ed checkout). The PRIMARY
  checkout on ANY branch resolves to `""`. Linked-vs-primary is detected via
  git itself — `git rev-parse --absolute-git-dir` (the per-worktree git dir)
  differs from `--git-common-dir` (the repo's shared dir) iff this is a
  linked worktree — not by sniffing `.git` file-vs-directory (which
  submodules also trip). A consumer that keys on `worktree` keeps the
  primary checkout DEFAULT on every branch, so a plain `forge up`/`deploy`
  on a feature branch is byte-identical to today.
- **`branch` resolution:** the current branch (`git rev-parse --abbrev-ref
  HEAD`), `""` on a detached HEAD or outside a repo. Reported for the
  primary checkout too, so a consumer that WANTS a stack-per-branch workflow
  can key on it — the author's choice.
- **Sanitization:** lowercased, reduced to DNS-safe `[a-z0-9-]`, collapsed
  dash runs, no leading/trailing dash, bounded to 24 chars.
- **Quoting:** both options are QUOTED KCL string literals so an all-digit
  worktree/branch name stays `str` (the same coercion fix as `image_tag`).
- **Empty fact ⇒ no `-D` arg**, so `option("worktree")` is `None`
  (KCL default `""`). The primary checkout with no branch info emits nothing.

There is deliberately **no `--instance` flag** and no override flag. If an
explicit override is ever needed it gets a specific name later — not now.

### Primitive 2 — `forge.allocate_port(base, key)` (memoized, hides the index)

A forge-resolved KCL builtin, registered alongside the existing
`resolve_port` (CGO plugin bridge in `internal/kclplugin`):

```python
port = forge.allocate_port(base, key)   # -> base + block(key)*100
```

- **`block(key)`** is a stable small integer forge assigns the FIRST time it
  sees `key` and MEMOIZES in a registry. The index is **INTERNAL to forge
  and never surfaces in KCL** — KCL only ever sees the final port.
- **`key == ""` ⇒ block 0 ⇒ returns `base` unchanged** (the byte-identical
  default), with no registry/lock touch.
- **One block PER KEY:** every `allocate_port(*, key)` call for the same key
  shares that key's block, so all of a stack's ports shift by the SAME
  offset (gateway, grpc, controller, reliant-api, browser ports …).
- **DETERMINISTIC — NO availability-stepping.** `base + block*100` must equal
  externally-fixed ports (a k3d pre-mapped host port; the host reliant's
  LISTEN port). Stepping off a busy port would break that mapping and make
  up and deploy disagree. This is the contract-port correctness fix.
- **The `key → block` registry** lives at `.forge/blocks.json` and is
  read-modify-written under a single advisory file lock
  (`.forge/blocks.lock`, `flock(2)`, blocking). BOTH `forge up` AND `forge
  deploy` resolve `allocate_port` through this same lock-guarded registry, so
  the two commands render IDENTICAL ports for a key, and the concurrent
  first-`up` of two worktrees serializes (cannot race to the same block).
  The lock auto-releases on process exit. The default key `""` takes no lock.

The engine (`devstack.AllocatePort`) is injected into the plugin via
`kclplugin.UseBlockAllocator`, armed once per command on the up/deploy path.
When no allocator is armed (e.g. `forge ci`, a read-only render),
`allocate_port` returns `base` unchanged for any key — no state written.

### `resolve_port` is KEPT

`resolve_port(name, preferred)` — the general, availability-checked
single-port primitive — stays, with its persisted store
(`.forge/ports-<env>.json`) armed by `kclplugin.UsePortStore` on BOTH `up`
and `deploy` so the two commands agree (the original up-vs-deploy fix for
floating ports). `allocate_port` is the NEW keyed/memoized primitive the
parallel dev stack uses; `resolve_port` remains for ports that may float.

### The render-context globals

`internal/devstack` arms two process-global render-context seams once per
command, before the first render (mirroring how forge threads the port store
through its ~20 fixed-signature render call sites):

- `SetActive(Options)` — every render path (entity + manifest) pushes the
  active `option("worktree")`/`option("branch")` (nil → byte-identical
  default).
- `UseBlockAllocator(fn)` — backs `allocate_port` with the lock-guarded
  registry.

## The KCL-side contract (for the consuming project)

forge pushes the facts; KCL composes them, deterministically and purely.
A consumer that wants the primary checkout to stay DEFAULT keys on
`worktree`:

```python
_key = option("worktree") or ""        # primary checkout -> "" -> default

# Names — pure string interpolation:
_sfx        = "-" + _key if _key else ""
namespace   = "control-plane-dev" + _sfx
db_name     = "controlplane" + _sfx.replace("-", "_")   # per-stack DB
nats_prefix = _key                                      # subject/account prefix
temporal_ns = _key                                      # per-stack namespace

# Ports — every host port via the one memoized primitive; no index math,
# no resolve_port in the dev KCL:
gateway_http   = forge.allocate_port(28080, _key)
gateway_grpc   = forge.allocate_port(29190, _key)
reliant_api    = forge.allocate_port(3091,  _key)
```

`base + block*100` gives each key a disjoint 100-port block; the registry
keeps that block stable across runs and identical under `up` and `deploy`.
The default key `""` ⇒ block 0 ⇒ every port is `base` unchanged ⇒ historical
names and ports, byte-identical.

**Topology:** shared k3d cluster(s); per-key NAMESPACE + host-port block.
Pre-map a host-port RANGE at cluster-create (e.g. `28080..28080+N*100`) so a
new stack needs no cluster recreate. The launcher (Taskfile/bootstrap) sets
the host reliant LISTEN port = the SAME `allocate_port(3091, _key)` value.

## Consequences

- **Default path unchanged.** Primary checkout, no worktree ⇒ `option`s
  emit nothing, `allocate_port(base, "") == base`, historical port-store
  path, no block registry, no lock. Byte-identical renders.
- **`up` and `deploy` agree by construction.** Both resolve `allocate_port`
  through the one lock-guarded block registry and `resolve_port` through the
  one store — no flag to keep in sync (v1's `--instance` had to match on
  both commands; v2 derives the key from the same git facts on both).
- **Files forge now owns** (all under `.forge/`, gitignored, machine-local):
  `blocks.json` (key→block registry), `blocks.lock` (allocation lock),
  `ports-<env>.json` (resolve_port store).
- General win for ALL forge users: `up` and `deploy` can no longer disagree
  on a port.
