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
	if !strings.Contains(p.Description, "webhook") {
		t.Errorf("Description should mention webhook, got: %s", p.Description)
	}

	if p.Config.Section != "auth" {
		t.Errorf("Config.Section = %q, want %q", p.Config.Section, "auth")
	}

	// Check dependencies
	wantDeps := map[string]bool{
		"github.com/clerk/clerk-sdk-go/v2": false,
		"github.com/svix/svix-webhooks":    false,
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

	// Check files
	if len(p.Files) != 3 {
		t.Errorf("len(Files) = %d, want 3", len(p.Files))
	}
	templateNames := make(map[string]bool)
	for _, f := range p.Files {
		templateNames[f.Template] = true
	}
	if !templateNames["clerk_auth.go.tmpl"] {
		t.Error("files should include clerk_auth.go.tmpl")
	}
	if !templateNames["clerk_webhook.go.tmpl"] {
		t.Error("files should include clerk_webhook.go.tmpl")
	}
	if !templateNames["clerk_user_entity.proto.tmpl"] {
		t.Error("files should include clerk_user_entity.proto.tmpl")
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
		"ClerkJWTValidator",
		"ClerkValidatorConfig",
		"NewClerkJWTValidator",
		"ValidateToken",
		"CLERK_DOMAIN",
		".well-known/jwks.json",
		"extractClaims",
		"org_id",
		"org_role",
		"org_permissions",
		"func (v *ClerkJWTValidator) Close()",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("clerk_auth.go.tmpl should contain %q", check)
		}
	}
}

func TestClerkWebhookTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("clerk/templates/clerk_webhook.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"ClerkWebhookHandler",
		"OnUserCreated",
		"OnUserUpdated",
		"OnUserDeleted",
		"OnOrganizationCreated",
		"OnOrganizationUpdated",
		"OnMembershipCreated",
		"OnMembershipDeleted",
		"svix",
		"user.created",
		"user.updated",
		"user.deleted",
		"organization.created",
		"organization.updated",
		"organizationMembership.created",
		"organizationMembership.deleted",
		"CLERK_WEBHOOK_SECRET",
		"WebhookRouter",
		"ServeHTTP",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("clerk_webhook.go.tmpl should contain %q", check)
		}
	}
}

func TestClerkUserEntityTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("clerk/templates/clerk_user_entity.proto.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"entity_options",
		"field_options",
		"clerk_user_id",
		"table_name",
		"soft_delete",
		"timestamps",
		"primary_key",
		"auto_increment",
		"unique",
		"not_null",
		"message User",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("clerk_user_entity.proto.tmpl should contain %q", check)
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
		"InitAuth",
		"CloseAuth",
		"GeneratedAuthInterceptor",
		"DevAuthEnabled",
		"DevClaims",
		"clerkValidator",
		"CLERK_DOMAIN",
		"ContextWithClaims",
		"DEV_USER_ID",
		"DEV_ORG_ID",
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
