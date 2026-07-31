package generator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// fakeForgeCheckout writes the minimum a forge source tree needs for the
// web-runtime bridge to consider it usable: a web-runtime/ directory carrying
// a package.json.
func fakeForgeCheckout(t *testing.T, root string) string {
	t.Helper()
	pkgDir := filepath.Join(root, "web-runtime")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir web-runtime: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"`+WebRuntimePackage+`"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	return root
}

// pinDevBuild makes IsDevBuild / DiscoverDevForgeRootFromSource deterministic
// for the duration of a test. Under `go test` the ambient binary always looks
// like a dev build resolving to the LIVE forge checkout, so the release path
// is otherwise unreachable.
func pinDevBuild(t *testing.T, dev bool, root string) {
	t.Helper()
	stamped := buildinfo.DevForgeRoot
	buildinfo.DevForgeRoot = ""
	buildinfo.SetDevBuild(dev)
	buildinfo.SetDiscoveredForgeRoot(root)
	t.Cleanup(func() {
		buildinfo.DevForgeRoot = stamped
		buildinfo.ClearDevBuild()
		buildinfo.ClearDiscoveredForgeRoot()
	})
}

// writeFrontendManifest lays down frontends/<name>/package.json with the given
// dependencies body and returns the project dir.
func writeFrontendManifest(t *testing.T, projectDir, feName, deps string) string {
	t.Helper()
	feDir := filepath.Join(projectDir, "frontends", feName)
	if err := os.MkdirAll(feDir, 0o755); err != nil {
		t.Fatalf("mkdir frontend: %v", err)
	}
	body := `{
  "name": "` + feName + `",
  "version": "0.1.0",
  "dependencies": {` + deps + `
  }
}
`
	if err := os.WriteFile(filepath.Join(feDir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	return filepath.Join(feDir, "package.json")
}

// readDeps returns the parsed "dependencies" map of a manifest.
func readDeps(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var doc struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("manifest is not valid JSON after reconcile: %v\n%s", err, raw)
	}
	return doc.Dependencies
}

func TestEnsureWebRuntimeDependency_DevBuildWritesFileSpec(t *testing.T) {
	// Both trees under one root, so the relative path never has to climb
	// above the home directory.
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, true, forgeRoot)

	projectDir := filepath.Join(base, "app")
	manifest := writeFrontendManifest(t, projectDir, "web", "\n    \"react\": \"^19.1.0\"")

	EnsureWebRuntimeDependency(projectDir, filepath.Join("frontends", "web"), "web")

	got := readDeps(t, manifest)[WebRuntimePackage]
	if want := "file:../../../forge/web-runtime"; got != want {
		t.Errorf("specifier = %q, want %q", got, want)
	}

	// The resolved path must actually be the package directory.
	resolved := filepath.Join(projectDir, "frontends", "web", strings.TrimPrefix(got, "file:"))
	if _, err := os.Stat(filepath.Join(resolved, "package.json")); err != nil {
		t.Errorf("specifier does not resolve to the package: %v", err)
	}
}

// TestWebRuntimeFileSpec is the path-hygiene gate. The manifest is a
// COMMITTED file, so no shape of checkout may put an absolute path, a home
// directory or a username into it.
func TestWebRuntimeFileSpec(t *testing.T) {
	sep := string(filepath.Separator)
	home := filepath.Join(sep, "Users", "someone")
	abs := func(parts ...string) string { return filepath.Join(append([]string{sep}, parts...)...) }

	tests := []struct {
		name   string
		fe     string
		target string
		home   string
		want   string
	}{
		{
			name:   "siblings under home use a plain relative path",
			fe:     filepath.Join(home, "src", "app", "frontends", "web"),
			target: filepath.Join(home, "src", "forge", "web-runtime"),
			home:   home,
			want:   "file:../../../forge/web-runtime",
		},
		{
			name:   "project outside home falls back to the ~ form",
			fe:     abs("tmp", "throwaway", "frontends", "web"),
			target: filepath.Join(home, "src", "forge", "web-runtime"),
			home:   home,
			want:   "file:~/src/forge/web-runtime",
		},
		{
			name:   "neither path under home keeps the relative form",
			fe:     abs("srv", "app", "frontends", "web"),
			target: abs("opt", "forge", "web-runtime"),
			home:   home,
			want:   "file:../../../../opt/forge/web-runtime",
		},
		{
			name:   "sibling of home is still a descent through the username",
			fe:     abs("Users", "other", "app", "frontends", "web"),
			target: filepath.Join(home, "forge", "web-runtime"),
			home:   home,
			want:   "file:~/forge/web-runtime",
		},
		{
			name:   "no home directory to protect keeps the relative form",
			fe:     abs("srv", "app", "frontends", "web"),
			target: filepath.Join(home, "forge", "web-runtime"),
			home:   "",
			want:   "file:../../../../Users/someone/forge/web-runtime",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := webRuntimeFileSpec(tc.fe, tc.target, tc.home)
			if got != tc.want {
				t.Fatalf("spec = %q, want %q", got, tc.want)
			}
			if tc.home == "" {
				return
			}
			if strings.Contains(got, tc.home) {
				t.Errorf("spec %q embeds the home directory", got)
			}
			if user := filepath.Base(tc.home); strings.Contains(got, user) {
				t.Errorf("spec %q embeds the username %q", got, user)
			}
			if strings.HasPrefix(got, "file:/") {
				t.Errorf("spec %q is an absolute path", got)
			}
		})
	}
}

