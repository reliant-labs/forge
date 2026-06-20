package codegen

// Tests for the table-shaped bootstrap.go (2026-06 appkit migration).
//
// Design rule under test: GENERATED FILES ARE TABLES, NOT PROGRAMS.
// bootstrap.go must contain only dumb rows (names, constants, and
// type-capturing closures) over pkg/appkit; the orchestration behavior
// (filtering, hook firing, REST transcoding, diagnostics boot, the
// controller-manager runtime) lives in the library where downstream
// projects can't fork it away from regeneration.

import (
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderRepresentativeBootstrap generates bootstrap.go for a "kitchen
// sink" project: two services (one with webhooks), two packages (one
// fallible), two workers (one fallible), one operator, REST enabled,
// strict-wiring diagnostics on. Returns the rendered source.
func renderRepresentativeBootstrap(t *testing.T) string {
	t.Helper()
	targetDir := t.TempDir()

	yaml := "name: proj\nmodule_path: example.com/proj\napi:\n  rest: true\n"
	if err := os.WriteFile(filepath.Join(targetDir, "forge.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}

	services := []ServiceDef{
		{Name: "AdminServerService", ModulePath: "example.com/proj"},
		{Name: "OrdersService", ModulePath: "example.com/proj"},
	}
	packages := []BootstrapPackageData{
		{Name: "cache", Package: "cache", ImportPath: "cache", FieldName: "Cache", VarName: "cache", Alias: "cache", HasLogger: true},
		{Name: "audit", Package: "audit", ImportPath: "audit", FieldName: "Audit", VarName: "audit", Alias: "audit", Fallible: true, HasLogger: true, HasConfig: true},
	}
	workers := []BootstrapWorkerData{
		{Name: "emailer", Package: "emailer", ImportPath: "emailer", FieldName: "Emailer", VarName: "emailer", Alias: "emailer"},
		{Name: "trader", Package: "trader", ImportPath: "trader", FieldName: "Trader", VarName: "trader", Alias: "trader", Fallible: true},
	}
	operators := []BootstrapOperatorData{
		{Name: "scaler", Package: "scaler", ImportPath: "scaler", FieldName: "Scaler", VarName: "scaler", Alias: "scaler"},
	}
	webhookServices := map[string]bool{"adminserver": true}

	features := BootstrapFeatures{DiagnosticsEnabled: true, StrictWiringEnabled: true}
	if err := GenerateBootstrap(BootstrapGenInput{
		GenContext:      GenContext{ProjectDir: targetDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:        services,
		Packages:        packages,
		Workers:         workers,
		Operators:       operators,
		DatabaseDriver:  "",
		OrmEnabled:      false,
		ConfigFields:    nil,
		WebhookServices: webhookServices,
		Features:        features,
	}); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return string(data)
}

// renderRepresentativeServiceRows renders the same kitchen-sink config
// and returns pkg/app/services_gen.go — where the per-service row
// constructors live since the registration-in-code rework.
func renderRepresentativeServiceRows(t *testing.T) string {
	t.Helper()
	targetDir := t.TempDir()

	yaml := "name: proj\nmodule_path: example.com/proj\napi:\n  rest: true\n"
	if err := os.WriteFile(filepath.Join(targetDir, "forge.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	services := []ServiceDef{
		{Name: "AdminServerService", ModulePath: "example.com/proj"},
		{Name: "OrdersService", ModulePath: "example.com/proj"},
	}
	webhookServices := map[string]bool{"adminserver": true}
	features := BootstrapFeatures{DiagnosticsEnabled: true, StrictWiringEnabled: true}
	if err := GenerateBootstrap(BootstrapGenInput{
		GenContext:      GenContext{ProjectDir: targetDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:        services,
		Packages:        nil,
		Workers:         nil,
		Operators:       nil,
		DatabaseDriver:  "",
		OrmEnabled:      false,
		ConfigFields:    nil,
		WebhookServices: webhookServices,
		Features:        features,
	}); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "services_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return string(data)
}

// TestBootstrapTable_RepresentativeConfig_ParsesAndFormats is the
// "rendered template output is valid Go" gate for the full-featured
// config — the closest offline proxy to the scaffold-and-build e2e.
// (Byte-exact gofmt equality is NOT required: `forge generate` runs
// goimports over generated files post-write, same as before this
// template existed.)
func TestBootstrapTable_RepresentativeConfig_ParsesAndFormats(t *testing.T) {
	content := renderRepresentativeBootstrap(t)

	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "bootstrap.go", content, parser.AllErrors); perr != nil {
		t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", perr, content)
	}

	if _, err := format.Source([]byte(content)); err != nil {
		t.Fatalf("format.Source: %v", err)
	}
}

// TestBootstrapTable_DelegatesToAppkit asserts the table shape: thin
// Bootstrap delegating to BootstrapOnly, a single appkit.Run call, and
// one def row per component — with the historical public signatures
// preserved so cmd/server.go, testing.go, and downstream code compile
// unchanged.
func TestBootstrapTable_DelegatesToAppkit(t *testing.T) {
	content := renderRepresentativeBootstrap(t)

	// Public surface preserved verbatim. WorkerList/OperatorList return
	// the serverkit interface types and RunOperators carries the
	// healthProbeAddr parameter — the methods the cmd-server shim reads
	// off *app.App to compose serverkit.Server; the table migration must
	// not regress them.
	for _, sig := range []string{
		"func Bootstrap(mux *http.ServeMux, logger *slog.Logger, cfg *config.Config, opts ...connect.HandlerOption) (*App, error)",
		"func (a *App) WorkerList() []serverkit.Worker",
		"func (a *App) OperatorList() []serverkit.Operator",
		"func (a *App) HasOperators() bool",
		"func (a *App) RunOperators(ctx context.Context, logger *slog.Logger, healthProbeAddr string) error",
		"func (a *App) Shutdown(ctx context.Context) error",
	} {
		if !strings.Contains(content, sig) {
			t.Errorf("bootstrap.go lost public signature %q", sig)
		}
	}

	// Ctx-aware worker forwarding: every worker row goes through
	// appkit.WrapWorker, the runtime type-switch that exposes RunContext
	// on the wrapper exactly when the underlying worker implements it —
	// so the serverkit supervisor's ContextWorker preference assertion
	// keeps working through the generated table (see
	// pkg/appkit/worker_wrap_test.go for the assertion-level proof).
	for _, row := range []string{
		`appkit.WrapWorker("emailer", a.Workers.Emailer),`,
		`appkit.WrapWorker("trader", a.Workers.Trader),`,
	} {
		if !strings.Contains(content, row) {
			t.Errorf("bootstrap.go WorkerList missing WrapWorker row %q", row)
		}
	}

	// String-keyed selection retired (FORGE_SHAPE_REDESIGN §2): no
	// BootstrapOnly / name filter / appkit.Options.
	if strings.Contains(content, "BootstrapOnly") {
		t.Error("BootstrapOnly should be retired — string-keyed selection moved to the cmd layer over internal/app.Inventory")
	}
	if strings.Contains(content, "appkit.Options") {
		t.Error("appkit.Options (the string filter) should be retired")
	}
	// Exactly one orchestration entry point, now filter-free.
	if got := strings.Count(content, "appkit.Run(def, mux, logger)"); got != 1 {
		t.Errorf("expected exactly one appkit.Run(def, mux, logger) call, got %d", got)
	}
	// Hooks plumbed through so setup.go can customize orchestration.
	if !strings.Contains(content, "Hooks: func() *appkit.Hooks { return &app.Hooks }") {
		t.Error("def table should expose app.Hooks to appkit")
	}

	// One row per worker/operator/package component. SERVICE rows come
	// from the user-owned RegisteredServices since the
	// registration-in-code rework — see the services_gen.go assertions
	// below.
	for _, row := range []string{
		`Services: RegisteredServices(app, cfg, logger, opts...)`,
		`{Name: "cache", Construct: func() error {`,
		`{Name: "audit", Construct: func() error {`,
		`{Name: "emailer", Construct: func() error {`,
		`{Name: "trader", Construct: func() error {`,
		`{Name: "scaler", Construct: func() error {`,
	} {
		if !strings.Contains(content, row) {
			t.Errorf("bootstrap.go missing def row %q", row)
		}
	}

	// wire_gen contract intact: the worker/operator rows reference
	// wireXxxDeps; service wire calls moved to services_gen.go.
	for _, wire := range []string{
		"wireWorkerEmailerDeps(app, cfg, logger)",
		"wireWorkerTraderDeps(app, cfg, logger)",
		"wireOperatorScalerDeps(app, cfg, logger)",
	} {
		if !strings.Contains(content, wire) {
			t.Errorf("bootstrap.go missing wire_gen call %q", wire)
		}
	}

	// Service row constructors carry the Name rows + wire calls now.
	rows := renderRepresentativeServiceRows(t)
	for _, want := range []string{
		`Name: "admin-server",`,
		`Name: "orders",`,
		"wireAdminServerDeps(app, cfg, logger)",
		"wireOrdersDeps(app, cfg, logger)",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("services_gen.go missing %q", want)
		}
	}

	// Fallible vs infallible constructor shapes preserved.
	if !strings.Contains(content, `fmt.Errorf("initializing trader worker: %w", err)`) {
		t.Error("fallible worker row should wrap its construction error with the historical message")
	}
	if !strings.Contains(content, "app.Workers.Emailer = emailer.New(emailerWkrDeps)") {
		t.Error("infallible worker row should assign New(...) directly")
	}
	if !strings.Contains(content, `fmt.Errorf("initializing audit: %w", err)`) {
		t.Error("fallible package row should wrap its construction error with the historical message")
	}
}

// TestBootstrapTable_BehaviorMovedToLibrary asserts that the program
// parts of the old bootstrap are gone from the generated file: no
// inline filtering, no inline vanguard, no inline diagnostics emitters,
// no inline controller-manager runtime.
func TestBootstrapTable_BehaviorMovedToLibrary(t *testing.T) {
	content := renderRepresentativeBootstrap(t)

	for needle, where := range map[string]string{
		"runAll":                       "BootstrapOnly name filtering (appkit.Options.Only)",
		"nameSet":                      "BootstrapOnly name filtering (appkit.Options.Only)",
		"unknown service/worker":       "unknown-name warning (appkit.Run)",
		"vanguard.NewTranscoder":       "REST transcoding (appkit.RESTDef)",
		"vanguard.NewService":          "REST transcoding (appkit.RESTDef)",
		"diagnostics.Default":          "diagnostics boot (appkit.DiagnosticsMode)",
		"ctrl.GetConfig":               "controller-manager runtime (operatorkit.Run)",
		"ctrl.NewManager":              "controller-manager runtime (operatorkit.Run)",
		"AddToScheme(mgr.GetScheme())": "scheme registration (operatorkit.Run)",
		"server filter active":         "loud filter banner (appkit.Run)",
		"registered, excluded":         "loud filter banner (appkit.Run)",
	} {
		if strings.Contains(content, needle) {
			t.Errorf("generated bootstrap.go still contains %q — that behavior belongs to %s", needle, where)
		}
	}

	// Diagnostics + operator runtime are data rows / delegates now.
	if !strings.Contains(content, "Diagnostics: appkit.DiagnosticsStrict,") {
		t.Error("strict-wiring should be a data field on the def table")
	}
	if !strings.Contains(content, "return operatorkit.Run(ctx, logger, operatorkit.Options{") {
		t.Error("RunOperators should delegate to operatorkit.Run")
	}
	if !strings.Contains(content, `{Name: "scaler", AddToScheme: scaler.AddToScheme, SetupWithManager: a.Operators.Scaler.SetupWithManager},`) {
		t.Error("RunOperators should carry one dumb operatorkit.Controller row per operator")
	}
}

// TestBootstrapTable_ShrinksRenderedOutput pins the size win: the
// table-shaped bootstrap for the representative config must stay well
// under the old program-shaped output (~660 lines for this config).
// If this regresses, behavior is probably leaking back into the
// template.
func TestBootstrapTable_ShrinksRenderedOutput(t *testing.T) {
	content := renderRepresentativeBootstrap(t)
	lines := strings.Count(content, "\n")
	if lines > 420 {
		t.Errorf("rendered bootstrap.go is %d lines; the table shape should keep the kitchen-sink config under 420 (old program shape was ~660)", lines)
	}
}

// TestBootstrapTable_MinimalConfig_ParsesAndCompilesShape covers the
// other extreme: a single service, nothing else, all features off. The
// import set must not contain anything unused (fmt/slices/middleware
// gating) — gofmt cleanliness + parse is the offline proxy.
func TestBootstrapTable_MinimalConfig_Parses(t *testing.T) {
	targetDir := t.TempDir()
	services := []ServiceDef{{Name: "APIService", ModulePath: "example.com/proj"}}
	if err := GenerateBootstrap(BootstrapGenInput{
		GenContext:      GenContext{ProjectDir: targetDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:        services,
		Packages:        nil,
		Workers:         nil,
		Operators:       nil,
		DatabaseDriver:  "",
		OrmEnabled:      false,
		ConfigFields:    nil,
		WebhookServices: nil,
		Features:        BootstrapFeatures{},
	}); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "bootstrap.go", content, parser.AllErrors); perr != nil {
		t.Fatalf("minimal bootstrap.go does not parse:\n%v\n\nSource:\n%s", perr, content)
	}
	if _, err := format.Source(data); err != nil {
		t.Fatalf("format.Source: %v", err)
	}

	// Feature-off config must not reference feature rows.
	for _, needle := range []string{"RESTDef", "Diagnostics:", "operatorkit", "Workers: []appkit.WorkerDef", "Packages: []appkit.PackageDef"} {
		if strings.Contains(content, needle) {
			t.Errorf("minimal bootstrap.go should not contain %q", needle)
		}
	}
	// No-operator projects keep the no-op RunOperators so cmd/server.go
	// compiles unchanged.
	if !strings.Contains(content, "// RunOperators is a no-op when no operators are configured.") {
		t.Error("minimal bootstrap.go should keep the no-op RunOperators")
	}
}

// TestBootstrapTable_NoComponents_Parses covers the degenerate empty
// project (no services/workers/operators/packages): the def table is
// just Setup + Hooks and everything still parses.
func TestBootstrapTable_NoComponents_Parses(t *testing.T) {
	targetDir := t.TempDir()
	if err := GenerateBootstrap(BootstrapGenInput{
		GenContext:      GenContext{ProjectDir: targetDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:        nil,
		Packages:        nil,
		Workers:         nil,
		Operators:       nil,
		DatabaseDriver:  "",
		OrmEnabled:      false,
		ConfigFields:    nil,
		WebhookServices: nil,
		Features:        BootstrapFeatures{},
	}); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "bootstrap.go", string(data), parser.AllErrors); perr != nil {
		t.Fatalf("empty-project bootstrap.go does not parse:\n%v\n\nSource:\n%s", perr, string(data))
	}
	if _, err := format.Source(data); err != nil {
		t.Fatalf("format.Source: %v", err)
	}

	// The zero-component def table is exactly Setup + Hooks: no
	// ServiceDef / WorkerDef / OperatorDef rows may appear. This is the
	// bare `forge new` shape (zero services scaffolded by default — a
	// binary is a deployment unit, not a domain entity), so the table
	// must compile-and-run with empty rows rather than inventing a
	// service from the project name.
	content := string(data)
	for _, row := range []string{"appkit.ServiceDef", "appkit.WorkerDef", "appkit.OperatorDef"} {
		if strings.Contains(content, row) {
			t.Errorf("zero-component bootstrap.go must not emit %s rows; got:\n%s", row, content)
		}
	}
	for _, want := range []string{"Setup: func() error { return Setup(app, cfg) }", "Hooks: func() *appkit.Hooks { return &app.Hooks }"} {
		if !strings.Contains(content, want) {
			t.Errorf("zero-component bootstrap.go missing def-table entry %q", want)
		}
	}
	// The empty Services holder still exists so user code referencing
	// app.Services compiles before the first `forge add service`.
	if !strings.Contains(content, "type Services struct {\n}") {
		t.Errorf("zero-component bootstrap.go should declare an empty Services struct; got:\n%s", content)
	}
}

// TestAppGen_DeclaresHooksField asserts app_gen.go grew the Hooks
// surface the def table reads back after Setup.
func TestAppGen_DeclaresHooksField(t *testing.T) {
	targetDir := t.TempDir()
	if err := GenerateAppGen(false, false, true, false, false, false, targetDir, nil); err != nil {
		t.Fatalf("GenerateAppGen() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "app_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "Hooks appkit.Hooks") {
		t.Error("app_gen.go should declare `Hooks appkit.Hooks` on App")
	}
	if !strings.Contains(content, `"github.com/reliant-labs/forge/pkg/appkit"`) {
		t.Error("app_gen.go should import pkg/appkit")
	}
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "app_gen.go", content, parser.AllErrors); perr != nil {
		t.Fatalf("app_gen.go does not parse:\n%v\n\nSource:\n%s", perr, content)
	}
	if _, err := format.Source(data); err != nil {
		t.Fatalf("format.Source: %v", err)
	}
}
