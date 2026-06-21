package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempProject creates a temp directory, writes a minimal forge.yaml
// (or whatever the caller supplies), changes the test process cwd to it,
// and registers a cleanup that restores the previous cwd.
//
// It previously lived in add_library_test.go; when `forge add` moved to
// the internal/cli/add group that copy went with it. friction_test.go (and
// future internal/cli tests) keep this cli-package copy.
func withTempProject(t *testing.T, forgeYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if forgeYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(forgeYAML), 0o644); err != nil {
			t.Fatalf("write forge.yaml: %v", err)
		}
	}
	t.Chdir(dir)
	return dir
}
