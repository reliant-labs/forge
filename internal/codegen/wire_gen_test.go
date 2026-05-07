package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseServiceDeps_BareDepsTrio asserts the bare-Deps trio
// (Logger, Config, Authorizer) is extracted in declaration order from
// a typical handler/<svc>/service.go. The wire_gen.go generator
// preserves the field order in its emitted struct literal, so a stable
// parse order keeps the rendered file stable across regenerates.
func TestParseServiceDeps_BareDepsTrio(t *testing.T) {
	dir := t.TempDir()
	source := `package api

import (
	"log/slog"
)

type Deps struct {
	Logger     *slog.Logger
	Config     *Config
	Authorizer Authorizer
}

type Config struct{}
type Authorizer interface{ Check() }
`
	if err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	fields, err := ParseServiceDeps(dir)
	if err != nil {
		t.Fatalf("ParseServiceDeps: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("expected 3 Deps fields, got %d: %+v", len(fields), fields)
	}

	want := []DepsField{
		{Name: "Logger", Type: "*slog.Logger"},
		{Name: "Config", Type: "*Config"},
		{Name: "Authorizer", Type: "Authorizer"},
	}
	for i, w := range want {
		if fields[i].Name != w.Name {
			t.Errorf("fields[%d].Name = %q, want %q", i, fields[i].Name, w.Name)
		}
		if fields[i].Type != w.Type {
			t.Errorf("fields[%d].Type = %q, want %q", i, fields[i].Type, w.Type)
		}
	}
}

// TestParseServiceDeps_RichDeps asserts that user-added fields beyond
// the bare-Deps trio are captured with their full type strings —
// wire_gen needs the Type to handle the orm.Context / *sql.DB
// distinction, and to render a useful TODO message when no producer
// matches.
func TestParseServiceDeps_RichDeps(t *testing.T) {
	dir := t.TempDir()
	source := `package api

import (
	"log/slog"

	"github.com/reliant-labs/forge/pkg/orm"
)

type Deps struct {
	Logger *slog.Logger
	DB     orm.Context
	Repo   *Repository
	Cache  CacheService
}

type Repository struct{}
type CacheService interface{}
`
	if err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	fields, err := ParseServiceDeps(dir)
	if err != nil {
		t.Fatalf("ParseServiceDeps: %v", err)
	}
	if len(fields) != 4 {
		t.Fatalf("expected 4 Deps fields, got %d: %+v", len(fields), fields)
	}

	got := map[string]string{}
	for _, f := range fields {
		got[f.Name] = f.Type
	}
	if got["DB"] != "orm.Context" {
		t.Errorf("DB type = %q, want %q", got["DB"], "orm.Context")
	}
	if got["Repo"] != "*Repository" {
		t.Errorf("Repo type = %q, want %q", got["Repo"], "*Repository")
	}
	if got["Cache"] != "CacheService" {
		t.Errorf("Cache type = %q, want %q", got["Cache"], "CacheService")
	}
}

// TestParseServiceDeps_MissingDir returns nil on a non-existent
// directory — wire_gen treats that as "no fields to wire" rather than
// erroring, so a pristine project before its first service compiles.
func TestParseServiceDeps_MissingDir(t *testing.T) {
	fields, err := ParseServiceDeps(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("ParseServiceDeps on missing dir should not error: %v", err)
	}
	if fields != nil {
		t.Errorf("ParseServiceDeps on missing dir should return nil, got %+v", fields)
	}
}

// TestWireExpressionFor_Conventional asserts the conventional
// resolution rules emit the expected expressions. These are the
// load-bearing strings — wire_gen inserts them verbatim into the
// rendered file.
func TestWireExpressionFor_Conventional(t *testing.T) {
	tests := []struct {
		field        DepsField
		ormEnabled   bool
		runtimeName  string
		wantExpr     string
		wantUnresolv bool
	}{
		{
			field:       DepsField{Name: "Logger", Type: "*slog.Logger"},
			runtimeName: "admin-server",
			wantExpr:    `logger.With("service", "admin-server")`,
		},
		{
			field:    DepsField{Name: "Config", Type: "*config.Config"},
			wantExpr: "cfg",
		},
		{
			field:    DepsField{Name: "Authorizer", Type: "middleware.Authorizer"},
			wantExpr: "authz",
		},
		{
			field:      DepsField{Name: "DB", Type: "orm.Context"},
			ormEnabled: true,
			wantExpr:   "app.ORM",
		},
		{
			field:    DepsField{Name: "DB", Type: "*sql.DB"},
			wantExpr: "app.DB",
		},
		{
			// orm.Context with ORM disabled — falls through to
			// unresolved + nil placeholder. Catches the "user added
			// orm.Context but the project has no entities yet" case.
			field:        DepsField{Name: "DB", Type: "orm.Context"},
			ormEnabled:   false,
			wantExpr:     "nil",
			wantUnresolv: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.field.Name+"/"+tt.field.Type, func(t *testing.T) {
			expr, _, hint := wireExpressionFor(tt.field, nil, tt.ormEnabled, tt.runtimeName)
			if expr != tt.wantExpr {
				t.Errorf("expr = %q, want %q", expr, tt.wantExpr)
			}
			if tt.wantUnresolv && hint == "" {
				t.Error("expected unresolved hint, got empty")
			}
			if !tt.wantUnresolv && hint != "" {
				t.Errorf("expected resolved (no hint), got hint = %q", hint)
			}
		})
	}
}

// TestWireExpressionFor_AppFieldByName resolves an unconventional
// field name against a known *App field. This is the user-extension
// path: add a field to *App + setup it in setup.go, and wire_gen
// picks it up automatically by exact-name match.
func TestWireExpressionFor_AppFieldByName(t *testing.T) {
	appFields := map[string]string{
		"Stripe": "*stripe.Client",
		"Audit":  "audit.Logger",
	}
	expr, _, hint := wireExpressionFor(DepsField{Name: "Stripe", Type: "*stripe.Client"}, appFields, false, "billing")
	if expr != "app.Stripe" {
		t.Errorf("expr = %q, want %q", expr, "app.Stripe")
	}
	if hint != "" {
		t.Errorf("expected resolved (no hint), got %q", hint)
	}
	// Audit also resolves by exact-name match.
	expr, _, hint = wireExpressionFor(DepsField{Name: "Audit", Type: "audit.Logger"}, appFields, false, "billing")
	if expr != "app.Audit" {
		t.Errorf("Audit expr = %q, want %q", expr, "app.Audit")
	}
	if hint != "" {
		t.Errorf("Audit expected resolved, got hint = %q", hint)
	}
}

// TestGenerateWireGen_EmitsPerServiceFn writes a minimal scaffold and
// verifies the rendered wire_gen.go contains the expected
// `wireXxxDeps` function and assignment lines for the bare-Deps
// trio. Goldens would be too brittle here (the file evolves) — this
// test pins the structural pieces that bootstrap.go depends on.
func TestGenerateWireGen_EmitsPerServiceFn(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "api")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package api

import (
	"log/slog"
)

type Deps struct {
	Logger     *slog.Logger
	Config     *Config
	Authorizer Authorizer
}

type Config struct{}
type Authorizer interface{ Check() }
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}
	if err := GenerateWireGen(services, nil, nil, nil, "example.com/proj", projectDir, false, nil); err != nil {
		t.Fatalf("GenerateWireGen: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "wire_gen.go"))
	if err != nil {
		t.Fatalf("read wire_gen.go: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"// Code generated by forge. DO NOT EDIT.",
		"package app",
		"func wireAPIDeps(app *App, cfg *config.Config, logger *slog.Logger, devMode bool) api.Deps",
		`Logger: logger.With("service", "api")`,
		"Config: cfg",
		"Authorizer: authz",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("wire_gen.go missing %q\n--- content ---\n%s", want, content)
		}
	}
}

// TestParseServiceDeps_OptionalDepMarker asserts that fields tagged
// with `// forge:optional-dep` (in either doc-comment or trailing
// inline-comment slots) get Optional=true on the parsed DepsField,
// while untagged fields keep Optional=false. This is the substrate
// wire_gen + the upgrade codemod read to know which fields are
// allowed-nil at runtime.
func TestParseServiceDeps_OptionalDepMarker(t *testing.T) {
	dir := t.TempDir()
	source := `package api

import (
	"log/slog"
)

type Deps struct {
	Logger     *slog.Logger
	Config     *Config
	Authorizer Authorizer
	Repo       Repository

	// NATSPublisher publishes domain events; nil disables rollback.
	// forge:optional-dep
	NATSPublisher EventPublisher

	// Audit is a fallback when the org-scoped audit sink is missing.
	Audit AuditSink // forge:optional-dep
}

type Config struct{}
type Authorizer interface{ Check() }
type Repository interface{}
type EventPublisher interface{}
type AuditSink interface{}
`
	if err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	fields, err := ParseServiceDeps(dir)
	if err != nil {
		t.Fatalf("ParseServiceDeps: %v", err)
	}
	if len(fields) != 6 {
		t.Fatalf("expected 6 Deps fields, got %d: %+v", len(fields), fields)
	}

	wantOptional := map[string]bool{
		"Logger":        false,
		"Config":        false,
		"Authorizer":    false,
		"Repo":          false,
		"NATSPublisher": true, // doc-comment marker
		"Audit":         true, // inline trailing-comment marker
	}
	for _, f := range fields {
		got := f.Optional
		want, ok := wantOptional[f.Name]
		if !ok {
			t.Errorf("unexpected field %q in result", f.Name)
			continue
		}
		if got != want {
			t.Errorf("field %q Optional = %v, want %v", f.Name, got, want)
		}
	}
}

// TestGenerateWireGen_OptionalDepSilent asserts that a Deps field
// tagged `// forge:optional-dep` falls through to a typed-zero
// assignment WITHOUT the inline TODO comment and WITHOUT contributing
// to the UNRESOLVED FIELDS header. Untagged unresolved fields still
// trigger both — the marker is opt-in and silence is its only effect.
func TestGenerateWireGen_OptionalDepSilent(t *testing.T) {
	projectDir := t.TempDir()
	handlerDir := filepath.Join(projectDir, "handlers", "api")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	source := `package api

import (
	"log/slog"

	"example.com/proj/pkg/config"
	"example.com/proj/pkg/middleware"
)

type Deps struct {
	Logger     *slog.Logger
	Config     *config.Config
	Authorizer middleware.Authorizer

	// Stripe is required production-only; lint catches missing wiring.
	Stripe StripeClient

	// NATSPublisher is intentionally optional.
	// forge:optional-dep
	NATSPublisher EventPublisher
}

type StripeClient interface{}
type EventPublisher interface{}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}
	if err := GenerateWireGen(services, nil, nil, nil, "example.com/proj", projectDir, false, nil); err != nil {
		t.Fatalf("GenerateWireGen: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "wire_gen.go"))
	if err != nil {
		t.Fatalf("read wire_gen.go: %v", err)
	}
	content := string(data)

	// Required-but-unresolved Stripe: TODO + UNRESOLVED entry.
	if !strings.Contains(content, "TODO: wire Stripe") {
		t.Errorf("expected TODO marker for Stripe in wire_gen.go:\n%s", content)
	}
	if !strings.Contains(content, "Stripe (StripeClient)") {
		t.Errorf("expected UNRESOLVED entry for Stripe in wire_gen.go:\n%s", content)
	}

	// Optional NATSPublisher: zero-value assignment but no TODO and no
	// UNRESOLVED entry.
	if strings.Contains(content, "TODO: wire NATSPublisher") {
		t.Errorf("optional NATSPublisher should NOT carry a TODO:\n%s", content)
	}
	if strings.Contains(content, "NATSPublisher (EventPublisher)") {
		t.Errorf("optional NATSPublisher should NOT appear in UNRESOLVED list:\n%s", content)
	}
	// The assignment line is still emitted (so the rendered struct
	// literal is complete), just with the typed zero. nil is fine for
	// the interface type.
	if !strings.Contains(content, "NATSPublisher: nil") {
		t.Errorf("expected `NATSPublisher: nil` zero-value assignment:\n%s", content)
	}
}
