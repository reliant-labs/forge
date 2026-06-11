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
	buildDate string = "unknown"
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
)

// pkgVersionRE accepts semver module versions, e.g. v0.3.0 or
// v1.2.3-rc.1 (Go pseudo-versions also match — they are valid go.mod
// require versions). Anything else is treated as "no published version"
// so a malformed stamp degrades to the dev flow instead of emitting an
// unresolvable require into user go.mod files.
var pkgVersionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// Set records the forge binary's version metadata. It is intended to be
// called exactly once, from the main entrypoint.
func Set(v, date, commit string) {
	mu.Lock()
	defer mu.Unlock()
	version = v
	buildDate = date
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

// BuildDate returns the build date recorded via Set (or "unknown").
func BuildDate() string {
	mu.RLock()
	defer mu.RUnlock()
	return buildDate
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
