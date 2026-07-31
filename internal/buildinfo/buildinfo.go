// Package buildinfo exposes the forge binary's version metadata to packages
// that cannot depend on internal/cli (to avoid import cycles). The cmd/forge
// entrypoint and internal/cli are responsible for seeding this data at
// startup via Set; anything that wants to stamp the forge version into
// generated artifacts should read it from here.
package buildinfo

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// forgeModulePath and forgePkgModulePath are the go.mod module paths that
// together identify a genuine forge source checkout on disk: the root module
// and its companion runtime-library submodule. Runtime source discovery
// (DiscoverDevForgeRootFromSource) confirms BOTH before treating a directory
// as the local forge root.
const (
	forgeModulePath    = "github.com/reliant-labs/forge"
	forgePkgModulePath = "github.com/reliant-labs/forge/pkg"
)

// DevForgeRoot is the absolute path to the LOCAL forge source checkout that
// THIS forge binary was built from. It is injected ONLY on dev builds, via
// ldflags, from the builder's own checkout — never committed, never
// machine-specific in source:
//
//	go install -ldflags \
//	  "-X github.com/reliant-labs/forge/internal/buildinfo.DevForgeRoot=$(git rev-parse --show-toplevel)" \
//	  ./cmd/forge
//
// (see the `dev` target in the Makefile / Taskfile and CONTRIBUTING.md).
// Released and CI builds never pass this ldflag, so it stays "" for anything
// a user installs.
//
// Its only consumer is the project scaffolder (internal/cli/new.go): on a dev
// build it writes a gitignored go.work into the freshly-created project that
// `use`s this path, so the scaffold builds against the in-development
// forge/pkg instead of the last published tag — no manual `go mod edit
// -replace` dance. The path lands ONLY in that generated, gitignored,
// machine-local go.work; it is never written into committed forge source or
// any committed project file.
var DevForgeRoot string

// DiscoverDevForgeRootFromSource locates the LOCAL forge source checkout that
// THIS binary was COMPILED from, at runtime, with no build-time ldflag. It is
// the embedder-independent complement to DevForgeRoot: forge's CLI runs both
// standalone (cmd/forge) AND embedded in other binaries — reliant imports
// github.com/reliant-labs/forge/cli and runs `reliant forge project new`
// in-process. Only forge's own `make dev` stamps DevForgeRoot; an embedder's
// build almost never remembers to pass forge's internal ldflag, so on a dev
// forge running inside reliant DevForgeRoot is "" and the scaffolder would
// silently fall back to the last PUBLISHED forge/pkg tag — which cannot
// satisfy generated code that targets not-yet-published forge/pkg APIs. Rather
// than make every embedder stamp the flag, we recover the path the compiler
// baked into this source file.
//
// How: runtime.Caller yields the absolute path of THIS file as it existed on
// the build machine. Contributor/dev builds are NOT built with -trimpath, so
// that path is a real, absolute checkout path; we walk upward to the module
// root (the go.mod declaring github.com/reliant-labs/forge) and confirm its
// pkg/ submodule (github.com/reliant-labs/forge/pkg) exists on disk.
//
// It returns "" — bridging nothing — for anything that is not a genuine,
// present-on-disk forge checkout, which is exactly the release/shipped case:
//   - release builds use -trimpath, so the baked path is a module-relative
//     stub that does not exist as an absolute path;
//   - a dev binary copied to another machine no longer finds its source tree;
//   - any walk that fails to find both go.mod module markers.
//
// So it is safe to call unconditionally — it self-limits to the machine that
// built a from-source forge. Callers still gate on IsDevBuild first, so a
// clean release never even attempts discovery.
func DiscoverDevForgeRootFromSource() string {
	mu.RLock()
	ov, ovSet := discoverOverride, discoverOverrideSet
	mu.RUnlock()
	if ovSet {
		return ov
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" || !filepath.IsAbs(file) {
		return ""
	}
	return forgeRootFromFile(file)
}

// discoverOverride / discoverOverrideSet are a test seam mirroring
// devBuildOverride: under `go test`, runtime.Caller always resolves to the
// LIVE forge checkout, so a test can never naturally exercise the
// "not discoverable" path — it pins the discovery result here instead.
var (
	discoverOverride    string
	discoverOverrideSet bool
)

// SetDiscoveredForgeRoot overrides what DiscoverDevForgeRootFromSource returns,
// bypassing the runtime.Caller walk. Test-only seam (pairs with SetDevBuild):
// pass "" to exercise the not-discoverable/hint path, or a fixture root to
// exercise the bridge path deterministically. Pair with
// ClearDiscoveredForgeRoot in a t.Cleanup.
func SetDiscoveredForgeRoot(v string) {
	mu.Lock()
	defer mu.Unlock()
	discoverOverride = v
	discoverOverrideSet = true
}

// ClearDiscoveredForgeRoot removes any override set by SetDiscoveredForgeRoot,
// restoring the real runtime.Caller discovery.
func ClearDiscoveredForgeRoot() {
	mu.Lock()
	defer mu.Unlock()
	discoverOverride = ""
	discoverOverrideSet = false
}

