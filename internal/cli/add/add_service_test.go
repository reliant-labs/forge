package add

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestRunAddService_RollsBackOnPipelineFailure is the F3 revert regression:
// when the post-scaffold generate pipeline (buf validate / authz-completeness
// lint / build) fails on a FRESH `forge add service`, the files the scaffold
// just wrote — the handler dir and proto/services/<pkg> — must be removed, so
// a failed validation doesn't strand a half-created service that confuses the
// next run.
func TestRunAddService_RollsBackOnPipelineFailure(t *testing.T) {
	dir := withTempProject(t, "name: testproj\nmodule_path: example.com/testproj\n")
	writeComponentsJSON(t, dir) // stamp pkg/app so the project derives to service kind

	f := testFactory()
	f.Gen.RunPipeline = func(string) error { return fmt.Errorf("simulated validation failure") }

	err := runAddService(f, "orders", 0, false, false)
	if err == nil {
		t.Fatal("expected runAddService to surface the pipeline failure")
	}

	// Both freshly-scaffolded trees must be gone after rollback.
	for _, p := range []string{
		filepath.Join(dir, "internal", "handlers", "orders"),
		filepath.Join(dir, "proto", "services", "orders"),
	} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("rollback did not remove %s (stat err=%v)", p, statErr)
		}
	}
}

// TestRunAddService_RollbackPreservesPreexistingDirs proves the rollback only
// removes what THIS run created: a handler dir present before the add (a
// --resume-style recovery, or a manual edit) must survive a pipeline failure.
func TestRunAddService_RollbackPreservesPreexistingDirs(t *testing.T) {
	dir := withTempProject(t, "name: testproj\nmodule_path: example.com/testproj\n")
	writeComponentsJSON(t, dir)

	// Pre-create the handler dir with a sentinel file (simulating a prior
	// partial scaffold the user is recovering).
	handlerDir := filepath.Join(dir, "internal", "handlers", "orders")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(handlerDir, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("preexisting"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := testFactory()
	f.Gen.RunPipeline = func(string) error { return fmt.Errorf("simulated validation failure") }

	// --resume so an existing dir is tolerated by the conflict check.
	if err := runAddService(f, "orders", 0, true, false); err == nil {
		t.Fatal("expected runAddService to surface the pipeline failure")
	}

	// The pre-existing handler dir (and its sentinel) must be preserved,
	// because --resume is not a fresh add — rollback must not fire.
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("rollback removed a pre-existing handler dir on a --resume run: %v", err)
	}
}

// TestRollbackServiceScaffold_OnlyRemovesFreshDirs unit-tests the rollback
// helper directly: a dir flagged pre-existing is preserved; a fresh one is
// removed.
func TestRollbackServiceScaffold_OnlyRemovesFreshDirs(t *testing.T) {
	root := t.TempDir()
	handlerDir := filepath.Join(root, "internal", "handlers", "svc")
	protoDir := filepath.Join(root, "proto", "services", "svc")
	for _, d := range []string{handlerDir, protoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// handler pre-existed (preserve), proto is fresh (remove).
	rollbackServiceScaffold(root, "svc", true, false)

	if _, err := os.Stat(handlerDir); err != nil {
		t.Errorf("pre-existing handler dir must be preserved: %v", err)
	}
	if _, err := os.Stat(protoDir); !os.IsNotExist(err) {
		t.Errorf("fresh proto dir must be removed (stat err=%v)", err)
	}
}
