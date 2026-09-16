package buildinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

func TestNextPatch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v0.1.15", "v0.1.16"},
		{"v1.2.3", "v1.2.4"},
		{"v0.0.0", "v0.0.1"},
		{"v1.2.9", "v1.2.10"},
		// Not a clean vX.Y.Z: incrementing a prerelease or a build-metadata
		// tag is ambiguous, so the caller falls back to v0.0.0 instead.
		{"v1.2.3-rc.1", ""},
		{"v1.2.3+meta", ""},
		{"1.2.3", ""},
		{"latest", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := nextPatch(c.in); got != c.want {
			t.Errorf("nextPatch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fixtureRepo builds a throwaway git repo with one commit, optionally tagged.
func fixtureRepo(t *testing.T, tag string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "fixture")
	if tag != "" {
		run("tag", "-a", tag, "-m", tag)
	}
	return dir
}

// TestDeriveGitVersion_TaggedSortsBetweenReleases is the property the whole
// change exists for: a commit after vX.Y.Z must order strictly after it and
// strictly before vX.Y.(Z+1).
//
// Getting the FORM wrong breaks this silently. `git describe`'s natural
// output for the same commit is `v0.1.15-3-gabc123`, which reads as a
// PRE-release of v0.1.15 and therefore sorts BEFORE the tag it is three
// commits ahead of — the exact inversion this test would catch.
func TestDeriveGitVersion_TaggedSortsBetweenReleases(t *testing.T) {
	got := deriveGitVersion(fixtureRepo(t, "v0.1.15"))
	if got == "" {
		t.Fatal("deriveGitVersion returned empty for a clean tagged repo")
	}
	assertUsableVersion(t, got)
	if semver.Compare(got, "v0.1.15") <= 0 {
		t.Errorf("%q must sort AFTER v0.1.15", got)
	}
	if semver.Compare(got, "v0.1.16") >= 0 {
		t.Errorf("%q must sort BEFORE v0.1.16", got)
	}
	if !IsDevVersion(got) {
		t.Errorf("%q should read as a dev version", got)
	}
}

// TestDeriveGitVersion_IsNeverInstallable is the regression for a CI outage.
//
// A derived version exists for ORDERING and IDENTITY. It is not a pinnable
// reference: this function only runs when build info says "(devel)" — a local
// source build or a workspace-embedded one — and neither is on any module
// proxy. The commit it names may not even be pushed.
//
// Emitted bare, InstallableVersion() handed it back and scaffolds wrote
// `require github.com/reliant-labs/forge v0.0.0-...-abefea71` into go.mod, a
// commit nothing could resolve. In CI it was guaranteed: a tagless shallow
// clone (hence the v0.0.0 base) on an ephemeral merge commit that exists on no
// remote. Every scaffold-and-build job failed with "invalid version: unknown
// revision".
func TestDeriveGitVersion_IsNeverInstallable(t *testing.T) {
	for name, tag := range map[string]string{"tagged": "v0.1.15", "untagged": ""} {
		t.Run(name, func(t *testing.T) {
			got := deriveGitVersion(fixtureRepo(t, tag))
			if got == "" {
				t.Fatal("deriveGitVersion returned empty")
			}
			if installableVersionRE.MatchString(got) {
				t.Errorf("deriveGitVersion = %q matches the installable-ref pattern — a scaffold "+
					"would pin a commit no proxy can resolve", got)
			}
			if !strings.Contains(got, "+") {
				t.Errorf("deriveGitVersion = %q carries no build metadata; the \"+\" is what keeps "+
					"InstallableVersion and IsDevVersion honest about an unpublishable build", got)
			}
		})
	}
}

// trimBuild drops semver build metadata, so the pseudo-version SHAPE can be
// checked independently of the "+dev"/"+dirty" marking.
func trimBuild(v string) string {
	if i := strings.Index(v, "+"); i >= 0 {
		return v[:i]
	}
	return v
}

// TestDeriveGitVersion_UntaggedUsesTheZeroForm pins the OTHER pseudo-version
// form, and it is not cosmetic.
//
// After a tag the form is `vX.Y.(Z+1)-0.<ts>-<sha>` — the `-0.` making it a
// prerelease of the next patch. With no tag there is nothing to be "after",
// and reusing that form yields `v0.0.0-0.<ts>-<sha>`, which claims to precede
// v0.0.0. The go command rejects it:
//
//	invalid pseudo-version: version before v0.0.0 would have negative patch
//
// A tagless SHALLOW CLONE is the ordinary way to reach this, so CI hit it on
// every run while every developer machine took the tagged branch. The first
// version of this test asserted module.IsPseudoVersion, which only checks
// SHAPE — and the invalid form is shaped correctly. Hence assertUsableVersion.
func TestDeriveGitVersion_UntaggedUsesTheZeroForm(t *testing.T) {
	got := deriveGitVersion(fixtureRepo(t, ""))
	if got == "" {
		t.Fatal("deriveGitVersion returned empty for a clean untagged repo")
	}
	assertUsableVersion(t, got)
	if strings.HasPrefix(trimBuild(got), "v0.0.0-0.") {
		t.Errorf("deriveGitVersion = %q uses the after-a-tag form with no tag; "+
			"v0.0.0-0.… claims to precede v0.0.0 and the go command refuses it", got)
	}
	if semver.Compare(got, "v0.0.1") >= 0 {
		t.Errorf("deriveGitVersion = %q, want a v0.0.0-based pseudo-version", got)
	}
}

// assertUsableVersion checks what the go command checks, not merely that the
// string is pseudo-version-SHAPED.
//
// Finding the right gate took two tries, which is the point: both
// module.IsPseudoVersion AND module.Check ACCEPT the invalid
// v0.0.0-0.<ts>-<sha> form. module.PseudoVersionBase is the one that refuses
// it, and it reports the same words the go command does — "version before
// v0.0.0 would have negative patch number".
func assertUsableVersion(t *testing.T, v string) {
	t.Helper()
	base := trimBuild(v)
	if !module.IsPseudoVersion(base) {
		t.Fatalf("%q is not a Go pseudo-version", v)
	}
	if _, err := module.PseudoVersionBase(base); err != nil {
		t.Fatalf("%q is not a version the go command accepts: %v", v, err)
	}
}

// A dirty tree is not the commit it names, and both IsDevVersion and
// InstallableVersion key on the "+" to stay honest about that.
func TestDeriveGitVersion_DirtyIsMarked(t *testing.T) {
	dir := fixtureRepo(t, "v0.1.15")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := deriveGitVersion(dir)
	if got == "" {
		t.Fatal("deriveGitVersion returned empty for a dirty repo")
	}
	if !hasSuffix(got, "+dirty") {
		t.Errorf("deriveGitVersion = %q, want a +dirty suffix for a modified tree", got)
	}
	// Still ordered with its commit: semver ignores build metadata.
	if semver.Compare(got, "v0.1.15") <= 0 {
		t.Errorf("%q must still sort after v0.1.15", got)
	}
	if installableVersionRE.MatchString(got) {
		t.Errorf("%q must not be installable — no proxy can serve a dirty tree", got)
	}
}

// Not a repo, and no root at all: return "" rather than inventing a version.
// The caller's whole contract is that "" means "fall through", and a guess
// here is what the old VERSION-file floor did wrong.
func TestDeriveGitVersion_RefusesToGuess(t *testing.T) {
	if got := deriveGitVersion(""); got != "" {
		t.Errorf("deriveGitVersion(\"\") = %q, want \"\"", got)
	}
	if got := deriveGitVersion(t.TempDir()); got != "" {
		t.Errorf("deriveGitVersion(non-repo) = %q, want \"\"", got)
	}
}

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
