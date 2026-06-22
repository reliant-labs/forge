package buildinfo

import "testing"

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
