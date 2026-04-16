package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestJWTAuthPackManifest(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack(jwt-auth) error: %v", err)
	}

	if p.Name != "jwt-auth" {
		t.Errorf("Name = %q, want %q", p.Name, "jwt-auth")
	}
	if !strings.Contains(p.Description, "JWKS") {
		t.Errorf("Description should mention JWKS, got: %s", p.Description)
	}
	if !strings.Contains(p.Description, "dev-mode") {
		t.Errorf("Description should mention dev-mode, got: %s", p.Description)
	}

	// Check dependencies
	wantDeps := map[string]bool{
		"github.com/golang-jwt/jwt/v5":  false,
		"github.com/MicahParks/keyfunc/v3": false,
	}
	for _, dep := range p.Dependencies {
		if _, ok := wantDeps[dep]; ok {
			wantDeps[dep] = true
		}
	}
	for dep, found := range wantDeps {
		if !found {
			t.Errorf("missing dependency: %s", dep)
		}
	}

	// Check files reference the correct templates
	templateNames := make(map[string]bool)
	for _, f := range p.Files {
		templateNames[f.Template] = true
	}
	if !templateNames["jwt_validator.go.tmpl"] {
		t.Error("files should include jwt_validator.go.tmpl")
	}
	if !templateNames["dev_auth.go.tmpl"] {
		t.Error("files should include dev_auth.go.tmpl")
	}

	// Check generate hook
	if len(p.Generate) != 1 {
		t.Fatalf("len(Generate) = %d, want 1", len(p.Generate))
	}
	if p.Generate[0].Template != "auth_gen_override.go.tmpl" {
		t.Errorf("Generate[0].Template = %q, want %q", p.Generate[0].Template, "auth_gen_override.go.tmpl")
	}
}

func TestJWTAuthTemplatesRender(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack(jwt-auth) error: %v", err)
	}

	data := map[string]any{
		"ModulePath":  "github.com/example/myapp",
		"ProjectName": "myapp",
		"PackConfig":  p.Config.Defaults,
	}

	// Test all file templates render without error
	allFiles := append(p.Files, p.Generate...)
	for _, f := range allFiles {
		t.Run(f.Template, func(t *testing.T) {
			tmplPath := "jwt-auth/templates/" + f.Template
			tmplContent, err := packsFS.ReadFile(tmplPath)
			if err != nil {
				t.Fatalf("read template %s: %v", tmplPath, err)
			}

			tmpl, err := template.New(f.Template).Funcs(templates.FuncMap()).Parse(string(tmplContent))
			if err != nil {
				t.Fatalf("parse template %s: %v", f.Template, err)
			}

			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("execute template %s: %v", f.Template, err)
			}

			output := buf.String()
			if len(output) == 0 {
				t.Errorf("template %s produced empty output", f.Template)
			}

			// Verify output contains expected package declaration
			if !strings.Contains(output, "package middleware") {
				t.Errorf("template %s output missing 'package middleware'", f.Template)
			}
		})
	}
}

func TestJWTValidatorTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/jwt_validator.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	// Should contain key types and functions
	checks := []string{
		"JWTValidator",
		"JWTValidatorConfig",
		"NewJWTValidator",
		"ValidateToken",
		"keyfunc.NewDefault",
		"func (v *JWTValidator) Close()",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("jwt_validator.go.tmpl should contain %q", check)
		}
	}
}

func TestDevAuthTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/dev_auth.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"DevAuthEnabled",
		"DevClaims",
		"dev-user-001",
		"ENVIRONMENT",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("dev_auth.go.tmpl should contain %q", check)
		}
	}
}

func TestAuthGenOverrideTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/auth_gen_override.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"InitAuth",
		"CloseAuth",
		"GeneratedAuthInterceptor",
		"DevAuthEnabled",
		"jwtValidator",
		"JWT_JWKS_URL",
		"JWT_SIGNING_METHOD",
		"ContextWithClaims",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("auth_gen_override.go.tmpl should contain %q", check)
		}
	}
}
