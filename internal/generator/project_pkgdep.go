// Package generator — forge/pkg dependency resolution for scaffolded
// go.mod files.
//
// Generated service projects import github.com/reliant-labs/forge/pkg/*
// (serverkit, appkit, orm, ...). forge/pkg is a PUBLISHED module (git
// submodule tags pkg/vX.Y.Z), so a scaffold simply pins
// `require github.com/reliant-labs/forge/pkg vX.Y.Z` with NO replace and
// NO vendoring; `go mod tidy` resolves it from the module proxy like any
// other dependency.
//
// Which version to pin:
//
//   - RELEASE flow: release builds of forge are stamped (via ldflags, see
//     cmd/forge/main.go PkgVersion) with the published forge/pkg version
//     they were released against.
//   - DEV flow: dev builds carry no ldflags stamp, so they pin the latest
//     published tag (defaultPublishedForgePkgVersion). During active forge
//     development the generated code may target UNPUBLISHED forge/pkg APIs;
//     maintainers bridge the generated project to their local forge/pkg
//     checkout with a gitignored `go.work` (a maintainer-only concern that
//     lives OUTSIDE forge — forge never writes a replace directive, because
//     a committed local-path replace breaks every other dev and CI).
package generator

import "github.com/reliant-labs/forge/internal/buildinfo"

// defaultPublishedForgePkgVersion is the forge/pkg release a scaffold pins
// when this binary carries no ldflags-stamped version (dev builds). Keep it
// pointed at the latest published pkg/vX.Y.Z submodule tag — bumping it is
// step 1 of docs/releasing.md's checklist.
const defaultPublishedForgePkgVersion = "v0.1.6"

// resolveForgePkgVersion returns the published forge/pkg version a
// scaffolded go.mod should require. Release builds use their ldflags stamp;
// dev builds fall back to the latest published tag. The result is always a
// clean version pin — forge never emits a `replace` for forge/pkg.
func resolveForgePkgVersion() string {
	if v := buildinfo.PkgVersion(); v != "" {
		return v
	}
	return defaultPublishedForgePkgVersion
}
