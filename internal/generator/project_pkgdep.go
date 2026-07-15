// Package generator — forge/pkg dependency resolution for scaffolded
// go.mod files.
//
// Generated service projects import github.com/reliant-labs/forge/pkg/*
// (serverkit, appkit, orm, ...). How a fresh scaffold satisfies that
// dependency depends on how the running forge binary was built:
//
//   - RELEASE flow: release builds of forge are stamped (via ldflags,
//     see cmd/forge/main.go PkgVersion) with the published forge/pkg
//     version they were released against — pkg/vX.Y.Z submodule tags
//     created by scripts/release-pkg.sh. The scaffolded go.mod pins
//     `require github.com/reliant-labs/forge/pkg vX.Y.Z` with NO
//     replace; `go mod tidy` resolves it from the module proxy like any
//     other dependency.
//
//   - DEV flow: dev builds have no published (ldflags-stamped) pkg version.
//     When the new project sits next to a forge checkout (the common
//     `~/src/{forge,myproject}` layout), the scaffolded go.mod gets a
//     host-absolute `replace github.com/reliant-labs/forge/pkg =>
//     <sibling>/forge/pkg`; the first `forge generate` then vendors that
//     source into <project>/.forge-pkg/ and rewrites the replace to
//     `./.forge-pkg` so docker builds see the same code (see
//     internal/cli/dev_pkg_replace.go).
//
//     With no sibling checkout either (the daemon and any `go install`'d
//     forge), we pin the forge/pkg pseudo-version THIS binary was built
//     against, read from its own build info (buildinfo.PkgModuleVersion) —
//     a real, proxy-resolvable, already-cached version. This is what lets a
//     fresh scaffold's per-module `go mod tidy` (root AND the separate gen/
//     submodule, which the go.work replace does NOT reach) resolve forge/pkg
//     instead of choking on the unresolvable `v0.0.0` the templates used to
//     hard-code. Only when even the build info carries no usable version (a
//     local go.work build, where forge/pkg shows as "(devel)") is nothing
//     emitted and `go mod tidy` left to resolve a pseudo-version from the
//     proxy — the historical fallback.
//
// The full model is documented in docs/pkg-versioning.md.
package generator

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// forgePkgModule is the canonical module path of the published forge
// runtime-library module. Mirrors internal/cli.forgePkgModulePath (the
// cli package depends on this one, so the constant cannot be shared
// without an import cycle).
const forgePkgModule = "github.com/reliant-labs/forge/pkg"

// resolveForgePkgDep decides how the scaffolded go.mod should depend on
// forge/pkg. Returns (pinnedVersion, devReplaceTarget); at most one is
// non-empty.
//
//   - pinnedVersion non-empty → emit `require forge/pkg <version>`, no
//     replace (release flow).
//   - devReplaceTarget non-empty → emit a host-absolute replace that the
//     first `forge generate` vendors into ./.forge-pkg (dev flow with a
//     sibling forge checkout).
//   - both empty → emit nothing; `go mod tidy` resolves a proxy
//     pseudo-version (dev flow without a sibling checkout).
func resolveForgePkgDep(projectPath string) (pinnedVersion, devReplaceTarget string) {
	// 1. Release stamp (ldflags) — a published forge/pkg version.
	if v := buildinfo.PkgVersion(); v != "" {
		return v, ""
	}
	// 2. Sibling forge checkout — prefer a live replace so local forge/pkg
	//    edits flow into the scaffold immediately (the `~/src/{forge,proj}`
	//    dev layout).
	if sib := siblingForgePkgDir(projectPath); sib != "" {
		return "", sib
	}
	// 3. No sibling (daemon / `go install`'d forge): pin the forge/pkg
	//    pseudo-version this binary was compiled against. Real, resolvable,
	//    and already cached — unlike the templates' old `v0.0.0` fallback,
	//    which the separate gen/ submodule's `go mod tidy` could never fetch.
	if v := buildinfo.PkgModuleVersion(); v != "" {
		return v, ""
	}
	// 4. Nothing usable (local go.work build, forge/pkg == "(devel)"): emit
	//    neither pin nor replace; `go mod tidy` resolves from the proxy.
	return "", ""
}

// siblingForgePkgDir returns the absolute path of a sibling forge
// checkout's pkg/ directory (<parent-of-project>/forge/pkg), or "" when
// none exists. The go.mod module-path check guards against a directory
// that merely happens to be named forge/. Mirrors
// internal/cli.siblingForgePkg, which performs the same detection at
// `forge generate` time.
func siblingForgePkgDir(projectPath string) string {
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(abs), "forge", "pkg")
	data, err := os.ReadFile(filepath.Join(candidate, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			if strings.TrimSpace(strings.TrimPrefix(line, "module")) == forgePkgModule {
				return candidate
			}
			return ""
		}
	}
	return ""
}
