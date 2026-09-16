# ADR 0002: Declare the forge KCL module as an OCI dependency

**Status:** proposed
**Supersedes:** [ADR 0001: Always vendor the forge KCL module](0001-always-vendor-forge-kcl.md)

## Context

ADR 0001 made the vendored `.forge-kcl/` copy the single supported mechanism,
on every build of forge. That decision was correct and should not be read as a
mistake: it replaced a published-git-tag dependency (`kcl-v0.1.0`) that was
never pushed, which made every project a released forge scaffolded unresolvable
from birth. Its sharpest line is the one this ADR is designed against:

> A string assertion cannot distinguish a dependency that resolves from one
> that does not, which is precisely why this shipped.

What ADR 0001 did not anticipate is the cost of the vendored copy being a
**generated artifact that is also committed**. `forge generate` rewrites
`.forge-kcl/` from the running binary's embedded copy every run, and the only
record of which forge produced the copy on disk is a version string in
`.forge-kcl/.forge-version`. A version cannot express lineage, and three
incidents followed from that:

1. `.forge-kcl/` carried forge#202's namespace-scoped operator RBAC under a
   `v0.1.15` stamp — a release without that fix. `checkDowngrade` allows an
   equal-version overwrite, so a released `v0.1.15` binary would have silently
   reverted it, restoring a ClusterRole collision. `cut-release.yml` re-runs
   generate with exactly that pinned version, so it was reachable from CI.

2. A workspace build of main reported `v0.1.15+dev`, which semver compares
   EQUAL to a released `v0.1.15` — the same overwrite, from the other
   direction. Fixed separately by deriving a truthful version, but the guard
   only had a version to reason about in the first place.

3. control-plane's deploy branch vendors deploy-tier schemas
   (`SimpleBackend`, `StaticSite`) produced by an unmerged forge branch. A
   main-built `forge generate` deleted ~880 lines of them as a routine
   forge-owned-file write, and the failure surfaced later as an unknown-schema
   error in `env render` — a different command, with no mention of the cause.

These are one bug wearing three hats. `.forge-kcl/` is a **generated
artifact**, so whatever binary runs next rewrites it. A **declared
dependency** cannot be rewritten by a binary that merely happens to run.
That distinction, not the transport, is the thing to fix.

Two facts make the change smaller than it looks:

- forge's KCL option plumbing already resolves deps through
  `kpm.ResolveDepsIntoMap` and already handles registry deps generically
  (`internal/kcloptions/kcloptions.go` resolves non-absolute dep paths against
  the kpm home). **The render path needs no changes to consume an OCI dep.**
- forge already ensures a standalone OCI registry exists for local k3d
  clusters, deliberately not owned by any cluster so it survives
  `k3d cluster delete` (`internal/cli/cluster_registry.go`).

## Decision

**A project's `deploy/kcl/kcl.mod` declares the forge KCL module as an OCI
dependency pinned to a version. Nothing is materialized into the project, and
`.forge-kcl/` is deleted.**

`forge generate` stops writing the module and stops owning a copy of it. The
version it declares is the running forge's own version — the same value
`forge.yaml`'s `forge_version` and the `go.mod` require already carry, so the
CLI, the Go library and the KCL schemas move as one artifact.

**One mechanism on every build — ADR 0001's central rule is kept.** There is
no dev-vs-release branch, because the release-only path going unexercised by
maintainers is exactly what shipped `kcl-v0.1.0`. Instead there is one
mechanism (an OCI dep) against two registries:

- released forge resolves from the shared registry;
- a dev build publishes its embedded module to the standalone local registry
  forge already manages, and declares that. Contributors therefore exercise
  the same resolution code path as consumers, every run.

**Publishing is atomic with the release and verified by resolution.**
`task release:forge` pushes the KCL artifact and then proves a COLD resolve of
the published reference before the git tag is pushed — asserting that the
dependency resolves, never that kcl.mod contains the expected string. This is
the direct lesson of ADR 0001, and it is the step whose absence burned
`kcl-v0.1.0`. It mirrors the immutable-version proxy gate the same script
already runs for the Go module.

**Offline render is preserved through kpm's own cache, not a forge copy.**
kpm caches resolved packages in the kpm home, and supports a vendor mode for
committing them. The existing offline guarantee is re-expressed as "resolves
with no network from a warm kpm cache" rather than "resolves from a directory
forge wrote".

## Consequences

Deleted: `internal/kclvendor` and its stamp, `checkDowngrade`, the
`--allow-kcl-downgrade` flag, `.forge-kcl/` from every project, and the
provenance guesswork that motivated stamping content hashes. None of it has
anything left to protect once the module is a declared dependency.

Gained: feature PRs stop carrying hundreds of lines of re-vendored KCL diff;
a rebase stops conflicting on generated KCL; and a project pinned to forge
vX.Y.Z gets vX.Y.Z's schemas by declaration rather than by whichever binary
last ran generate.

Costs, accepted:

- A cold CI run needs registry network and credentials to resolve the KCL
  module, where before it needed neither. CI already authenticates to the
  registry for images.
- Publishing becomes a release-blocking step. That is deliberate: ADR 0001's
  failure was a publish step that did not exist, and the mitigation is that
  the release script refuses to tag until a cold resolve succeeds.
- An unreleased forge cannot hand its schemas to a project without pushing
  them somewhere. For contributors that is the local registry; for a spike
  branch like `forge-deploy` it means publishing a prerelease tag rather than
  relying on a vendored copy in a consumer repo (which is how that spike's
  only second copy came to live in control-plane).

## Open

The registry path is not decided here. The obvious candidate is the existing
Artifact Registry already used for images
(`us-central1-docker.pkg.dev/reliant-labs-475814/...`), under a `kcl/` prefix,
but that is an infrastructure call rather than a design one — as is whether
local dev publishes to the standalone k3d registry or a pull-through.

The exact kcl.mod OCI dependency spelling is likewise deliberately not
recorded here. kpm models it as `Source.Oci{Reg, Repo, Tag}`; the TOML form
must be pinned by a test that RESOLVES the dependency, not by one that
compares the written string — see ADR 0001.
