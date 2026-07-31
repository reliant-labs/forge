package generator

import (
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// A release-stamped forge binary pins the published forge/pkg version it was
// released against — a clean require, no replace, no vendoring.
func TestResolveForgePkgVersion_ReleasePin(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion("") })
	buildinfo.SetPkgVersion("v0.3.0")

	if got := resolveForgePkgVersion(); got != "v0.3.0" {
		t.Errorf("resolveForgePkgVersion = %q, want v0.3.0", got)
	}
}

// A dev forge binary (no ldflags stamp) falls back to the latest published
// tag. There is NO replace and NO vendoring — a maintainer building against
// unpublished forge/pkg bridges with a gitignored go.work, handled outside
// forge.
func TestResolveForgePkgVersion_DevFallsBackToPublished(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion("") })
	buildinfo.SetPkgVersion("") // dev build

	if got := resolveForgePkgVersion(); got != defaultPublishedForgePkgVersion {
		t.Errorf("resolveForgePkgVersion = %q, want %q (default published tag)", got, defaultPublishedForgePkgVersion)
	}
}

// The resolved version is always a canonical semver pin — never a bare
// placeholder like v0.0.0 that would need a replace to resolve.
func TestResolveForgePkgVersion_NeverPlaceholder(t *testing.T) {
	t.Cleanup(func() { buildinfo.SetPkgVersion("") })
	buildinfo.SetPkgVersion("")

	if got := resolveForgePkgVersion(); got == "" || got == "v0.0.0" {
		t.Errorf("resolveForgePkgVersion = %q, want a concrete published version", got)
	}
}
