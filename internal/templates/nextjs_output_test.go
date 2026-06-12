package templates

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestNextJSConfig_DefaultsToStandalone guards the new-project default:
// production builds emit a self-contained Node server, not a static
// export.
//
// The previous default ("static" → `output: "export"` gated on
// NODE_ENV=production) broke `npm run build` on every project the
// moment it had one CRUD entity: forge generates dynamic detail/edit
// routes (`/<slug>/[id]`) whose ids only exist at runtime, and
// `output: "export"` requires generateStaticParams() on every dynamic
// segment — Next.js fails the build with 'Page "/<slug>/[id]" is
// missing "generateStaticParams()"'. Standalone supports dynamic
// client routes AND is the shape the shipped Dockerfile copies
// (.next-prod/standalone/server.js in production).
//
// This test fails if anyone:
//   - drops `output: "standalone"` / outputFileTracingRoot from the
//     standalone branch, or
//   - reverts the empty-Output default to the static-export shape.
func TestNextJSConfig_DefaultsToStandalone(t *testing.T) {
	for _, output := range []string{"standalone", ""} {
		content, err := FrontendTemplates().Render(
			filepath.Join("nextjs", "next.config.ts.tmpl"),
			FrontendTemplateData{
				FrontendName: "dashboard",
				ProjectName:  "testproject",
				Output:       output,
			},
		)
		if err != nil {
			t.Fatalf("render nextjs/next.config.ts.tmpl (output=%q): %v", output, err)
		}
		s := string(content)

		if !strings.Contains(s, `output: "standalone"`) {
			t.Errorf("next.config.ts (output=%q) must contain `output: \"standalone\"` so the build emits .next-prod/standalone/server.js for the Dockerfile; got:\n%s", output, s)
		}
		if !strings.Contains(s, `outputFileTracingRoot: path.join(__dirname)`) {
			t.Errorf("next.config.ts (output=%q) must contain outputFileTracingRoot so the standalone bundle lands at .next-prod/standalone/server.js (not under a workspace-rooted subpath the Dockerfile can't find); got:\n%s", output, s)
		}
		// The default must NOT emit the static-export conditional — that
		// shape fails `next build` on the generated dynamic [id] routes.
		if strings.Contains(s, `{ output: "export" }`) {
			t.Errorf("next.config.ts (output=%q) must NOT contain the static-export conditional — `output: \"export\"` breaks `npm run build` on generated dynamic CRUD routes; got:\n%s", output, s)
		}
		// The distDir fence keeps a production build from clobbering the
		// dev server's .next directory (journey fr-cb84c64912).
		fence := `distDir: process.env.NODE_ENV === "production" ? ".next-prod" : ".next",`
		if !strings.Contains(s, fence) {
			t.Errorf("next.config.ts (output=%q) must carry the distDir fence %q so `npm run build` can't clobber a live dev server's .next; got:\n%s", output, fence, s)
		}
	}
}

// TestNextJSConfig_StaticOptIn guards the opt-in path. When the user
// sets `output: static` in forge.yaml (or passes `--output static` to
// `forge add frontend`), the rendered next.config.ts must emit the
// NODE_ENV-gated static-export spread — production builds emit `out/`
// for CDN/object-store hosting while `next dev` stays unchanged.
//
// This mode is deliberately NOT the default: it is incompatible with
// the generated dynamic CRUD routes (see the standalone-default test).
func TestNextJSConfig_StaticOptIn(t *testing.T) {
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

	// The opt-in must document its dynamic-route incompatibility — the
	// user who picks static needs to know `npm run build` fails on
	// generated `/<slug>/[id]` pages.
	if !strings.Contains(s, "generateStaticParams") {
		t.Errorf("next.config.ts (output=static) must mention generateStaticParams so the dynamic-route incompatibility is documented at the point of use; got:\n%s", s)
	}

	// Static mode must NOT carry the distDir fence: in export mode
	// Next.js treats a custom distDir as the export destination — the
	// site would land in .next-prod instead of the documented out/ —
	// while build intermediates go to .next regardless. Verified
	// empirically against Next 15 (the basepath corpus fixture asserts
	// out/ exists after a static build).
	if strings.Contains(s, "distDir:") {
		t.Errorf("next.config.ts (output=static) must not set distDir — export mode would emit the site there instead of out/; got:\n%s", s)
	}

	// Static mode must not ALSO emit standalone output — contradictory.
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

// TestNextJSConfig_StaticOptIn_BasePathGuard verifies that the static
// branch keeps its base-path build guard: when forge.yaml declares a
// base_path but NEXT_PUBLIC_BASE_PATH empties it at build time, the
// production build must fail loudly instead of baking root-mounted
// URLs into a static export.
func TestNextJSConfig_StaticOptIn_BasePathGuard(t *testing.T) {
	content, err := FrontendTemplates().Render(
		filepath.Join("nextjs", "next.config.ts.tmpl"),
		FrontendTemplateData{
			FrontendName: "admin",
			ProjectName:  "testproject",
			Output:       "static",
			BasePath:     "/admin",
		},
	)
	if err != nil {
		t.Fatalf("render nextjs/next.config.ts.tmpl: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, `refusing to bake a `) || !strings.Contains(s, `throw new Error(`) {
		t.Errorf("next.config.ts (output=static, base_path=/admin) must keep the fail-loud base-path guard; got:\n%s", s)
	}
}

// TestNextJSConfig_StandaloneExplicit verifies the explicit standalone
// opt-in renders identically in shape to the default: `output:
// "standalone"` + `outputFileTracingRoot: path.join(__dirname)` so the
// scaffold-shipped Dockerfile finds `.next-prod/standalone/server.js`.
func TestNextJSConfig_StandaloneExplicit(t *testing.T) {
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
		t.Errorf("next.config.ts (output=standalone) must contain `output: \"standalone\"`; got:\n%s", s)
	}
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
