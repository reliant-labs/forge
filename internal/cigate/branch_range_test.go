package cigate

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/testkit"
)

// BranchRange is what the gate's secret-scan row scans: the commits a
// branch adds over its merge-base with origin/<default>, the last commit on
// the default branch, and the whole history only for a first commit — never
// the history a project had before it pinned the forge with the row.
func TestBranchRange(t *testing.T) {
	testkit.Slow(t, "execs git")
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	ctx := context.Background()
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "first")
	if r, why := BranchRange(ctx, root, ""); r != "" || !strings.Contains(why, "first commit") {
		t.Errorf("first commit: %q (%s), want the whole history", r, why)
	}
	run("commit", "-q", "--allow-empty", "-m", "second")
	if r, why := BranchRange(ctx, root, ""); r != "HEAD^..HEAD" || !strings.Contains(why, "no origin/<default branch>") {
		t.Errorf("no remote: %q (%s), want the last commit", r, why)
	}
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	if r, why := BranchRange(ctx, root, ""); r != "HEAD^..HEAD" || !strings.Contains(why, "default branch main") {
		t.Errorf("on main: %q (%s), want the last commit", r, why)
	}
	base := run("rev-parse", "HEAD")
	run("switch", "-q", "-c", "topic")
	run("commit", "-q", "--allow-empty", "-m", "a")
	run("commit", "-q", "--allow-empty", "-m", "b")
	if r, why := BranchRange(ctx, root, ""); r != base+"..HEAD" || !strings.Contains(why, "since origin/main") {
		t.Errorf("on a branch: %q (%s), want %s..HEAD", r, why, base)
	}
	if r, _ := BranchRange(ctx, root, "HEAD~1"); r != run("rev-parse", "HEAD~1")+"..HEAD" {
		t.Errorf("an explicit base (CI_BASE) must win: %q", r)
	}
}
