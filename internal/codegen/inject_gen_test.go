package codegen

import (
	"go/parser"
	"go/token"
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

// TestGenerateCompose_ReconcileAddsNewService is the F3#1 regression: a
// service added AFTER the initial compose.go scaffold must be wired into the
// (write-once, user-owned) compose.go additively — its import, its Components
// field, and its NewComponents construction — so the regenerated
// mounts_services.go's `c.<Field>` reference resolves instead of failing to
// compile. The existing service must be left intact and the result must be
// valid Go; re-running with the same set must be a no-op (idempotent).
func TestGenerateCompose_ReconcileAddsNewService(t *testing.T) {
	dir := newInjectProject(t)
	writeComponentDeps(t, dir, "internal/handlers", "user", "user",
		"\tLogger *slog.Logger\n\tConfig *config.Config")
	writeInfra(t, dir, "\tLog *slog.Logger\n\tCfg *config.Config")

	// First emit: only the user service.
	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "UserService", ModulePath: "example.com/proj"}},
	}); err != nil {
		t.Fatalf("first GenerateCompose: %v", err)
	}
	first := readInject(t, dir)
	if !strings.Contains(first, "c.User =") {
		t.Fatalf("first emit missing c.User:\n%s", first)
	}
	if strings.Contains(first, "c.Billing =") {
		t.Fatalf("first emit must not carry billing yet:\n%s", first)
	}

	// Add a second service on disk and regenerate over BOTH. compose.go
	// already exists (write-once), so this exercises the reconciler.
	writeComponentDeps(t, dir, "internal/handlers", "billing", "billing",
		"\tLogger *slog.Logger\n\tConfig *config.Config")
	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services: []ServiceDef{
			{Name: "UserService", ModulePath: "example.com/proj"},
			{Name: "BillingService", ModulePath: "example.com/proj"},
		},
	}); err != nil {
		t.Fatalf("reconcile GenerateCompose: %v", err)
	}
	out := readInject(t, dir)

	// Billing is now wired: construction, struct field, and import.
	if !strings.Contains(out, "c.Billing =") {
		t.Fatalf("reconcile did not add the c.Billing construction:\n%s", out)
	}
	if !strings.Contains(out, "example.com/proj/internal/handlers/billing") {
		t.Fatalf("reconcile did not add the billing import:\n%s", out)
	}
	if !strings.Contains(out, "Billing billing.Service") {
		t.Fatalf("reconcile did not add the Billing Components field:\n%s", out)
	}
	// The pre-existing user service must be untouched.
	if !strings.Contains(out, "c.User =") {
		t.Fatalf("reconcile clobbered the existing user wiring:\n%s", out)
	}
	// The whole file must remain valid Go.
	if _, err := parser.ParseFile(token.NewFileSet(), "compose.go", out, parser.SkipObjectResolution); err != nil {
		t.Fatalf("reconciled compose.go is not valid Go: %v\n----\n%s", err, out)
	}

	// Idempotent: regenerating with the same set changes nothing.
	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services: []ServiceDef{
			{Name: "UserService", ModulePath: "example.com/proj"},
			{Name: "BillingService", ModulePath: "example.com/proj"},
		},
	}); err != nil {
		t.Fatalf("idempotent GenerateCompose: %v", err)
	}
	if again := readInject(t, dir); again != out {
		t.Fatalf("reconcile is not idempotent; second run diverged:\n%s", again)
	}
}

// TestGenerateCompose_ReconcileWiresAuthorizerService proves the reconciler
// emits the authz-var preamble + the fallible New(...) error branch for a
// service whose Deps declare an Authorizer and whose New returns (T, error) —
// the shape a real `forge add service` scaffolds — and pulls in fmt/config/
// middleware imports as needed.
func TestGenerateCompose_ReconcileWiresAuthorizerService(t *testing.T) {
	dir := newInjectProject(t)
	writeInfra(t, dir, "\tLog *slog.Logger\n\tCfg *config.Config")
	// Seed compose.go with a trivial first service (no authz/fallible) so the
	// authz/config/middleware imports are ABSENT and the reconciler must add
	// them for the second service.
	writeComponentDeps(t, dir, "internal/handlers", "user", "user",
		"\tLogger *slog.Logger")
	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "UserService", ModulePath: "example.com/proj"}},
	}); err != nil {
		t.Fatalf("first GenerateCompose: %v", err)
	}

	// A fallible, authorizer-bearing service — mirrors the real scaffold.
	billingDir := filepath.Join(dir, "internal", "handlers", "billing")
	if err := os.MkdirAll(billingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	billingSrc := `package billing

type Service interface{ Do() }

type Deps struct {
	Logger     *slog.Logger
	Authorizer middleware.Authorizer
}

func New(d Deps) (Service, error) { return nil, nil }

func NewAuthorizer() middleware.Authorizer { return nil }
`
	if err := os.WriteFile(filepath.Join(billingDir, "contract.go"), []byte(billingSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services: []ServiceDef{
			{Name: "UserService", ModulePath: "example.com/proj"},
			{Name: "BillingService", ModulePath: "example.com/proj"},
		},
	}); err != nil {
		t.Fatalf("reconcile GenerateCompose: %v", err)
	}
	out := readInject(t, dir)

	for _, want := range []string{
		"billingAuthz middleware.Authorizer = billing.NewAuthorizer()",
		"config.DevAuthBypass(infra.Cfg)",
		"billingInst, err := billing.New(billing.Deps{",
		"return nil, fmt.Errorf(\"Billing: %w\", err)",
		"c.Billing = billingInst",
		"\"fmt\"",
		"example.com/proj/pkg/config",
		"example.com/proj/pkg/middleware",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reconciled compose.go missing %q:\n%s", want, out)
		}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "compose.go", out, parser.SkipObjectResolution); err != nil {
		t.Fatalf("reconciled compose.go is not valid Go: %v\n----\n%s", err, out)
	}
}

