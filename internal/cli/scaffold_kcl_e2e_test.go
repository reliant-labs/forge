//go:build e2e

package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EScaffoldKCLRendersDevManifest runs `kcl run` against the generated
// project's dev environment with a distinctive image_tag value and checks
// that the tag ends up in the rendered YAML.
//
// This guards two things:
//  1. The scaffold's KCL files import `schema.k` / `base.k` / `render.k`
//     correctly — a broken import is caught here rather than on first
//     deploy.
//  2. The -D override contract documented in main.k (via `option()`) stays
//     wired up. Regressing this would force operators to hard-code image
//     tags per environment, which silently breaks CI image promotion.
//
// kcl is an optional tool; the test is a clear log-and-skip if it's not
// installed. Maintainers running this locally don't need the kcl binary
// but CI should provide it.
func TestE2EScaffoldKCLRendersDevManifest(t *testing.T) {
	if !toolAvailable("kcl") {
		t.Skip("kcl not available — skipping KCL render check")
	}

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// A minimal scaffold without --frontend is enough: KCL files come
	// from the `new` flow regardless of services/frontends.
	runCmd(t, dir, forgeBin,
		"new", "kclapp",
		"--mod", "example.com/kclapp",
	)

	projectDir := filepath.Join(dir, "kclapp")

	// Locate the dev manifest. The test is intentionally explicit about
	// the path so a regression that moves the file surfaces here.
	devManifest := filepath.Join(projectDir, "deploy", "kcl", "dev", "main.k")
	assertPathExistsE2E(t, devManifest)

	// Use a distinctive tag so string-matching is unambiguous.
	const tag = "test123-unique-marker"

	// kcl run executes from the manifest's directory by default; we pass
	// the absolute path so it doesn't matter.
	cmd := exec.Command("kcl", "run", "-D", "image_tag="+tag, devManifest)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kcl run failed: %v\noutput:\n%s", err, string(out))
	}

	if !strings.Contains(string(out), tag) {
		// Dump the rendered output to help debug — a passing test
		// would have the tag baked in somewhere in the YAML.
		t.Fatalf("expected kcl output to contain %q (from -D image_tag=%s); got:\n%s",
			tag, tag, string(out))
	}
}
