// Tests for `forge unfork <file>` — see unfork.go for the friction
// rationale (cp-forge port-workers, 2026-06-03).
//
// The tests build a synthetic project root (forge.yaml + .forge/
// checksums.json), chdir into it, and drive runUnfork directly. We
// avoid spawning the cobra binary so the assertions can read /
// re-decode the checksum file post-write without any process boundary.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// withUnforkProjectRoot creates a tmpdir with a forge.yaml stub and a
// `.forge/checksums.json` matching cs, chdirs into it for the duration
// of the test, and returns the absolute project root path. Restores the
// caller's cwd on cleanup.
func withUnforkProjectRoot(t *testing.T, cs *checksums.FileChecksums) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "forge.yaml"), []byte("name: test\n"), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	if err := checksums.Save(root, cs); err != nil {
		t.Fatalf("seed checksums: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prev)
	})
	return root
}

// TestUnfork_FlipsForkedFlagAndPersists is the happy path: a single
// forked Tier-1 entry, named explicitly, gets Forked=false written to
// the on-disk checksums file.
func TestUnfork_FlipsForkedFlagAndPersists(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {
				Hash:    "abc123",
				History: []string{"abc123"},
				Tier:    1,
				Forked:  true,
			},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runUnfork([]string{"pkg/app/bootstrap.go"}, false, false, false); err != nil {
		t.Fatalf("runUnfork: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load checksums: %v", err)
	}
	entry, ok := got.Files["pkg/app/bootstrap.go"]
	if !ok {
		t.Fatal("entry vanished from .forge/checksums.json after unfork")
	}
	if entry.Forked {
		t.Errorf("Forked still true after unfork; got entry=%+v", entry)
	}
	// Hash + History should NOT have been touched.
	if entry.Hash != "abc123" {
		t.Errorf("Hash changed after unfork: got %q want %q", entry.Hash, "abc123")
	}
	if len(entry.History) != 1 || entry.History[0] != "abc123" {
		t.Errorf("History mutated after unfork: got %v want [abc123]", entry.History)
	}
}

// TestUnfork_RejectsUnknownPath covers the "refuses if the file doesn't
// exist in the checksums map" requirement.
func TestUnfork_RejectsUnknownPath(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1, Forked: true},
		},
	}
	withUnforkProjectRoot(t, cs)

	err := runUnfork([]string{"not/a/real/path.go"}, false, false, false)
	if err == nil {
		t.Fatal("expected error for unknown path; got nil")
	}
	if !strings.Contains(err.Error(), "not in .forge/checksums.json") {
		t.Errorf("error message should explain the path is untracked; got %q", err.Error())
	}
}

// TestUnfork_RejectsTier2 pins the "refuses if the file isn't a Tier-1
// generated file" requirement. A Tier-2 (scaffold-once) entry has no
// fork notion — there's nothing to undo, and undoing it could mislead a
// user into expecting next generate to clobber a scaffold.
func TestUnfork_RejectsTier2(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"handlers/users/users.go": {Hash: "xyz", Tier: 2, Forked: true},
		},
	}
	withUnforkProjectRoot(t, cs)

	err := runUnfork([]string{"handlers/users/users.go"}, false, false, false)
	if err == nil {
		t.Fatal("expected error for Tier-2 path; got nil")
	}
	if !strings.Contains(err.Error(), "Tier-1") {
		t.Errorf("error should explain Tier-1 restriction; got %q", err.Error())
	}
}

