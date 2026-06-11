// Tests for `forge unfork <file>` — the legacy-fork MIGRATION tool
// (one release only; see unfork.go).
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

// TestUnfork_ConvertsLegacyForkToDisowned is the happy path: a single
// legacy forked entry, named explicitly, becomes a disowned Tier-2
// entry on disk.
func TestUnfork_ConvertsLegacyForkToDisowned(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {
				Hash:     "abc123",
				History:  []string{"abc123"},
				Tier:     1,
				Forked:   true,
				ForkedAt: "2026-01-02T00:00:00Z",
			},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runUnfork([]string{"pkg/app/bootstrap.go"}, false, false, false, false); err != nil {
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
	if entry.Forked || entry.Tier != 2 || !entry.Disowned {
		t.Errorf("entry = %+v, want converted to {tier:2 disowned:true forked:false}", entry)
	}
	// The legacy fork timestamp carries over as the disowned-since time.
	if entry.DisownedAt != "2026-01-02T00:00:00Z" {
		t.Errorf("DisownedAt = %q, want inherited ForkedAt", entry.DisownedAt)
	}
	// Hash + History should NOT have been touched.
	if entry.Hash != "abc123" {
		t.Errorf("Hash changed after unfork: got %q want %q", entry.Hash, "abc123")
	}
	if len(entry.History) != 1 || entry.History[0] != "abc123" {
		t.Errorf("History mutated after unfork: got %v want [abc123]", entry.History)
	}
}

// TestUnfork_ReadoptDeletesFileAndReturnsOwnership pins --readopt: the
// on-disk content is discarded and the entry returns to Tier-1 so the
// next `forge generate` re-emits the pristine render. Works on both
// legacy forked and disowned entries.
func TestUnfork_ReadoptDeletesFileAndReturnsOwnership(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "a", Tier: 1, Forked: true},
			"pkg/app/wire_gen.go":  {Hash: "b", Tier: 2, Disowned: true, DisownedAt: "2026-06-01T00:00:00Z"},
		},
	}
	root := withUnforkProjectRoot(t, cs)
	for _, rel := range []string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package app // user content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := runUnfork([]string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"}, false, false, false, true); err != nil {
		t.Fatalf("runUnfork --readopt: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	for _, rel := range []string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"} {
		e := got.Files[rel]
		if e.Tier != 1 || e.Disowned || e.Forked || e.DisownedAt != "" {
			t.Errorf("%s = %+v, want clean Tier-1 entry", rel, e)
		}
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s still on disk after --readopt (content must be discarded)", rel)
		}
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

	err := runUnfork([]string{"not/a/real/path.go"}, false, false, false, false)
	if err == nil {
		t.Fatal("expected error for unknown path; got nil")
	}
	if !strings.Contains(err.Error(), "not in .forge/checksums.json") {
		t.Errorf("error message should explain the path is untracked; got %q", err.Error())
	}
}

// TestUnfork_RejectsNonForkedEntry: plain unfork only converts LEGACY
// forked entries. Forge-owned and already-disowned entries are refused
// with pointers at the current commands (disown / delete+generate).
func TestUnfork_RejectsNonForkedEntry(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1},
			"pkg/app/wire_gen.go":  {Hash: "def", Tier: 2, Disowned: true},
		},
	}
	withUnforkProjectRoot(t, cs)

	for _, path := range []string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"} {
		err := runUnfork([]string{path}, false, false, false, false)
		if err == nil {
			t.Fatalf("%s: expected error for non-legacy-forked path; got nil", path)
		}
		if !strings.Contains(err.Error(), "legacy") {
			t.Errorf("%s: error should explain the legacy-fork restriction; got %q", path, err.Error())
		}
	}
}

