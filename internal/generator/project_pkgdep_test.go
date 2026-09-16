package generator

import (
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// restoreBuildinfo puts the process-global version stamp back to its zero
// state. buildinfo has no Clear for Set, and these tests are not parallel
// because of it.
func restoreBuildinfo(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { buildinfo.Set("dev", "", "unknown") })
}

// TestResolveForgeVersion_ReleasePin: a released binary pins its own version,
// which is the whole point of one module — the version that generated the
// code and the version the code compiles against are the same number.
func TestResolveForgeVersion_ReleasePin(t *testing.T) {
	restoreBuildinfo(t)
	buildinfo.Set("v0.3.0", "", "")

	if got := resolveForgeVersion(); got != "v0.3.0" {
		t.Errorf("resolveForgeVersion = %q, want v0.3.0", got)
	}
}

// TestResolveForgeVersion_UntaggedCommitPinsPseudoVersion: `go install
// .../cmd/forge@main` records a pseudo-version, which IS proxy-resolvable and
// is what control-plane's commit-pinning mode depends on. It must be pinned
// verbatim, not rounded to a tag.
func TestResolveForgeVersion_UntaggedCommitPinsPseudoVersion(t *testing.T) {
	restoreBuildinfo(t)
	const pseudo = "v0.1.16-0.20260916085636-c01e07ec6ef2"
	buildinfo.Set(pseudo, "", "")

	if got := resolveForgeVersion(); got != pseudo {
		t.Errorf("resolveForgeVersion = %q, want the pseudo-version %q", got, pseudo)
	}
}

// TestResolveForgeVersion_UnreleasableBuildPinsNothing is the regression this
// whole design exists for. A dirty local build's bytes are on no proxy, so
// there is no honest version to require. It used to fall back to a
// hand-maintained "last published tag" constant, which told projects to pin a
// release that could not satisfy the code being generated —
// `undefined: testkit.StubNotConfigured`, after the tree was already
// rewritten.
//
// "" means "this build needs a source bridge". Anything else here is a bug.
func TestResolveForgeVersion_UnreleasableBuildPinsNothing(t *testing.T) {
	restoreBuildinfo(t)

	for _, v := range []string{
		"v0.1.16-0.20260916085636-c01e07ec6ef2+dirty", // dirty tree
		"dev",         // no stamp at all
		"(devel)",     // plain `go build`
		"v0.1.15+dev", // the VERSION-file floor
	} {
		t.Run(v, func(t *testing.T) {
			buildinfo.Set(v, "", "")
			if got := resolveForgeVersion(); got != "" {
				t.Errorf("resolveForgeVersion = %q for build %q, want \"\" — pinning a version "+
					"this build is not is what produced the StubNotConfigured class of failure", got, v)
			}
		})
	}
}
