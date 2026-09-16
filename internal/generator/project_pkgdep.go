// Package generator — forge dependency resolution for scaffolded go.mod files.
//
// Generated projects import github.com/reliant-labs/forge/pkg/* (serverkit,
// appkit, orm, ...). Those are packages inside the SINGLE forge module —
// forge/pkg stopped being a companion submodule — so a scaffold pins ONE
// requirement, `require github.com/reliant-labs/forge vX.Y.Z`, with no
// replace and no vendoring; `go mod tidy` resolves it from the module proxy
// like any other dependency.
//
// WHICH VERSION. Whatever this binary can honestly name as itself, and only
// when a module proxy can serve it. buildinfo.InstallableVersion() is exactly
// that predicate — a release tag, or the clean pseudo-version recorded by
// `go install .../cmd/forge@main` / `@<sha>` — and "" for a local `go build`
// or any dirty tree, whose bytes exist nowhere a consumer could fetch them.
//
// THE EMPTY CASE IS NOT A FALLBACK. It used to be: a dev build pinned the
// last published tag (a hand-maintained `defaultPublishedForgePkgVersion`
// constant), which meant a binary generating code against UNRELEASED forge
// source told the project to require the last RELEASE. The generated code
// then called symbols that version did not have, and the project failed to
// compile — `undefined: testkit.StubNotConfigured`, after codegen had already
// rewritten the tree. Generator and runtime shipped as one module now, so the
// only honest answer for an unreleasable build is "pin nothing, bridge to my
// source tree": see the go.work bridge in internal/cli/new.go for scaffolds,
// and the pre-codegen check in internal/cli/generate_pkg_compat.go for
// projects that already exist.
package generator

import "github.com/reliant-labs/forge/internal/buildinfo"

// resolveForgeVersion returns the forge version a scaffolded go.mod should
// require, or "" when this binary's build is not resolvable from a module
// proxy.
//
// Callers MUST treat "" as "this build needs a source bridge", never as "use
// a default" — inventing a version here is what the package doc describes.
func resolveForgeVersion() string {
	return buildinfo.InstallableVersion()
}
