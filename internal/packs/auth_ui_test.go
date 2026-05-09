package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

// TestAuthUIPackManifest confirms the auth-ui pack is shaped as a frontend
// pack with the expected variant knob and provider-keyed npm deps.
func TestAuthUIPackManifest(t *testing.T) {
	t.Parallel()

	p, err := LoadPack("auth-ui")
	if err != nil {
		t.Fatalf("LoadPack(auth-ui): %v", err)
	}

	if !p.IsFrontendKind() {
		t.Errorf("auth-ui must be Kind=frontend, got %q", p.Kind)
	}
	if p.Subpath != "src/components/auth" {
		t.Errorf("Subpath = %q, want src/components/auth", p.Subpath)
	}
	if got, ok := p.Config.Defaults["provider"].(string); !ok || got != "jwt-auth" {
		t.Errorf("default provider = %v, want jwt-auth", p.Config.Defaults["provider"])
	}

	// Required utility deps shipped to every variant.
	wantDeps := map[string]bool{
		"react-hook-form":         false,
		"zod":                     false,
		"@hookform/resolvers":     false,
		"zustand":                 false,
	}
	for _, dep := range p.NPMDependencies {
		// npm specs split on the *last* `@` so scoped packages like
		// `@hookform/resolvers@^3.9.0` resolve to `@hookform/resolvers`.
		modPath := dep
		if i := strings.LastIndex(dep, "@"); i > 0 {
			modPath = dep[:i]
		}
		if _, ok := wantDeps[modPath]; ok {
			wantDeps[modPath] = true
		}
	}
	for dep, found := range wantDeps {
		if !found {
			t.Errorf("auth-ui must declare npm dep %q", dep)
		}
	}

	// Provider-keyed extras: clerk needs @clerk/nextjs, firebase-auth
	// needs firebase. jwt-auth is intentionally empty (no SDK).
	for provider, wantPrefix := range map[string]string{
		"clerk":         "@clerk/nextjs",
		"firebase-auth": "firebase",
	} {
		extras := p.ProviderNPMDependencies[provider]
		found := false
		for _, e := range extras {
			if strings.HasPrefix(e, wantPrefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("provider %q must include npm dep starting with %q, got %v", provider, wantPrefix, extras)
		}
	}

	// Every output path must template into the per-frontend tree so a single
	// manifest installs into all declared frontends.
	for _, f := range p.Files {
		if !strings.Contains(f.Output, "{{.FrontendPath}}") {
			t.Errorf("auth-ui file %q output must reference {{.FrontendPath}}", f.Output)
		}
		if !strings.Contains(f.Output, "src/components/auth/") {
			t.Errorf("auth-ui file %q must land under src/components/auth/", f.Output)
		}
		if f.Overwrite != "once" {
			t.Errorf("auth-ui file %q must be overwrite: once (got %q)", f.Output, f.Overwrite)
		}
	}
}

// TestAuthUITemplatesRenderPerProvider asserts every (template × provider)
// pair renders without error and produces non-empty output. This is the
// first-line guard that the {{if eq .PackConfig.provider …}} branches all
// type-check at template-parse time.
func TestAuthUITemplatesRenderPerProvider(t *testing.T) {
	t.Parallel()

	p, err := LoadPack("auth-ui")
	if err != nil {
		t.Fatalf("LoadPack(auth-ui): %v", err)
	}

	providers := []string{"jwt-auth", "clerk", "firebase-auth"}
	for _, provider := range providers {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			cfg := mergePackConfig(p.Config.Defaults, map[string]any{"provider": provider})

			data := map[string]any{
				"ModulePath":   "github.com/example/myapp",
				"ProjectName":  "myapp",
				"PackConfig":   cfg,
				"FrontendName": "web",
				"FrontendPath": "frontends/web",
				"FrontendType": "nextjs",
				"FrontendKind": "web",
			}

			for _, f := range p.Files {
				f := f
				t.Run(f.Template, func(t *testing.T) {
					t.Parallel()
					tmplPath := "auth-ui/templates/" + f.Template
					tmplContent, err := packsFS.ReadFile(tmplPath)
					if err != nil {
						t.Fatalf("read %s: %v", tmplPath, err)
					}
					tmpl, err := template.New(f.Template).Funcs(templates.FuncMap()).Parse(string(tmplContent))
					if err != nil {
						t.Fatalf("parse %s: %v", f.Template, err)
					}
					var buf bytes.Buffer
					if err := tmpl.Execute(&buf, data); err != nil {
						t.Fatalf("execute %s for provider=%s: %v", f.Template, provider, err)
					}
					if buf.Len() == 0 {
						t.Errorf("%s for provider=%s produced empty output", f.Template, provider)
					}
				})
			}
		})
	}
}

