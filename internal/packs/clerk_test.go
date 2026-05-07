package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestClerkPackManifest(t *testing.T) {
	p, err := LoadPack("clerk")
	if err != nil {
		t.Fatalf("LoadPack(clerk) error: %v", err)
	}

	if p.Name != "clerk" {
		t.Errorf("Name = %q, want %q", p.Name, "clerk")
	}
	if p.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", p.Version, "1.0.0")
	}
	if !strings.Contains(p.Description, "Clerk") {
		t.Errorf("Description should mention Clerk, got: %s", p.Description)
	}
	if !strings.Contains(p.Description, "JWKS") {
		t.Errorf("Description should mention JWKS, got: %s", p.Description)
	}

	if p.Config.Section != "auth" {
		t.Errorf("Config.Section = %q, want %q", p.Config.Section, "auth")
	}

	// Check dependencies (modulePath substring match — pinned versions are
	// appended after "@"). Webhook-specific deps (svix-webhooks) moved out
	// with the clerk-webhook starter — they should NOT appear here anymore.
	wantDepPrefixes := []string{
		"github.com/clerk/clerk-sdk-go/v2",
		"github.com/MicahParks/keyfunc/v3",
		"github.com/golang-jwt/jwt/v5",
	}
	for _, prefix := range wantDepPrefixes {
		found := false
		for _, dep := range p.Dependencies {
			if dep == prefix || strings.HasPrefix(dep, prefix+"@") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing dependency: %s", prefix)
		}
	}
	for _, dep := range p.Dependencies {
		if strings.HasPrefix(dep, "github.com/svix/") {
			t.Errorf("clerk pack must not depend on svix-webhooks anymore (moved to clerk-webhook starter); got %q", dep)
		}
	}

	templateNames := make(map[string]bool)
	for _, f := range p.Files {
		templateNames[f.Template] = true
	}
	if !templateNames["clerk_auth.go.tmpl"] {
		t.Error("files should include clerk_auth.go.tmpl")
	}
	if !templateNames["clerk_dev_auth.go.tmpl"] {
		t.Error("files should include clerk_dev_auth.go.tmpl")
	}
	// The webhook + user-entity templates have moved to the clerk-webhook
	// starter — make sure the pack does NOT still ship them.
	if templateNames["clerk_webhook.go.tmpl"] {
		t.Error("clerk_webhook.go.tmpl must NOT be a pack template anymore (moved to clerk-webhook starter)")
	}
	if templateNames["clerk_user_entity.proto.tmpl"] {
		t.Error("clerk_user_entity.proto.tmpl must NOT be a pack template anymore (project-specific data model)")
	}

	// All middleware-package files land under pkg/middleware/auth/clerk/ —
	// the per-pack nested subpackage rule.
	for _, f := range p.Files {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/clerk/") {
			t.Errorf("file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/clerk/<file>", f.Output)
		}
	}
	for _, f := range p.Generate {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/clerk/") {
			t.Errorf("generate file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/clerk/<file>", f.Output)
		}
	}

	// Subpath hint must match the actual installation tree so users see the
	// right thing in `forge pack info`.
	if p.Subpath != "middleware/auth/clerk" {
		t.Errorf("Subpath = %q, want %q", p.Subpath, "middleware/auth/clerk")
	}

	// Check generate hook
	if len(p.Generate) != 1 {
		t.Fatalf("len(Generate) = %d, want 1", len(p.Generate))
	}
	if p.Generate[0].Template != "clerk_auth_gen.go.tmpl" {
		t.Errorf("Generate[0].Template = %q, want %q", p.Generate[0].Template, "clerk_auth_gen.go.tmpl")
	}
}

func TestClerkTemplatesRender(t *testing.T) {
	p, err := LoadPack("clerk")
	if err != nil {
		t.Fatalf("LoadPack(clerk) error: %v", err)
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
			tmplPath := "clerk/templates/" + f.Template
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
		})
	}
}

func TestClerkAuthTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("clerk/templates/clerk_auth.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package clerk",
		"type Validator struct",
		"type ValidatorConfig struct",
		"func NewValidator(",
		"ValidateToken",
		"CLERK_DOMAIN",
		".well-known/jwks.json",
		"extractClaims",
		"org_id",
		"org_role",
		"org_permissions",
		"func (v *Validator) Close()",
		"middleware.Claims",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("clerk_auth.go.tmpl should contain %q", check)
		}
	}
}

func TestClerkDevAuthTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("clerk/templates/clerk_dev_auth.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package clerk",
		// Unprefixed names — the package itself namespaces them.
		"func DevAuthEnabled(",
		"func DevClaims(",
		"CLERK_DOMAIN",
		"DEV_USER_ID",
		"DEV_ORG_ID",
		"middleware.Claims",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("clerk_dev_auth.go.tmpl should contain %q", check)
		}
	}

	// Negative checks: the prefixed names from the pre-subpackage era
	// must not reappear.
	for _, banned := range []string{"ClerkDevAuthEnabled", "ClerkDevClaims"} {
		if strings.Contains(content, banned) {
			t.Errorf("clerk_dev_auth.go.tmpl must not declare prefixed %q (now lives in package clerk)", banned)
		}
	}
}

func TestClerkAuthGenTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("clerk/templates/clerk_auth_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package clerk",
		"func Init(",
		"func Close(",
		"func Interceptor(",
		"validator",
		"CLERK_DOMAIN",
		"middleware.ContextWithClaims",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("clerk_auth_gen.go.tmpl should contain %q", check)
		}
	}
}

func TestListPacksIncludesClerk(t *testing.T) {
	packs, err := ListPacks()
	if err != nil {
		t.Fatalf("ListPacks() error: %v", err)
	}

	found := false
	for _, p := range packs {
		if p.Name == "clerk" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListPacks() did not include clerk")
	}
}

func TestGetPackClerk(t *testing.T) {
	p, err := GetPack("clerk")
	if err != nil {
		t.Fatalf("GetPack(clerk) error: %v", err)
	}
	if p.Name != "clerk" {
		t.Errorf("Name = %q, want %q", p.Name, "clerk")
	}
}

// Defense-in-depth: the dropped packs must NOT be loadable as packs anymore.
// They should now exist as starters via `forge starter add`.
func TestDroppedPacksNotLoadable(t *testing.T) {
	for _, name := range []string{"stripe", "twilio"} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadPack(name); err == nil {
				t.Errorf("LoadPack(%s) succeeded — should be a starter now, not a pack", name)
			}
		})
	}
}
