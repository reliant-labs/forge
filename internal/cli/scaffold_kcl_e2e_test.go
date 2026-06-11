//go:build e2e

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EScaffoldKCLRendersDevManifest runs `kcl run` against the
// generated project's dev environment with a distinctive image_tag
// value and checks that the tag ends up in the rendered YAML.
//
// This guards two things:
//  1. The scaffold's KCL files `import forge` correctly — a broken
//     module import is caught here rather than on first deploy.
//  2. The -D override contract documented in main.k (via `option()`)
//     stays wired up. Regressing this would force operators to
//     hard-code image tags per environment, which silently breaks CI
//     image promotion.
//
// Uses `-E forge=<repo>/kcl` to point at the local module rather than
// the published git tag — the tag won't exist until the next release.
//
// kcl is an optional tool; the test is a clear log-and-skip if it's
// not installed. Maintainers running this locally don't need the kcl
// binary but CI should provide it.
func TestE2EScaffoldKCLRendersDevManifest(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
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

	// Resolve the forge KCL module path in the repo so we can rewrite
	// the project's kcl.mod to depend on the in-tree path instead of
	// the not-yet-published git tag.
	forgeModule := repoRelativePath(t, "kcl")
	rewriteKclModForLocalForge(t, filepath.Join(projectDir, "kcl.mod"), forgeModule)

	// kcl run executes from the manifest's directory by default; we
	// pass the absolute path so it doesn't matter. We select `-S
	// manifests` because that's the identifier the scaffold names the
	// rendered k8s YAML.
	cmd := exec.Command("kcl", "run",
		"-D", "image_tag="+tag,
		"-S", "manifests",
		devManifest,
	)
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

// rewriteKclModForLocalForge swaps the `git = ...` dependency in
// kcl.mod for a `path = <forgeModule>` reference. This lets the e2e
// test exercise the scaffold against the in-tree forge module without
// requiring the kcl-v0.1.0 git tag to exist yet (it ships in this
// commit).
func rewriteKclModForLocalForge(t *testing.T, kclModPath, forgeModule string) {
	t.Helper()
	body, err := os.ReadFile(kclModPath)
	if err != nil {
		t.Fatalf("read kcl.mod: %v", err)
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "forge = ") {
			lines[i] = `forge = { path = "` + forgeModule + `" }`
		}
	}
	if err := os.WriteFile(kclModPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("write kcl.mod: %v", err)
	}
}

// repoRelativePath returns an absolute path to <repo-root>/<rel> by
// walking up from cwd looking for the kcl/ module marker. Keeps the
// test independent of the test runner's cwd.
func repoRelativePath(t *testing.T, rel string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(root, "kcl", "kcl.mod")); err == nil {
			return filepath.Join(root, rel)
		}
		root = filepath.Dir(root)
	}
	t.Fatalf("could not locate forge repo root from cwd %s", wd)
	return ""
}
