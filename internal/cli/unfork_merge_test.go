// Tests for `forge unfork --merge` — the three-way reconcile path.
// Layout mirrors unfork_test.go: synthetic project root, chdir, drive
// runUnforkMerge directly. Requires git on PATH (skipped otherwise);
// the production code degrades with a UserErr in that case.
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// seedMergeFixture creates a legacy forked Tier-1 entry at rel with the given
// on-disk (ours), merge-base, and latest-render (theirs) contents
// inside a fresh project root, then chdirs into it.
func seedMergeFixture(t *testing.T, rel string, ours, base, theirs []byte) string {
	t.Helper()
	cs := &checksums.FileChecksums{Files: map[string]checksums.FileChecksumEntry{}}
	cs.RecordFile(rel, ours)
	entry := cs.Files[rel]
	entry.Tier = 1
	entry.Forked = true
	entry.ForkedAt = "2026-06-01T00:00:00Z"
	cs.Files[rel] = entry
	root := withUnforkProjectRoot(t, cs)

	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, ours, 0o644); err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string][]byte{
		filepath.Join(root, checksums.RenderBaseDir, rel): base,
		filepath.Join(root, checksums.RenderDir, rel):     theirs,
	} {
		if content == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func TestUnforkMerge_CleanMergeReAdopts(t *testing.T) {
	requireGit(t)
	const rel = "pkg/app/wire_gen.go"
	base := []byte("package app\n\nfunc wireA() {}\n\nfunc wireB() {}\n")
	// Ours: user added a custom function at the bottom.
	ours := []byte("package app\n\nfunc wireA() {}\n\nfunc wireB() {}\n\nfunc userCustom() {}\n")
	// Theirs: template grew a new generated function at the top.
	theirs := []byte("package app\n\nfunc wireNew() {}\n\nfunc wireA() {}\n\nfunc wireB() {}\n")
	root := seedMergeFixture(t, rel, ours, base, theirs)

	if err := runUnforkMerge([]string{rel}); err != nil {
		t.Fatalf("runUnforkMerge: %v", err)
	}

	merged, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"func wireNew()", "func userCustom()"} {
		if !strings.Contains(string(merged), want) {
			t.Errorf("merged content missing %q:\n%s", want, merged)
		}
	}
	if strings.Contains(string(merged), "<<<<<<<") {
		t.Errorf("clean merge left conflict markers:\n%s", merged)
	}

	cs, err := checksums.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := cs.Files[rel]
	if entry.Forked || entry.Disowned || entry.Tier != 1 {
		t.Errorf("clean merge did not re-adopt the entry into forge ownership; got %+v", entry)
	}
	if entry.ForkedAt != "" || entry.DisownedAt != "" {
		t.Errorf("timestamps not cleared: %+v", entry)
	}
	// Merged content recorded so the next generate's drift guard treats
	// it as a known render state.
	if entry.Hash != checksums.Hash(merged) {
		t.Errorf("merged content not recorded (hash=%s)", entry.Hash)
	}
	// Side renders cleaned.
	for _, p := range []string{
		filepath.Join(root, checksums.RenderDir, rel),
		filepath.Join(root, checksums.RenderBaseDir, rel),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not cleaned after clean merge", p)
		}
	}
}

func TestUnforkMerge_ConflictSettlesAsDisowned(t *testing.T) {
	requireGit(t)
	const rel = "pkg/app/wire_gen.go"
	base := []byte("package app\n\nfunc wire() { old() }\n")
	ours := []byte("package app\n\nfunc wire() { userVersion() }\n")
	theirs := []byte("package app\n\nfunc wire() { templateVersion() }\n")
	root := seedMergeFixture(t, rel, ours, base, theirs)

	err := runUnforkMerge([]string{rel})
	if err == nil {
		t.Fatal("expected a conflict error; got nil")
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("error should mention conflicts; got %q", err.Error())
	}

	// Conflict markers written in place, both sides present.
	merged, rerr := os.ReadFile(filepath.Join(root, rel))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, want := range []string{"<<<<<<<", ">>>>>>>", "userVersion()", "templateVersion()"} {
		if !strings.Contains(string(merged), want) {
			t.Errorf("conflicted file missing %q:\n%s", want, merged)
		}
	}

	// Entry stays user-owned (a conflicted legacy fork settles as
	// disowned); side renders stay parked for manual inspection.
	cs, lerr := checksums.Load(root)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if e := cs.Files[rel]; !e.Disowned || e.Tier != 2 || e.Forked {
		t.Errorf("conflicted merge must settle the entry as disowned; got %+v", e)
	}
	for _, p := range []string{
		filepath.Join(root, checksums.RenderDir, rel),
		filepath.Join(root, checksums.RenderBaseDir, rel),
	} {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("side render %s removed despite conflict", p)
		}
	}
}

// TestUnforkMerge_MissingSideRenders covers legacy forks with no parked
// renders (or any fork after the parking machinery was removed): the
// merge inputs are gone for good, so the user is pointed at manual
// reconcile or --readopt instead of a re-run that can't help.
func TestUnforkMerge_MissingSideRenders(t *testing.T) {
	requireGit(t)
	const rel = "pkg/app/wire_gen.go"
	root := seedMergeFixture(t, rel, []byte("package app // fork\n"), nil, nil)
	_ = root

	err := runUnforkMerge([]string{rel})
	if err == nil {
		t.Fatal("expected error for missing side renders; got nil")
	}
	if !strings.Contains(err.Error(), "--readopt") {
		t.Errorf("error should point at the --readopt escape hatch; got %q", err.Error())
	}
}

func TestUnforkMerge_RejectsForgeOwnedPath(t *testing.T) {
	requireGit(t)
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/wire_gen.go": {Hash: "abc", Tier: 1},
		},
	}
	withUnforkProjectRoot(t, cs)

	err := runUnforkMerge([]string{"pkg/app/wire_gen.go"})
	if err == nil {
		t.Fatal("expected error for a forge-owned path; got nil")
	}
	if !strings.Contains(err.Error(), "neither legacy-forked nor disowned") {
		t.Errorf("error should explain there is nothing to reconcile; got %q", err.Error())
	}
}

// TestUnforkCmd_MergeFlag pins the cobra surface addition.
func TestUnforkCmd_MergeFlag(t *testing.T) {
	cmd := newUnforkCmd()
	if cmd.Flags().Lookup("merge") == nil {
		t.Fatal("forge unfork is missing --merge flag")
	}
	if !strings.Contains(strings.ToLower(cmd.Long), "--merge") {
		t.Errorf("forge unfork --help should document --merge")
	}
}
