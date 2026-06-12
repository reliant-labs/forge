package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateFrontendFiles_DefaultsToStandalone verifies the
// new-scaffold default: a Next.js frontend scaffolded WITHOUT
// FrontendGenOptions.Output set emits a next.config.ts with
// `output: "standalone"` — the shape the shipped Dockerfile copies
// (.next/standalone/server.js) and the only default that builds with
// the dynamic `[id]` CRUD routes forge generates. The previous static
// default broke `npm run build` on every project the moment it had
// one entity ('Page "/<slug>/[id]" is missing "generateStaticParams()"
// so it cannot be used with "output: export"').
func TestGenerateFrontendFiles_DefaultsToStandalone(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFrontendFiles(dir, "example.com/myapp", "myapp", "web", 8080, ""); err != nil {
		t.Fatalf("GenerateFrontendFiles: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "frontends", "web", "next.config.ts"))
	if err != nil {
		t.Fatalf("read next.config.ts: %v", err)
	}
	s := string(body)

	if !strings.Contains(s, `output: "standalone"`) {
		t.Errorf("next.config.ts must default to `output: \"standalone\"` (Dockerfile + dynamic CRUD routes); got:\n%s", s)
	}
	if !strings.Contains(s, `outputFileTracingRoot`) {
		t.Errorf("next.config.ts default must contain outputFileTracingRoot so the standalone bundle lands at the path the Dockerfile expects; got:\n%s", s)
	}
	// The static-export conditional must NOT appear in the default —
	// it fails `next build` on the generated dynamic [id] routes.
	if strings.Contains(s, `{ output: "export" }`) {
		t.Errorf("next.config.ts default emitted the static-export conditional — that breaks `npm run build` on generated dynamic CRUD routes; got:\n%s", s)
	}
}

// TestGenerateFrontendFiles_StaticOptIn verifies that passing
// Output="static" through FrontendGenOptions yields the CDN/static
// export shape — for projects with no dynamic routes that want to drop
// the build artifacts on a CDN or object store.
func TestGenerateFrontendFiles_StaticOptIn(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFrontendFilesWithOptions(
		dir, "example.com/myapp", "myapp", "web", 8080, "",
		FrontendGenOptions{Output: "static"},
	); err != nil {
		t.Fatalf("GenerateFrontendFilesWithOptions: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "frontends", "web", "next.config.ts"))
	if err != nil {
		t.Fatalf("read next.config.ts: %v", err)
	}
	s := string(body)

	want := `...(process.env.NODE_ENV === "production" ? { output: "export" } : {}),`
	if !strings.Contains(s, want) {
		t.Errorf("next.config.ts (Output=static) must contain the NODE_ENV-gated static-export shape %q; got:\n%s", want, s)
	}
	// No active standalone wiring in static mode (the literal may
	// appear in explanatory comments).
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if strings.Contains(trimmed, `output: "standalone"`) {
			t.Errorf("next.config.ts (Output=static) emitted active `output: \"standalone\"` line outside comments; got:\n%s\nfull file:\n%s", trimmed, s)
		}
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
