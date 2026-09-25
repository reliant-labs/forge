# forge versioning: what a project pins, and what a dev build does instead

forge is **one Go module**, `github.com/reliant-labs/forge`. Generated projects
import `github.com/reliant-labs/forge/pkg/*` (serverkit, appkit, orm, observe,
testkit, ...) — those are packages inside that module, not a separate one.

This document is the canonical description of how that dependency is versioned.
The implementation lives in:

- `internal/buildinfo/buildinfo.go` — `InstallableVersion()`, the single
  predicate for "can a module proxy serve this build?"
- `internal/generator/project_pkgdep.go` — the scaffold-time decision.
- `internal/templates/project/go.mod.tmpl`, `gen-go.mod.tmpl` — what is emitted.
- `internal/cli/generate_pkg_compat.go` — the pre-codegen compatibility gate.
- `internal/cli/new.go` (`writeDevForgeGoWork`) — the dev-build source bridge.

## One version, by construction

There used to be two modules: the CLI, and a `github.com/reliant-labs/forge/pkg`
submodule holding the runtime libraries. They were released in lockstep, at the
same commit, with the same version — and keeping that true took three
hand-maintained syncs, two of which failed in production:

- the root module's `require forge/pkg` went stale, so `go install
.../cmd/forge@main` fetched an older pkg than main's code needed and forge's
  main branch was uninstallable;
- a hand-listed registry of "forge/pkg symbols the generator emits" went stale
  when `testkit.StubNotConfigured` was added, so `forge generate` rewrote a
  project tree and then failed its own validate.

Merging the modules makes both unrepresentable. The generator and the runtime it
generates calls into are now the same artifact at the same version, so there is
nothing left to keep in sync. Import paths did not change.

## What a scaffold pins

Whatever the running binary can honestly name as itself, and only when a module
proxy can serve it. That is exactly `buildinfo.InstallableVersion()`:

| How forge was built                            | What the project requires                                                                                            |
| ---------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `go install .../cmd/forge@v0.1.16` (a release) | `require github.com/reliant-labs/forge v0.1.16`                                                                      |
| `go install .../cmd/forge@main` or `@<sha>`    | the pseudo-version, verbatim — it is proxy-resolvable, and this is what a consumer in commit-pinning mode depends on |
| `go build ./cmd/forge` (local tree)            | **nothing**                                                                                                          |
| a dirty working tree                           | **nothing**                                                                                                          |

The last two rows are the important ones. A local or dirty build exists nowhere
a consumer could fetch it, so there is no version that describes it.

An EMBEDDED forge (`reliant forge …`) is judged by forge's own dependency entry,
never by the host's build stamps: a host built from a checkout that got forge
from the proxy (a go.sum hash, no `replace`) pins that forge version. Reading the
host's `vcs.*` stamps instead made every released reliant pin nothing.

A binary that can neither pin nor bridge (below) is refused by
`forge project new` before anything is written. It used to scaffold anyway, and
`go mod tidy` — with nothing requiring forge — resolved
`github.com/reliant-labs/forge/pkg/*` to the retired `forge/pkg` module, so the
project failed its first `forge generate`. `task build` and `task install`
stamp the source root for exactly this reason. forge used
to fall back to a `defaultPublishedForgePkgVersion` constant naming the last
release — which told projects to require a version that could not satisfy the
code being generated. That constant is gone, and an empty version now means
"this build needs a source bridge", never "use a default".

## The dev build's bridge

A binary installed with `task install:dev` carries `buildinfo.DevForgeRoot`: the
absolute path of the checkout it was built from, injected by ldflags from the
builder's own tree (never committed). An embedder that does not pass that ldflag
— reliant runs forge in-process — is covered by
`DiscoverDevForgeRootFromSource()`, which recovers the path the compiler baked
into forge's own source files.

With that path, a dev forge:

- **scaffolding a new project** writes a gitignored `go.work` into it that
  `use`s the forge checkout, so the project compiles against your source;
- **generating into an existing project** that resolves forge from a proxy
  refuses _before_ codegen touches the tree, and prints the `go work use`
  command to run.

A released binary never carries the stamp and never writes a `go.work`.

## The compatibility gate

`forge generate` checks, before writing anything, that the forge the project
compiles against can satisfy the code about to be generated:

- the project's forge must be **>=** the generating binary's version, since
  generated code can only call symbols that existed at the generator's version.
  A project resolving a NEWER forge is fine — that is the ordinary upgrade
  order;
- a project bridged to a local checkout always passes: it compiles against
  source, so there is no version to be behind;
- an unreleasable binary against a proxy-pinned project is refused, with the
  bridge command;
- a DIRECT require on the retired `forge/pkg` module is refused. Both
  `github.com/reliant-labs/forge` and `github.com/reliant-labs/forge/pkg`
  provide `forge/pkg/*` import paths, so requiring both makes every one of
  them ambiguous and nothing compiles. Only the project's own go.mod is read:
  forge does NOT inspect the dependency graph looking for it. It used to, and
  that failed a project's generate because one of its DEPENDENCIES had not
  bumped — a graph forge does not own, and advice the reader could not act on.
  `go.mod` resolution is sufficient: MVS selects one version of forge for the
  whole build, so the library side is coherent whatever individual pins say,
  and a genuine binary/library mismatch is an ordinary compile error that
  emit-validate-rollback already catches empirically.

This replaced a probe that compiled a throwaway program against a hand-listed
set of symbols. The inequality covers every symbol ever added, needs no registry
to keep current, and costs one `go list` instead of a `go build`.

## Releasing

`task release:forge -- vX.Y.Z` — one tag, one commit. See `docs/releasing.md`.
