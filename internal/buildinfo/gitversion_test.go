package buildinfo

import (
	"os"
	"os/exec"
	"path/filepath"
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
	if !module.IsPseudoVersion(got) {
		t.Fatalf("deriveGitVersion = %q, want a Go pseudo-version", got)
	}
	if semver.Compare(got, "v0.1.15") <= 0 {
		t.Errorf("%q must sort AFTER v0.1.15", got)
	}
	if semver.Compare(got, "v0.1.16") >= 0 {
		t.Errorf("%q must sort BEFORE v0.1.16", got)
	}
	if IsDevVersion(got) != true {
		t.Errorf("%q should read as a dev version (it is a pseudo-version)", got)
	}
}

// An untagged repo has no release to be "after", so v0.0.0 is the base — the
// same choice the go command makes.
func TestDeriveGitVersion_UntaggedUsesZeroBase(t *testing.T) {
	got := deriveGitVersion(fixtureRepo(t, ""))
	if got == "" {
		t.Fatal("deriveGitVersion returned empty for a clean untagged repo")
	}
	if !module.IsPseudoVersion(got) {
		t.Fatalf("deriveGitVersion = %q, want a Go pseudo-version", got)
	}
	if semver.Major(got)+"."+semver.MajorMinor(got) == "" || semver.Compare(got, "v0.0.1") >= 0 {
		t.Errorf("deriveGitVersion = %q, want a v0.0.0-based pseudo-version", got)
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
