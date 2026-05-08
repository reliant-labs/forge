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

	// Check dependencies. Match by module path prefix so version-pinned
	// entries (e.g. "module@v1.2.3") still satisfy the check.
	wantDeps := map[string]bool{
		"github.com/golang-jwt/jwt/v5":     false,
		"github.com/MicahParks/keyfunc/v3": false,
	}
	for _, dep := range p.Dependencies {
		modPath := dep
		if i := strings.Index(dep, "@"); i >= 0 {
			modPath = dep[:i]
		}
		if _, ok := wantDeps[modPath]; ok {
			wantDeps[modPath] = true
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

	// Check that all middleware-package files land under
	// pkg/middleware/auth/jwtauth/ — the per-pack nested subpackage that
	// prevents collisions with other auth packs (e.g. clerk).
	for _, f := range p.Files {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/jwtauth/") {
			t.Errorf("file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/jwtauth/<file>", f.Output)
		}
	}
	for _, f := range p.Generate {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/jwtauth/") {
			t.Errorf("generate file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/jwtauth/<file>", f.Output)
		}
	}

	// Subpath hint must match the actual installation tree so users see the
	// right thing in `forge pack info`.
	if p.Subpath != "middleware/auth/jwtauth" {
		t.Errorf("Subpath = %q, want %q", p.Subpath, "middleware/auth/jwtauth")
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

			// Verify output contains expected package declaration. After
			// the per-pack-subpackage refactor, all jwt-auth code lives in
			// package jwtauth (under pkg/middleware/auth/jwtauth/).
			if !strings.Contains(output, "package jwtauth") {
				t.Errorf("template %s output missing 'package jwtauth'", f.Template)
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

	// Should contain key types and functions. Names are unprefixed because
	// the file lives in its own subpackage (jwtauth) — no collision with
	// other auth packs.
	checks := []string{
		"package jwtauth",
		"type Validator struct",
		"type ValidatorConfig struct",
		"func NewValidator(",
		"ValidateToken",
		"keyfunc.NewDefault",
		"func (v *Validator) Close()",
		// Cross-package references for shared Claims type.
		"middleware.Claims",
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
		"package jwtauth",
		// Unprefixed names — the package itself namespaces them.
		"func DevAuthEnabled(",
		"func DevClaims(",
		"dev-user-001",
		"ENVIRONMENT",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("dev_auth.go.tmpl should contain %q", check)
		}
	}

	// Negative checks: the old prefixed names belong to the pre-subpackage
	// era. They must not reappear, otherwise we lose the structural
	// collision-free property that the subpackage layout buys us.
	for _, banned := range []string{"JWTDevAuthEnabled", "JWTDevClaims"} {
		if strings.Contains(content, banned) {
			t.Errorf("dev_auth.go.tmpl must not declare prefixed %q (now lives in package jwtauth)", banned)
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
		"package jwtauth",
		"func Init(",
		"func Close(",
		"func Interceptor(",
		"DevAuthEnabled",
		"validator",
		"JWT_JWKS_URL",
		"JWT_SIGNING_METHOD",
		"middleware.ContextWithClaims",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("auth_gen_override.go.tmpl should contain %q", check)
		}
	}
}
