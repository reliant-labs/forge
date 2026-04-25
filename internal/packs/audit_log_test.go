package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestAuditLogPackManifest(t *testing.T) {
	p, err := LoadPack("audit-log")
	if err != nil {
		t.Fatalf("LoadPack(audit-log) error: %v", err)
	}

	if p.Name != "audit-log" {
		t.Errorf("Name = %q, want %q", p.Name, "audit-log")
	}
	if p.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", p.Version, "1.0.0")
	}
	if !strings.Contains(p.Description, "Audit") {
		t.Errorf("Description should mention Audit, got: %s", p.Description)
	}

	// Check config section
	if p.Config.Section != "audit_log" {
		t.Errorf("Config.Section = %q, want %q", p.Config.Section, "audit_log")
	}

	// Check config defaults
	if enabled, ok := p.Config.Defaults["enabled"]; !ok || enabled != true {
		t.Errorf("Config.Defaults[enabled] = %v, want true", enabled)
	}
	if persist, ok := p.Config.Defaults["persist_to_db"]; !ok || persist != true {
		t.Errorf("Config.Defaults[persist_to_db] = %v, want true", persist)
	}

	// No external dependencies
	if len(p.Dependencies) != 0 {
		t.Errorf("len(Dependencies) = %d, want 0", len(p.Dependencies))
	}

	// Check files
	if len(p.Files) != 3 {
		t.Errorf("len(Files) = %d, want 3", len(p.Files))
	}
	templateNames := make(map[string]bool)
	for _, f := range p.Files {
		templateNames[f.Template] = true
	}
	for _, want := range []string{"audit_migration.up.sql.tmpl", "audit_migration.down.sql.tmpl", "audit_store.go.tmpl"} {
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

	// Check generate hook
	if len(p.Generate) != 1 {
		t.Fatalf("len(Generate) = %d, want 1", len(p.Generate))
	}
	if p.Generate[0].Template != "audit_interceptor_gen.go.tmpl" {
		t.Errorf("Generate[0].Template = %q, want %q", p.Generate[0].Template, "audit_interceptor_gen.go.tmpl")
	}
}

func TestAuditLogTemplatesRender(t *testing.T) {
	p, err := LoadPack("audit-log")
	if err != nil {
		t.Fatalf("LoadPack(audit-log) error: %v", err)
	}

	data := map[string]any{
		"ModulePath":  "github.com/example/myapp",
		"ProjectName": "myapp",
		"PackConfig":  p.Config.Defaults,
	}

	// Test all templates render without error
	allFiles := append(p.Files, p.Generate...)
	for _, f := range allFiles {
		t.Run(f.Template, func(t *testing.T) {
			tmplPath := "audit-log/templates/" + f.Template
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

func TestAuditLogMigrationContent(t *testing.T) {
	upContent, err := packsFS.ReadFile("audit-log/templates/audit_migration.up.sql.tmpl")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}

	content := string(upContent)
	checks := []string{
		"CREATE TABLE IF NOT EXISTS audit_log",
		"id UUID PRIMARY KEY",
		"user_id VARCHAR(255)",
		"email VARCHAR(255)",
		"procedure VARCHAR(512) NOT NULL",
		"peer_address VARCHAR(255)",
		"duration_ms INTEGER",
		"status VARCHAR(50)",
		"error_code VARCHAR(50)",
		"error_message TEXT",
		"metadata JSONB",
		"idx_audit_log_user_id",
		"idx_audit_log_procedure",
		"idx_audit_log_timestamp",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("audit_migration.up.sql.tmpl should contain %q", check)
		}
	}

	downContent, err := packsFS.ReadFile("audit-log/templates/audit_migration.down.sql.tmpl")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	if !strings.Contains(string(downContent), "DROP TABLE IF EXISTS audit_log") {
		t.Error("down migration should DROP TABLE audit_log")
	}
}

func TestAuditStoreTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("audit-log/templates/audit_store.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)
	checks := []string{
		"package audit",
		"AuditStore",
		"AuditEntry",
		"AuditFilter",
		"DBAuditStore",
		"NewDBAuditStore",
		"Log(ctx context.Context, entry AuditEntry) error",
		"Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error)",
		"INSERT INTO audit_log",
		"SELECT",
		"database/sql",
		"encoding/json",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("audit_store.go.tmpl should contain %q", check)
		}
	}
}

func TestAuditInterceptorGenTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("audit-log/templates/audit_interceptor_gen.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)
	checks := []string{
		"Code generated by forge generate (audit-log pack). DO NOT EDIT.",
		"package middleware",
		"AuditInterceptor",
		"audit.AuditStore",
		"ClaimsFromContext",
		"slog.LevelWarn",
		"slog.LevelInfo",
		"connect.CodeOf",
		"context.WithTimeout",
		"store.Log",
		"log_type",
		"fire-and-forget",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("audit_interceptor_gen.go.tmpl should contain %q", check)
		}
	}
}

func TestAuditLogInListPacks(t *testing.T) {
	packs, err := ListPacks()
	if err != nil {
		t.Fatalf("ListPacks() error: %v", err)
	}

	found := false
	for _, p := range packs {
		if p.Name == "audit-log" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListPacks() did not include audit-log")
	}
}
