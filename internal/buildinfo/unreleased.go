package buildinfo

import (
	"regexp"
	"strings"
)

// IsUnreleasedVersion reports whether the binary's reported version
// string corresponds to a non-release build that should suppress the
// version-pin warning. Covers:
//   - empty / unknown
//   - "dev" (local make-build sentinel)
//   - "(devel)" (Go's runtime.BuildInfo placeholder for go-run / go-test)
//   - any Go pseudoversion, in either form.
//
// Both pseudoversion forms matter. `v0.0.0-<ts>-<commit>` is what an
// un-tagged commit produces, and it was the only one recognised here.
// But once a repository HAS tags, a build from a commit after the latest
// one stamps `v0.1.25-0.<ts>-<commit>` — the base-version form — and that
// fell through to the warning. So every contributor running `task
// install:dev` in a tagged repo got told to `forge project upgrade`
// against a version no module proxy can serve, which is the exact
// "noise to dogfood owners" this function exists to prevent.
func IsUnreleasedVersion(v string) bool {
	switch v {
	case "", "dev", "(devel)":
		return true
	}
	// v0.0.0- is kept as a defensive prefix match independent of the
	// suffix shape: nothing is ever released at v0.0.0, so anything
	// wearing it is a synthesised version whatever follows.
	return strings.HasPrefix(v, "v0.0.0-") || IsPseudoVersion(v)
}

// pseudoVersionRE matches Go's pseudo-version suffix: a 14-digit UTC
// timestamp and a 12-hex-digit commit prefix, optionally preceded by the
// `0.` / `pre.0.` / `<n>.` counter Go inserts when the base version comes
// from a real tag. Anchoring on the SUFFIX rather than the `v0.0.0-`
// prefix is what makes both forms match — `v0.0.0-<ts>-<commit>` and
// `vX.Y.Z-0.<ts>-<commit>` alike — without mistaking an ordinary
// pre-release tag such as `v1.2.3-rc1` for one.
//
// A trailing `+dirty` (forge's own marker for a build with uncommitted
// changes) is tolerated: it makes the version LESS fetchable, not more.
var pseudoVersionRE = regexp.MustCompile(`-(?:[\w.]+\.)?[0-9]{14}-[0-9a-f]{12}(?:\+.*)?$`)

// IsPseudoVersion reports whether v is a Go pseudo-version: a version
// synthesised for a commit that no tag names, which therefore no module
// proxy can serve.
func IsPseudoVersion(v string) bool {
	return pseudoVersionRE.MatchString(v)
}
