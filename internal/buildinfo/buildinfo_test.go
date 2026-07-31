package buildinfo

import "testing"

// TestIsDevBuildClassification pins the release-vs-dev discriminator the
// scaffolder relies on before ever writing a local-forge go.work. Only a
// clean, tagged semver from an unmodified tree counts as a release; "(devel)",
// pseudo-versions, dirty trees, and garbage are all dev.
func TestIsDevBuildClassification(t *testing.T) {
	cases := []struct {
		name        string
		mainVersion string
		vcsModified bool
		wantDev     bool
	}{
		{"release tag clean tree", "v1.2.3", false, false},
		{"prerelease tag clean tree", "v1.2.3-rc.1", false, false},
		{"release tag dirty tree is dev", "v1.2.3", true, true},
		{"devel marker is dev", "(devel)", false, true},
		{"empty version is dev", "", false, true},
		{"pseudo-version is dev", "v0.0.0-20260612070344-a3e3b883c97c", false, true},
		{"pseudo-version on tag base is dev", "v1.2.3-0.20260612070344-a3e3b883c97c", false, true},
		{"garbage is dev", "latest", false, true},
		{"missing v prefix is dev", "1.2.3", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDevBuildFrom(c.mainVersion, c.vcsModified); got != c.wantDev {
				t.Errorf("isDevBuildFrom(%q, modified=%v) = %v, want %v", c.mainVersion, c.vcsModified, got, c.wantDev)
			}
		})
	}
}

// TestIsDevBuildOverride pins the test seam: SetDevBuild wins over the ambient
// build info, and ClearDevBuild restores the real read.
func TestIsDevBuildOverride(t *testing.T) {
	t.Cleanup(ClearDevBuild)

	SetDevBuild(false)
	if IsDevBuild() {
		t.Error("SetDevBuild(false): IsDevBuild() = true, want false")
	}
	SetDevBuild(true)
	if !IsDevBuild() {
		t.Error("SetDevBuild(true): IsDevBuild() = false, want true")
	}
	ClearDevBuild()
	// After clearing, IsDevBuild reads the ambient test binary, which is
	// always "(devel)" under `go test` → dev.
	if !IsDevBuild() {
		t.Error("after ClearDevBuild: IsDevBuild() = false for a (devel) test binary, want true")
	}
}

// TestInstallableVersion pins the contract that InstallableVersion()
// only ever returns a ref `go install ...@<ref>` can resolve from a
// module proxy: a release tag or a clean pseudo-version, never a
// `+dirty` build (which fails every CI run — fr-8c8a24ea97). On a
// non-installable version it returns "" so the CI template falls back
// to pinning by git SHA.
func TestInstallableVersion(t *testing.T) {
	t.Cleanup(func() { Set("dev", "unknown", "unknown") })

	cases := []struct {
		name string
		set  string
		want string
	}{
		{"release tag", "v1.2.3", "v1.2.3"},
		{"prerelease tag", "v1.2.3-rc.1", "v1.2.3-rc.1"},
		{"clean pseudo-version", "v0.0.0-20260612070344-a3e3b883c97c", "v0.0.0-20260612070344-a3e3b883c97c"},
		{"dirty pseudo-version rejected", "v0.0.0-20260612070344-a3e3b883c97c+dirty", ""},
		{"dirty release rejected", "v1.2.3+dirty", ""},
		{"dev sentinel rejected", "dev", ""},
		{"missing v prefix rejected", "1.2.3", ""},
		{"garbage rejected", "latest", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Set a non-"dev" value so Version() returns it verbatim
			// (Version falls through to ReadBuildInfo only for ""/"dev").
			Set(c.set, "unknown", "deadbeef")
			if got := InstallableVersion(); got != c.want {
				t.Errorf("Set(%q): InstallableVersion() = %q, want %q", c.set, got, c.want)
			}
		})
	}
}

// TestPkgVersionValidation pins the contract that PkgVersion() only ever
// returns a value safe to write into a generated project's go.mod
// `require github.com/reliant-labs/forge/pkg <v>` directive: canonical
// semver or nothing. A mis-stamped release build must degrade to the
// dev flow ("" → .forge-pkg vendoring), never emit a broken require.
func TestPkgVersionValidation(t *testing.T) {
	t.Cleanup(func() { SetPkgVersion("") })

	cases := []struct {
		name string
		set  string
		want string
	}{
		{"dev default (empty)", "", ""},
		{"canonical release", "v0.3.0", "v0.3.0"},
		{"prerelease", "v1.2.3-rc.1", "v1.2.3-rc.1"},
		{"missing v prefix", "0.3.0", ""},
		{"pseudo-version accepted (valid require version)", "v0.0.0-20260610120000-abcdef123456", "v0.0.0-20260610120000-abcdef123456"},
		{"garbage rejected", "latest", ""},
		{"tag-with-prefix rejected", "pkg/v0.3.0", ""},
		{"build metadata rejected", "v1.0.0+dirty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			SetPkgVersion(c.set)
			if got := PkgVersion(); got != c.want {
				t.Errorf("SetPkgVersion(%q): PkgVersion() = %q, want %q", c.set, got, c.want)
			}
		})
	}
}

// TestIsDevVersion pins the identity half of forge versioning: given a
// version STRING (a forge.yaml pin, a report line), is this a release or a
// build somebody made locally? Ordering is a separate question answered by
// SemVer comparison — conflating the two is what made a locally-built forge's
// pseudo-version indistinguishable from an ancient project.
func TestIsDevVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		// Released tags.
		{"v0.0.3", false},
		{"v0.1.0", false},
		{"v1.4.2", false},
		{"0.0.3", false}, // leading "v" optional
		{"v1.0.0-rc.1", false},
		// Sentinels.
		{"", true},
		{"dev", true},
		{"(devel)", true},
		// Go pseudo-versions: an untagged commit, i.e. a build nobody
		// published.
		{"v0.0.0-20260430002332-8f05b089372c", true},
		{"v0.0.4-0.20260724212501-dfb85daf8474", true},
		// Build metadata is only ever stamped from a modified tree.
		{"v0.0.4-0.20260724212501-dfb85daf8474+dirty", true},
		{"v1.4.2+dirty", true},
		// Not a version at all.
		{"main", true},
		{"latest", true},
	}
	for _, tt := range tests {
		if got := IsDevVersion(tt.version); got != tt.want {
			t.Errorf("IsDevVersion(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}
