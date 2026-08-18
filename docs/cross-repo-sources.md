# Cross-repo sources: building a component whose code lives in another repo

Some components do not live in the repository that declares them. The
clearest case is a frontend: control-plane declares `reliant-web`, the
primary customer-facing SPA, but the code is in the `reliant` repository's
`web/` directory.

Until this existed, the only way to say that was a filesystem path:

```yaml
frontends:
  - name: reliant-web
    type: vite-spa
    path: ../reliant/web     # works on a laptop, absent in CI
```

This document describes what replaced it, and why the replacement is a
pinned source rather than a checkout step in CI.

The implementation lives in:

- `internal/gitsource/` — the resolver, the cache, the overrides file
- `internal/config/config.go` — the `forge.yaml` schema (`GitSource`)
- `kcl/schema.k` — the KCL schema (`forge.GitSource`)
- `internal/cli/frontend_source.go` — the build/deploy resolution seam

## The problem is reproducibility, not just CI

The obvious failure is that `../reliant/web` does not exist in CI, where
`actions/checkout` clones one repository, so the build dies:

```
frontends[name=reliant-web] (type=vite-spa) declared in forge.yaml but path
"../reliant/web" does not exist (expected at /tmp/.../reliant/web)
```

The less obvious failure is the one that matters more. Where the sibling
checkout *is* present, the build succeeds — and ships whatever happened to
be checked out in that directory. There is no pin, so two machines with
different `../reliant` states produce different artifacts from identical
commits. A deploy is not reproducible, and nothing reports that.

The asymmetry states it best. control-plane's **Go** dependency on reliant
is a pinned, proxy-resolved module:

```
require github.com/reliant-labs/reliant v1.6.2
```

Its **frontend** dependency on the same repository was an unpinned
filesystem path. Same dependency, same repo, two entirely different levels
of rigor.

## Why not just check out the sibling repo in CI

That workaround exists — a second `actions/checkout` with `repository:` and
`path: ../reliant` — and it does make the build pass. It is not enough for
two reasons.

It is imperative glue that duplicates the declaration. The KCL already says
this project depends on reliant; the workflow file says it again, in a
different language, and the two drift silently. Worse, neither of them
agrees with the pinned Go version, so a project can ship a frontend built
from `main` against a backend pinned to `v1.6.2` with nothing flagging it.

And it re-introduces the thing forge's philosophy targets directly: state
of the world that must be set up, in the right order, before the
declarative layer runs. A step that must be *run* is a step that can be
skipped, half-completed, or lost when a teammate clones the repo.

## The declaration

```python
forge.Frontend {
    name = "reliant-web"
    type = "vite"
    source = forge.GitSource {
        repo = "github.com/reliant-labs/reliant"
        ref = "v1.6.3"          # tag, branch, or commit sha
        subdir = "web"
    }
    deploy = forge.FirebaseHosting { site = "reliant-prod", ... }
}
```

and the `forge.yaml` equivalent:

```yaml
frontends:
  - name: reliant-web
    type: vite-spa
    source:
      repo: github.com/reliant-labs/reliant
      ref: v1.6.3
      subdir: web
```

`repo` accepts the canonical `host/owner/name` shorthand, which forge
expands to an https clone URL, or an explicit `https://` / `ssh://` /
`git@host:owner/name` URL used verbatim — a private host must not be forced
through a spelling forge invented.

`ref` is **required**. forge does not default to a repository's default
branch, because an unpinned cross-repo dependency is exactly the problem
this feature solves.

`path` and `source` are mutually exclusive. With both set there are two
answers to "where is this frontend's code", and whichever forge silently
preferred, the other would be a lie that reads as truth in review.

## The cache

A fetch lands in a machine-local cache under
`<UserCacheDir>/forge/sources/<repo-slug>-<digest>`, keyed by repo **and**
ref. A second build of the same pin does no network work at all.

Two properties of the key are deliberate:

- **Ref is in the key**, so two pins of one repository coexist rather than
  fighting over one directory. Bumping a ref in one environment cannot
  silently change another.
- **Subdir is not**, so a project consuming two directories of one
  repository fetches the repository once and takes two views into it.

A materialized entry records what produced it in `.forge-source.json`,
written last. Its presence is what makes a directory a cache hit, so a
fetch interrupted halfway leaves nothing a later build mistakes for a
complete checkout.

Fetches are shallow (`--depth 1`) for one ref rather than full clones,
with an automatic unshallowed retry when the server refuses — fetching a
bare commit sha requires `uploadpack.allowReachableSHA1InWant`, which many
hosts do not enable, and a pin by sha is the most reproducible thing a user
can write. It must not be the one that fails.

### Refs that move

A commit sha, or a tag under a project that does not move tags, is
immutable: the cache can never be stale.

A **branch** ref is cached the same way — forge does not re-fetch a branch
on every build. That is intentional, and it is the same reasoning as
everything above: a dependency that silently changes underneath a build is
the non-reproducibility this feature exists to remove. To move, bump the
ref, or drop the cache entry.

## Local iteration: `.forge/source-overrides.yaml`

Pinning without an escape hatch would force every one-line frontend edit
through a commit, a push, a tag and a re-fetch. That is how a feature gets
resented rather than adopted, so a source can be overridden to a local
working copy:

```yaml
# .forge/source-overrides.yaml
sources:
  github.com/reliant-labs/reliant: ../reliant
```

Paths are relative to the project root (or absolute). The map is keyed by
**repo**, not by component, so one entry covers every component sourced
from that repository — and it survives a ref bump, which a repo+ref key
would not.

Three properties make this safe:

- **It is explicit.** forge never auto-adopts a sibling checkout it happens
  to find on disk. Silent adoption would reintroduce the unpinned build
  invisibly, on the one machine — a maintainer's laptop — least likely to
  notice.
- **It cannot be committed.** `.forge/*` is gitignored, so an override
  cannot follow a change into CI. The pin is what builds there, always.
- **It is reported.** Every overridden resolution prints
  `(local override — NOT the pinned ref)`. A build that quietly stopped
  honoring its pin is the exact failure this feature removes.

A stale override — one pointing at a directory that no longer exists — is a
hard error naming the file, not a silent fall back to the pin. Falling back
would build something other than what the developer believes they are
building.

## What is covered today

**Frontends**, end to end: schema (`forge.yaml` + KCL), resolver, cache,
overrides, and the build and deploy paths.

**Not yet covered** — both are noted as follow-ups on the tracking issue:

- `External.cwd`, whose schema still documents "a `cwd` that doesn't exist
  on disk is a HARD build failure".
- `forge.yaml docker.build_contexts`, still a map to local directories.

Both are the same resolver applied at a different call site, but each has
its own consumers to thread and its own tests to write. They were left out
rather than half-done.

## Compatibility

This is purely additive. A project that declares no `source:` anywhere
constructs no resolver, touches no cache, and renders byte-identically —
the resolution pass returns immediately when nothing needs it. The KCL
render emits `source: null` for a path-declared frontend and the Go struct
omits the field entirely when absent.
