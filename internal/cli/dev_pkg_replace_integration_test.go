package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncDevForgePkgReplace_AgainstControlPlaneNext is an opt-in
// integration check that runs the dev-mode vendor sync against the
// actual control-plane-next checkout sitting next to forge in the
// workspace.
//
// We do NOT mutate state we can't undo: the test refreshes the existing
// vendor directory (a no-op when it's already in sync, or pulls in
// drifted files when forge/pkg has moved ahead). If go.mod's replace
// is already at `./.forge-pkg`, no rewrite happens. If it's pointing
// at an absolute path (which it shouldn't be after the workaround was
// applied), we restore it after the test.
//
// Skipped automatically when control-plane-next isn't available next to
// the forge checkout.
func TestSyncDevForgePkgReplace_AgainstControlPlaneNext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test under -short")
	}

	// Resolve sibling control-plane-next via the forge repo root.
	repoRoot := findForgeRepoRoot(t)
	cpn := filepath.Join(filepath.Dir(repoRoot), "control-plane-next")
	if _, err := os.Stat(filepath.Join(cpn, "go.mod")); err != nil {
		t.Skipf("control-plane-next not found at %s — skipping", cpn)
	}
	if _, err := os.Stat(filepath.Join(cpn, ".forge-pkg")); err != nil {
		t.Skipf(".forge-pkg/ not present at %s — skipping (project not in dev-vendor state)", cpn)
	}

	// Snapshot go.mod so we can restore on failure or unexpected mutation.
	goModPath := filepath.Join(cpn, "go.mod")
	goModBefore, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	t.Cleanup(func() {
		// Always restore go.mod to its pre-test state. The .forge-pkg/
		// content is allowed to drift (forward-only); a follow-up
		// `forge generate` would reproduce the same result.
		if err := os.WriteFile(goModPath, goModBefore, 0o644); err != nil {
			t.Logf("warning: failed to restore go.mod: %v", err)
		}
	})

	// Invoke the sync. Should be idempotent (or refresh from sibling).
	vendored, err := syncDevForgePkgReplace(cpn)
	if err != nil {
		t.Fatalf("syncDevForgePkgReplace: %v", err)
	}
	if !vendored {
		t.Fatal("expected vendored=true (control-plane-next has .forge-pkg/)")
	}

	// go.mod must still declare the relative replace.
	goModAfter, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod after: %v", err)
	}
	if !strings.Contains(string(goModAfter), "replace github.com/reliant-labs/forge/pkg => ./.forge-pkg") {
		t.Fatalf("expected replace to point at ./.forge-pkg after sync; got:\n%s", string(goModAfter))
	}

	// Re-run: must be byte-stable on go.mod (idempotent rewrite).
	goModBeforeSecond, _ := os.ReadFile(goModPath)
	if _, err := syncDevForgePkgReplace(cpn); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	goModAfterSecond, _ := os.ReadFile(goModPath)
	if string(goModBeforeSecond) != string(goModAfterSecond) {
		t.Fatalf("go.mod drifted between idempotent runs:\n  before: %s\n  after:  %s",
			goModBeforeSecond, goModAfterSecond)
	}
}
