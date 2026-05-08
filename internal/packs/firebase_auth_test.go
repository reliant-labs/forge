package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestFirebaseAuthPackManifest(t *testing.T) {
	p, err := LoadPack("firebase-auth")
	if err != nil {
		t.Fatalf("LoadPack(firebase-auth) error: %v", err)
	}

	if p.Name != "firebase-auth" {
		t.Errorf("Name = %q, want %q", p.Name, "firebase-auth")
	}
	if !strings.Contains(p.Description, "Firebase") {
		t.Errorf("Description should mention Firebase, got: %s", p.Description)
	}
	if !strings.Contains(p.Description, "JWKS") {
		t.Errorf("Description should mention JWKS, got: %s", p.Description)
	}

	// Dependencies match the auth-pack standard set.
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

	// All middleware-package files must land under
	// pkg/middleware/auth/firebase/ — the per-pack nested subpackage that
	// prevents collisions with other auth packs.
	for _, f := range p.Files {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/firebase/") {
			t.Errorf("file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/firebase/<file>", f.Output)
		}
	}
	for _, f := range p.Generate {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/firebase/") {
			t.Errorf("generate file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/firebase/<file>", f.Output)
		}
	}

	if p.Subpath != "middleware/auth/firebase" {
		t.Errorf("Subpath = %q, want %q", p.Subpath, "middleware/auth/firebase")
	}

	if len(p.Generate) != 1 {
		t.Fatalf("len(Generate) = %d, want 1", len(p.Generate))
	}
	if p.Generate[0].Template != "firebase_auth_gen.go.tmpl" {
		t.Errorf("Generate[0].Template = %q, want %q", p.Generate[0].Template, "firebase_auth_gen.go.tmpl")
	}
}

func TestFirebaseAuthTemplatesRender(t *testing.T) {
	p, err := LoadPack("firebase-auth")
	if err != nil {
		t.Fatalf("LoadPack(firebase-auth) error: %v", err)
	}

	data := map[string]any{
		"ModulePath":  "github.com/example/myapp",
		"ProjectName": "myapp",
		"PackConfig":  p.Config.Defaults,
	}

	allFiles := append(p.Files, p.Generate...)
	for _, f := range allFiles {
		t.Run(f.Template, func(t *testing.T) {
			tmplPath := "firebase-auth/templates/" + f.Template
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

			// All firebase-auth code lives in package firebase under
			// pkg/middleware/auth/firebase/.
			if !strings.Contains(output, "package firebase") {
				t.Errorf("template %s output missing 'package firebase'", f.Template)
			}
		})
	}
}

func TestFirebaseValidatorTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("firebase-auth/templates/firebase_validator.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package firebase",
		"type Validator struct",
		"type ValidatorConfig struct",
		"func NewValidator(",
		"func NewValidatorFromEnv(",
		"ValidateToken",
		"keyfunc.NewDefault",
		"securetoken.google.com",
		"securetoken@system.gserviceaccount.com",
		"middleware.Claims",
		"FIREBASE_PROJECT_IDS",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("firebase_validator.go.tmpl should contain %q", check)
		}
	}
}

func TestFirebaseDevAuthTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("firebase-auth/templates/firebase_dev_auth.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package firebase",
		"func DevAuthEnabled(",
		"func DevClaims(",
		"FIREBASE_PROJECT_IDS",
		"FIREBASE_PROJECT_ID",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("firebase_dev_auth.go.tmpl should contain %q", check)
		}
	}
}

func TestFirebaseAuthGenTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("firebase-auth/templates/firebase_auth_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package firebase",
		"func Init(",
		"func Close(",
		"func Interceptor(",
		"DevAuthEnabled",
		"validator",
		"middleware.ContextWithClaims",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("firebase_auth_gen.go.tmpl should contain %q", check)
		}
	}
}
