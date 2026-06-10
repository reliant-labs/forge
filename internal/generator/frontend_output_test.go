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
