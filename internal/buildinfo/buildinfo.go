// Package buildinfo exposes the forge binary's version metadata to packages
// that cannot depend on internal/cli (to avoid import cycles). The cmd/forge
// entrypoint and internal/cli are responsible for seeding this data at
// startup via Set; anything that wants to stamp the forge version into
// generated artifacts should read it from here.
package buildinfo

import (
	"regexp"
	"runtime/debug"
	"sync"
)

var (
	mu        sync.RWMutex
	version   string = "dev"
	gitCommit string = "unknown"

	// pkgVersion is the published version of the companion
	// github.com/reliant-labs/forge/pkg module that THIS forge binary
	// scaffolds against. Empty on dev builds. Release builds stamp it
	// via ldflags (see Taskfile `release:` notes and
	// scripts/release-pkg.sh):
	//
	//	go build -ldflags "-X main.PkgVersion=v0.3.0" ./cmd/forge
	//
	// Consumers: the project scaffolder pins
	// `require github.com/reliant-labs/forge/pkg <pkgVersion>` (no
	// replace) when this is set, and falls back to the dev-mode
	// `.forge-pkg` vendoring flow when it is not. `forge doctor` warns
	// when a project is still on dev vendoring even though the running
	// forge release knows a published pkg version. See
	// docs/pkg-versioning.md for the full dev-vs-release model.
	pkgVersion string = ""

	// pkgModuleVersionOverride is a test seam. When set (via
	// SetPkgModuleVersion), PkgModuleVersion returns it instead of reading
	// the ambient binary build info — build info is fixed at compile time
	// and varies with GOWORK, so tests must be able to pin it deterministically.
	pkgModuleVersionOverride    string
	pkgModuleVersionOverrideSet bool
)

// pkgVersionRE accepts semver module versions, e.g. v0.3.0 or
// v1.2.3-rc.1 (Go pseudo-versions also match — they are valid go.mod
// require versions). Anything else is treated as "no published version"
// so a malformed stamp degrades to the dev flow instead of emitting an
// unresolvable require into user go.mod files.
var pkgVersionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// Set records the forge binary's version metadata. It is intended to be
// called exactly once, from the main entrypoint. The date argument is
// accepted for call-site compatibility but is not currently retained.
func Set(v, _, commit string) {
	mu.Lock()
	defer mu.Unlock()
	version = v
	gitCommit = commit
}

// SetPkgVersion records the published forge/pkg module version this
// binary scaffolds against. Called from the main entrypoint when the
// release build stamped one via ldflags. Safe to call with "" (dev).
func SetPkgVersion(v string) {
	mu.Lock()
	defer mu.Unlock()
	pkgVersion = v
}

// SetPkgModuleVersion overrides the value PkgModuleVersion returns, bypassing
// the ambient binary build info. Test-only seam: build info is baked at
// compile time and depends on GOWORK, so scaffolder tests pin it here to stay
// deterministic. Pass "" to force the "no build-info version" path. Pair with
// ClearPkgModuleVersion in a t.Cleanup.
func SetPkgModuleVersion(v string) {
	mu.Lock()
	defer mu.Unlock()
	pkgModuleVersionOverride = v
	pkgModuleVersionOverrideSet = true
}

// ClearPkgModuleVersion removes any override set by SetPkgModuleVersion,
// restoring the real build-info read.
func ClearPkgModuleVersion() {
	mu.Lock()
	defer mu.Unlock()
	pkgModuleVersionOverride = ""
	pkgModuleVersionOverrideSet = false
}

// PkgVersion returns the published forge/pkg module version this binary
// was released against, or "" when none is known (dev builds, or a
// malformed stamp). A non-empty return is always a canonical semver
// version (vX.Y.Z[-pre]) safe to write into a go.mod require directive.
func PkgVersion() string {
	mu.RLock()
	v := pkgVersion
	mu.RUnlock()
	if pkgVersionRE.MatchString(v) {
		return v
	}
	return ""
}

