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
//   - DEV flow: dev builds have no published pkg version. When the new
//     project sits next to a forge checkout (the common
//     `~/src/{forge,myproject}` layout), the scaffolded go.mod gets a
//     host-absolute `replace github.com/reliant-labs/forge/pkg =>
//     <sibling>/forge/pkg`; the first `forge generate` then vendors that
//     source into <project>/.forge-pkg/ and rewrites the replace to
//     `./.forge-pkg` so docker builds see the same code (see
//     internal/cli/dev_pkg_replace.go). With no sibling checkout either,
//     nothing is emitted and `go mod tidy` falls back to resolving a
//     pseudo-version from the proxy — today's pre-existing behavior.
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
	if v := buildinfo.PkgVersion(); v != "" {
		return v, ""
	}
	return "", siblingForgePkgDir(projectPath)
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
