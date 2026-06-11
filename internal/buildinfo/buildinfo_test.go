package buildinfo

import "testing"

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
