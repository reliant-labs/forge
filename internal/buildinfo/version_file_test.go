package buildinfo

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// workspaceEmbeddedInfo reproduces the exact shape from the briefing: a host
// binary (reliant) built with go.work `use ../forge`, so the forge
// dependency resolves from source and reports "(devel)" instead of a real
// module-cache version.
func workspaceEmbeddedInfo() *debug.BuildInfo {
	return &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/reliant-labs/reliant", Version: "v1.7.1+dirty"},
		Deps: []*debug.Module{{Path: forgeCmdModulePath, Version: "(devel)"}},
	}
}

// TestVersionFromInfo_WorkspaceEmbeddedFallsBackToVersionFileFloor pins the
// fix for the go.work embedding defect. Before the fix this fell all the way
// through to the bare "dev" sentinel, which upgrade.go then used as a
// meaningless upgrade target and bumpForgeVersion refused to pin.
func TestVersionFromInfo_WorkspaceEmbeddedFallsBackToVersionFileFloor(t *testing.T) {
	got := versionFromInfo(workspaceEmbeddedInfo(), "dev")

	if got == "dev" {
		t.Fatalf("versionFromInfo = %q, want the VERSION file floor, not the bare dev sentinel", got)
	}
	if got != "v0.1.1+dev" {
		t.Errorf("versionFromInfo = %q, want the embedded VERSION file (v0.1.1) marked as a dev build (+dev)", got)
	}
}

// TestVersionFromInfo_LdflagsStillWinsOverVersionFile pins tier 1: a release
// build's explicit stamp must never be shadowed by the new file-based floor.
func TestVersionFromInfo_LdflagsStillWinsOverVersionFile(t *testing.T) {
	got := versionFromInfo(workspaceEmbeddedInfo(), "v9.9.9")
	if got != "v9.9.9" {
		t.Errorf("versionFromInfo = %q, want the ldflags stamp v9.9.9 to win over the VERSION file floor", got)
	}
}

// TestVersionFromInfo_RealDepVersionStillWinsOverVersionFile pins tier 2: a
// genuinely resolvable forge dep version (the ordinary `go get` case, no
// go.work involved) must never be shadowed by the file-based floor.
func TestVersionFromInfo_RealDepVersionStillWinsOverVersionFile(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/reliant-labs/reliant", Version: "v1.7.1"},
		Deps: []*debug.Module{{Path: forgeCmdModulePath, Version: "v0.0.3"}},
	}
	got := versionFromInfo(info, "dev")
	if got != "v0.0.3" {
		t.Errorf("versionFromInfo = %q, want the real forge dep version v0.0.3 to win over the VERSION file floor", got)
	}
}

// TestIsDevBuild_WorkspaceEmbeddedStaysDevAfterVersionFloor pins that the
// new floor is a REPORTING fallback only — it must never leak into the
// dev/release classification. A workspace build must still be a dev build:
// the scaffolder gates writing a local-forge go.work bridge on this.
func TestIsDevBuild_WorkspaceEmbeddedStaysDevAfterVersionFloor(t *testing.T) {
	info := workspaceEmbeddedInfo()
	dep, embedded := forgeModuleDep(info)
	if !embedded {
		t.Fatal("forge must be detected as embedded")
	}
	if !isDevBuildFrom(dep.Version, false) {
		t.Error("a go.work-embedded forge ((devel) dep) must still classify as a dev build")
	}
}

// TestInstallableVersion_WorkspaceFloorStaysUninstallable pins that the
// VERSION-file floor, being marked with "+dev" build metadata, is never
// mistaken for an installable ref: InstallableVersion must keep returning ""
// so generated CI falls back to pinning by git SHA (a "+dev" ref cannot be
// resolved by `go install ...@<ref>` from any module proxy).
func TestInstallableVersion_WorkspaceFloorStaysUninstallable(t *testing.T) {
	got := versionFromInfo(workspaceEmbeddedInfo(), "dev")
	if installableVersionRE.MatchString(got) {
		t.Errorf("VERSION-file floor %q must not match the installable-ref pattern", got)
	}
}

// TestBuildFrom_WorkspaceEmbeddedUsesVersionFloorAndStaysHonest covers the
// Build.String()/Describe() path (identity.go's buildFrom), the other
// consumer of the same "(devel)" dep shape.
func TestBuildFrom_WorkspaceEmbeddedUsesVersionFloorAndStaysHonest(t *testing.T) {
	b := buildFrom(workspaceEmbeddedInfo(), "", "")

	if b.Version == "(devel)" {
		t.Fatalf("Build.Version = %q, want the VERSION file floor, not the raw (devel) marker", b.Version)
	}
	if b.Version != "v0.1.1+dev" {
		t.Errorf("Build.Version = %q, want v0.1.1+dev", b.Version)
	}
	if s := b.String(); strings.Contains(s, "(devel)") {
		t.Errorf("String() = %q must not surface the raw (devel) marker to a user", s)
	}
}

// TestVersionFromFile pins the parsing/validation rules for the embedded
// VERSION file content: trims whitespace, accepts a clean release tag,
// rejects anything else (missing file, malformed content) by degrading to
// "" rather than trusting garbage into a reported version.
func TestVersionFromFile(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"clean tag", "v0.1.1", "v0.1.1"},
		{"trailing newline", "v0.1.1\n", "v0.1.1"},
		{"surrounding whitespace", "  v0.1.1  \n", "v0.1.1"},
		{"prerelease tag", "v1.2.3-rc.1", "v1.2.3-rc.1"},
		{"empty file", "", ""},
		{"missing v prefix", "0.1.1", ""},
		{"garbage", "not-a-version", ""},
		{"devel marker", "(devel)", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := versionFromFile(c.raw); got != c.want {
				t.Errorf("versionFromFile(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestWithDevFloorSuffix pins the "+dev" marking convention: it must never
// double up on existing build metadata, and must leave "" alone.
func TestWithDevFloorSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v0.1.1", "v0.1.1+dev"},
		{"", ""},
		{"v0.1.1+dirty", "v0.1.1+dirty"},
	}
	for _, c := range cases {
		if got := withDevFloorSuffix(c.in); got != c.want {
			t.Errorf("withDevFloorSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEmbeddedVersionFileMatchesSource mirrors internal/assets'
// TestEmbeddedForgeProtoMatchesSource: this package's embedded copy of
// VERSION must stay byte-identical to the repo-root source of truth, since
// go:embed cannot reach outside its own package directory to read it
// directly. Sync with:
//
//	cp VERSION internal/buildinfo/VERSION
func TestEmbeddedVersionFileMatchesSource(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/buildinfo -> ../../ -> repo root
	sourcePath := filepath.Join(cwd, "..", "..", "VERSION")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read source VERSION at %s: %v", sourcePath, err)
	}
	if !bytes.Equal(source, []byte(embeddedVersionFile)) {
		t.Fatalf("embedded VERSION (%q) is out of sync with source VERSION (%q) — "+
			"sync with: cp VERSION internal/buildinfo/VERSION", embeddedVersionFile, string(source))
	}
}