// TestUnfork_ReadoptAcceptsDisownedOnly: --readopt on a forge-owned
// entry is refused (nothing to re-adopt).
func TestUnfork_ReadoptAcceptsDisownedOnly(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1},
		},
	}
	withUnforkProjectRoot(t, cs)

	err := runUnfork([]string{"pkg/app/bootstrap.go"}, false, false, false, true)
	if err == nil {
		t.Fatal("expected error for --readopt on a forge-owned path; got nil")
	}
	if !strings.Contains(err.Error(), "neither legacy-forked nor disowned") {
		t.Errorf("error should explain there is nothing to re-adopt; got %q", err.Error())
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

	if err := runUnfork([]string{"pkg/app/bootstrap.go"}, true, false, false, false); err != nil {
		t.Fatalf("runUnfork --dry-run: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	entry := got.Files["pkg/app/bootstrap.go"]
	if !entry.Forked || entry.Disowned {
		t.Errorf("--dry-run mutated the entry on disk; got %+v", entry)
	}
}

// TestUnfork_AllConvertsEveryLegacyFork pins the --all flag: every
// legacy forked entry converts to disowned. --yes suppresses the
// interactive confirm so the test doesn't deadlock on stdin.
func TestUnfork_AllConvertsEveryLegacyFork(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "a", Tier: 1, Forked: true},
			"pkg/app/wire_gen.go":  {Hash: "b", Tier: 1, Forked: true},
			"pkg/app/testing.go":   {Hash: "c", Tier: 1}, // forge-owned, untouched
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runUnfork(nil, false, true, true, false); err != nil {
		t.Fatalf("runUnfork --all --yes: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	for _, path := range []string{"pkg/app/bootstrap.go", "pkg/app/wire_gen.go"} {
		e := got.Files[path]
		if e.Forked || !e.Disowned || e.Tier != 2 {
			t.Errorf("%s: got %+v, want converted to disowned", path, e)
		}
	}
	if e := got.Files["pkg/app/testing.go"]; e.Disowned || e.Tier != 1 {
		t.Errorf("testing.go: got %+v, want untouched Tier-1", e)
	}
}

// TestUnfork_AllAndArgsConflict rejects the user error where someone
// passes both --all and explicit path arguments.
func TestUnfork_AllAndArgsConflict(t *testing.T) {
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
	withUnforkProjectRoot(t, cs)

	err := runUnfork([]string{"some/path.go"}, false, true, true, false)
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

	err := runUnfork(nil, false, false, false, false)
	if err == nil {
		t.Fatal("expected error when no paths and no --all; got nil")
	}
	if !strings.Contains(err.Error(), "no paths") {
		t.Errorf("error should explain a path or --all is required; got %q", err.Error())
	}
}

// TestUnfork_AllWithNoLegacyForks prints a friendly nothing-to-do line
// and exits cleanly. Important so `forge unfork --all` is safe to
// re-run in a script.
func TestUnfork_AllWithNoLegacyForks(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "a", Tier: 1},
		},
	}
	withUnforkProjectRoot(t, cs)

	if err := runUnfork(nil, false, true, true, false); err != nil {
		t.Fatalf("runUnfork --all on clean tree should succeed; got %v", err)
	}
}

// TestUnforkCmd_FlagsAndHelp pins the cobra surface: the command exists,
// exposes --dry-run / --all / --yes / --readopt / --merge, and the help
// text frames it as one-release legacy-fork migration tooling.
func TestUnforkCmd_FlagsAndHelp(t *testing.T) {
	cmd := newUnforkCmd()
	if cmd == nil {
		t.Fatal("newUnforkCmd returned nil")
	}
	for _, flag := range []string{"dry-run", "all", "yes", "readopt", "merge"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("forge unfork is missing --%s flag", flag)
		}
	}
	long := strings.ToLower(cmd.Long)
	for _, kw := range []string{"migration", "legacy", "disown", "removed next release"} {
		if !strings.Contains(long, strings.ToLower(kw)) {
			t.Errorf("forge unfork --help long text should mention %q so users understand its migration-tool framing", kw)
		}
	}
}
