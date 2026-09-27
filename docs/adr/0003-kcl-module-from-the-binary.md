# ADR 0003: The forge KCL module comes from the binary, not the project

**Status:** accepted — supersedes the vendored-copy half of
[ADR 0001](0001-always-vendor-forge-kcl.md)

## Context

ADR 0001 made one mechanism resolve every project's `import forge`: materialize
the KCL module embedded in the forge binary into `<project>/.forge-kcl/`, point
`deploy/kcl/kcl.mod` at it (`forge = { path = "../../.forge-kcl" }`), and
commit the copy so containers and CI resolve it. The single-mechanism,
from-the-binary, offline decision was right. Sharing the copy through git was
not, and every failure it produced came from two forge builds fighting over
one committed directory:

1. **Backwards refresh broke prod.** A developer ran `forge generate` in
   control-plane with a binary older than the one that last vendored the copy.
   Generate rewrote the committed `.forge-kcl/schema.k` with the older schema
   (an outdated Gateway listener rule), and prod's `env render` failed later,
   in a different command.
2. **The guard for (1) broke CI.** The fix was a downgrade refusal plus an
   `--allow-kcl-downgrade` escape hatch. A scaffolded project (houndersclub)
   then went red in `forge ci verify-generated`: its workflow pinned a forge
   one commit older than go.mod, and the refusal fired — "refusing to overwrite
   .forge-kcl/ with an OLDER forge's KCL module".
3. **The stamp churned.** `.forge-kcl/.forge-version` recorded whichever build
   last wrote it, so every developer's differently-built forge fought over one
   committed line.

The user's direction: `.forge-kcl` is "a dev thing that only we face because
we use non-tagged versions — and we only really ever want to do this locally,
never on a deployed version", and a project should resolve forge's KCL from a
proper forge release. Committed files must be byte-identical whichever forge
build generated them.

## Decision

**Every KCL evaluation forge performs supplies the `forge` package from the
binary performing it, as a KCL external package.** The project's kcl.mod
declares no `forge` dependency, and the project holds no copy of the module.

- The binary materializes its embedded module once into a content-addressed
  user cache, `<UserCacheDir>/forge/kcl/<hash>/` (override:
  `FORGE_KCL_MODULE_CACHE`), and passes `forge=<dir>` to kpm
  (`client.WithExternalPkgs`) at render and to `kcl.ListOptions`
  (`ExternalPkgs`) at option discovery. Both paths go through
  `internal/kclvendor`; the only render seam is `internal/kclrender.Run`.
- A project pinned to a released forge renders against exactly that release's
  module on every machine: CI installs forge at the pin, and that binary IS the
  module. No network, no git, no registry, nothing to publish or vendor.
- A dev build uses the identical mechanism with its own embedded module. There
  is no dev/release branch, so ADR 0001's "primitives, not modes" holds.
- The scaffold kcl.mod has an empty `[dependencies]` table. It is byte-identical
  under every forge build, and `forge generate` never rewrites it.

**Migration.** `forge generate` removes any legacy `forge = …` line (with the
marker block forge maintained around it) from `deploy/kcl/kcl.mod` and a
root `kcl.mod`, strips the `forge` entry from `kcl.mod.lock` (kpm resolves a
locked package ahead of an external one, so a stale entry would shadow the
binary's module), and deletes `.forge-kcl/`. A render that meets an
unmigrated kcl.mod refuses with that instruction instead of evaluating a stale
copy. `forge lint` and `forge ci verify-generated` fail while `.forge-kcl/` is
still tracked, naming `git rm -r --cached .forge-kcl`.

## Alternatives considered

**Keep `.forge-kcl/`, but gitignore it and sync it at every render.** This
fixes the sharing: every machine materializes its own copy from its own
binary, so there is no second forge to overwrite and the downgrade refusal
protects nothing. We built it first. Rejected, because the project-relative
`path =` dependency still makes every environment — CI, containers, the deploy
path — materialize a project-local directory before anything renders, and it
leaves a machine-local artifact in every tree for nothing. The external package
removes the directory entirely without losing anything.

**Point kcl.mod at the forge git repo at the release tag `vX.Y.Z`.** Every
release has that tag (unlike ADR 0001's never-published `kcl-v0.1.0`), and kpm
supports git dependencies. Rejected: it needs network and git credentials at
every render (CI, containers, air-gapped machines), it names the module
version in a committed file that must then be bumped in lockstep with go.mod,
and a dev build needs an override that must not touch committed files — the
dev/release split ADR 0001 deleted, returning as a mode.

**Publish the module to an OCI registry.** Same network cost as the git tag,
plus a release step that must be remembered. Worth revisiting only if third
parties consume the KCL module without forge.

## Consequences

- A fresh clone, a CI checkout and a container render immediately, with
  nothing materialized into the project and no network.
- The module version can never disagree with the forge rendering it, so the
  version stamp, the render-time staleness warning, the downgrade refusal and
  `--allow-kcl-downgrade` are all deleted rather than maintained.
- The stock `kcl` CLI no longer resolves `import forge` in a forge project,
  because kpm does not know about the module. Render through forge (`forge env
  render <env>`). That was already the rule: a bare `kcl run` also misses
  `kcl_plugin.forge`, forge's `-D` bindings and its preflights. CI templates
  that shelled out to `kcl run` must render through forge.
- Editor/LSP support for `import forge` without forge in the loop is not
  provided. If it is wanted, it belongs to a deliberate, gitignored,
  dev-only link — never a committed dependency.

## Regression guard

- `TestRunSuppliesForgeModuleFromTheBinary` (internal/kclrender) renders a
  project whose kcl.mod declares no `forge` dependency and asserts
  `import forge` resolves and the render writes nothing into the project. On
  the pre-change code it fails with `CannotFindModule: pkgpath forge not found`.
- `TestReleaseBuildScaffoldResolvesAndRenders` (internal/templates) scaffolds
  with a release-stamped build, asserts no `forge` dependency and no
  `.forge-kcl/`, and renders every env offline (`GIT_ALLOW_PROTOCOL=none`).
- `TestSyncForgeKCL_OutputIsIdenticalUnderEveryBuild` (internal/cli) asserts a
  release build and a dev build migrate kcl.mod to identical bytes.