// TestEnsureWebRuntimeDependency_ManifestCarriesNoHomePath is the same gate at
// the file level: whatever this machine's layout is, the manifest forge writes
// never names the user.
func TestEnsureWebRuntimeDependency_ManifestCarriesNoHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory on this machine")
	}
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, true, forgeRoot)

	projectDir := filepath.Join(base, "app")
	manifest := writeFrontendManifest(t, projectDir, "web", "")

	EnsureWebRuntimeDependency(projectDir, filepath.Join("frontends", "web"), "web")

	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(raw), home) {
		t.Errorf("manifest embeds the home directory %q:\n%s", home, raw)
	}
	if user := filepath.Base(home); strings.Contains(string(raw), user) {
		t.Errorf("manifest embeds the username %q:\n%s", user, raw)
	}
}

func TestEnsureWebRuntimeDependency_ReleaseBuildWritesAVersionRange(t *testing.T) {
	forgeRoot := fakeForgeCheckout(t, t.TempDir())
	pinDevBuild(t, false, forgeRoot)

	projectDir := filepath.Join(t.TempDir(), "app")
	manifest := writeFrontendManifest(t, projectDir, "web", "\n    \""+WebRuntimePackage+"\": \"file:../../../forge/web-runtime\"")

	EnsureWebRuntimeDependency(projectDir, filepath.Join("frontends", "web"), "web")

	// A release forge normalises a bridge away — a machine-local path must
	// never survive into a project a released binary regenerates.
	if got := readDeps(t, manifest)[WebRuntimePackage]; got != webRuntimePublishedRange {
		t.Errorf("specifier = %q, want %q", got, webRuntimePublishedRange)
	}
}

func TestEnsureWebRuntimeDependency_UndiscoverableRootKeepsAnExistingBridge(t *testing.T) {
	pinDevBuild(t, true, "")

	projectDir := filepath.Join(t.TempDir(), "app")
	const existing = "file:../../../forge/web-runtime"
	manifest := writeFrontendManifest(t, projectDir, "web", "\n    \""+WebRuntimePackage+"\": \""+existing+"\"")

	EnsureWebRuntimeDependency(projectDir, filepath.Join("frontends", "web"), "web")

	if got := readDeps(t, manifest)[WebRuntimePackage]; got != existing {
		t.Errorf("a dev build that cannot find its own source rewrote the bridge: %q", got)
	}
}

func TestEnsureWebRuntimeDependency_IdempotentAndFormatPreserving(t *testing.T) {
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, true, forgeRoot)

	projectDir := filepath.Join(base, "app")
	feRel := filepath.Join("frontends", "web")
	manifest := writeFrontendManifest(t, projectDir, "web", "\n    \"react\": \"^19.1.0\"")

	EnsureWebRuntimeDependency(projectDir, feRel, "web")
	first, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for i := 0; i < 3; i++ {
		EnsureWebRuntimeDependency(projectDir, feRel, "web")
	}
	again, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(again) != string(first) {
		t.Errorf("re-running changed the manifest:\n--- first ---\n%s\n--- again ---\n%s", first, again)
	}
	if n := strings.Count(string(again), WebRuntimePackage); n != 1 {
		t.Errorf("package declared %d times, want 1:\n%s", n, again)
	}
	// The neighbouring entry and its formatting survive.
	if !strings.Contains(string(again), `"react": "^19.1.0"`) {
		t.Errorf("reconcile disturbed the surrounding manifest:\n%s", again)
	}
}

func TestEnsureWebRuntimeDependency_CorrectsAStalePath(t *testing.T) {
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, true, forgeRoot)

	projectDir := filepath.Join(base, "app")
	manifest := writeFrontendManifest(t, projectDir, "web",
		"\n    \""+WebRuntimePackage+"\": \"file:../../../../somewhere/else/web-runtime\"")

	EnsureWebRuntimeDependency(projectDir, filepath.Join("frontends", "web"), "web")

	if got := readDeps(t, manifest)[WebRuntimePackage]; got != "file:../../../forge/web-runtime" {
		t.Errorf("stale path not corrected: %q", got)
	}
}

func TestEnsureWebRuntimeDependency_EmptyDependenciesObject(t *testing.T) {
	base := t.TempDir()
	forgeRoot := fakeForgeCheckout(t, filepath.Join(base, "forge"))
	pinDevBuild(t, true, forgeRoot)

	projectDir := filepath.Join(base, "app")
	manifest := writeFrontendManifest(t, projectDir, "web", "")

	EnsureWebRuntimeDependency(projectDir, filepath.Join("frontends", "web"), "web")

	if got := readDeps(t, manifest)[WebRuntimePackage]; got != "file:../../../forge/web-runtime" {
		t.Errorf("specifier = %q", got)
	}
}

// TestWebRuntimePublishedRangeTracksPackage keeps the release specifier from
// silently drifting away from the version web-runtime actually declares.
func TestWebRuntimePublishedRangeTracksPackage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web-runtime", "package.json"))
	if err != nil {
		t.Skipf("web-runtime package.json not readable: %v", err)
	}
	var doc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse web-runtime package.json: %v", err)
	}
	if doc.Name != WebRuntimePackage {
		t.Errorf("web-runtime package name = %q, want %q", doc.Name, WebRuntimePackage)
	}
	majorMinor := func(v string) string {
		parts := strings.SplitN(strings.TrimPrefix(v, "^"), ".", 3)
		if len(parts) < 2 {
			return v
		}
		return parts[0] + "." + parts[1]
	}
	if got, want := majorMinor(webRuntimePublishedRange), majorMinor(doc.Version); got != want {
		t.Errorf("webRuntimePublishedRange %q does not track web-runtime version %q",
			webRuntimePublishedRange, doc.Version)
	}
}
