package lint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/testkit"
)

func TestSplitBufFormatDiff(t *testing.T) {
	out := `diff -u proto/a/v1/a.proto.orig proto/a/v1/a.proto
--- proto/a/v1/a.proto.orig	2026-11-17 22:23:40.644332794 +0000
+++ proto/a/v1/a.proto	2026-11-17 22:23:40.644332794 +0000
@@ -1,2 +1,2 @@
-import "b.proto";
+import "a.proto";
diff -u proto/b/v1/b.proto.orig proto/b/v1/b.proto
--- proto/b/v1/b.proto.orig	2026-11-17 22:23:40.644332794 +0000
+++ proto/b/v1/b.proto	2026-11-17 22:23:40.644332794 +0000
@@ -3 +3 @@
-message   B {}
+message B {}
`
	got := splitBufFormatDiff(out)
	if len(got) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(got), got)
	}
	if got[0].File != "proto/a/v1/a.proto" || got[1].File != "proto/b/v1/b.proto" {
		t.Errorf("files = %q, %q", got[0].File, got[1].File)
	}
	if !strings.Contains(got[0].Diff, `+import "a.proto";`) || strings.Contains(got[0].Diff, "message B") {
		t.Errorf("a.proto's diff is not its own:\n%s", got[0].Diff)
	}
	if !strings.Contains(got[1].Diff, "+message B {}") {
		t.Errorf("b.proto's diff is missing its hunk:\n%s", got[1].Diff)
	}
	if splitBufFormatDiff("") != nil {
		t.Error("no diff must be no pending files")
	}
}

// The diff under a finding is graded with it: a pin still in the rule's grace
// period sees a warning and a passing lane, lint.rules can turn the whole
// report off, and past GatesFrom it gates.
func TestProtoFormatDiffIsGradedWithItsFinding(t *testing.T) {
	ro := lintRollouts[ruleProtoFormat]
	findings := func() []lintJSONFinding {
		return []lintJSONFinding{
			{File: "proto/a.proto", Severity: lintSevError, Rule: ruleProtoFormat, Message: "would reformat"},
			{Severity: lintSevError, Rule: "external", Message: "-a\n+b", rollout: ruleProtoFormat},
		}
	}
	if fs, gated := gradeProjectLane(pinnedCfg(ro.IntroducedIn, nil), findings(), true); gated || anyErrorFinding(fs) {
		t.Errorf("pin %s (before GatesFrom %s): the lane still gates: %+v", ro.IntroducedIn, ro.GatesFrom, fs)
	}
	if fs, gated := gradeProjectLane(pinnedCfg(ro.IntroducedIn, map[string]string{ruleProtoFormat: "off"}), findings(), true); gated || len(fs) != 0 {
		t.Errorf("lint.rules off: findings survive: %+v", fs)
	}
	if _, gated := gradeProjectLane(pinnedCfg(ro.GatesFrom, nil), findings(), true); !gated {
		t.Errorf("pin %s: the lane must gate", ro.GatesFrom)
	}
}

// The lane runs the pre-commit hook's check: a proto buf would rewrite fails
// it with buf's diff, --fix writes buf's output, and a project with no
// buf.yaml has no lane.
func TestProtoFormatLane(t *testing.T) {
	testkit.Slow(t, "execs buf")
	if _, err := exec.LookPath("buf"); err != nil {
		t.Skip("buf not on PATH")
	}
	ctx := context.Background()
	root := t.TempDir()
	if fs, gated, err := collectProtoFormatJSON(ctx, root, nil, false); err != nil || gated || len(fs) != 0 {
		t.Fatalf("no buf.yaml: want no findings, got %v gated=%v err=%v", fs, gated, err)
	}
	if err := os.WriteFile(filepath.Join(root, "buf.yaml"), []byte("version: v2\nmodules:\n  - path: proto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "proto", "a", "v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clean := "syntax = \"proto3\";\n\npackage a.v1;\n\nmessage A {\n  string id = 1;\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "clean.proto"), []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.proto"), []byte("syntax = \"proto3\";\npackage a.v1;\nmessage   B {string id = 1;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs, gated, err := collectProtoFormatJSON(ctx, root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !gated || len(fs) != 2 || fs[0].File != "proto/a/v1/dirty.proto" || fs[0].Rule != ruleProtoFormat {
		t.Fatalf("want one forge-proto-format finding on dirty.proto plus its diff, got gated=%v %+v", gated, fs)
	}
	if !strings.Contains(fs[1].Message, "+message B {") {
		t.Errorf("the finding's diff is not buf's:\n%s", fs[1].Message)
	}

	if fs, _, err := collectProtoFormatJSON(ctx, root, nil, true); err != nil || len(fs) != 0 {
		t.Fatalf("--fix: want no findings, got %+v (%v)", fs, err)
	}
	if fs, gated, _ := collectProtoFormatJSON(ctx, root, nil, false); gated || len(fs) != 0 {
		t.Errorf("after --fix the lane still fails: %+v", fs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "clean.proto")); string(b) != clean {
		t.Errorf("--fix rewrote a clean file:\n%s", b)
	}
}
