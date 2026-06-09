// Tests for `forge generate accept-fork <file>...` — see accept_fork.go
// for the friction rationale (cp-forge 2026-06-08, 11 long-accepted
// forks repeatedly re-reported).
//
// The tests build a synthetic project root (forge.yaml + .forge/
// checksums.json), chdir into it, and drive runAcceptFork directly.
// We avoid spawning the cobra binary so the assertions can read /
// re-decode the checksum file post-write without any process boundary.
package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// TestAcceptFork_FlipsAcceptedFlagAndPersists is the happy path: a
// forked Tier-1 entry has its Accepted flag flipped to true; the
// Forked flag is preserved (accept-fork silences the warning without
// changing the fork status).
func TestAcceptFork_FlipsAcceptedFlagAndPersists(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {
				Hash:     "abc123",
				History:  []string{"abc123"},
				Tier:     1,
				Forked:   true,
				Accepted: false,
			},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runAcceptFork([]string{"pkg/app/bootstrap.go"}, false); err != nil {
		t.Fatalf("runAcceptFork: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load checksums: %v", err)
	}
	entry, ok := got.Files["pkg/app/bootstrap.go"]
	if !ok {
		t.Fatal("entry vanished from .forge/checksums.json after accept-fork")
	}
	if !entry.Accepted {
		t.Errorf("Accepted not set true after accept-fork; got entry=%+v", entry)
	}
	// Forked MUST remain true — accept-fork silences a fork, doesn't unfork it.
	if !entry.Forked {
		t.Errorf("Forked changed after accept-fork; expected unchanged true; got entry=%+v", entry)
	}
	// Hash + History should NOT have been touched.
	if entry.Hash != "abc123" {
		t.Errorf("Hash changed after accept-fork: got %q want %q", entry.Hash, "abc123")
	}
}

// TestAcceptFork_BulkAcceptMultipleEntries pins the documented use
// case: cp-forge has 11 long-accepted forks; this command needs to
// flip all 11 in one shot.
func TestAcceptFork_BulkAcceptMultipleEntries(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go":         {Hash: "h1", Tier: 1, Forked: true},
			"pkg/app/wire_gen.go":          {Hash: "h2", Tier: 1, Forked: true},
			"pkg/app/migrate.go":           {Hash: "h3", Tier: 1, Forked: true},
			"pkg/app/bootstrap_testing.go": {Hash: "h4", Tier: 1, Forked: true},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	args := []string{
		"pkg/app/bootstrap.go",
		"pkg/app/wire_gen.go",
		"pkg/app/migrate.go",
		"pkg/app/bootstrap_testing.go",
	}
	if err := runAcceptFork(args, false); err != nil {
		t.Fatalf("runAcceptFork: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	for _, p := range args {
		if !got.Files[p].Accepted {
			t.Errorf("%s: Accepted not set true after bulk accept-fork; got %+v", p, got.Files[p])
		}
		if !got.Files[p].Forked {
			t.Errorf("%s: Forked changed after accept-fork", p)
		}
	}
}

// TestAcceptFork_RejectsUnknownPath covers "refuses if the file isn't
// in the checksums map".
func TestAcceptFork_RejectsUnknownPath(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1, Forked: true},
		},
	}
	withUnforkProjectRoot(t, cs)

	err := runAcceptFork([]string{"not/a/real/path.go"}, false)
	if err == nil {
		t.Fatal("expected error for unknown path; got nil")
	}
	if !strings.Contains(err.Error(), "not in .forge/checksums.json") {
		t.Errorf("error message should explain the path is untracked; got %q", err.Error())
	}
}

// TestAcceptFork_RejectsNotForked covers "refuses on a non-forked
// path". The user almost certainly meant `forge generate --accept` on
// a hand-edited Tier-1 file — accept-fork is for ALREADY-forked
// entries that you want to silence.
func TestAcceptFork_RejectsNotForked(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1, Forked: false},
		},
	}
	withUnforkProjectRoot(t, cs)

	err := runAcceptFork([]string{"pkg/app/bootstrap.go"}, false)
	if err == nil {
		t.Fatal("expected error for non-forked path; got nil")
	}
	if !strings.Contains(err.Error(), "not currently marked `forked: true`") {
		t.Errorf("error should explain the not-forked restriction; got %q", err.Error())
	}
}

// TestAcceptFork_DryRunPreservesState pins --dry-run: targets are
// listed but on-disk checksums file is untouched.
func TestAcceptFork_DryRunPreservesState(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1, Forked: true, Accepted: false},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	if err := runAcceptFork([]string{"pkg/app/bootstrap.go"}, true); err != nil {
		t.Fatalf("runAcceptFork --dry-run: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Files["pkg/app/bootstrap.go"].Accepted {
		t.Errorf("--dry-run mutated Accepted on disk; expected unchanged")
	}
}

// TestAcceptFork_IdempotentOnAlreadyAccepted pins the "running twice
// is harmless" contract: an already-accepted entry is a no-op, the
// caller gets a friendly skip line rather than an error.
func TestAcceptFork_IdempotentOnAlreadyAccepted(t *testing.T) {
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1, Forked: true, Accepted: true},
		},
	}
	withUnforkProjectRoot(t, cs)

	if err := runAcceptFork([]string{"pkg/app/bootstrap.go"}, false); err != nil {
		t.Fatalf("runAcceptFork on already-accepted entry should not error; got %v", err)
	}
}

// TestAcceptFork_QuietsReportForkedSkips pins the end-to-end contract:
// after accept-fork flips Accepted=true, the next reportForkedSkips
// pass over the SAME path is silent. This is the bug-12 acceptance
// criterion ("subsequent forge generate should print Code generation
// complete with NO forked-skip warnings").
func TestAcceptFork_QuietsReportForkedSkips(t *testing.T) {
	defer checksums.ResetSkipWrite()

	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/bootstrap.go": {Hash: "abc", Tier: 1, Forked: true, Accepted: false},
		},
	}
	withUnforkProjectRoot(t, cs)

	// Accept-fork the path.
	if err := runAcceptFork([]string{"pkg/app/bootstrap.go"}, false); err != nil {
		t.Fatalf("runAcceptFork: %v", err)
	}

	// Re-load to mirror what a subsequent `forge generate` would see.
	reloaded, err := checksums.Load(".")
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}

	// Simulate the silent-skip recording the next generate pipeline would do.
	checksums.SkippedForkedThisRun = []string{"pkg/app/bootstrap.go"}

	buf, restore := captureStderr(t)
	reportForkedSkips(reloaded)
	restore()

	if got := buf.String(); got != "" {
		t.Errorf("expected silent reportForkedSkips after accept-fork; got %q", got)
	}
}
