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
// (.next-prod/standalone/server.js) and the only default that builds with
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
// the scaffold must NOT ship with mock mode silently enabled. Since the
// hand-editable .env.local was ripped out, the guarantee is now
// STRUCTURAL: no committed dotenv can bake mock on, and connect.ts gates
// mock strictly on the NEXT_PUBLIC_MOCK_API env var (unset => real
// backend). When mock IS on the generated layout renders a visible "MOCK
// DATA" banner so a working-looking UI can never masquerade as a working
// stack.
func TestGenerateFrontendFiles_RealBackendByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFrontendFiles(dir, "example.com/myapp", "myapp", "web", 8080, ""); err != nil {
		t.Fatalf("GenerateFrontendFiles: %v", err)
	}
	feDir := filepath.Join(dir, "frontends", "web")

	// The hand-editable .env.local is gone — mock mode can no longer be
	// silently baked into a committed dotenv; it is an explicit runtime
	// env-var opt-in (or KCL config).
	if _, err := os.Stat(filepath.Join(feDir, ".env.local")); !os.IsNotExist(err) {
		t.Errorf("scaffold must not ship a .env.local (stat err = %v); mock is a runtime env opt-in", err)
	}

	// connect.ts is the structural guard: mock engages ONLY when
	// NEXT_PUBLIC_MOCK_API is explicitly set, so the default (unset) is the
	// real backend.
	connect, err := os.ReadFile(filepath.Join(feDir, "src", "lib", "connect.ts"))
	if err != nil {
		t.Fatalf("read connect.ts: %v", err)
	}
	if !strings.Contains(string(connect), "NEXT_PUBLIC_MOCK_API") {
		t.Error("connect.ts must gate mock mode on NEXT_PUBLIC_MOCK_API (real backend is the unset default)")
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

	if _, err := os.Stat(filepath.Join(feDir, ".env.local")); !os.IsNotExist(err) {
		t.Errorf("scaffold must not ship a .env.local (stat err = %v); mock is a runtime env opt-in", err)
	}

	connect, err := os.ReadFile(filepath.Join(feDir, "src", "lib", "connect.ts"))
	if err != nil {
		t.Fatalf("read connect.ts: %v", err)
	}
	if !strings.Contains(string(connect), "VITE_MOCK_API") {
		t.Error("connect.ts must gate mock mode on VITE_MOCK_API (real backend is the unset default)")
	}

	app, err := os.ReadFile(filepath.Join(feDir, "src", "App.tsx"))
	if err != nil {
		t.Fatalf("read App.tsx: %v", err)
	}
	if !strings.Contains(string(app), "MOCK DATA — backend not connected") {
		t.Error("App.tsx should render the mock-mode banner when VITE_MOCK_API is enabled")
	}
}