// TestAuthUIProviderBranching confirms the rendered LoginForm.tsx contains
// provider-specific code: SignIn for clerk, signInWithEmailAndPassword for
// firebase-auth, fetch(...) for jwt-auth. This catches regressions where
// the {{if eq}} branches accidentally collapse to a single provider.
func TestAuthUIProviderBranching(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"jwt-auth":      "fetch(loginPath",
		"clerk":         "@clerk/nextjs",
		"firebase-auth": "signInWithEmailAndPassword",
	}

	p, err := LoadPack("auth-ui")
	if err != nil {
		t.Fatalf("LoadPack(auth-ui): %v", err)
	}

	tmplContent, err := packsFS.ReadFile("auth-ui/templates/LoginForm.tsx.tmpl")
	if err != nil {
		t.Fatalf("read LoginForm.tsx.tmpl: %v", err)
	}

	for provider, marker := range cases {
		provider := provider
		marker := marker
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			cfg := mergePackConfig(p.Config.Defaults, map[string]any{"provider": provider})
			tmpl, err := template.New("LoginForm.tsx.tmpl").Funcs(templates.FuncMap()).Parse(string(tmplContent))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var buf bytes.Buffer
			err = tmpl.Execute(&buf, map[string]any{
				"PackConfig":   cfg,
				"ProjectName":  "myapp",
				"FrontendName": "web",
			})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if !strings.Contains(buf.String(), marker) {
				t.Errorf("provider=%s LoginForm output should contain %q (got %d bytes)",
					provider, marker, buf.Len())
			}
		})
	}
}

// TestParseConfigOverrides covers the CLI-side `--config key=value`
// parser. Empty input returns nil; valid pairs round-trip; invalid pairs
// surface a useful error.
func TestParseConfigOverrides(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		out, err := ParseConfigOverrides(nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if out != nil {
			t.Errorf("got %v, want nil", out)
		}
	})

	t.Run("valid", func(t *testing.T) {
		out, err := ParseConfigOverrides([]string{"provider=clerk", "dev_mode_banner=false"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if out["provider"] != "clerk" {
			t.Errorf("provider = %v, want clerk", out["provider"])
		}
		if out["dev_mode_banner"] != "false" {
			t.Errorf("dev_mode_banner = %v, want false", out["dev_mode_banner"])
		}
	})

	t.Run("missing_equals", func(t *testing.T) {
		_, err := ParseConfigOverrides([]string{"provider"})
		if err == nil {
			t.Fatal("expected error for missing =")
		}
	})

	t.Run("empty_key", func(t *testing.T) {
		_, err := ParseConfigOverrides([]string{"=clerk"})
		if err == nil {
			t.Fatal("expected error for empty key")
		}
	})
}

// TestMergePackConfig confirms shallow-merge semantics: overrides win,
// defaults retained for keys not in overrides, both inputs may be nil.
func TestMergePackConfig(t *testing.T) {
	t.Parallel()

	defaults := map[string]any{"provider": "jwt-auth", "dev_mode_banner": true}
	overrides := map[string]any{"provider": "clerk"}

	out := mergePackConfig(defaults, overrides)
	if out["provider"] != "clerk" {
		t.Errorf("override should win: got %v", out["provider"])
	}
	if out["dev_mode_banner"] != true {
		t.Errorf("default should remain: got %v", out["dev_mode_banner"])
	}

	// Defaults shouldn't be mutated.
	if defaults["provider"] != "jwt-auth" {
		t.Errorf("defaults map was mutated: %v", defaults)
	}

	if got := mergePackConfig(nil, nil); len(got) != 0 {
		t.Errorf("nil×nil should produce empty map, got %v", got)
	}
}
