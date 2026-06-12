package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
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
			// Nil-safe accessor — `app.ORM` directly wraps a nil
			// *orm.Client into a typed-nil that defeats validateDeps
			// and panics on the first RPC (J-round fix 3).
			field:      DepsField{Name: "DB", Type: "orm.Context"},
			ormEnabled: true,
			wantExpr:   "app.ORMContext()",
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

// TestParseAppFields_PlaceholderMarker asserts that AppExtras fields
// carrying `// forge:placeholder: <Type>` (doc-comment, inline-comment,
// or struct-tag shape) get Placeholder set on the parsed AppField.
// App-struct fields never get the marker (it's user-only by design).
func TestParseAppFields_PlaceholderMarker(t *testing.T) {
	dir := t.TempDir()

	// app_gen.go (forge-owned) — App struct + a forge-managed field
	// carrying a (would-be) placeholder marker that should be IGNORED.
	appGen := `package app

type App struct {
	*AppExtras
	// forge:placeholder: db.Repository
	DB any
}
`
	if err := os.WriteFile(filepath.Join(dir, "app_gen.go"), []byte(appGen), 0o644); err != nil {
		t.Fatal(err)
	}

	// app_extras.go (user-owned) — three placeholder shapes, one
	// already-tightened field, one untagged field for control.
	appExtras := `package app

type AppExtras struct {
	// UserRepo is shared across services.
	// forge:placeholder: user.Repository
	UserRepo any

	// AdminRepo uses the inline-comment shape.
	AdminRepo any // forge:placeholder: admin.Repository

	// TenantRepo uses the struct-tag shape.
	TenantRepo any ` + "`forge:placeholder:\"tenant.Repository\"`" + `

	// Already tightened — placeholder marker is a no-op once the
	// field type matches the target. wire_gen still emits the typed
	// resolver; the assertion is a no-op at runtime.
	// forge:placeholder: orgs.Repository
	OrgRepo orgs.Repository

	// Untagged — no placeholder, normal app.<Field> resolution.
	Stripe *stripe.Client
}
`
	if err := os.WriteFile(filepath.Join(dir, "app_extras.go"), []byte(appExtras), 0o644); err != nil {
		t.Fatal(err)
	}

	fields, err := ParseAppFields(dir)
	if err != nil {
		t.Fatalf("ParseAppFields: %v", err)
	}

	want := map[string]string{
		"DB":         "",                   // App-struct field; marker IGNORED
		"UserRepo":   "user.Repository",    // doc-comment shape
		"AdminRepo":  "admin.Repository",   // inline-comment shape
		"TenantRepo": "tenant.Repository",  // struct-tag shape
		"OrgRepo":    "orgs.Repository",    // tightened; marker still recorded
		"Stripe":     "",                   // untagged
	}
	got := map[string]string{}
	for _, f := range fields {
		got[f.Name] = f.Placeholder
	}
	for name, w := range want {
		if g, ok := got[name]; !ok {
			t.Errorf("expected field %q in result", name)
		} else if g != w {
			t.Errorf("field %q Placeholder = %q, want %q", name, g, w)
		}
	}
}

// TestGenerateWireGen_PlaceholderResolver asserts the codegen path:
// when a Deps field name matches an AppExtras field with a placeholder
// AND the AppExtras field is already tightened to the target type,
// wire_gen emits a typed `resolve<Field>(app)` accessor and renders the
// helper at file scope. The wire_gen.go consumes the result via the
// accessor instead of `app.<Field>` directly.
func TestGenerateWireGen_PlaceholderResolver(t *testing.T) {
	projectDir := t.TempDir()
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// AppExtras field is already tightened to user.Repository — the
	// marker is a no-op but should still cause wire_gen to emit the
	// typed accessor (the `any(...).(T)` assertion compiles either way
	// and stays consistent across the field's pre/post-tightening
	// lifetime).
	appExtras := `package app

type App struct {
	*AppExtras
}

type AppExtras struct {
	// forge:placeholder: user.Repository
	UserRepo user.Repository
}
`
	if err := os.WriteFile(filepath.Join(appDir, "app_extras.go"), []byte(appExtras), 0o644); err != nil {
		t.Fatal(err)
	}

	handlerDir := filepath.Join(projectDir, "handlers", "api")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package api

import (
	"log/slog"
)

type Deps struct {
	Logger   *slog.Logger
	UserRepo UserRepository
}

type UserRepository interface{}
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
		"func resolveUserRepo(app *App) user.Repository",
		"UserRepo: resolveUserRepo(app)",
		"typed accessor for forge:placeholder",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("wire_gen.go missing %q\n--- content ---\n%s", want, content)
		}
	}
	// Should NOT carry a TODO since the field was matched.
	if strings.Contains(content, "TODO: wire UserRepo") {
		t.Errorf("UserRepo should not have a TODO when resolved via placeholder:\n%s", content)
	}
}

