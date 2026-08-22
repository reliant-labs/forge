# ADR 0001: Always vendor the forge KCL module

**Status:** accepted

## Context

A generated project's `deploy/kcl/kcl.mod` depends on the `forge` KCL module —
the typed schemas and render layer its per-env `main.k` files import. Nothing
deploys, and no manifest renders, unless that dependency resolves.

Forge resolved it two different ways depending on how forge itself was built:

- **Dev builds** materialized the module embedded in the binary into
  `<project>/.forge-kcl/` and rewrote the dependency to a relative path.
- **Release builds** scaffolded a published git tag,
  `forge = { git = "…/forge.git", tag = "kcl-v0.1.0" }`, and actively
  _un-vendored_ — deleting `.forge-kcl/` and rewriting the dependency back to
  that tag — whenever they encountered a project a dev build had vendored.

`kcl-v0.1.0` was never published. `git ls-remote --tags origin` returns no
`kcl-v*` tag at all. Two consequences, both reproduced against a
release-stamped binary:

1. Every project scaffolded by a released forge was unresolvable from birth.
   `forge ci validate-kcl` on a fresh scaffold fails with
   `error: pathspec 'kcl-v0.1.0' did not match any file(s) known to git`.
2. Upgrading forge broke working projects. A release-build `forge generate`
   over a project with a working `.forge-kcl/` rewrote the dependency to the
   dead tag, and the project stopped rendering.

Maintainers never saw either one, because a dev build takes the vendoring path
and a dev build is what maintainers run.

Every test that existed asserted the dependency _string_ — that kcl.mod carried
the expected `tag = "kcl-v0.1.0"` line. All of them passed. The line was
written exactly as intended; the tag simply did not exist. A string assertion
cannot distinguish a dependency that resolves from one that does not, which is
precisely why this shipped.

## Decision

**The vendored `.forge-kcl/` copy is the single supported mechanism, on every
build of forge.** Materializing the module and pointing kcl.mod at it are
unconditional. The un-vendor direction, the published-tag constants, and the
release-vs-dev branch are deleted rather than fixed.

The scaffold template emits the relative path directly, so a project is
resolvable the instant it is written rather than depending on a later patch.

## Rationale

**One mechanism, not two.** Forge's philosophy is "primitives, not modes," and
warns that an enum of blessed configurations "leaves dead branches in every
project that picked one." The dev/release split was exactly that, and the
branch nobody ran was the broken one.

**It deletes a failure class rather than a failure.** Publishing the tag would
have fixed the immediate symptom and left the shape intact: two paths, one
exercised far less than the other, and a release step that must be remembered
every time. That step had already been forgotten once, silently. Gone with the
branch: `unvendorForgeKCL`, `RestorePublishedDep`, the published-tag constants,
and the "release build breaks a working project" path.

**It works offline.** A git-tag dependency needs network _and_ git credentials
at render time — in CI, in a container, on an air-gapped machine, at deploy
time. The vendored copy needs neither. `kclvendor`'s own doc comment already
argued the vendored copy resolves identically across "containers, CI checkouts,
and other machines"; that argument was always the stronger one.

**The cost is small and already paid.** The module is 16 small `.k` files, and
projects commit `.forge-kcl/` regardless.

## The counter-argument, and what we did about it

The strongest case against vendoring is **staleness**: a vendored module
refreshes only when `forge generate` runs, so a project can sit on a copy an
older forge wrote. A git tag has no such drift — the pin in kcl.mod names the
version, visibly, in a file people read.

This is a real cost and we did not wave it away. The mitigation is to make the
drift _legible_, which is the property the tag actually provided:

- `Materialize` writes `.forge-kcl/.forge-version` recording the forge version
  that produced the copy. The pin is still visible in the tree; it just lives
  beside the module instead of in kcl.mod.
- `kclvendor.Stale` compares that stamp against the running binary, and
  `kclrender.Run` — the single seam through which forge evaluates any KCL —
  warns once per process on a mismatch, naming `forge generate` as the fix.

Without the stamp, the symptom of drift is a KCL schema error naming a field,
which points at the user's code rather than at the stale module. That was the
genuine risk, and it is what the stamp closes.

Note that the tag's advantage here was narrower than it looks: an _unpublished_
tag has no version-legibility benefit at all, and even a published one only
updates when someone edits kcl.mod — which is the same "only when a human acts"
property being held against vendoring.

## Alternatives considered

**Publish the `kcl-v0.1.0` tag.** Smallest diff, and it makes the released
scaffold resolve. Rejected: it keeps two mechanisms and keeps the un-vendor
path, so a release build still deletes a working `.forge-kcl/` — the upgrade
regression survives. It also adds a permanent release step, requires network
and git auth at render time, and preserves the exact structure that let this
ship unnoticed.

**Vendor, but keep un-vendoring available behind a flag.** Rejected on the same
"primitives, not modes" ground. A flag that exists to re-enable a path that
broke every project it touched is not an escape hatch; it is the dead branch
with a switch on it. Projects that genuinely need a different module source
already have one — a hand-authored `forge = { … }` shape, which the patcher
detects and refuses to rewrite, warning instead of clobbering.

**Publish the module to an OCI registry.** A more conventional distribution
story, and worth revisiting if third parties ever consume the KCL module
independently of forge. Rejected for now: it solves a distribution problem
forge does not have (the binary already carries the module) while reintroducing
the network dependency at render time.

## Consequences

- A project scaffolded by any forge build renders offline, immediately.
- Upgrading forge cannot break a project's KCL resolution; a release-build
  generate over a working project is a no-op on kcl.mod.
- Releases have one less step, and it is a step that cannot be forgotten
  because it no longer exists.
- Projects that hand-authored a non-vendored `forge = { … }` dependency keep
  it; forge warns rather than rewriting shapes it does not manage.
- `forge generate` is now the only thing that refreshes the KCL module, and the
  version stamp plus render-time warning is what keeps that honest.

## Regression guard

`TestReleaseBuildScaffoldResolvesAndRenders` (internal/templates) scaffolds
with a stamped pkg version — the exact discriminator that used to select the
broken path — and then **resolves and renders** every env through
`kclrender.Run`, requiring real manifests out the other end. It runs with
`GIT_ALLOW_PROTOCOL=none`, so a dependency that needs the network fails rather
than passing on a machine that happens to have one.

Verified to FAIL on the pre-fix code with the real defect —
`error: pathspec 'kcl-v0.1.0' did not match any file(s) known to git`, with
network access available — and to pass after.
