# forge/pkg versioning: the dev flow and the release flow

Generated projects import the `github.com/reliant-labs/forge/pkg` module
(serverkit, appkit, orm, observe, testkit, ...). This document is the
canonical description of how that dependency is versioned, vendored, and
released. The implementation lives in:

- `internal/generator/project_pkgdep.go` — scaffold-time decision
- `internal/templates/project/go.mod.tmpl` — what gets emitted
- `internal/cli/dev_pkg_replace.go` — dev-mode `.forge-pkg` vendor sync
- `internal/cli/doctor_pkgpin.go` — `forge doctor` "stuck on dev path" check
- `internal/buildinfo` + `cmd/forge/main.go` — the embedded pkg version
- `scripts/release-pkg.sh` / `task release:pkg` — tagging discipline

## The two flows

### Release flow (published forge binary)

1. A maintainer tags a pkg release: `task release:pkg -- vX.Y.Z`.
   The script validates the version shape, a clean `pkg/` tree, no
   pre-existing tag, and — critically — that the pkg module builds and
   vets **standalone** (`GOWORK=off`, exactly the consumer's view), then
   creates the Go submodule tag `pkg/vX.Y.Z`.
2. **Manual step:** push the tag — `git push origin pkg/vX.Y.Z`.
3. Release builds of the forge binary are stamped with that version:

   ```sh
   go build -trimpath -ldflags "-X main.PkgVersion=vX.Y.Z" -o bin/forge ./cmd/forge
   ```

4. `forge new` run by such a binary emits a **clean version pin** into
   the project's go.mod — `require github.com/reliant-labs/forge/pkg
   vX.Y.Z` with **no replace directive**. `go mod tidy`, docker builds,
   and CI all resolve it from the module proxy like any other dependency.
   No `.forge-pkg/` directory exists in this mode and the Dockerfile has
   no `COPY .forge-pkg/` line.

A malformed `PkgVersion` stamp (anything that isn't a valid go.mod
require version) is discarded by `buildinfo.PkgVersion()` and the binary
degrades to the dev flow — it can never emit an unresolvable require.

### Dev flow (forge built from source, `Version=dev`, no PkgVersion stamp)

The pkg module evolves in lockstep with forge HEAD, so there is nothing
published to pin. Instead:

1. `forge new` looks for a sibling forge checkout
   (`<parent-of-project>/forge/pkg` declaring the right module path —
   the common `~/src/{forge,myproject}` layout) and, when found, emits a
   host-absolute `replace github.com/reliant-labs/forge/pkg =>
   <that path>` into go.mod. With no sibling, nothing is emitted and
   `go mod tidy` resolves a pseudo-version from the proxy.
2. The first `forge generate` sees the host-absolute replace, vendors
   the source into `<project>/.forge-pkg/` (~the full pkg tree), and
   rewrites the replace to `./.forge-pkg` so `docker build` resolves the
   same bytes as the host build (the Dockerfile gains a
   `COPY .forge-pkg/ ./.forge-pkg/` line).
3. Subsequent `forge generate` runs refresh `.forge-pkg/` from the
   sibling checkout when one exists, and silently leave the vendored
   copy alone when it doesn't (the copy is then the source of truth).

Opt out with `forge.yaml → dev.vendor_local_forge_pkg: false`.

## Safety invariants

- The vendor sync **never** touches a project whose go.mod has a clean
  version pin and no replace — pinned release projects pass through
  `forge generate` byte-identical, even with a sibling forge checkout on
  disk (`TestSyncDevForgePkgReplace_CleanVersionPinUntouched`).
- A project already vendored with no sibling checkout to refresh from
  is a silent no-op, not a warning (`forge generate` regression guard
  for kalshi-trader #14, commit 064e019).
- `forge doctor` warns — with exact switch-over commands — when a
  project still carries the dev replace while the running forge release
  publishes a pkg version ("stuck on the dev path").

## Switching a project from dev → release

```sh
go mod edit -dropreplace=github.com/reliant-labs/forge/pkg
go get github.com/reliant-labs/forge/pkg@vX.Y.Z && go mod tidy
rm -rf .forge-pkg/
forge generate   # refreshes the Dockerfile (drops the COPY line)
```

## Note on the forge.v1 annotation protos (the "forge/gen" module)

There is **no** published `github.com/reliant-labs/forge/gen` module and
none is needed. Scaffolded projects vendor `proto/forge/v1/forge.proto`
with its `go_package` rewritten to `<project module>/gen/forge/v1`; the
project's own `buf generate` run (out: `gen`, `paths=source_relative`)
then emits `gen/forge/v1/forge.pb.go` inside the project's `gen`
submodule, and every other generated `.pb.go` imports that project-local
package. Historically the rewrite silently no-oped and scaffolds shipped
imports of the nonexistent `forge/gen` module; the invariant is now
pinned by `internal/assets/embedded_test.go`
(`TestWriteForgeV1ProtoRewritesGoPackage`).
