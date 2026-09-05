package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureDevWebRuntimeLink_WritesAGitignoredBridge is the other half of the
// contract: the link must still EXIST, just not in a tracked file.
func TestEnsureDevWebRuntimeLink_WritesAGitignoredBridge(t *testing.T) {
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, true, forgeRoot)

	projectDir := filepath.Join(base, "app")
	writeFrontendManifest(t, projectDir, "web", "")

	EnsureDevWebRuntimeLink(projectDir)

	// The workspace member symlink resolves to the real package directory.
	link := filepath.Join(projectDir, devLinkDir, "web-runtime")
	if _, err := os.Stat(filepath.Join(link, "package.json")); err != nil {
		t.Fatalf("bridge does not resolve to the web-runtime package: %v", err)
	}

	// The gitignored workspace root declares the link glob AND the frontends,
	// which is what makes npm hoist the member over the registry copy.
	rootManifest := filepath.Join(projectDir, "package.json")
	raw, err := os.ReadFile(rootManifest)
	if err != nil {
		t.Fatalf("read workspace root manifest: %v", err)
	}
	for _, want := range []string{devLinkDir + "/*", "frontends/*"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("workspace root does not declare %q:\n%s", want, raw)
		}
	}

	// ...and forge ensures the ignore entries exist rather than assuming it.
	ignore, err := os.ReadFile(filepath.Join(projectDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, want := range []string{"/package.json", "/package-lock.json", "/" + devLinkDir + "/"} {
		if !gitignoreHasEntry(string(ignore), want) {
			t.Errorf(".gitignore is missing %q — the bridge would be committable:\n%s", want, ignore)
		}
	}
}

// TestEnsureDevWebRuntimeLink_ReleaseBuildWritesNothing keeps a released binary
// from scattering a dev-only workspace root into a user's project.
func TestEnsureDevWebRuntimeLink_ReleaseBuildWritesNothing(t *testing.T) {
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, false, forgeRoot)

	projectDir := filepath.Join(base, "app")
	writeFrontendManifest(t, projectDir, "web", "")

	EnsureDevWebRuntimeLink(projectDir)

	for _, p := range []string{"package.json", devLinkDir} {
		if _, err := os.Stat(filepath.Join(projectDir, p)); !os.IsNotExist(err) {
			t.Errorf("release build created %s at the project root", p)
		}
	}
}

// TestEnsureDevWebRuntimeLink_Idempotent — `forge generate` runs constantly;
// a second run must not append a duplicate ignore entry or rewrite the root.
func TestEnsureDevWebRuntimeLink_Idempotent(t *testing.T) {
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, true, forgeRoot)

	projectDir := filepath.Join(base, "app")
	writeFrontendManifest(t, projectDir, "web", "")

	EnsureDevWebRuntimeLink(projectDir)
	first := readAll(t, projectDir)
	for i := 0; i < 3; i++ {
		EnsureDevWebRuntimeLink(projectDir)
	}
	if again := readAll(t, projectDir); again != first {
		t.Errorf("re-running changed the bridge:\n--- first ---\n%s\n--- again ---\n%s", first, again)
	}
}

// readAll concatenates the two files the bridge owns, for change detection.
func readAll(t *testing.T, projectDir string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range []string{"package.json", ".gitignore"} {
		raw, err := os.ReadFile(filepath.Join(projectDir, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		b.WriteString(p + ":\n" + string(raw) + "\n")
	}
	return b.String()
}
