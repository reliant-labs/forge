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

// seedMergeFixture creates a forked Tier-1 entry at rel with the given
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

func TestUnforkMerge_CleanMergeUnforks(t *testing.T) {
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
	if entry.Forked {
		t.Errorf("clean merge did not unfork the entry")
	}
	if entry.ForkedAt != "" {
		t.Errorf("ForkedAt not cleared: %q", entry.ForkedAt)
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

func TestUnforkMerge_ConflictKeepsFork(t *testing.T) {
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

	// Entry stays forked; side renders stay parked for a retry.
	cs, lerr := checksums.Load(root)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !cs.Files[rel].Forked {
		t.Errorf("conflicted merge must NOT unfork the entry")
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

// TestUnforkMerge_MissingSideRenders covers pre-existing forks (created
// before side renders existed): the user is told to run forge generate
// once to produce base + theirs.
func TestUnforkMerge_MissingSideRenders(t *testing.T) {
	requireGit(t)
	const rel = "pkg/app/wire_gen.go"
	root := seedMergeFixture(t, rel, []byte("package app // fork\n"), nil, nil)
	_ = root

	err := runUnforkMerge([]string{rel})
	if err == nil {
		t.Fatal("expected error for missing side renders; got nil")
	}
	if !strings.Contains(err.Error(), "forge generate") {
		t.Errorf("error should tell the user to run forge generate once; got %q", err.Error())
	}
}

func TestUnforkMerge_RejectsNonForkedPath(t *testing.T) {
	requireGit(t)
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			"pkg/app/wire_gen.go": {Hash: "abc", Tier: 1, Forked: false},
		},
	}
	withUnforkProjectRoot(t, cs)

	err := runUnforkMerge([]string{"pkg/app/wire_gen.go"})
	if err == nil {
		t.Fatal("expected error for non-forked path; got nil")
	}
	if !strings.Contains(err.Error(), "not forked") {
		t.Errorf("error should explain the path is not forked; got %q", err.Error())
	}
}

// TestUnfork_FoldsResolvedContentIntoHistory pins the post-conflict
// flow: after the user resolves `unfork --merge` markers, plain
// `forge unfork <path>` records the resolved on-disk content so the
// next generate's drift guard auto-heals over it instead of bouncing
// the user back to --accept.
func TestUnfork_FoldsResolvedContentIntoHistory(t *testing.T) {
	const rel = "pkg/app/wire_gen.go"
	cs := &checksums.FileChecksums{
		Files: map[string]checksums.FileChecksumEntry{
			rel: {Hash: "fork-time-hash", History: []string{"fork-time-hash"}, Tier: 1, Forked: true},
		},
	}
	root := withUnforkProjectRoot(t, cs)

	resolved := []byte("package app // resolved after merge\n")
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, resolved, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runUnfork([]string{rel}, false, false, false); err != nil {
		t.Fatalf("runUnfork: %v", err)
	}

	got, err := checksums.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := got.Files[rel]
	if entry.Forked {
		t.Errorf("still forked")
	}
	if entry.Hash != checksums.Hash(resolved) {
		t.Errorf("resolved content not folded into history (hash=%s)", entry.Hash)
	}
	if got.IsFileModified(root, rel) {
		t.Errorf("drift guard would still flag the resolved content as modified")
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