// TestUnfork_DryRunPreservesState exercises the --dry-run flag: targets
// are printed but the on-disk checksum file is untouched.
func TestUnfork_DryRunPreservesState(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", History: []string{"abc"}, Tier: 1, Forked: true},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runUnfork([]string{"pkg/app/bootstrap.go"}, true, false, false); err != nil {
		t.Fatalf("runUnfork --dry-run: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	entry := got.Files["pkg/app/bootstrap.go"]
	if !entry.Forked {
		t.Errorf("--dry-run mutated Forked flag on disk; expected unchanged")
	}
}

// TestUnfork_AllFlipsEveryForkedEntry pins the --all flag: every
// currently-forked entry has its Forked flag cleared. --yes suppresses
// the interactive confirm so the test doesn't deadlock on stdin.
func TestUnfork_AllFlipsEveryForkedEntry(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "a", Tier: 1, Forked: true},
			"pkg/app/wire_gen.go":  {Hash: "b", Tier: 1, Forked: true},
			"pkg/app/testing.go":   {Hash: "c", Tier: 1, Forked: false}, // not forked
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runUnfork(nil, false, true, true); err != nil {
		t.Fatalf("runUnfork --all --yes: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	for path, want := range map[string]bool{
		"pkg/app/bootstrap.go": false, // was true, should now be false
		"pkg/app/wire_gen.go":  false, // was true, should now be false
		"pkg/app/testing.go":   false, // was already false
	} {
		if got.Files[path].Forked != want {
			t.Errorf("%s: Forked=%v want %v", path, got.Files[path].Forked, want)
		}
	}
}

// TestUnfork_AllAndArgsConflict rejects the user error where someone
// passes both --all and explicit path arguments. They're mutually
// exclusive — the implementation refuses up-front rather than letting
// --all silently win.
func TestUnfork_AllAndArgsConflict(t *testing.T) {
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
	withUnforkProjectRoot(t, cs)

	err := runUnfork([]string{"some/path.go"}, false, true, true)
	if err == nil {
		t.Fatal("expected error when both --all and paths are given; got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should explain --all + paths are mutually exclusive; got %q", err.Error())
	}
}

// TestUnfork_NoArgsAndNoAllErrors covers the empty-invocation case.
func TestUnfork_NoArgsAndNoAllErrors(t *testing.T) {
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
	withUnforkProjectRoot(t, cs)

	err := runUnfork(nil, false, false, false)
	if err == nil {
		t.Fatal("expected error when no paths and no --all; got nil")
	}
	if !strings.Contains(err.Error(), "no paths") {
		t.Errorf("error should explain a path or --all is required; got %q", err.Error())
	}
}

// TestUnfork_AllWithNoForkedEntries prints a friendly nothing-to-do
// line and exits cleanly. Important so `forge unfork --all` is safe to
// re-run in a script.
func TestUnfork_AllWithNoForkedEntries(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "a", Tier: 1, Forked: false},
		},
	}
	withUnforkProjectRoot(t, cs)

	if err := runUnfork(nil, false, true, true); err != nil {
		t.Fatalf("runUnfork --all on clean tree should succeed; got %v", err)
	}
}

// TestUnfork_TreatsLegacyTier0AsTier1 — pre-tier checksum entries have
// Tier=0 (the zero value) and were written by emitters that are
// Tier-1-equivalent. The stomp guard treats Tier=0 as Tier-1; unfork
// must do the same so a project upgraded across the tier introduction
// can still unfork old entries.
func TestUnfork_TreatsLegacyTier0AsTier1(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "a", Tier: 0, Forked: true},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runUnfork([]string{"pkg/app/bootstrap.go"}, false, false, false); err != nil {
		t.Fatalf("runUnfork on legacy Tier=0 entry: %v", err)
	}
	got, _ := checksums.Load(root)
	if got.Files["pkg/app/bootstrap.go"].Forked {
		t.Errorf("legacy Tier-0 entry not unforked")
	}
}

// TestUnforkCmd_FlagsAndHelp pins the cobra surface: the command exists,
// exposes --dry-run / --all / --yes, and the help text documents the
// scenario users reach for it in. A future refactor that drops one of
// these flags or the long-form rationale trips this test.
func TestUnforkCmd_FlagsAndHelp(t *testing.T) {
	cmd := newUnforkCmd()
	if cmd == nil {
		t.Fatal("newUnforkCmd returned nil")
	}
	for _, flag := range []string{"dry-run", "all", "yes"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("forge unfork is missing --%s flag", flag)
		}
	}
	long := strings.ToLower(cmd.Long)
	for _, kw := range []string{"unfork", "tier-1", "--accept", "scaffold-once"} {
		if !strings.Contains(long, strings.ToLower(kw)) {
			t.Errorf("forge unfork --help long text should mention %q so users can find this command from related concepts", kw)
		}
	}
}
