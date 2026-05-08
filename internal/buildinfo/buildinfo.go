// Package buildinfo exposes the forge binary's version metadata to packages
// that cannot depend on internal/cli (to avoid import cycles). The cmd/forge
// entrypoint and internal/cli are responsible for seeding this data at
// startup via Set; anything that wants to stamp the forge version into
// generated artifacts should read it from here.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

var (
	mu        sync.RWMutex
	version   string = "dev"
	buildDate string = "unknown"
	gitCommit string = "unknown"
)

// Set records the forge binary's version metadata. It is intended to be
// called exactly once, from the main entrypoint.
func Set(v, date, commit string) {
	mu.Lock()
	defer mu.Unlock()
	version = v
	buildDate = date
	gitCommit = commit
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
