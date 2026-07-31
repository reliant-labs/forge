// Package forgepkg embeds the forge/pkg runtime-library module's own
// source tree so a dev build of the forge CLI can vendor it into
// generated projects.
//
// Why the module rides inside the forge binary: dev builds of forge have
// no published github.com/reliant-labs/forge/pkg version to pin (releases
// stamp one via ldflags — see cmd/forge/main.go PkgVersion). Historically
// the dev flow depended on a sibling `../forge/pkg` checkout being on
// disk, so a project scaffolded anywhere else silently pinned the stale
// PUBLISHED pkg and local pkg changes were unreachable. This embed makes
// the local source travel INSIDE the binary — exactly like the forge KCL
// module does (see kcl/embed.go) — so `forge generate` can materialize it
// into `<project>/.forge-pkg/` and point go.mod's replace at it, from any
// directory and with no sibling checkout, daemon, or `go install`'d binary
// required. The vendored pkg is therefore always byte-identical to the pkg
// the forge binary was built against — no build-time/runtime version skew.
//
// Cost containment: this package sits at the forge/pkg MODULE ROOT, which
// nothing imports in normal use (consumers import the subpackages
// pkg/crud, pkg/serverkit, …). So the ~1.5 MB embed is compiled ONLY into
// binaries that import this root package — the forge CLI via
// internal/pkgvendor — never into a user's service binary.
package forgepkg

import "embed"

// Source is the embedded forge/pkg source tree, rooted at this directory
// (paths like "go.mod", "crud/crud.go", "serverkit/server.go"). The `*`
// pattern captures every top-level entry that is not dot- or
// underscore-prefixed — all subpackages (recursively), go.mod, and go.sum
// — which is exactly the set internal/pkgvendor materializes into a
// project's .forge-pkg/. Dotfiles (.gitignore, editor droppings, a stray
// .DS_Store) are deliberately excluded so the embed stays deterministic
// across dev machines.
//
//go:embed *
var Source embed.FS
