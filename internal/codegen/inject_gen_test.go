package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeComponentDeps writes a minimal component package (contract.go with a
// Deps struct) under the given role root so ParseServiceDeps + disk
// resolution see it. body is the Deps struct field block.
func writeComponentDeps(t *testing.T, projectDir, roleRoot, leaf, pkg, depsBody string) {
	t.Helper()
	dir := filepath.Join(projectDir, filepath.FromSlash(roleRoot), leaf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := "package " + pkg + "\n\ntype Service interface{ Do() }\n\ntype Deps struct {\n" + depsBody + "\n}\n\nfunc New(d Deps) Service { return nil }\n"
	if err := os.WriteFile(filepath.Join(dir, "contract.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}
}

func newInjectProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yaml := "name: proj\nmodule_path: example.com/proj\n"
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	return dir
}

// writeInfra writes a minimal internal/app/providers.go with an Infra
// struct whose fields are the given body.
func writeInfra(t *testing.T, projectDir, fieldsBody string) {
	t.Helper()
	appDir := filepath.Join(projectDir, "internal", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	src := "package app\n\ntype Repository interface{ Get() }\n\ntype Infra struct {\n" + fieldsBody + "\n}\n"
	if err := os.WriteFile(filepath.Join(appDir, "providers.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write infra: %v", err)
	}
}

func readInject(t *testing.T, projectDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectDir, "internal", "app", "compose.go"))
	if err != nil {
		t.Fatalf("read compose.go: %v", err)
	}
	return string(data)
}

// TestGenerateInject_TypeTopoOrder: billing.Deps.Users typed user.Service
// means user is constructed before billing — by TYPE, not field name.
func TestGenerateInject_TypeTopoOrder(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "user", "user", "\tRepo Repository")
	writeComponentDeps(t, dir, "internal/handlers", "billing", "billing", "\tUsers user.Service")
	// user's Deps references a local Repository type; declare it so the dir
	// parses. (ParseServiceDeps is AST-only; the type need not resolve.)
	appendType(t, dir, "internal/handlers/user", "type Repository interface{ Get() }")
	// Infra provides Repo by exact name — the compile-time backstop fills
	// User.Deps.Repo (the temp project doesn't type-check, so the matcher
	// can't PROVE assignability; the exact-name backstop is the loud,
	// deterministic policy).
	writeInfra(t, dir, "\tRepo Repository")

	services := []ServiceDef{
		{Name: "UserService", ModulePath: "example.com/proj"},
		{Name: "BillingService", ModulePath: "example.com/proj"},
	}
	err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   services,
	})
	if err != nil {
		t.Fatalf("GenerateInject: %v", err)
	}
	out := readInject(t, dir)
	ui := strings.Index(out, "c.User =")
	bi := strings.Index(out, "c.Billing =")
	if ui < 0 || bi < 0 {
		t.Fatalf("missing assignments in:\n%s", out)
	}
	if ui > bi {
		t.Fatalf("User must be constructed before Billing:\n%s", out)
	}
	// Billing's Users field is filled by the producer's local var (suffixed
	// "Inst" so it never shadows the package import alias), not infra.
	if !strings.Contains(out, "Users: userInst,") {
		t.Fatalf("Billing.Users should be wired to the user producer var:\n%s", out)
	}
}

// TestGenerateInject_MissingProviderIsLoud: a required collaborator field
// (interface, no producer, no Infra) raises a generate-time error naming
// the type + component + field. (No providers.go on disk, so the matcher
// can't prove an Infra field; but the field is also not a producer and not
// scalar — it must be loud.)
func TestGenerateInject_MissingProviderIsLoud(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "billing", "billing", "\tStripe StripeClient")
	appendType(t, dir, "internal/handlers/billing", "type StripeClient interface{ Charge() }")

	err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "BillingService", ModulePath: "example.com/proj"}},
	})
	if err == nil {
		t.Fatalf("expected MissingProvider error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"Billing", "Stripe", "StripeClient", "providers.go"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q:\n%s", want, msg)
		}
	}
}

