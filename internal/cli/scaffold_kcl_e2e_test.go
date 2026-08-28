//go:build e2e

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EScaffoldKCLRendersDevManifest runs `kcl run` against the
// generated project's dev environment with a distinctive image_tag
// value and checks the render succeeds against the vendored module.
//
// This guards two things:
//  1. The scaffold's KCL files `import forge` correctly — a broken
//     module import is caught here rather than on first deploy. On a
//     dev-built forge binary the project is BORN with the module
//     vendored into `.forge-kcl/` and deploy/kcl/kcl.mod pointing at
//     it by relative path (internal/kclvendor), so no kcl.mod rewrite
//     is needed — the render exercises exactly what a user gets.
//  2. The -D override contract documented in main.k (via `option()`)
//     stays accepted by the entrypoint. NOTE: whether the tag appears
//     in the output depends on deploy/kcl/workloads.k declaring
//     workloads; a fresh scaffold with no services declares none, so the
//     tag-containment check is conditional on a workload image being
//     rendered at all.
//
// kcl is required via requireTool: a maintainer without the binary gets a
// named skip, and CI — which installs kcl in e2e-suite.yml — gets a hard
// failure if that install ever stops working.
func TestE2EScaffoldKCLRendersDevManifest(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	requireTool(t, "kcl")

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// Scaffold WITH a service so the deploy-as-data render has a
	// component source to draw from once component derivation lands.
	runCmd(t, dir, forgeBin,
		"project", "new", "kclapp",
		"--mod", "example.com/kclapp",
		"--service", "item",
	)

	projectDir := filepath.Join(dir, "kclapp")

	// Locate the dev manifest. The test is intentionally explicit about
	// the path so a regression that moves the file surfaces here.
	devManifest := filepath.Join(projectDir, "deploy", "kcl", "dev", "main.k")
	assertPathExistsE2E(t, devManifest)

	// Born-vendored: the dev-build scaffold must have materialized the
	// embedded forge KCL module and pointed kcl.mod at it by RELATIVE
	// path — no hand-patching, no network, no forge checkout needed.
	assertPathExistsE2E(t, filepath.Join(projectDir, ".forge-kcl", "kcl.mod"))
	kclMod, err := os.ReadFile(filepath.Join(projectDir, "deploy", "kcl", "kcl.mod"))
	if err != nil {
		t.Fatalf("read deploy/kcl/kcl.mod: %v", err)
	}
	if !strings.Contains(string(kclMod), `forge = { path = "../../.forge-kcl" }`) {
		t.Fatalf("deploy/kcl/kcl.mod does not carry the relative vendored dep:\n%s", kclMod)
	}

	// Use a distinctive tag so string-matching is unambiguous.
	const tag = "test123-unique-marker"

	// Render THROUGH FORGE, not with a bare `kcl run`. A forge project's
	// KCL imports kcl_plugin.forge (resolve_port, allocate_port, …), and
	// that namespace is registered by the forge process itself — a raw
	// `kcl run` fails with "the plugin package `kcl_plugin.forge` is not
	// found" no matter how the module is vendored, so invoking kcl directly
	// tests a configuration no user is ever in. `forge env render` is the
	// read-only path: no kubectl context, no cluster, no image build.
	out := runCmdOutput(t, projectDir, forgeBin, "env", "render", "dev", "--tag", tag)

	// The render must produce the env's namespace — proof the forge
	// module resolved and the schema hierarchy evaluated.
	if !strings.Contains(out, "kclapp-dev") {
		t.Fatalf("rendered manifests missing the kclapp-dev namespace:\n%s", out)
	}
	// Tag-containment only applies when a workload image was rendered
	// (deploy-as-data component derivation may produce none for a fresh
	// scaffold — then no image exists to stamp the tag onto).
	if strings.Contains(out, "image:") && !strings.Contains(out, tag) {
		t.Fatalf("workload images rendered without the --tag %s override:\n%s", tag, out)
	}
}
