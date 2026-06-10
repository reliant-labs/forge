package templates

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestNextJSConfig_DefaultsToStaticExport guards the new-project default:
// production builds emit a static export, not a Node sidecar.
//
// The common forge shape — Next.js shell + Connect RPC against a Go
// backend — has no need for a Node runtime in production. A static
// export drops to a CDN / object store; standalone runs a Node server
// that exists only to serve the same static skeleton. The old default
// (`output: "standalone"`) was paying for runtime users didn't use.
//
// This test fails if anyone:
//   - drops the conditional `{ output: "export" }` shape, or
//   - reverts the empty-Output default to "standalone".
func TestNextJSConfig_DefaultsToStaticExport(t *testing.T) {
	content, err := FrontendTemplates().Render(
		filepath.Join("nextjs", "next.config.ts.tmpl"),
		FrontendTemplateData{
			FrontendName: "dashboard",
			ProjectName:  "testproject",
			Output:       "static",
		},
	)
	if err != nil {
		t.Fatalf("render nextjs/next.config.ts.tmpl: %v", err)
	}
	s := string(content)

	// The load-bearing shape — a NODE_ENV-gated spread that pins
	// `output: "export"` in production only. Dev stays as `next dev`.
	want := `...(process.env.NODE_ENV === "production" ? { output: "export" } : {}),`
	if !strings.Contains(s, want) {
		t.Errorf("next.config.ts (output=static) must contain %q so production builds emit a static export and dev stays unchanged; got:\n%s", want, s)
	}

	// Guard against accidental revert to the old standalone default.
	// `output: "standalone"` shouldn't appear in any active (non-comment)
	// line under output=static. The scaffold's explanatory comment
	// mentions the literal as a pointer to the opt-in.
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if strings.Contains(trimmed, `output: "standalone"`) {
			t.Errorf("next.config.ts (output=static) emitted active `output: \"standalone\"` line outside comments; got:\n%s\nfull file:\n%s", trimmed, s)
		}
	}
}

// TestNextJSConfig_EmptyOutputDefaultsToStatic verifies the canonicalisation
// path: passing Output="" through the template (i.e. a caller who didn't
// thread the field) renders the same shape as Output="static". The
// generator layer normalises empty → "static" before the template runs,
// so this exercises the template's own `else` branch as the safety net.
func TestNextJSConfig_EmptyOutputDefaultsToStatic(t *testing.T) {
	content, err := FrontendTemplates().Render(
		filepath.Join("nextjs", "next.config.ts.tmpl"),
		FrontendTemplateData{
			FrontendName: "dashboard",
			ProjectName:  "testproject",
			// Output deliberately empty.
		},
	)
	if err != nil {
		t.Fatalf("render nextjs/next.config.ts.tmpl: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, `{ output: "export" }`) {
		t.Errorf("next.config.ts (output=\"\") must fall through to the static shape so callers that don't thread Output still get the new default; got:\n%s", s)
	}
}

// TestNextJSConfig_StandaloneOptIn guards the opt-in path. When the
// user sets `output: standalone` in forge.yaml (or passes
// `--output standalone` to `forge add frontend`), the rendered
// next.config.ts must reinstate the old shape: `output: "standalone"`
// + `outputFileTracingRoot: path.join(__dirname)` so the
// scaffold-shipped Dockerfile still finds `.next/standalone/server.js`
// where it expects.
func TestNextJSConfig_StandaloneOptIn(t *testing.T) {
	content, err := FrontendTemplates().Render(
		filepath.Join("nextjs", "next.config.ts.tmpl"),
		FrontendTemplateData{
			FrontendName: "dashboard",
			ProjectName:  "testproject",
			Output:       "standalone",
		},
	)
	if err != nil {
		t.Fatalf("render nextjs/next.config.ts.tmpl: %v", err)
	}
	s := string(content)

	if !strings.Contains(s, `output: "standalone"`) {
		t.Errorf("next.config.ts (output=standalone) must contain `output: \"standalone\"` so the build emits .next/standalone/server.js for the Dockerfile; got:\n%s", s)
	}
	if !strings.Contains(s, `outputFileTracingRoot: path.join(__dirname)`) {
		t.Errorf("next.config.ts (output=standalone) must contain outputFileTracingRoot so the standalone bundle lands at .next/standalone/server.js (not under a workspace-rooted subpath the Dockerfile can't find); got:\n%s", s)
	}
	// Standalone mode must NOT also emit the static-export conditional —
	// that would be contradictory and Next.js would error.
	if strings.Contains(s, `{ output: "export" }`) {
		t.Errorf("next.config.ts (output=standalone) must NOT contain the static-export conditional; got:\n%s", s)
	}
}

// TestNextJSConfig_ServerMode verifies the third opt-in: no `output:`
// field at all. Used when the project wants full Next.js for both dev
// and prod (server components, ISR, custom server, managed host).
func TestNextJSConfig_ServerMode(t *testing.T) {
	content, err := FrontendTemplates().Render(
		filepath.Join("nextjs", "next.config.ts.tmpl"),
		FrontendTemplateData{
			FrontendName: "dashboard",
			ProjectName:  "testproject",
			Output:       "server",
		},
	)
	if err != nil {
		t.Fatalf("render nextjs/next.config.ts.tmpl: %v", err)
	}
	s := string(content)

	// Server mode = no output: key at all (neither "standalone" nor
	// the export conditional). The only `output` mentions allowed are
	// inside comments.
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if strings.Contains(trimmed, `output: "standalone"`) ||
			strings.Contains(trimmed, `{ output: "export" }`) {
			t.Errorf("next.config.ts (output=server) must not emit an `output:` field outside comments; offending line:\n%s\nfull file:\n%s", trimmed, s)
		}
	}
}
