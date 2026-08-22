//go:build e2e

package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestE2EScaffoldKCLVendorFlow exercises the forge KCL module vendor
// mechanism end to end with the real binary and the real embedded
// kpm/kcl-go runtime (no external `kcl` needed):
//
//  1. Born correct: `forge project new` materializes `.forge-kcl/` and
//     emits deploy/kcl/kcl.mod with a RELATIVE path dependency — no git
//     tag to resolve, no hand-patched absolute path.
//  2. Resolves + renders: `forge ci validate-kcl` (the same
//     internal/kclrender seam `forge env deploy` and the generate
//     pipeline's ingress-ports step use) succeeds for every env.
//  3. Heals: a project whose kcl.mod carries the dead git tag an older
//     forge scaffolded is vendored + patched by one `forge generate`
//     and then renders.
//  4. Idempotent: a second `forge generate` leaves kcl.mod and the
//     vendored tree byte-identical.
func TestE2EScaffoldKCLVendorFlow(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	runCmd(t, dir, forgeBin,
		"project", "new", "kclvendorapp",
		"--mod", "example.com/kclvendorapp",
		"--service", "item",
	)
	projectDir := filepath.Join(dir, "kclvendorapp")
	deployMod := filepath.Join(projectDir, "deploy", "kcl", "kcl.mod")

	// 1. Born correct.
	assertPathExistsE2E(t, filepath.Join(projectDir, ".forge-kcl", "kcl.mod"))
	assertPathExistsE2E(t, filepath.Join(projectDir, ".forge-kcl", "schema.k"))
	born := readFileE2EString(t, deployMod)
	if !strings.Contains(born, `forge = { path = "../../.forge-kcl" }`) {
		t.Fatalf("scaffold not born with the relative vendored dep:\n%s", born)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "kcl.mod")); err == nil {
		t.Fatalf("scaffold emitted a legacy project-root kcl.mod; the KCL package root is deploy/kcl/")
	}

	// 2. Resolves + renders through the embedded runtime.
	runCmd(t, projectDir, forgeBin, "ci", "validate-kcl")

	// 3. Heals a project an older forge left broken: dead git tag, no
	// vendor dir.
	if err := os.RemoveAll(filepath.Join(projectDir, ".forge-kcl")); err != nil {
		t.Fatalf("remove vendor dir: %v", err)
	}
	brokenMod := `[package]
name = "kclvendorapp-deploy"
edition = "v0.11.0"
version = "0.0.1"

[dependencies]
forge = { git = "https://github.com/reliant-labs/forge.git", tag = "kcl-v0.1.0" }
`
	if err := os.WriteFile(deployMod, []byte(brokenMod), 0o644); err != nil {
		t.Fatalf("write broken kcl.mod: %v", err)
	}
	runCmd(t, projectDir, forgeBin, "generate")
	healed := readFileE2EString(t, deployMod)
	if !strings.Contains(healed, `forge = { path = "../../.forge-kcl" }`) {
		t.Fatalf("generate did not heal the dead git tag:\n%s", healed)
	}
	assertPathExistsE2E(t, filepath.Join(projectDir, ".forge-kcl", "kcl.mod"))
	runCmd(t, projectDir, forgeBin, "ci", "validate-kcl")

	// 4. Byte-idempotent across a second generate.
	before := hashKCLVendorState(t, projectDir)
	runCmd(t, projectDir, forgeBin, "generate")
	after := hashKCLVendorState(t, projectDir)
	if before != after {
		t.Fatalf("second generate churned kcl.mod / .forge-kcl: %s != %s", before, after)
	}
}

// hashKCLVendorState digests deploy/kcl/kcl.mod plus every file under
// .forge-kcl/ (path + content), giving a stable fingerprint for the
// generate-twice idempotence check.
func hashKCLVendorState(t *testing.T, projectDir string) string {
	t.Helper()
	paths := []string{filepath.Join(projectDir, "deploy", "kcl", "kcl.mod")}
	vendorDir := filepath.Join(projectDir, ".forge-kcl")
	_ = filepath.Walk(vendorDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		rel, _ := filepath.Rel(projectDir, p)
		_, _ = io.WriteString(h, rel+"\n")
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		_, _ = h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// readFileE2EString reads a file or fails the test.
func readFileE2EString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