// forgeRootFromFile is the pure upward walk behind
// DiscoverDevForgeRootFromSource, split out so it can be unit-tested against a
// fixture tree without depending on where the test binary was compiled.
func forgeRootFromFile(file string) string {
	dir := filepath.Dir(file)
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if modfile.ModulePath(data) == forgeModulePath {
				// Root module matches. Confirm the companion pkg submodule is
				// present too, so we never point a go.work `use` at a tree
				// that lacks pkg/ (the only forge module scaffolds import).
				pd, err := os.ReadFile(filepath.Join(dir, "pkg", "go.mod"))
				if err == nil && modfile.ModulePath(pd) == forgePkgModulePath {
					return dir
				}
				return ""
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached the filesystem root without a match
		}
		dir = parent
	}
}

var (
	mu        sync.RWMutex
	version   string = "dev"
	gitCommit string = "unknown"

	// devBuildOverride is a test seam mirroring pkgModuleVersionOverride:
	// IsDevBuild reads the ambient binary build info, which under `go test`
	// is always the "(devel)" test binary, so tests that need to exercise
	// the release path pin it here. Pair Set/Clear in a t.Cleanup.
	devBuildOverride    bool
	devBuildOverrideSet bool

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
	// replace) when this is set, and falls back to the latest published
	// pkg tag (generator.defaultPublishedForgePkgVersion) when it is not.
	// Either way the scaffold gets a clean published-version pin.
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

// releaseTagRE matches a clean release tag: vX.Y.Z with an optional
// pre-release suffix. Note a Go pseudo-version (v0.0.0-<ts>-<sha>) ALSO
// matches this shape, so IsDevBuild rejects pseudo-versions separately via
// module.IsPseudoVersion before consulting this.
var releaseTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// SetDevBuild overrides what IsDevBuild reports, bypassing the ambient binary
// build info. Test-only seam: build info is baked at compile time and is
// always the "(devel)" test binary under `go test`, so tests pin it here to
// exercise both the dev and release paths deterministically. Pair with
// ClearDevBuild in a t.Cleanup.
func SetDevBuild(v bool) {
	mu.Lock()
	defer mu.Unlock()
	devBuildOverride = v
	devBuildOverrideSet = true
}

// ClearDevBuild removes any override set by SetDevBuild, restoring the real
// build-info read.
func ClearDevBuild() {
	mu.Lock()
	defer mu.Unlock()
	devBuildOverride = false
	devBuildOverrideSet = false
}

// IsDevBuild reports whether THIS forge binary is a development build rather
// than a released, tagged one. It is the guard the scaffolder pairs with
// DevForgeRoot before writing a local-forge go.work bridge: a released binary
// must NEVER write one, even if a DevForgeRoot value somehow leaked in.
//
// A build is treated as a RELEASE (returns false) only when the binary's own
// module build info records a clean, tagged semver version (vX.Y.Z[-pre]) from
// an unmodified working tree — i.e. `go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`
// or a tag-checkout release build. Everything else is a dev build (returns
// true):
//   - no build info at all,
//   - the "(devel)" marker (a plain `go build`/`go install` from the source
//     tree — the ordinary contributor loop),
//   - a Go pseudo-version (an untagged commit, e.g. `...@main`),
//   - vcs.modified=true (a dirty working tree).
func IsDevBuild() bool {
	mu.RLock()
	ov, ovSet := devBuildOverride, devBuildOverrideSet
	mu.RUnlock()
	if ovSet {
		return ov
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return true
	}
	vcsModified := false
	for _, s := range info.Settings {
		if s.Key == "vcs.modified" && s.Value == "true" {
			vcsModified = true
			break
		}
	}
	return isDevBuildFrom(info.Main.Version, vcsModified)
}

// isDevBuildFrom is the pure decision behind IsDevBuild, split out so the
// release-vs-dev classification can be unit-tested without controlling the
// ambient binary build info (which is always the "(devel)" test binary under
// `go test`).
func isDevBuildFrom(mainVersion string, vcsModified bool) bool {
	if mainVersion == "" || mainVersion == "(devel)" {
		return true
	}
	if vcsModified {
		return true
	}
	// A pseudo-version (v0.0.0-<ts>-<sha>) is an untagged commit → dev.
	if module.IsPseudoVersion(mainVersion) {
		return true
	}
	// A clean, tagged release version from an unmodified tree → release.
	return !releaseTagRE.MatchString(mainVersion)
}

// IsDevVersion reports whether a forge version STRING denotes a development
// build rather than a published release tag.
//
// IsDevBuild answers that question about the RUNNING binary by reading its own
// build info. IsDevVersion answers it about a version string recorded
// somewhere else — most importantly the `forge_version` a project pins in
// forge.yaml, which outlives the binary that wrote it. A project scaffolded by
// a local forge checkout records a Go pseudo-version (`v0.0.4-0.<ts>-<sha>`,
// optionally `+dirty`). That is honest IDENTITY — it names the exact commit —
// but it is not a release, and anything that REPORTS a version should say so
// rather than let a reader mistake it for a tag.
//
// Development (true): the "dev" / "(devel)" sentinels, an empty value, any
// version carrying build metadata (`+dirty` — only a modified working tree
// produces one), any Go pseudo-version, and anything that is not valid semver.
// Release (false): a canonical tagged semver version.
//
// It is deliberately a pure function of the string: ordering and identity are
// separate concerns, and callers that need ordering compare semver directly.
func IsDevVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" || v == "(devel)" {
		return true
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if strings.Contains(v, "+") {
		return true
	}
	if !semver.IsValid(v) {
		return true
	}
	return module.IsPseudoVersion(v)
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
