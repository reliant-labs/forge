package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateFrontendFiles_DefaultsToStaticExport verifies the
// new-scaffold default: a Next.js frontend scaffolded WITHOUT
// FrontendGenOptions.Output set emits a next.config.ts that produces a
// static export in production (and unchanged dev). The whole point of
// the static-default switchover is that this is the common case for
// "Next.js shell + Connect RPC against a Go backend", so the path
// taken by every existing caller (which doesn't know about the new
// Output field) must land on the new default.
func TestGenerateFrontendFiles_DefaultsToStaticExport(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFrontendFiles(dir, "example.com/myapp", "myapp", "web", 8080, ""); err != nil {
		t.Fatalf("GenerateFrontendFiles: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "frontends", "web", "next.config.ts"))
	if err != nil {
		t.Fatalf("read next.config.ts: %v", err)
	}
	s := string(body)

	// Static-export shape is gated on NODE_ENV=production. Dev stays
	// as `next dev`.
	want := `...(process.env.NODE_ENV === "production" ? { output: "export" } : {}),`
	if !strings.Contains(s, want) {
		t.Errorf("next.config.ts must default to the static-export shape; expected to find %q, got:\n%s", want, s)
	}
	// The literal `output: "standalone"` substring shows up in the
	// scaffold's explanatory comment in the static branch — skip
	// comment / whitespace-only lines and check the remaining code
	// has no actual standalone wiring.
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if strings.Contains(trimmed, `output: "standalone"`) {
			t.Errorf("next.config.ts default emitted active `output: \"standalone\"` line outside comments; got:\n%s\nfull file:\n%s", trimmed, s)
		}
	}
}

// TestGenerateFrontendFiles_StandaloneOptIn verifies that passing
// Output="standalone" through FrontendGenOptions reinstates the old
// Node-sidecar shape — needed for users who actually run server
// components / server actions in production and use the scaffolded
// Dockerfile.
func TestGenerateFrontendFiles_StandaloneOptIn(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFrontendFilesWithOptions(
		dir, "example.com/myapp", "myapp", "web", 8080, "",
		FrontendGenOptions{Output: "standalone"},
	); err != nil {
		t.Fatalf("GenerateFrontendFilesWithOptions: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "frontends", "web", "next.config.ts"))
	if err != nil {
		t.Fatalf("read next.config.ts: %v", err)
	}
	s := string(body)

	if !strings.Contains(s, `output: "standalone"`) {
		t.Errorf("next.config.ts (Output=standalone) must contain `output: \"standalone\"`; got:\n%s", s)
	}
	if !strings.Contains(s, `outputFileTracingRoot`) {
		t.Errorf("next.config.ts (Output=standalone) must contain outputFileTracingRoot so the standalone bundle lands at the path the Dockerfile expects; got:\n%s", s)
	}
}

// TestGenerateFrontendFiles_RealBackendByDefault pins the J-round fix 4:
// the scaffold must NOT ship with mock mode silently enabled. The
// default is the real backend (apiurl_gen wiring); mock mode is an
// explicit .env.local opt-in, and when it IS on the generated layout
// renders a visible "MOCK DATA" banner so a working-looking UI can
// never masquerade as a working stack.
func TestGenerateFrontendFiles_RealBackendByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFrontendFiles(dir, "example.com/myapp", "myapp", "web", 8080, ""); err != nil {
		t.Fatalf("GenerateFrontendFiles: %v", err)
	}
	feDir := filepath.Join(dir, "frontends", "web")

	env, err := os.ReadFile(filepath.Join(feDir, ".env.local"))
	if err != nil {
		t.Fatalf("read .env.local: %v", err)
	}
	for _, line := range strings.Split(string(env), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "NEXT_PUBLIC_MOCK_API=") && strings.TrimPrefix(trimmed, "NEXT_PUBLIC_MOCK_API=") != "" {
			t.Errorf(".env.local silently enables mock mode (%q) — mock must be an explicit opt-in", trimmed)
		}
	}

	layout, err := os.ReadFile(filepath.Join(feDir, "src", "app", "layout.tsx"))
	if err != nil {
		t.Fatalf("read layout.tsx: %v", err)
	}
	if !strings.Contains(string(layout), "MOCK DATA — backend not connected") {
		t.Error("layout.tsx should render the mock-mode banner when NEXT_PUBLIC_MOCK_API is enabled")
	}
	if !strings.Contains(string(layout), "MockModeBanner") {
		t.Error("layout.tsx should mount MockModeBanner")
	}
}

// TestGenerateFrontendFiles_ViteRealBackendByDefault is the vite-spa
// flavor of the same pin.
func TestGenerateFrontendFiles_ViteRealBackendByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFrontendFiles(dir, "example.com/myapp", "myapp", "web", 8080, "vite-spa"); err != nil {
		t.Fatalf("GenerateFrontendFiles: %v", err)
	}
	feDir := filepath.Join(dir, "frontends", "web")

	env, err := os.ReadFile(filepath.Join(feDir, ".env.local"))
	if err != nil {
		t.Fatalf("read .env.local: %v", err)
	}
	for _, line := range strings.Split(string(env), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "VITE_MOCK_API=") && strings.TrimPrefix(trimmed, "VITE_MOCK_API=") != "" {
			t.Errorf(".env.local silently enables mock mode (%q) — mock must be an explicit opt-in", trimmed)
		}
	}

	app, err := os.ReadFile(filepath.Join(feDir, "src", "App.tsx"))
	if err != nil {
		t.Fatalf("read App.tsx: %v", err)
	}
	if !strings.Contains(string(app), "MOCK DATA — backend not connected") {
		t.Error("App.tsx should render the mock-mode banner when VITE_MOCK_API is enabled")
	}
}
