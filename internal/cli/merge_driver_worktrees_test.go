package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Only forge's own entries that cannot merge through the pinned forge are
// stale: anything else in a config.worktree is someone's on purpose.
func TestStaleWorktreeDriverEntry(t *testing.T) {
	live := filepath.Join(t.TempDir(), "merge-driver", "f1e8f2ebdbb6", "forge")
	custom := filepath.Join(t.TempDir(), "custom-merge-tool")
	for _, bin := range []string{live, custom} {
		writeTreeFileT(t, bin, "#!/bin/sh\n")
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	key := "merge.forge-generated.driver"
	reg := "forge-merge-driver.61efb1e2ce8c.command"
	gone := shellQuote(filepath.Join(t.TempDir(), "merge-driver", "f1e8f2ebdbb6", "forge"))
	for _, tc := range []struct {
		name, key, value string
		want             bool
	}{
		{"bare command naming a gone forge", key, gone + " merge-generated %O %A %B %L %P", true},
		{"bare command from before %P", key, gone + " merge-generated %O %A %B %L", true},
		{"custom live command with forge-shaped argv", key, shellQuote(custom) + " merge-generated %O %A %B %L %P", false},
		{"custom live command from before %P", key, shellQuote(custom) + " merge-generated %O %A %B %L", false},
		{"unknown binary outside forge's copy layout", key, "'/gone/forge' merge-generated %O %A %B %L %P", false},
		{"bare command naming a live forge (predates the hand-off)", key, shellQuote(live) + " merge-generated %O %A %B %L %P", true},
		{"guarded command naming a gone forge", key, "[ -x " + gone + " ] && exec " + gone + " merge-generated %O %A %B %L %P", true},
		{"guarded command naming a live forge", key, driverCommandLine(live, "forge"), false},
		{"a driver forge never writes", key, "my-merge-tool %O %A %B", false},
		{"registration naming a gone forge", reg, "'/gone/forge' merge-generated %O %A %B %L %P", true},
		{"registration naming a live forge", reg, shellQuote(live) + " merge-generated %O %A %B %L %P", false},
		{"an unrelated key", "merge.other.driver", "'/gone/forge' merge-generated %O %A %B %L %P", false},
	} {
		if got := staleWorktreeDriverEntry(tc.key, tc.value); got != tc.want {
			t.Errorf("%s: staleWorktreeDriverEntry(%q, %q) = %v, want %v", tc.name, tc.key, tc.value, got, tc.want)
		}
	}
}

func TestRemoveStaleDriverEntries_PreservesMultilineValue(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns git to read and write config; runs in task test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot contain newlines")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	live := filepath.Join(dir, "custom\nforge")
	writeTreeFileT(t, live, "#!/bin/sh\n")
	if err := os.Chmod(live, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.worktree")
	const key = "forge-merge-driver.multiline.command"
	const stale = "'/gone/merge-driver/f1e8f2ebdbb6/forge' merge-generated %O %A %B %L %P"
	usable := shellQuote(live) + " merge-generated %O %A %B %L %P"
	gitT(t, dir, "config", "--file", file, "--add", key, stale)
	gitT(t, dir, "config", "--file", file, "--add", key, usable)
	gitT(t, dir, "config", "--file", file, "merge.forge-generated.driver", stale)

	if !removeStaleDriverEntries(file) {
		t.Fatal("did not remove the entirely stale driver key")
	}
	out, err := exec.Command("git", "config", "--file", file, "--null", "--get-all", key).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := stale + "\x00" + usable + "\x00"; string(out) != want {
		t.Errorf("repeated key values = %q, want %q", out, want)
	}
	if err := exec.Command("git", "config", "--file", file, "--get", "merge.forge-generated.driver").Run(); err == nil {
		t.Error("entirely stale driver key was preserved")
	}

	// A custom live executable may implement forge's argv without being
	// forge's old driver. Keep both its command and its display name.
	gitT(t, dir, "config", "--file", file, "merge.forge-generated.driver", usable)
	gitT(t, dir, "config", "--file", file, "merge.forge-generated.name", "custom merge")
	if removeStaleDriverEntries(file) {
		t.Error("custom live driver was treated as stale")
	}
	if got := strings.TrimSpace(gitT(t, dir, "config", "--file", file, "--get", "merge.forge-generated.driver")); got != usable {
		t.Errorf("custom driver = %q, want %q", got, usable)
	}
	if got := strings.TrimSpace(gitT(t, dir, "config", "--file", file, "--get", "merge.forge-generated.name")); got != "custom merge" {
		t.Errorf("custom driver name = %q, want custom merge", got)
	}
}

// A stale per-worktree driver an earlier forge wrote shadows the shared,
// self-healing command in EVERY worktree that carries it, and `git worktree
// add` copies the main worktree's into each new one (forge reports 1b2d77d0,
// 316e4d7b, 3b4b7d99). Wiring from any one worktree removes forge's stale
// entries from all of them, and leaves everything else alone.
func TestEnsureGeneratedMergeDriver_RemovesStaleDriversFromEveryWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real git repositories; runs in task test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	writeTreeFileT(t, filepath.Join(repo, "forge.yaml"), "name: svc\nmodule_path: example.com/svc\ngit:\n  merge_driver: auto\n")
	writeTreeFileT(t, filepath.Join(repo, "README"), "x\n")
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-qm", "init")
	gitT(t, repo, "config", "--local", "extensions.worktreeConfig", "true")

	const staleDriver = "'/gone/merge-driver/f1e8f2ebdbb6/forge' merge-generated %O %A %B %L %P"
	const staleReg = "'/gone/merge-driver/61efb1e2ce8c/forge' merge-generated %O %A %B %L %P"
	// The main worktree carries the stale pair, as control-plane's did, and
	// `git worktree add` copies it into each worktree made after it.
	gitT(t, repo, "config", "--worktree", "merge.forge-generated.driver", staleDriver)
	gitT(t, repo, "config", "--worktree", "forge-merge-driver.61efb1e2ce8c.command", staleReg)
	wtA := filepath.Join(t.TempDir(), "wt-a")
	wtB := filepath.Join(t.TempDir(), "wt-b")
	wtC := filepath.Join(t.TempDir(), "wt-c")
	gitT(t, repo, "worktree", "add", "-q", "-b", "a", wtA)
	gitT(t, repo, "worktree", "add", "-q", "-b", "b", wtB)
	gitT(t, repo, "worktree", "add", "-q", "-b", "c", wtC)
	// Set explicitly too: a git older than 2.36 copies nothing on add.
	// Worktree A also has a key of its own; worktree B a driver forge never
	// writes, set on purpose. Worktree C, where forge runs, starts clean.
	for _, wt := range []string{wtA, wtB} {
		gitT(t, wt, "config", "--worktree", "merge.forge-generated.driver", staleDriver)
		gitT(t, wt, "config", "--worktree", "forge-merge-driver.61efb1e2ce8c.command", staleReg)
	}
	gitT(t, wtA, "config", "--worktree", "user.name", "kept")
	gitT(t, wtB, "config", "--worktree", "merge.forge-generated.driver", "my-merge-tool %O %A %B")
	_ = exec.Command("git", "-C", wtC, "config", "--worktree", "--unset", "merge.forge-generated.driver").Run()
	_ = exec.Command("git", "-C", wtC, "config", "--worktree", "--unset", "forge-merge-driver.61efb1e2ce8c.command").Run()

	prevExe, prevCheck, prevID := mergeDriverExecutable, mergeDriverPinCheck, mergeDriverIdentity
	t.Cleanup(func() { mergeDriverExecutable, mergeDriverPinCheck, mergeDriverIdentity = prevExe, prevCheck, prevID })
	mergeDriverPinCheck = func(string) (string, string, bool) { return "", "", false }
	t.Setenv("FORGE_NO_MERGE_DRIVER", "")
	t.Setenv("TMPDIR", t.TempDir())
	forgeBin := filepath.Join(t.TempDir(), "pin", "forge")
	writeTreeFileT(t, forgeBin, "#!/bin/sh\n")
	if err := os.Chmod(forgeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	mergeDriverExecutable = func() (string, error) { return forgeBin, nil }
	mergeDriverIdentity = func() string { return "pin" }

	var notice strings.Builder
	ensureGeneratedMergeDriverTo(&notice, wtC)

	for _, wt := range []string{repo, wtA, wtB, wtC} {
		if wt == wtB {
			if got := gitConfig(t, wt, "forge-merge-driver.61efb1e2ce8c.command"); got != "" {
				t.Errorf("worktree B still registers a gone forge: %q", got)
			}
			continue
		}
		if got := gitT(t, wt, "config", "--get", "merge.forge-generated.driver"); !strings.HasPrefix(got, "[ -x '"+forgeBin+"' ]") {
			t.Errorf("worktree %s merges with %q: want the shared command, its stale copy removed", wt, got)
		}
		if got := gitConfig(t, wt, "forge-merge-driver.61efb1e2ce8c.command"); got != "" {
			t.Errorf("worktree %s still registers a gone forge: %q", wt, got)
		}
	}
	if got := strings.TrimSpace(gitT(t, wtB, "config", "--get", "merge.forge-generated.driver")); got != "my-merge-tool %O %A %B" {
		t.Errorf("a driver forge never writes was changed in worktree B: %q", got)
	}
	if got := strings.TrimSpace(gitT(t, wtA, "config", "--worktree", "--get", "user.name")); got != "kept" {
		t.Errorf("worktree A's own key = %q, want it left alone", got)
	}
	if !strings.Contains(notice.String(), "removed forge's stale per-worktree driver entries (config.worktree) from 3 worktree(s)") {
		t.Errorf("the clean-up must be announced, naming the worktrees:\n%s", notice.String())
	}

	// A worktree made after the clean-up inherits nothing stale.
	wtD := filepath.Join(t.TempDir(), "wt-d")
	gitT(t, repo, "worktree", "add", "-q", "-b", "d", wtD)
	if got := gitT(t, wtD, "config", "--get", "merge.forge-generated.driver"); !strings.HasPrefix(got, "[ -x '"+forgeBin+"' ]") {
		t.Errorf("a new worktree merges with %q: want the shared command", got)
	}

	// Nothing left to clean: a second run removes nothing and says nothing of it.
	var again strings.Builder
	ensureGeneratedMergeDriverTo(&again, wtC)
	if strings.Contains(again.String(), "stale per-worktree") {
		t.Errorf("a clean clone was cleaned again:\n%s", again.String())
	}
}
