package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestAPIKeyPackManifest(t *testing.T) {
	p, err := LoadPack("api-key")
	if err != nil {
		t.Fatalf("LoadPack(api-key) error: %v", err)
	}

	if p.Name != "api-key" {
		t.Errorf("Name = %q, want %q", p.Name, "api-key")
	}
	if p.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", p.Version, "1.0.0")
	}
	if !strings.Contains(p.Description, "API key") {
		t.Errorf("Description should mention API key, got: %s", p.Description)
	}

	// Check config section
	if p.Config.Section != "auth" {
		t.Errorf("Config.Section = %q, want %q", p.Config.Section, "auth")
	}

	// Check dependencies
	wantDeps := map[string]bool{
		"golang.org/x/crypto": false,
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

	// Pack-to-pack dep: api-key requires audit-log to be installed first
	// because audit-log creates the audit_events table that api-key writes
	// audit entries to. forge auto-installs declared deps in topological
	// order (see ResolveInstallOrder).
	if len(p.DependsOn) != 1 || p.DependsOn[0] != "audit-log" {
		t.Errorf("DependsOn = %v, want [audit-log]", p.DependsOn)
	}

	// Check files (non-migration). Migrations live in their own block since
	// IDs are allocated at install time — see PackMigration in pack.go.
	if len(p.Files) != 1 {
		t.Errorf("len(Files) = %d, want 1", len(p.Files))
	}
	templateNames := make(map[string]bool)
	for _, f := range p.Files {
		templateNames[f.Template] = true
	}
	for _, want := range []string{"api_key_store.go.tmpl"} {
		if !templateNames[want] {
			t.Errorf("files should include %s", want)
		}
	}

	// Check overwrite policy
	for _, f := range p.Files {
		if f.Overwrite != "once" {
			t.Errorf("File %s has overwrite %q, want %q", f.Template, f.Overwrite, "once")
		}
	}

	// Check migrations
	if len(p.Migrations) != 1 {
		t.Fatalf("len(Migrations) = %d, want 1", len(p.Migrations))
	}
	mig := p.Migrations[0]
	if mig.Name != "api_keys" {
		t.Errorf("Migrations[0].Name = %q, want %q", mig.Name, "api_keys")
	}
	if mig.Up != "api_key_migration.sql.tmpl" {
		t.Errorf("Migrations[0].Up = %q, want %q", mig.Up, "api_key_migration.sql.tmpl")
	}
	if mig.Down != "api_key_migration_down.sql.tmpl" {
		t.Errorf("Migrations[0].Down = %q, want %q", mig.Down, "api_key_migration_down.sql.tmpl")
	}

	// Check generate hook
	if len(p.Generate) != 1 {
		t.Fatalf("len(Generate) = %d, want 1", len(p.Generate))
	}
	if p.Generate[0].Template != "api_key_validator_gen.go.tmpl" {
		t.Errorf("Generate[0].Template = %q, want %q", p.Generate[0].Template, "api_key_validator_gen.go.tmpl")
	}

	// Generate output must live under pkg/middleware/auth/apikey/ — the
	// nested subpath promised by the pack manifest. The store still lives
	// at pkg/apikey/ since it's a user-entity package, not auth wiring.
	wantPrefix := "pkg/middleware/auth/apikey/"
	if !strings.HasPrefix(p.Generate[0].Output, wantPrefix) {
		t.Errorf("Generate[0].Output = %q, want prefix %q", p.Generate[0].Output, wantPrefix)
	}

	if p.Subpath != "middleware/auth/apikey" {
		t.Errorf("Subpath = %q, want %q", p.Subpath, "middleware/auth/apikey")
	}
}

func TestAPIKeyTemplatesRender(t *testing.T) {
	p, err := LoadPack("api-key")
	if err != nil {
		t.Fatalf("LoadPack(api-key) error: %v", err)
	}

	data := map[string]any{
		"ModulePath":  "github.com/example/myapp",
		"ProjectName": "myapp",
		"PackConfig":  p.Config.Defaults,
	}

	// Test all file templates render without error
	allFiles := append(p.Files, p.Generate...)
	for _, m := range p.Migrations {
		allFiles = append(allFiles,
			PackFile{Template: m.Up},
			PackFile{Template: m.Down},
		)
	}
	for _, f := range allFiles {
		t.Run(f.Template, func(t *testing.T) {
			tmplPath := "api-key/templates/" + f.Template
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

func TestAPIKeyStoreTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("api-key/templates/api_key_store.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package apikey",
		"KeyStore",
		"CreateKey",
		"RevokeKey",
		"ListKeys",
		"RotateKey",
		"ValidateKey",
		"APIKey",
		"DBKeyStore",
		"NewDBKeyStore",
		"crypto/sha256",
		"crypto/rand",
		"database/sql",
		"fk_",
		"UpdateLastUsed",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("api_key_store.go.tmpl should contain %q", check)
		}
	}
}

func TestAPIKeyValidatorGenTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("api-key/templates/api_key_validator_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"Code generated by forge generate (api-key pack). DO NOT EDIT.",
		"package apikey",
		"type Validator struct",
		"func NewValidator(",
		"ValidateKey",
		"KeyStore",
		"middleware.Claims",
		"pkg/apikey",
		"pkg/middleware",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("api_key_validator_gen.go.tmpl should contain %q", check)
		}
	}
}

func TestAPIKeyMigrationTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("api-key/templates/api_key_migration.sql.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"BEGIN",
		"CREATE TABLE IF NOT EXISTS api_keys",
		"id UUID PRIMARY KEY",
		"prefix VARCHAR(12)",
		"key_hash VARCHAR(64)",
		"user_id UUID NOT NULL",
		"scopes TEXT[]",
		"expires_at TIMESTAMPTZ",
		"revoked_at TIMESTAMPTZ",
		"last_used_at TIMESTAMPTZ",
		"idx_api_keys_prefix",
		"idx_api_keys_user_id",
		"COMMIT",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("api_key_migration.sql.tmpl should contain %q", check)
		}
	}
}

func TestAPIKeyMigrationDownTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("api-key/templates/api_key_migration_down.sql.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"BEGIN",
		"DROP TABLE IF EXISTS api_keys",
		"COMMIT",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("api_key_migration_down.sql.tmpl should contain %q", check)
		}
	}
}

func TestAPIKeyInListPacks(t *testing.T) {
	packs, err := ListPacks()
	if err != nil {
		t.Fatalf("ListPacks() error: %v", err)
	}

	found := false
	for _, p := range packs {
		if p.Name == "api-key" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListPacks() did not include api-key")
	}
}