// pkgModulePath is the canonical module path of the companion forge
// runtime-library module, matched against this binary's dependency graph
// in PkgModuleVersion.
const pkgModulePath = "github.com/reliant-labs/forge/pkg"

// PkgModuleVersion returns the version of github.com/reliant-labs/forge/pkg
// that THIS forge binary was actually compiled against, read from the
// binary's own build info (runtime/debug). Unlike PkgVersion (a release
// ldflags stamp), this is populated for ordinary `go install
// .../cmd/forge@<ref>` builds — the binary records a real, proxy-resolvable
// pseudo-version (e.g. v0.0.0-20260624040937-ce5dfbd929ed) that is already
// in the build's module cache. Scaffolded projects can pin it and let
// `go mod tidy` resolve forge/pkg offline, instead of the unresolvable
// `v0.0.0` the templates hard-coded when no version was known.
//
// Returns "" when the version isn't a canonical require version — most
// importantly for a workspace build (local `go build` under go.work, where
// forge/pkg is replaced by the in-tree ./pkg and the dep shows as
// "(devel)"), in which case the dev sibling/vendoring flow applies instead.
// Robust to `forge_version: dev` binaries (the daemon): the "dev" label is
// the forge binary's own version, orthogonal to the forge/pkg dep version
// recorded here.
func PkgModuleVersion() string {
	mu.RLock()
	ov, ovSet := pkgModuleVersionOverride, pkgModuleVersionOverrideSet
	mu.RUnlock()
	if ovSet {
		if pkgVersionRE.MatchString(ov) {
			return ov
		}
		return ""
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		d := dep
		// Follow a replace directive to the effective module: the version
		// that actually resolves lives on the replacement.
		if d.Replace != nil {
			d = d.Replace
		}
		if d.Path != pkgModulePath {
			continue
		}
		if pkgVersionRE.MatchString(d.Version) {
			return d.Version
		}
		return ""
	}
	return ""
}

// Version returns the forge binary's version. When the binary was produced by
// `go install ...@<ref>`, Set will not have been called with a real value, so
// we fall back to reading the module version from runtime build info.
//
// Returns "dev" if neither source is available.
func Version() string {
	mu.RLock()
	v := version
	mu.RUnlock()

	if v != "" && v != "dev" {
		return v
	}

	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return v
}

// installableVersionRE matches a forge version string that is a valid
// `go install github.com/.../forge/cmd/forge@<ref>` target: either a
// release tag (vX.Y.Z[-pre]) or a clean Go pseudo-version
// (v0.0.0-<timestamp>-<commit>). Crucially it does NOT match build
// metadata (`+dirty`, `+incompatible` is allowed as a require but never
// as an install ref here): a `+dirty` pseudo-version is produced by
// building from a dirty working tree and no module proxy can ever serve
// it, so emitting it as a CI install target makes `go install` fail on
// every run (FRICTION fr-8c8a24ea97).
var installableVersionRE = regexp.MustCompile(
	`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// InstallableVersion returns the forge binary's version ONLY when it is a
// ref that `go install ...@<ref>` can actually resolve from a module
// proxy — a release tag or a clean Go pseudo-version. For anything else
// (the "dev" sentinel, "(devel)", an empty value, or a `+dirty`
// pseudo-version from a dirty-tree build) it returns "" so callers fall
// back to pinning by git SHA instead.
//
// This is the boundary that keeps `+dirty` out of generated CI: callers
// stamp this (not raw Version()) into the `go install` step, and the
// empty return routes the CI template's three-branch policy onto the SHA
// branch. See internal/templates/ci/github/ci.yml.tmpl.
func InstallableVersion() string {
	v := Version()
	if !installableVersionRE.MatchString(v) {
		return ""
	}
	return v
}

// GitCommit returns the git commit SHA recorded via Set. Falls back to the
// VCS revision from runtime build info when available.
func GitCommit() string {
	mu.RLock()
	c := gitCommit
	mu.RUnlock()

	if c != "" && c != "unknown" {
		return c
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return c
}