// TestGenerateInject_OptionalMissingIsSilent: an optional collaborator
// with no provider takes a typed nil and does NOT raise.
func TestGenerateInject_OptionalMissingIsSilent(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "billing", "billing",
		"\t// forge:optional-dep\n\tStripe StripeClient")
	appendType(t, dir, "internal/handlers/billing", "type StripeClient interface{ Charge() }")

	err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "BillingService", ModulePath: "example.com/proj"}},
	})
	if err != nil {
		t.Fatalf("optional missing dep must not raise: %v", err)
	}
	out := readInject(t, dir)
	if !strings.Contains(out, "Stripe: nil,") {
		t.Fatalf("optional dep should be typed nil:\n%s", out)
	}
}

// TestGenerateInject_ConventionalDeps: Logger/Config wire to infra.Log /
// infra.Cfg, never a producer or a MissingProvider.
func TestGenerateInject_ConventionalDeps(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "user", "user",
		"\tLogger *slog.Logger\n\tConfig *config.Config")

	err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "UserService", ModulePath: "example.com/proj"}},
	})
	if err != nil {
		t.Fatalf("GenerateInject: %v", err)
	}
	out := readInject(t, dir)
	if !strings.Contains(out, "Logger: infra.Log,") {
		t.Fatalf("Logger should wire to infra.Log:\n%s", out)
	}
	if !strings.Contains(out, "Config: infra.Cfg,") {
		t.Fatalf("Config should wire to infra.Cfg:\n%s", out)
	}
}

// TestGenerateInject_ScalarIsConfigNotMissing: a scalar Deps field is
// configuration, not a collaborator — it takes the typed-zero and never
// raises MissingProvider.
func TestGenerateInject_ScalarIsConfigNotMissing(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "user", "user", "\tMaxRetries int")

	err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "UserService", ModulePath: "example.com/proj"}},
	})
	if err != nil {
		t.Fatalf("scalar dep must not raise MissingProvider: %v", err)
	}
	out := readInject(t, dir)
	if !strings.Contains(out, "MaxRetries: 0,") {
		t.Fatalf("scalar should take typed zero:\n%s", out)
	}
}

// TestGenerateInject_ScalarResolvesFromConfig: a scalar Deps field that
// matches a typed Config field resolves from infra.Cfg.<field>, not a
// typed-zero. An unmatched scalar still takes the typed-zero. (Regression
// for kalshi's WTI EIAKey/FREDKey being reset to "" + TODO.)
func TestGenerateInject_ScalarResolvesFromConfig(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "wti", "wti",
		"\tEIAKey string\n\tUnmapped string")
	// Config struct carries EIAKey (matches) but not Unmapped.
	cfgDir := filepath.Join(dir, "pkg", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	cfgSrc := "package config\n\ntype Config struct {\n\tEIAKey string\n\tPort int32\n}\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.go"), []byte(cfgSrc), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "WtiService", ModulePath: "example.com/proj"}},
	})
	if err != nil {
		t.Fatalf("GenerateInject: %v", err)
	}
	out := readInject(t, dir)
	if !strings.Contains(out, "EIAKey: infra.Cfg.EIAKey,") {
		t.Fatalf("EIAKey should resolve from infra.Cfg:\n%s", out)
	}
	if !strings.Contains(out, "Unmapped: \"\",") {
		t.Fatalf("Unmapped scalar should take typed-zero:\n%s", out)
	}
}

// TestParseInfraFields reads the Infra struct fields from internal/app.
func TestParseInfraFields(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "internal", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := "package app\n\nimport \"log/slog\"\n\ntype Infra struct {\n\tLog *slog.Logger\n\tRepo *PostgresRepo\n\tunexported int\n}\n"
	if err := os.WriteFile(filepath.Join(appDir, "providers.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fields, err := parseInfraFields(appDir)
	if err != nil {
		t.Fatalf("parseInfraFields: %v", err)
	}
	if _, ok := fields["Log"]; !ok {
		t.Fatalf("Log field missing: %v", fields)
	}
	if f, ok := fields["Repo"]; !ok || f.Type != "*PostgresRepo" {
		t.Fatalf("Repo field wrong: %+v", fields)
	}
	if _, ok := fields["unexported"]; ok {
		t.Fatalf("unexported field should be skipped")
	}
}

// appendType appends a type declaration to a component's contract.go so a
// referenced local type parses (AST-only; need not type-check).
func appendType(t *testing.T, projectDir, rolePath, decl string) {
	t.Helper()
	path := filepath.Join(projectDir, filepath.FromSlash(rolePath), "contract.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read for append: %v", err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n"+decl+"\n")...), 0o644); err != nil {
		t.Fatalf("append: %v", err)
	}
}
