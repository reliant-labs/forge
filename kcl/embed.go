// Package kcl embeds the forge KCL module — the typed schemas +
// manifest render layer that generated projects' deploy/kcl env files
// import (`import forge`, `import forge.components`).
//
// Why the module rides inside the binary: dev builds of forge have no
// published `kcl-vX.Y.Z` git tag to resolve the scaffold's kcl.mod
// dependency against, exactly like dev builds have no published
// forge/pkg Go module version. The Go-side answer is vendoring a
// sibling checkout into `.forge-pkg/` (internal/cli/dev_pkg_replace.go);
// the KCL-side answer is materializing THIS embedded copy into the
// project at `.forge-kcl/` (internal/kclvendor) — which works even when
// no forge checkout exists on disk (daemon / `go install`'d binaries).
//
// The embed deliberately covers only what a consuming project needs to
// resolve and render: kcl.mod plus the schema/render sources (root *.k,
// components/, lib/). tests/ and example/ are module-development
// artifacts and are excluded; kcl.mod.lock is derived state.
package kcl

import "embed"

// Module is the embedded forge KCL module, rooted at this directory
// (paths like "kcl.mod", "schema.k", "components/expand.k").
//
//go:embed kcl.mod *.k components/*.k lib/*.k
var Module embed.FS