// TestGenerateCompose_ReconcileWiresDBOnExistingService is the F4 regression:
// a service ALREADY wired into compose.go that later GAINS a DB dep (the
// `DB orm.Context` field the first entity injects) must have that dep wired
// into its existing construction — a stale, write-once compose.go must not
// leave the by-type resolution un-applied (boot-time nil DB). The reconciler
// injects the assignment into the existing New(Deps{…}) literal.
func TestGenerateCompose_ReconcileWiresDBOnExistingService(t *testing.T) {
	dir := newInjectProject(t)
	// Infra exposes a DB field by exact name so the assignment resolves
	// deterministically without type-checking the temp project.
	writeInfra(t, dir, "\tLog *slog.Logger\n\tCfg *config.Config\n\tDB Repository")

	// settings service WITHOUT a DB dep initially.
	writeComponentDeps(t, dir, "internal/handlers", "settings", "settings",
		"\tLogger *slog.Logger\n\tConfig *config.Config")
	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "SettingsService", ModulePath: "example.com/proj"}},
	}); err != nil {
		t.Fatalf("first GenerateCompose: %v", err)
	}
	if first := readInject(t, dir); strings.Contains(first, "DB:") {
		t.Fatalf("first emit must not carry a DB assignment yet:\n%s", first)
	}

	// The service gains a DB dep — exactly what ensureDepsDBField does when the
	// first entity appears. Rewrite its Deps to include DB.
	writeComponentDeps(t, dir, "internal/handlers", "settings", "settings",
		"\tLogger *slog.Logger\n\tConfig *config.Config\n\tDB Repository")
	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "SettingsService", ModulePath: "example.com/proj"}},
	}); err != nil {
		t.Fatalf("reconcile GenerateCompose: %v", err)
	}
	out := readInject(t, dir)
	// gofmt aligns struct-literal values, so collapse whitespace before
	// matching the field/expr pairs.
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if !strings.Contains(flat(out), "DB: infra.DB") {
		t.Fatalf("reconcile did not wire the gained DB dep into the existing construction:\n%s", out)
	}
	// The prior Logger/Config wiring must remain.
	if !strings.Contains(flat(out), "Logger: infra.Log") || !strings.Contains(flat(out), "Config: infra.Cfg") {
		t.Fatalf("reconcile disturbed the existing assignments:\n%s", out)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "compose.go", out, parser.SkipObjectResolution); err != nil {
		t.Fatalf("reconciled compose.go is not valid Go: %v\n----\n%s", err, out)
	}
	// Idempotent: the assignment is not injected twice.
	if err := GenerateCompose(InjectGenInput{
		GenContext: GenContext{ProjectDir: dir, ModulePath: "example.com/proj"},
		Services:   []ServiceDef{{Name: "SettingsService", ModulePath: "example.com/proj"}},
	}); err != nil {
		t.Fatalf("idempotent GenerateCompose: %v", err)
	}
	if again := readInject(t, dir); strings.Count(flat(again), "DB: infra.DB") != 1 {
		t.Fatalf("DB assignment must be wired exactly once (idempotent):\n%s", again)
	}
}

// TestGenerateProviders_RetrofitsORMOnFirstEntity is the F4 provider-side
// regression: a providers.go scaffolded BEFORE the first entity has a
// `DB *sql.DB` pool but no ORM client. When the first entity turns on the ORM
// (ormEnabled), the write-once providers.go must be retrofitted with the
// `ORM *orm.Client` field + its OpenInfra construction so the service's
// `DB orm.Context` dep resolves — instead of a build break from *sql.DB not
// satisfying orm.Context.
func TestGenerateProviders_RetrofitsORMOnFirstEntity(t *testing.T) {
	dir := t.TempDir()

	// First emit: has a database driver but no entities yet (ormEnabled=false).
	if err := GenerateProviders("example.com/proj", "postgres", false, dir); err != nil {
		t.Fatalf("first GenerateProviders: %v", err)
	}
	path := filepath.Join(dir, "internal", "app", "providers.go")
	before, _ := os.ReadFile(path)
	if strings.Contains(string(before), "ORM *orm.Client") {
		t.Fatalf("pre-entity providers.go must not carry the ORM client yet:\n%s", before)
	}
	if !strings.Contains(string(before), "DB *sql.DB") {
		t.Fatalf("pre-entity providers.go should carry the *sql.DB pool:\n%s", before)
	}

	// First entity arrives → ormEnabled becomes true; retrofit fires.
	if err := GenerateProviders("example.com/proj", "postgres", true, dir); err != nil {
		t.Fatalf("retrofit GenerateProviders: %v", err)
	}
	after, _ := os.ReadFile(path)
	got := string(after)
	for _, want := range []string{
		"ORM *orm.Client",
		"orm.NewClientWithDB(db, \"postgres\")",
		"infra.ORM = ormClient",
		"github.com/reliant-labs/forge/pkg/orm",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("retrofitted providers.go missing %q:\n%s", want, got)
		}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "providers.go", got, parser.SkipObjectResolution); err != nil {
		t.Fatalf("retrofitted providers.go is not valid Go: %v\n----\n%s", err, got)
	}

	// Idempotent: a second retrofit run injects nothing more.
	if err := GenerateProviders("example.com/proj", "postgres", true, dir); err != nil {
		t.Fatalf("idempotent GenerateProviders: %v", err)
	}
	again, _ := os.ReadFile(path)
	if strings.Count(string(again), "ORM *orm.Client") != 1 {
		t.Fatalf("ORM field must be present exactly once (idempotent):\n%s", again)
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