// TestGenerateWireGen_UnresolvedPlaceholderFails asserts the build-time
// gate: an AppExtras field with `forge:placeholder` and still typed
// `any` causes GenerateWireGen to return an error rather than emit a
// silently-broken accessor. The error names the field and target type
// so the user knows exactly which declaration to tighten.
func TestGenerateWireGen_UnresolvedPlaceholderFails(t *testing.T) {
	projectDir := t.TempDir()
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appExtras := `package app

type App struct {
	*AppExtras
}

type AppExtras struct {
	// forge:placeholder: user.Repository
	UserRepo any
}
`
	if err := os.WriteFile(filepath.Join(appDir, "app_extras.go"), []byte(appExtras), 0o644); err != nil {
		t.Fatal(err)
	}
	handlerDir := filepath.Join(projectDir, "handlers", "api")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package api
type Deps struct {
	UserRepo UserRepository
}
type UserRepository interface{}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}
	err := GenerateWireGen(services, nil, nil, nil, "example.com/proj", projectDir, false, nil)
	if err == nil {
		t.Fatal("expected GenerateWireGen to error on unresolved placeholder, got nil")
	}
	for _, want := range []string{
		"forge:placeholder",
		"UserRepo",
		"user.Repository",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
}

// TestExtractPlaceholderType verifies the comment-shape recognition:
// doc-comment lines must be `// forge:placeholder: <Type>` exactly,
// inline-trailing-comment lines are tolerated, and unrelated text in
// the comment block doesn't trigger a false positive.
func TestExtractPlaceholderType(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"forge:placeholder: user.Repository", "user.Repository"},
		{"// forge:placeholder: user.Repository", "user.Repository"},
		{"  // forge:placeholder: user.Repository  ", "user.Repository"},
		{"forge:placeholder: \"user.Repository\"", "user.Repository"},
		{"some prose\nforge:placeholder: api.Client\n", "api.Client"},
		{"forge:placeholder:", ""},
		{"// forge:optional-dep", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := ExtractPlaceholderType(c.text)
		if got != c.want {
			t.Errorf("ExtractPlaceholderType(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// TestSnakeCanonicalNoCompactDupes is the deep regression test for Bug 8 /
// the cp-forge "snake↔compact duplicate-dir" bug class. The Jun-8 naming
// fixes commit (3425d9e) re-introduced the bug a second time after an
// earlier fix; that motivated a defence-in-depth test that doesn't just
// pin naming.ServicePackage() (which TestServicePackage already does)
// but exercises the FULL generate path against a multi-word-service
// fixture so any future refactor in naming.go, wire_gen.go, authz_gen.go,
// OR the handler scaffold path that breaks the snake-canonical contract
// fails THIS test, not a downstream e2e three regen layers later.
//
// The cp-forge layout it mirrors:
//   - Services with multi-word names ("admin-server", "admin_server",
//     "AdminServerService") and Go-initialism-containing names
//     ("llm-gateway", "LLMGatewayService") — these are the exact shapes
//     that historically silently emitted compact dirs.
//   - Pre-existing snake_case handler dirs (the canonical layout).
//   - User-owned files in the snake dir whose package decl matches the
//     snake form — a compact-form regen would orphan them.
//
// Assertions exercise the three failure modes the bug actually produced:
//
//  1. No `handlers/<compact>/` directory appears alongside the canonical
//     `handlers/<snake>/`. (Old bug: created `handlers/adminserver/`.)
//  2. `pkg/app/wire_gen.go` declares `wire<PascalSnake>Deps`, e.g.
//     `wireAdminServerDeps`, NOT `wireAdminserverDeps`. (Old bug: lower-
//     case-first-only collapse produced the latter, then bootstrap.go
//     called a function that didn't exist.)
//  3. The generated wire_gen.go parses as valid Go — guards against a
//     future refactor that produces a syntactically broken file even if
//     the substring assertions happen to pass.
func TestSnakeCanonicalNoCompactDupes(t *testing.T) {
	projectDir := t.TempDir()

	// The matrix of multi-word / Go-initialism service names that have
	// historically tripped the compact-form regression. Snake form is
	// the canonical handler dir; ProtoServiceName is what protoc-gen-forge
	// passes to GenerateWireGen / GenerateAuthorizer as ServiceDef.Name.
	cases := []struct {
		protoServiceName string // ServiceDef.Name shape (PascalCase + "Service")
		wantSnakeDir     string // canonical handlers/<snake> dir
		wantWireFn       string // wire_gen.go function name
		bannedCompactDir string // dir that must NOT exist (Bug 8 footprint)
	}{
		{"AdminServerService", "admin_server", "wireAdminServerDeps", "adminserver"},
		{"AuditLogService", "audit_log", "wireAuditLogDeps", "auditlog"},
		{"DaemonAdminService", "daemon_admin", "wireDaemonAdminDeps", "daemonadmin"},
		{"LLMGatewayService", "llm_gateway", "wireLLMGatewayDeps", "llmgateway"},
		{"BillingGatewayService", "billing_gateway", "wireBillingGatewayDeps", "billinggateway"},
	}

	services := make([]ServiceDef, 0, len(cases))
	for _, c := range cases {
		// Scaffold the canonical snake handler dir with a minimal
		// service.go that declares the bare Deps trio — wire_gen.go
		// needs this to emit a wireXxxDeps entry for the service.
		handlerDir := filepath.Join(projectDir, "handlers", c.wantSnakeDir)
		if err := os.MkdirAll(handlerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// User-owned authorizer.go in the snake dir — would be orphaned
		// by a compact-form regen, so we can also assert it survives.
		userAuthz := "package " + c.wantSnakeDir + "\n// hand-written authz\n"
		if err := os.WriteFile(filepath.Join(handlerDir, "authorizer.go"), []byte(userAuthz), 0o644); err != nil {
			t.Fatal(err)
		}
		// service.go declares Deps; wire_gen parses it.
		serviceGo := `package ` + c.wantSnakeDir + `

import "log/slog"

type Deps struct {
	Logger     *slog.Logger
	Config     *Config
	Authorizer Authorizer
}

type Config struct{}
type Authorizer interface{ Check() }
`
		if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(serviceGo), 0o644); err != nil {
			t.Fatal(err)
		}

		services = append(services, ServiceDef{Name: c.protoServiceName, ModulePath: "example.com/proj"})
	}

	// Run the two generators that historically diverged on the snake↔
	// compact boundary. Both must agree on the same dir / function name
	// shape, else bootstrap.go can't link.
	if err := GenerateWireGen(services, nil, nil, nil, "example.com/proj", projectDir, false, nil); err != nil {
		t.Fatalf("GenerateWireGen: %v", err)
	}
	if err := GenerateAuthorizer(services, "example.com/proj", projectDir, nil, (*checksums.FileChecksums)(nil)); err != nil {
		t.Fatalf("GenerateAuthorizer: %v", err)
	}

	// Assertion 1: NO compact-form handler dir exists alongside the
	// canonical snake form. This is the on-disk footprint of the bug
	// class — `handlers/adminserver/` next to `handlers/admin_server/`.
	for _, c := range cases {
		compactPath := filepath.Join(projectDir, "handlers", c.bannedCompactDir)
		if _, err := os.Stat(compactPath); err == nil {
			t.Errorf("compact-form handler dir leaked: %s exists alongside %s — snake-canonical contract broken",
				compactPath, c.wantSnakeDir)
		}
	}

	// Assertion 2: wire_gen.go declares the snake-respecting wire function.
	wireGenPath := filepath.Join(projectDir, "pkg", "app", "wire_gen.go")
	wireData, err := os.ReadFile(wireGenPath)
	if err != nil {
		t.Fatalf("read wire_gen.go: %v", err)
	}
	wireContent := string(wireData)
	for _, c := range cases {
		// Function signature substring — the templated form is
		// `func wireXxxDeps(app *App, cfg *config.Config, ...)`.
		wantSig := "func " + c.wantWireFn + "("
		if !strings.Contains(wireContent, wantSig) {
			t.Errorf("wire_gen.go missing %q for proto service %q (snake dir %q)",
				wantSig, c.protoServiceName, c.wantSnakeDir)
		}
		// And the BANNED compact form (e.g. wireAdminserverDeps) must
		// not appear — a future regression that produces the wrong
		// casing would silently break bootstrap.go calls.
		bannedFn := "wire" + strings.Title(c.bannedCompactDir) + "Deps" //nolint:staticcheck // strings.Title is fine for ASCII test fixtures
		if strings.Contains(wireContent, bannedFn+"(") {
			t.Errorf("wire_gen.go leaked compact-form function %q — bug 8 regression", bannedFn)
		}
	}

	// Assertion 3: the rendered wire_gen.go is syntactically valid Go.
	// Substring assertions can pass on a broken file (e.g. unbalanced
	// braces from a template refactor); parsing guards against that.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, wireGenPath, wireData, parser.AllErrors); err != nil {
		t.Errorf("wire_gen.go failed to parse: %v\n--- content ---\n%s", err, wireContent)
	}

	// Assertion 4: per-service authorizer_gen.go was emitted INTO the
	// snake dir (not a sibling compact dir), with the matching package
	// decl. This is the second-half of the snake-canonical contract
	// the cp-forge bug also broke (compact-form authorizer.go alongside
	// snake-form handlers.go → two packages, one dir → won't compile).
	for _, c := range cases {
		authPath := filepath.Join(projectDir, "handlers", c.wantSnakeDir, "authorizer_gen.go")
		authData, err := os.ReadFile(authPath)
		if err != nil {
			t.Errorf("authorizer_gen.go not emitted at %s: %v", authPath, err)
			continue
		}
		// Parse and pull the package name — match against the snake form.
		af, err := parser.ParseFile(fset, authPath, authData, parser.PackageClauseOnly)
		if err != nil {
			t.Errorf("%s: parse failed: %v", authPath, err)
			continue
		}
		if af.Name.Name != c.wantSnakeDir {
			t.Errorf("%s: package = %q, want %q (snake-canonical)",
				authPath, af.Name.Name, c.wantSnakeDir)
		}
	}

	// Assertion 5: the user-owned authorizer.go scaffolded in the snake
	// dir is still there. A compact-form regen would have scaffolded a
	// sibling compact dir AND left the snake dir's user file orphaned;
	// the dangling file would compile-error against a missing package.
	for _, c := range cases {
		userFile := filepath.Join(projectDir, "handlers", c.wantSnakeDir, "authorizer.go")
		if _, err := os.Stat(userFile); err != nil {
			t.Errorf("user-owned authorizer.go in snake dir was lost: %v", err)
		}
	}
}

// snakeCanonicalRe is a sanity-only regex: any function declaration in
// wire_gen.go matching `func wire<X>Deps` must have its X be either a
// PascalCase form whose snake-lowering matches a known handler dir, OR
// the worker/operator prefix variants. The TestSnakeCanonicalNoCompactDupes
// test above checks the inverse (banned compact form absent); this
// pattern is documented here so a future agent who adds new generated
// wire entry points knows the contract.
var snakeCanonicalRe = regexp.MustCompile(`^func\s+wire(?:Worker|Operator)?[A-Z]\w*Deps\(`)

var _ = snakeCanonicalRe // documented contract; consumed by future tests
