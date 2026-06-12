package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

func TestGenerateServiceStub_ZeroMethods_NoUnusedImports(t *testing.T) {
	// Create a temp directory to act as the target
	targetDir := filepath.Join(t.TempDir(), "ordersservice")

	svc := ServiceDef{
		Name:       "OrdersService",
		Package:    "orders.v1",
		GoPackage:  "github.com/test/proj/gen/proto/services/orders/v1",
		PkgName:    "ordersv1",
		Methods:    nil, // zero RPCs
		ProtoFile:  "proto/services/orders/v1/orders.proto",
		ModulePath: "github.com/test/proj",
	}

	if err := GenerateServiceStub(svc, targetDir); err != nil {
		t.Fatalf("GenerateServiceStub() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "service.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	// Must NOT contain "context" import (unused when no methods)
	if strings.Contains(content, `"context"`) {
		t.Errorf("generated stub with zero methods should not import \"context\", got:\n%s", content)
	}

	// The embedded template always imports pb for the keep-alive reference
	if !strings.Contains(content, `pb "`) {
		t.Error("generated stub should import pb (used for keep-alive reference)")
	}

	// Must still contain the connect package (used by struct embedding + Register)
	if !strings.Contains(content, `"connectrpc.com/connect"`) {
		t.Error("generated stub should still import connectrpc.com/connect")
	}

	// Must still contain the service connect package
	if !strings.Contains(content, `ordersv1connect`) {
		t.Error("generated stub should still import the service connect package")
	}

	// Must contain Deps struct with Logger
	if !strings.Contains(content, `type Deps struct`) {
		t.Error("generated stub should contain Deps struct")
	}
	if !strings.Contains(content, "Logger") || !strings.Contains(content, "*slog.Logger") {
		t.Error("generated stub should contain Logger field of type *slog.Logger in Deps")
	}

	// Must contain new fallible New() signature accepting Deps. The
	// service constructor is now always (*Service, error) so per-RPC
	// nil-check guards are unnecessary — bare-Deps validation runs once
	// at construction time inside validateDeps().
	if !strings.Contains(content, `func New(deps Deps) (*Service, error)`) {
		t.Error("generated stub should have New(deps Deps) (*Service, error) signature")
	}
	if !strings.Contains(content, `validateDeps`) {
		t.Error("generated stub should declare a validateDeps() helper used by New")
	}
	// (2026-05-07 wire-gen migration) ApplyDeps is gone; validateDeps
	// runs at New() time and the codegen'd wire_gen.go assembles the
	// full Deps before the call. No mutation method should remain.
	if strings.Contains(content, "func (s *Service) ApplyDeps(") {
		t.Error("generated stub should not define ApplyDeps anymore — wire_gen feeds full Deps into New()")
	}

	// Must NOT contain init() or registry.Register
	if strings.Contains(content, `func init()`) {
		t.Error("generated stub should not contain init() function")
	}
	if strings.Contains(content, `registry.Register`) {
		t.Error("generated stub should not contain registry.Register call")
	}
}

func TestGenerateServiceStub_WithMethods_IncludesImports(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "echoservice")

	svc := ServiceDef{
		Name:      "EchoService",
		Package:   "echo.v1",
		GoPackage: "github.com/test/proj/gen/proto/services/echo/v1",
		PkgName:   "echov1",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
		},
		ProtoFile:  "proto/services/echo/v1/echo.proto",
		ModulePath: "github.com/test/proj",
	}

	if err := GenerateServiceStub(svc, targetDir); err != nil {
		t.Fatalf("GenerateServiceStub() error = %v", err)
	}

	serviceData, err := os.ReadFile(filepath.Join(targetDir, "service.go"))
	if err != nil {
		t.Fatalf("ReadFile(service.go) error = %v", err)
	}

	serviceContent := string(serviceData)

	if !strings.Contains(serviceContent, `pb "`) {
		t.Error("service.go should import pb")
	}

	// handlers.go should be generated with method stubs
	handlersData, err := os.ReadFile(filepath.Join(targetDir, "handlers.go"))
	if err != nil {
		t.Fatalf("ReadFile(handlers.go) error = %v", err)
	}

	handlersContent := string(handlersData)

	if !strings.Contains(handlersContent, `"context"`) {
		t.Error("handlers.go with methods should import \"context\"")
	}

	if !strings.Contains(handlersContent, `func (s *Service) Echo(`) {
		t.Error("handlers.go should contain the Echo RPC stub")
	}
}

func TestGenerateMock_ZeroMethods_Skipped(t *testing.T) {
	mockDir := filepath.Join(t.TempDir(), "mocks")

	svc := ServiceDef{
		Name:       "OrdersService",
		Package:    "orders.v1",
		GoPackage:  "github.com/test/proj/gen/proto/services/orders/v1",
		PkgName:    "ordersv1",
		Methods:    nil,
		ProtoFile:  "proto/services/orders/v1/orders.proto",
		ModulePath: "github.com/test/proj",
	}

	written, err := GenerateMock(svc, mockDir)
	if err != nil {
		t.Fatalf("GenerateMock() error = %v", err)
	}
	if written {
		t.Error("expected written=false for zero-RPC service")
	}

	mockFile := filepath.Join(mockDir, "orders_mock.go")
	if _, err := os.Stat(mockFile); !os.IsNotExist(err) {
		t.Errorf("expected no mock file for zero-RPC service, but %s exists", mockFile)
	}
}

func TestGenerateBootstrap_MultipleServices(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
		{Name: "OrdersService", ModulePath: "example.com/proj"},
	}

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil, nil, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	// Must contain Services struct with both fields
	if !strings.Contains(content, `API *api.Service`) {
		t.Error("bootstrap.go should contain API field")
	}
	if !strings.Contains(content, `Orders *orders.Service`) {
		t.Error("bootstrap.go should contain Orders field")
	}

	// Must import both service packages
	if !strings.Contains(content, `"example.com/proj/handlers/api"`) {
		t.Error("bootstrap.go should import api service package")
	}
	if !strings.Contains(content, `"example.com/proj/handlers/orders"`) {
		t.Error("bootstrap.go should import orders service package")
	}

	// Must contain Bootstrap and BootstrapOnly functions
	if !strings.Contains(content, `func Bootstrap(`) {
		t.Error("bootstrap.go should contain Bootstrap function")
	}
	if !strings.Contains(content, `func BootstrapOnly(`) {
		t.Error("bootstrap.go should contain BootstrapOnly function")
	}

	// Must contain constructor calls. wire_gen owns the Deps literal;
	// the row constructors (which call wireXxxDeps then pass the result
	// into xxx.New) live in services_gen.go since the
	// registration-in-code rework, and bootstrap.go consumes them via
	// the user-owned RegisteredServices list.
	rowsData, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "services_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(services_gen.go) error = %v", err)
	}
	rows := string(rowsData)
	if !strings.Contains(rows, `api.New(apiDeps)`) {
		t.Error("services_gen.go should construct api service with wire_gen-built Deps")
	}
	if !strings.Contains(rows, `orders.New(ordersDeps)`) {
		t.Error("services_gen.go should construct orders service with wire_gen-built Deps")
	}
	if !strings.Contains(rows, `wireAPIDeps(app, cfg, logger`) {
		t.Error("services_gen.go should call wireAPIDeps(app, cfg, logger, devMode)")
	}
	if !strings.Contains(rows, `wireOrdersDeps(app, cfg, logger`) {
		t.Error("services_gen.go should call wireOrdersDeps(app, cfg, logger, devMode)")
	}
	if !strings.Contains(content, `RegisteredServices(app, cfg, logger, devMode, opts...)`) {
		t.Error("bootstrap.go should consume the user-owned RegisteredServices row list")
	}
	// The scaffold-once user-owned registration file lists every row.
	registryData, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "services.go"))
	if err != nil {
		t.Fatalf("ReadFile(services.go) error = %v", err)
	}
	for _, line := range []string{
		"serviceRowAPI(app, cfg, logger, devMode, opts...),",
		"serviceRowOrders(app, cfg, logger, devMode, opts...),",
	} {
		if !strings.Contains(string(registryData), line) {
			t.Errorf("scaffolded services.go missing row %q", line)
		}
	}

	// Must contain generated file header
	if !strings.Contains(content, `Code generated by forge. DO NOT EDIT.`) {
		t.Error("bootstrap.go should contain generated file header")
	}

	// Must return (*App, error)
	if !strings.Contains(content, `func Bootstrap(mux *http.ServeMux, logger *slog.Logger, cfg *config.Config, opts ...connect.HandlerOption) (*App, error)`) {
		t.Error("bootstrap.go Bootstrap() should return (*App, error)")
	}

	// (2026-05-07 wire-gen migration follow-up: the App struct itself now
	// lives in pkg/app/app_gen.go — generated by GenerateAppGen — so the
	// user-extension scaffold (app_extras.go) can embed AppExtras into it.
	// bootstrap.go owns Bootstrap/BootstrapOnly + the Services/Workers/etc.
	// holder structs, but no longer the App struct definition.)
	if strings.Contains(content, `type App struct`) {
		t.Error("bootstrap.go should NOT contain App struct (moved to app_gen.go)")
	}

	// Without packages, should not contain Packages struct
	if strings.Contains(content, `type Packages struct`) {
		t.Error("bootstrap.go without packages should not contain Packages struct")
	}
}

// TestGenerateBootstrap_RESTDisabled_NoVanguard verifies the default
// path: when `api.rest:` is absent / false, the generated bootstrap.go
// has no vanguard import, no transcoder wiring, and is byte-identical
// to the pre-vanguard shape. Existing projects must regenerate
// unchanged.
func TestGenerateBootstrap_RESTDisabled_NoVanguard(t *testing.T) {
	targetDir := t.TempDir()

	// Write a forge.yaml that does NOT set api.rest. The bootstrap should
	// see this as the canonical Connect-only mode.
	yaml := "name: proj\nmodule_path: example.com/proj\n"
	if err := os.WriteFile(filepath.Join(targetDir, "forge.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}
	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil, nil, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	if strings.Contains(content, "connectrpc.com/vanguard") {
		t.Error("bootstrap.go should NOT import vanguard when api.rest is off")
	}
	if strings.Contains(content, "vanguard.NewTranscoder") {
		t.Error("bootstrap.go should NOT call vanguard.NewTranscoder when api.rest is off")
	}
	if strings.Contains(content, "app.restHandler =") {
		t.Error("bootstrap.go should NOT assign app.restHandler when api.rest is off")
	}
}

// TestGenerateBootstrap_RESTEnabled_WrapsMux verifies that a project
// with `api.rest: true` in forge.yaml regenerates bootstrap.go with:
//   - an `appkit.RESTDef` row in the def table (the vanguard transcoder
//     construction itself lives in pkg/appkit, not in the generated
//     file — "tables, not programs"),
//   - one `ConnectName:` data field per service row keyed by the
//     Connect-generated `<X>ServiceName` constant, and
//   - the Assign closure pointing the wrapped handler at the
//     unexported `app.restHandler` field.
//
// The generated app_gen.go grows an unexported `restHandler http.Handler`
// field plus a `RESTHandler() http.Handler` accessor method (required by
// the serverkit.Application interface); this test confirms both are
// always emitted so serverkit can call the method unconditionally without
// a templated branch.
func TestGenerateBootstrap_RESTEnabled_WrapsMux(t *testing.T) {
	targetDir := t.TempDir()

	yaml := "name: proj\nmodule_path: example.com/proj\napi:\n  rest: true\n"
	if err := os.WriteFile(filepath.Join(targetDir, "forge.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
		{Name: "OrdersService", ModulePath: "example.com/proj"},
	}
	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil, nil, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}
	if err := GenerateAppGen(false, false, len(services) > 0, false, false, false, targetDir, nil); err != nil {
		t.Fatalf("GenerateAppGen() error = %v", err)
	}

	bootstrap, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile(bootstrap) error = %v", err)
	}
	bContent := string(bootstrap)

	// The transcoder construction moved into pkg/appkit — the generated
	// file carries only the data row that turns it on.
	if strings.Contains(bContent, "connectrpc.com/vanguard") {
		t.Error("bootstrap.go should NOT import vanguard directly — the transcoder construction lives in pkg/appkit")
	}
	if !strings.Contains(bContent, "REST: &appkit.RESTDef{") {
		t.Errorf("bootstrap.go should carry an appkit.RESTDef row when api.rest is on; got:\n%s", bContent)
	}
	// Per-service ConnectName data field with the connect ServiceName
	// constant — on the row constructors in services_gen.go since the
	// registration-in-code rework (the row data moved with the rows).
	rowsData, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "services_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(services_gen.go) error = %v", err)
	}
	rows := string(rowsData)
	if !strings.Contains(rows, "ConnectName: apiv1connect.APIServiceName") {
		t.Error("services_gen.go should reference apiv1connect.APIServiceName for APIService")
	}
	if !strings.Contains(rows, "ConnectName: ordersv1connect.OrdersServiceName") {
		t.Error("services_gen.go should reference ordersv1connect.OrdersServiceName for OrdersService")
	}
	// The Assign closure must land on the unexported restHandler field
	// — the backing store for the RESTHandler() accessor that
	// serverkit.Application requires (A2/serverkit shape), with the
	// transcoder construction itself in appkit (A5 table shape).
	if !strings.Contains(bContent, "app.restHandler = h") {
		t.Error("bootstrap.go RESTDef.Assign should point at app.restHandler (unexported field backing the RESTHandler() method)")
	}
	// Connect imports should appear in services_gen.go's import block
	// (bootstrap.go no longer references the connect packages).
	if !strings.Contains(rows, `"example.com/proj/gen/services/api/v1/apiv1connect"`) {
		t.Errorf("services_gen.go should import the apiv1connect package; got:\n%s", rows)
	}

	appGen, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "app_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(app_gen) error = %v", err)
	}
	if !strings.Contains(string(appGen), "restHandler http.Handler") {
		t.Error("app_gen.go should declare the unexported restHandler http.Handler field on App")
	}
	if !strings.Contains(string(appGen), "func (a *App) RESTHandler() http.Handler") {
		t.Error("app_gen.go should declare the RESTHandler() http.Handler accessor method on App")
	}

	// Sanity-parse the generated bootstrap.go to catch templating bugs
	// that produce non-Go syntax (mismatched braces, double-emitted
	// blocks, etc.) — the REST wrap nests two blocks per Bootstrap
	// function and is easy to mis-edit.
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "bootstrap.go", bContent, parser.AllErrors); perr != nil {
		t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", perr, bContent)
	}
}

// TestGenerateBootstrap_AutoWiresWebhookRoutes verifies the 2026-04-30
// LLM-port webhook auto-wire fix: when a service has webhooks declared in
// forge.yaml, the bootstrap calls RegisterWebhookRoutes(mux, stack) after
// RegisterHTTP(...) so generated webhook routes get mounted on the mux
// without the user having to hand-edit the user-owned RegisterHTTP body.
func TestGenerateBootstrap_AutoWiresWebhookRoutes(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "AdminServerService", ModulePath: "example.com/proj"}, // has webhooks
		{Name: "OrdersService", ModulePath: "example.com/proj"},      // no webhooks
	}

	// Snake_case package names match naming.ServicePackage output
	// (post-2026-06-08 snake-canonicalisation rule —
	// "AdminServerService" -> "admin_server", aligning with the
	// universal on-disk proto / handler dir convention).
	webhookServices := map[string]bool{
		"admin_server": true,
	}

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil, webhookServices, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	// The mount closures live in services_gen.go since the
	// registration-in-code rework.
	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "services_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	// Auto-wired RegisterWebhookRoutes for the webhook-bearing service.
	// (2026-05-07 wire-gen migration: services now hang directly off
	// `app.Services.<Field>` instead of a local `svcs` var.)
	if !strings.Contains(content, "app.Services.AdminServer.RegisterWebhookRoutes(mux, fmw.HTTPStack(logger, middleware.ClaimsFromContext))") {
		t.Errorf("services_gen.go should auto-wire RegisterWebhookRoutes for admin_server (has webhooks); got:\n%s", content)
	}

	// No auto-wire for the service without webhooks.
	if strings.Contains(content, "app.Services.Orders.RegisterWebhookRoutes(") {
		t.Errorf("services_gen.go should NOT auto-wire RegisterWebhookRoutes for orders (no webhooks)")
	}

	// Both services still get RegisterHTTP — the auto-wire is additive.
	if !strings.Contains(content, "app.Services.AdminServer.RegisterHTTP(mux, fmw.HTTPStack(logger, middleware.ClaimsFromContext))") {
		t.Errorf("services_gen.go should still call RegisterHTTP for admin_server")
	}
	if !strings.Contains(content, "app.Services.Orders.RegisterHTTP(mux, fmw.HTTPStack(logger, middleware.ClaimsFromContext))") {
		t.Errorf("services_gen.go should still call RegisterHTTP for orders")
	}
}

func TestGenerateBootstrap_WithPackages(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}

	packages := []BootstrapPackageData{
		{Name: "cache", Package: "cache", ImportPath: "cache", FieldName: "Cache", VarName: "cache"},
		{Name: "notifications", Package: "notifications", ImportPath: "notifications", FieldName: "Notifications", VarName: "notifications"},
	}

	if err := GenerateBootstrap(services, packages, nil, nil, "example.com/proj", false, false, targetDir, nil, nil, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}
	// app_gen.go owns the App struct definition (with Packages field
	// when present). Generate it here so the assertion below has a file
	// to read; the production pipeline runs both generators back-to-back.
	if err := GenerateAppGen(false, false, len(services) > 0, false, false, len(packages) > 0, targetDir, nil); err != nil {
		t.Fatalf("GenerateAppGen() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	// Must contain Packages struct
	if !strings.Contains(content, `type Packages struct`) {
		t.Error("bootstrap.go should contain Packages struct")
	}

	// Must contain package fields
	if !strings.Contains(content, `Cache cache.Service`) {
		t.Error("bootstrap.go should contain Cache field in Packages")
	}
	if !strings.Contains(content, `Notifications notifications.Service`) {
		t.Error("bootstrap.go should contain Notifications field in Packages")
	}

	// Must import internal packages
	if !strings.Contains(content, `"example.com/proj/internal/cache"`) {
		t.Error("bootstrap.go should import cache package")
	}
	if !strings.Contains(content, `"example.com/proj/internal/notifications"`) {
		t.Error("bootstrap.go should import notifications package")
	}

	// Must construct packages before services
	if !strings.Contains(content, `cache.New(cache.Deps{`) {
		t.Error("bootstrap.go should construct cache package")
	}
	if !strings.Contains(content, `notifications.New(notifications.Deps{`) {
		t.Error("bootstrap.go should construct notifications package")
	}

	// App struct now lives in pkg/app/app_gen.go (split out of
	// bootstrap.go in the wire-gen migration follow-up so AppExtras
	// embedding works). Read app_gen.go to verify the Packages field.
	appGenData, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "app_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(app_gen.go) error = %v", err)
	}
	if !strings.Contains(string(appGenData), `Packages *Packages`) {
		t.Error("app_gen.go App struct should contain Packages field")
	}
}

// TestInspectComponentDepsShape_DomainLocalConfig asserts that a
// package whose Deps.Config field is typed as a package-local Config
// (e.g. `enforcement.Config`) does NOT get HasConfig=true, because the
// bootstrap template would otherwise emit `Config: cfg` — where cfg is
// the project's `*config.Config` — and the codegen would fail to
// compile with "cannot use *config.Config as enforcement.Config".
//
// FRICTION 2026-06-02 (cp-forge layer-2 enforcement): the well-known
// name shortcut for "Config" bypassed type-matching entirely, forcing
// every package declaring its own Config struct to rename the field
// (Caps, EnforcementCaps, ...).
func TestInspectComponentDepsShape_DomainLocalConfig(t *testing.T) {
	projectDir := t.TempDir()

	// pkg/app/app_extras.go — empty AppExtras (no Config field there).
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app_extras.go"),
		[]byte("package app\n\ntype AppExtras struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// internal/enforcement/contract.go — Deps with a domain-local Config.
	pkgDir := filepath.Join(projectDir, "internal", "enforcement")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package enforcement

import "log/slog"

type Config struct{}

type Deps struct {
	Logger *slog.Logger
	Config Config
}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "contract.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	components := []BootstrapComponentData{
		{Name: "enforcement", Package: "enforcement", ImportPath: "enforcement"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if !components[0].HasLogger {
		t.Error("HasLogger should be true (Logger is *slog.Logger)")
	}
	if components[0].HasConfig {
		t.Error("HasConfig must be false when Deps.Config is a domain-local type (not *config.Config); bootstrap template would emit `Config: cfg` and fail to compile")
	}
}

// TestInspectComponentDepsShape_ProjectConfig asserts the canonical
// case still works: Deps.Config typed as *config.Config gets
// HasConfig=true so bootstrap emits `Config: cfg`.
func TestInspectComponentDepsShape_ProjectConfig(t *testing.T) {
	projectDir := t.TempDir()

	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app_extras.go"),
		[]byte("package app\n\ntype AppExtras struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgDir := filepath.Join(projectDir, "internal", "cache")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package cache

import (
	"log/slog"
	"example.com/proj/pkg/config"
)

type Deps struct {
	Logger *slog.Logger
	Config *config.Config
}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "contract.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	components := []BootstrapComponentData{
		{Name: "cache", Package: "cache", ImportPath: "cache"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if !components[0].HasLogger {
		t.Error("HasLogger should be true for *slog.Logger")
	}
	if !components[0].HasConfig {
		t.Error("HasConfig should be true for *config.Config")
	}
}

// canonicalAliasFixture writes a minimal project shape for the
// CanonicalAppField tests: pkg/app/app_extras.go with the given source,
// internal/<pkg>/contract.go with the given source, and a go.mod naming
// the module so import-path resolution is exact.
func canonicalAliasFixture(t *testing.T, appExtrasSrc, pkgName, contractSrc string) string {
	t.Helper()
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"),
		[]byte("module example.com/proj\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "app_extras.go"), []byte(appExtrasSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(projectDir, "internal", pkgName)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "contract.go"), []byte(contractSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectDir
}

// TestInspectComponentDepsShape_CanonicalServiceAlias mirrors the
// cp-forge svcdaemon shape: the package's Deps declares collaborator
// fields that have NO name match on App/AppExtras (DaemonRepo,
// URLBuilder — they are constructed inline in user-owned setup.go), so
// bootstrap's auto-wire cannot express the construction and the
// generated `daemon.New(Deps{...})` panics at boot ("Deps.DaemonRepo
// is required"). AppExtras DOES hold the canonical, fully-wired
// instance (`DaemonService daemon.Service`, assigned in Setup, which
// appkit runs before the package table). Expect CanonicalAppField to
// name it so the template emits an alias instead of a second,
// half-built instance.
func TestInspectComponentDepsShape_CanonicalServiceAlias(t *testing.T) {
	projectDir := canonicalAliasFixture(t, `package app

import daemon "example.com/proj/internal/daemon"

type AppExtras struct {
	Conn          string
	DaemonService daemon.Service
}
`, "daemon", `package daemon

import "log/slog"

type Repository interface{ Get() string }

type Service interface{ Do() }

type Deps struct {
	Logger     *slog.Logger
	Conn       string
	DaemonRepo Repository
}

func New(deps Deps) Service { return nil }
`)

	components := []BootstrapComponentData{
		{Name: "daemon", Package: "daemon", ImportPath: "daemon", FieldName: "Daemon", Alias: "daemon"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if got := components[0].CanonicalAppField; got != "DaemonService" {
		t.Errorf("CanonicalAppField = %q, want %q (Deps.DaemonRepo is unwireable and AppExtras.DaemonService holds the canonical instance)", got, "DaemonService")
	}
	// The name+type matches must still be recorded — testing.go and the
	// non-alias fallback paths read them.
	if !components[0].HasLogger {
		t.Error("HasLogger should still be true")
	}
}

// TestInspectComponentDepsShape_CanonicalAlias_FullyWiredStaysConstructed
// mirrors cp-forge's enforcement/Checker shape: every Deps field
// auto-wires by name+type, so even though AppExtras holds a field of
// the package's Service type, bootstrap keeps constructing its own
// instance — the alias mechanism is strictly for unexpressible wirings.
func TestInspectComponentDepsShape_CanonicalAlias_FullyWiredStaysConstructed(t *testing.T) {
	projectDir := canonicalAliasFixture(t, `package app

import enforcement "example.com/proj/internal/enforcement"

type AppExtras struct {
	EnforcementRepo string
	Checker         enforcement.Service
}
`, "enforcement", `package enforcement

import "log/slog"

type Service interface{ Check() }

type Deps struct {
	Logger          *slog.Logger
	EnforcementRepo string
}

func New(deps Deps) Service { return nil }
`)

	components := []BootstrapComponentData{
		{Name: "enforcement", Package: "enforcement", ImportPath: "enforcement", FieldName: "Enforcement", Alias: "enforcement"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if got := components[0].CanonicalAppField; got != "" {
		t.Errorf("CanonicalAppField = %q, want \"\" (all Deps fields auto-wire; no alias)", got)
	}
	if len(components[0].AppFieldRefs) != 1 || components[0].AppFieldRefs[0].DepsField != "EnforcementRepo" {
		t.Errorf("AppFieldRefs = %+v, want exactly [EnforcementRepo]", components[0].AppFieldRefs)
	}
}

// TestInspectComponentDepsShape_CanonicalAlias_ScalarOnlyGapStaysConstructed
// mirrors cp-forge's billing/APIKey shape: the only unwired Deps fields
// are configuration scalars (string / []byte / numeric). Scalars are
// never auto-wired and their zero value is the package's documented
// degraded mode — not the panic/no-op collaborator class — so the
// package keeps constructing even when a canonical instance exists.
func TestInspectComponentDepsShape_CanonicalAlias_ScalarOnlyGapStaysConstructed(t *testing.T) {
	projectDir := canonicalAliasFixture(t, `package app

import billing "example.com/proj/internal/billing"

type AppExtras struct {
	Stripe billing.Service
}
`, "billing", `package billing

type Service interface{ Charge() }

type Deps struct {
	APIKey    string
	PlansData []byte
}

func New(deps Deps) Service { return nil }
`)

	components := []BootstrapComponentData{
		{Name: "billing", Package: "billing", ImportPath: "billing", FieldName: "Billing", Alias: "billing"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if got := components[0].CanonicalAppField; got != "" {
		t.Errorf("CanonicalAppField = %q, want \"\" (only config scalars are unwired)", got)
	}
}

// TestInspectComponentDepsShape_CanonicalAlias_AliasedImport mirrors
// cp-forge's internal/user shape: app_extras.go imports the package
// under a renamed qualifier (internaluser "…/internal/user") so a
// package-name string compare would miss the field. The resolver must
// follow the file's import table.
func TestInspectComponentDepsShape_CanonicalAlias_AliasedImport(t *testing.T) {
	projectDir := canonicalAliasFixture(t, `package app

import internaluser "example.com/proj/internal/user"

type AppExtras struct {
	UserService internaluser.Service
}
`, "user", `package user

type AuditLogger interface{ Log() }

type Service interface{ Get() }

type Deps struct {
	Audit AuditLogger
}

func New(deps Deps) Service { return nil }
`)

	components := []BootstrapComponentData{
		{Name: "user", Package: "user", ImportPath: "user", FieldName: "PkgUser", Alias: "pkgUser"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if got := components[0].CanonicalAppField; got != "UserService" {
		t.Errorf("CanonicalAppField = %q, want %q (aliased import must resolve)", got, "UserService")
	}
}

// TestInspectComponentDepsShape_CanonicalAlias_AmbiguousSkips: two
// App/AppExtras fields of the package's Service type — there is no
// deterministic canonical instance, so fall back to construction (the
// deps-coverage lint surfaces the unwired field).
func TestInspectComponentDepsShape_CanonicalAlias_AmbiguousSkips(t *testing.T) {
	projectDir := canonicalAliasFixture(t, `package app

import daemon "example.com/proj/internal/daemon"

type AppExtras struct {
	DaemonService daemon.Service
	DaemonShadow  daemon.Service
}
`, "daemon", `package daemon

type Repository interface{ Get() string }

type Service interface{ Do() }

type Deps struct {
	DaemonRepo Repository
}

func New(deps Deps) Service { return nil }
`)

	components := []BootstrapComponentData{
		{Name: "daemon", Package: "daemon", ImportPath: "daemon", FieldName: "Daemon", Alias: "daemon"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if got := components[0].CanonicalAppField; got != "" {
		t.Errorf("CanonicalAppField = %q, want \"\" (two candidate fields is ambiguous)", got)
	}
}

// TestInspectComponentDepsShape_CanonicalAlias_OptionalDepDoesNotTrigger:
// a collaborator explicitly marked `// forge:optional-dep` is designed
// to be nil at construction — it must not count toward the
// "construction is unexpressible" trigger.
func TestInspectComponentDepsShape_CanonicalAlias_OptionalDepDoesNotTrigger(t *testing.T) {
	projectDir := canonicalAliasFixture(t, `package app

import notifier "example.com/proj/internal/notifier"

type AppExtras struct {
	NotifierService notifier.Service
}
`, "notifier", `package notifier

type Sink interface{ Send() }

type Service interface{ Notify() }

type Deps struct {
	// forge:optional-dep
	Sink Sink
}

func New(deps Deps) Service { return nil }
`)

	components := []BootstrapComponentData{
		{Name: "notifier", Package: "notifier", ImportPath: "notifier", FieldName: "Notifier", Alias: "notifier"},
	}
	inspectComponentDepsShape(components, projectDir, "internal")

	if got := components[0].CanonicalAppField; got != "" {
		t.Errorf("CanonicalAppField = %q, want \"\" (only optional-marked deps are unwired)", got)
	}
}

// TestGenerateBootstrap_CanonicalServiceAlias is the render-level
// regression for the cp-forge svcdaemon hand-edit: when a package's
// Deps cannot be auto-wired and AppExtras holds the canonical
// instance, bootstrap.go must emit
// `app.Packages.<Field> = app.<CanonicalField>` and must NOT construct
// a second instance.
func TestGenerateBootstrap_CanonicalServiceAlias(t *testing.T) {
	projectDir := canonicalAliasFixture(t, `package app

import daemon "example.com/proj/internal/daemon"

type AppExtras struct {
	DaemonService daemon.Service
}
`, "daemon", `package daemon

type Repository interface{ Get() string }

type Service interface{ Do() }

type Deps struct {
	DaemonRepo Repository
}

func New(deps Deps) Service { return nil }
`)

	packages, err := PackageDataFromNames([]string{"daemon"}, projectDir)
	if err != nil {
		t.Fatalf("PackageDataFromNames() error = %v", err)
	}
	if err := GenerateBootstrap(nil, packages, nil, nil, "example.com/proj", false, false, projectDir, nil, nil, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "app.Packages.Daemon = app.DaemonService") {
		t.Error("bootstrap.go should alias app.Packages.Daemon to the canonical app.DaemonService instance")
	}
	if strings.Contains(content, "daemon.New(daemon.Deps{") {
		t.Error("bootstrap.go must not construct a second daemon instance when the canonical alias applies")
	}
	// The Packages struct still declares the slot (typed by import).
	if !strings.Contains(content, "Daemon daemon.Service") {
		t.Error("bootstrap.go should keep the Packages.Daemon field declaration")
	}
}

func TestGenerateBootstrapTesting_MultipleServices(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
		{Name: "OrdersService", ModulePath: "example.com/proj"},
	}

	if err := GenerateBootstrapTesting(services, nil, nil, nil, "example.com/proj", false, targetDir, nil); err != nil {
		t.Fatalf("GenerateBootstrapTesting() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	// Must contain generated file header
	if !strings.Contains(content, `Code generated by forge. DO NOT EDIT.`) {
		t.Error("testing.go should contain generated file header")
	}

	// Must contain TestOption type
	if !strings.Contains(content, `type TestOption func(*testConfig)`) {
		t.Error("testing.go should contain TestOption type")
	}

	// Per-service dep override options return SERVICE-SCOPED option types
	// (review DI#8): WithAPIDeps must not be passable to NewTestOrders —
	// the old single TestOption type made that compile and silently no-op.
	if !strings.Contains(content, `func WithAPIDeps(deps api.Deps) APITestOption`) {
		t.Error("testing.go should contain WithAPIDeps option returning APITestOption")
	}
	if !strings.Contains(content, `func WithOrdersDeps(deps orders.Deps) OrdersTestOption`) {
		t.Error("testing.go should contain WithOrdersDeps option returning OrdersTestOption")
	}
	// Cross-cutting TestOption must satisfy each per-service interface.
	if !strings.Contains(content, `func (o TestOption) applyAPI(c *testConfig)`) {
		t.Error("testing.go should adapt TestOption to APITestOption")
	}
	if !strings.Contains(content, `func (o TestOption) applyOrders(c *testConfig)`) {
		t.Error("testing.go should adapt TestOption to OrdersTestOption")
	}

	// Must contain NewTestXxx functions taking the per-service option type
	if !strings.Contains(content, `func NewTestAPI(t *testing.T, opts ...APITestOption) *api.Service`) {
		t.Error("testing.go should contain NewTestAPI function")
	}
	if !strings.Contains(content, `func NewTestOrders(t *testing.T, opts ...OrdersTestOption) *orders.Service`) {
		t.Error("testing.go should contain NewTestOrders function")
	}

	// Must contain NewTestXxxServer functions
	if !strings.Contains(content, `func NewTestAPIServer(t *testing.T, opts ...APITestOption)`) {
		t.Error("testing.go should contain NewTestAPIServer function")
	}
	if !strings.Contains(content, `func NewTestOrdersServer(t *testing.T, opts ...OrdersTestOption)`) {
		t.Error("testing.go should contain NewTestOrdersServer function")
	}

	// The test server must mount the production interceptor chain shape:
	// AuthzInterceptor wired with the effective (default: permissive)
	// authorizer — never an empty connect.WithInterceptors().
	if !strings.Contains(content, `middleware.AuthzInterceptor(deps.Authorizer)`) {
		t.Error("testing.go should mount middleware.AuthzInterceptor in NewTestXxxServer")
	}
	if strings.Contains(content, "connect.WithInterceptors()") {
		t.Error("testing.go must not register with an EMPTY interceptor chain")
	}

	// AuthedContext re-export: claims-bearing ctx via the project's own
	// middleware.ContextWithClaims setter.
	if !strings.Contains(content, `func AuthedContext(t *testing.T, opts ...testkit.ClaimsOption) context.Context`) {
		t.Error("testing.go should re-export testkit.AuthedContext bound to middleware.ContextWithClaims")
	}
	if !strings.Contains(content, `testkit.AuthedContext(t, middleware.ContextWithClaims, opts...)`) {
		t.Error("testing.go AuthedContext should delegate to testkit with the project setter")
	}

	// Must import service packages
	if !strings.Contains(content, `"example.com/proj/handlers/api"`) {
		t.Error("testing.go should import api service package")
	}
	if !strings.Contains(content, `"example.com/proj/handlers/orders"`) {
		t.Error("testing.go should import orders service package")
	}

	// Must import connect packages
	if !strings.Contains(content, `"example.com/proj/gen/services/api/v1/apiv1connect"`) {
		t.Error("testing.go should import api connect package")
	}
	if !strings.Contains(content, `"example.com/proj/gen/services/orders/v1/ordersv1connect"`) {
		t.Error("testing.go should import orders connect package")
	}

	// Must use proto service names in connect client types
	if !strings.Contains(content, `apiv1connect.APIServiceClient`) {
		t.Error("testing.go should reference APIServiceClient")
	}
	if !strings.Contains(content, `ordersv1connect.OrdersServiceClient`) {
		t.Error("testing.go should reference OrdersServiceClient")
	}

	// Must contain defaultTestConfig with discard logger via testkit.
	if !strings.Contains(content, `testkit.DiscardLogger()`) {
		t.Error("testing.go should use testkit.DiscardLogger() for default logger")
	}
	// Must wire the testkit-backed permissive authorizer.
	if !strings.Contains(content, `testkit.PermissiveAuthorizer{}`) {
		t.Error("testing.go should default authz to testkit.PermissiveAuthorizer{}")
	}
	// Must import the testkit library.
	if !strings.Contains(content, `"github.com/reliant-labs/forge/pkg/testkit"`) {
		t.Error("testing.go should import forge/pkg/testkit")
	}
}

func TestGenerateBootstrapTesting_WithPackages(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}

	packages := []BootstrapPackageData{
		{Name: "cache", Package: "cache", ImportPath: "cache", FieldName: "Cache", VarName: "cache"},
	}

	if err := GenerateBootstrapTesting(services, packages, nil, nil, "example.com/proj", false, targetDir, nil); err != nil {
		t.Fatalf("GenerateBootstrapTesting() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	// Must import internal package
	if !strings.Contains(content, `"example.com/proj/internal/cache"`) {
		t.Error("testing.go should import cache package")
	}

	// Must contain WithCacheDeps option returning the package-scoped type
	// (review DI#8: a cache option must not silently no-op in NewTestAPI).
	if !strings.Contains(content, `func WithCacheDeps(deps cache.Deps) CacheTestOption`) {
		t.Error("testing.go should contain WithCacheDeps option returning CacheTestOption")
	}

	// Must contain NewTestCache function returning interface type
	if !strings.Contains(content, `func NewTestCache(t *testing.T, opts ...CacheTestOption) cache.Service`) {
		t.Error("testing.go should contain NewTestCache function")
	}
}

// TestGenerateBootstrapTesting_MigratedDBOptIn pins the DB harness
// contract for projects with embedded migrations: the DEFAULT test DB
// stays the bare in-memory SQLite (forge migrations are typically
// PostgreSQL-dialect — auto-applying them to SQLite would fail every
// scaffold out of the box), and a NewMigratedTestDB helper is emitted so
// tests opt in to the real schema loudly via WithDB(NewMigratedTestDB(t)).
func TestGenerateBootstrapTesting_MigratedDBOptIn(t *testing.T) {
	projectDir := t.TempDir()

	// A service whose Deps carry a DB field (AnyServiceHasDB → true).
	handlerDir := filepath.Join(projectDir, "handlers", "api")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	serviceGo := `package api

import (
	"log/slog"

	"github.com/reliant-labs/forge/pkg/orm"
)

type Deps struct {
	Logger *slog.Logger
	DB     orm.Context
}

type Service struct{ deps Deps }

func New(deps Deps) (*Service, error) { return &Service{deps: deps}, nil }
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(serviceGo), 0o644); err != nil {
		t.Fatal(err)
	}

	// Embedded migrations present (the GenerateMigrate predicate).
	migDir := filepath.Join(projectDir, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "00001_init.up.sql"), []byte("CREATE TABLE items (id TEXT);"), 0o644); err != nil {
		t.Fatal(err)
	}

	services := []ServiceDef{{Name: "APIService", ModulePath: "example.com/proj"}}
	if err := GenerateBootstrapTesting(services, nil, nil, nil, "example.com/proj", false, projectDir, nil); err != nil {
		t.Fatalf("GenerateBootstrapTesting() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `db:     testkit.NewSQLiteMemDB(t)`) {
		t.Error("default test DB must stay the BARE in-memory SQLite (migrations are opt-in)")
	}
	if !strings.Contains(content, `func NewMigratedTestDB(t *testing.T) orm.Context`) {
		t.Error("testing.go should emit the NewMigratedTestDB opt-in helper when migrations exist")
	}
	if !strings.Contains(content, `testkit.NewMigratedSQLiteDB(t, forgedb.MigrationsFS)`) {
		t.Error("NewMigratedTestDB should delegate to testkit.NewMigratedSQLiteDB over forgedb.MigrationsFS")
	}
	if !strings.Contains(content, `forgedb "example.com/proj/db"`) {
		t.Error("testing.go should import the project db package as forgedb")
	}
}

func TestPackageDataFromNames(t *testing.T) {
	pkgs, err := PackageDataFromNames([]string{"cache", "db", "notifications"}, t.TempDir())
	if err != nil {
		t.Fatalf("PackageDataFromNames: %v", err)
	}

	if len(pkgs) != 3 {
		t.Fatalf("expected 3 packages, got %d", len(pkgs))
	}

	if pkgs[0].Name != "cache" || pkgs[0].FieldName != "Cache" {
		t.Errorf("expected cache/Cache, got %s/%s", pkgs[0].Name, pkgs[0].FieldName)
	}
	if pkgs[1].Name != "db" || pkgs[1].FieldName != "DB" {
		t.Errorf("expected db/DB, got %s/%s", pkgs[1].Name, pkgs[1].FieldName)
	}
	if pkgs[2].Name != "notifications" || pkgs[2].FieldName != "Notifications" {
		t.Errorf("expected notifications/Notifications, got %s/%s", pkgs[2].Name, pkgs[2].FieldName)
	}
}

// Bug #19 regression: nested package names ("mcp/database") must produce
// distinct ImportPath / FieldName / VarName so two nested packages with the
// same leaf don't collide in the bootstrap struct.
func TestPackageDataFromNames_Nested(t *testing.T) {
	pkgs, err := PackageDataFromNames([]string{"mcp/database", "cache"}, t.TempDir())
	if err != nil {
		t.Fatalf("PackageDataFromNames: %v", err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(pkgs))
	}
	got := pkgs[0]
	if got.Package != "database" {
		t.Errorf("nested Package leaf = %q, want \"database\"", got.Package)
	}
	if got.ImportPath != "mcp/database" {
		t.Errorf("nested ImportPath = %q, want \"mcp/database\"", got.ImportPath)
	}
	// "MCP" is a registered Go initialism, so PascalCase upper-cases it.
	if got.FieldName != "MCPDatabase" {
		t.Errorf("nested FieldName = %q, want \"MCPDatabase\"", got.FieldName)
	}
	// VarName lowercases only the first rune (preserves the rest of the
	// initialism as-is — "mCPDatabase" is awkward but valid Go and unique).
	if got.VarName != "mCPDatabase" {
		t.Errorf("nested VarName = %q, want \"mCPDatabase\"", got.VarName)
	}
	// Flat names still work the same way.
	flat := pkgs[1]
	if flat.Package != "cache" || flat.ImportPath != "cache" || flat.FieldName != "Cache" || flat.VarName != "cache" {
		t.Errorf("flat = %+v, want all=cache/Cache", flat)
	}
}

func TestToExportedFieldName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"api", "API"},
		{"db", "DB"},
		{"orders", "Orders"},
		{"notifications", "Notifications"},
	}

	for _, tt := range tests {
		got := naming.ToExportedFieldName(tt.input)
		if got != tt.want {
			t.Errorf("naming.ToExportedFieldName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGenerateMissingHandlerStubs_GeneratesOnlyMissing(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "echoservice")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Simulate an existing handlers.go with only the Echo method implemented
	existingHandlers := `package echo

import (
	"context"
	"connectrpc.com/connect"
)

func (s *Service) Echo(
	ctx context.Context,
	req *connect.Request[any],
) (*connect.Response[any], error) {
	return nil, nil
}
`
	if err := os.WriteFile(filepath.Join(targetDir, "handlers.go"), []byte(existingHandlers), 0644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:      "EchoService",
		Package:   "echo.v1",
		GoPackage: "github.com/test/proj/gen/proto/services/echo/v1",
		PkgName:   "echov1",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
			{Name: "Ping", InputType: "PingRequest", OutputType: "PingResponse"},
			{Name: "Health", InputType: "HealthRequest", OutputType: "HealthResponse"},
		},
		ProtoFile:  "proto/services/echo/v1/echo.proto",
		ModulePath: "github.com/test/proj",
	}

	result, err := GenerateMissingHandlerStubs(svc, t.TempDir(), targetDir, nil, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}

	if result.AllUpToDate {
		t.Fatal("expected new methods to be generated, got AllUpToDate", nil)
	}

	if len(result.NewMethods) != 2 {
		t.Fatalf("expected 2 new methods, got %d: %v", len(result.NewMethods), result.NewMethods)
	}

	// Verify the method names
	expected := map[string]bool{"Ping": true, "Health": true}
	for _, name := range result.NewMethods {
		if !expected[name] {
			t.Errorf("unexpected method %q in new methods", name)
		}
	}

	// Verify the file was created
	data, err := os.ReadFile(filepath.Join(targetDir, "handlers_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(handlers_gen.go) error = %v", err)
	}

	content := string(data)

	// Should contain new methods
	if !strings.Contains(content, "func (s *Service) Ping(") {
		t.Error("handlers_gen.go should contain Ping stub")
	}
	if !strings.Contains(content, "func (s *Service) Health(") {
		t.Error("handlers_gen.go should contain Health stub")
	}

	// Should NOT contain the existing method
	if strings.Contains(content, "func (s *Service) Echo(") {
		t.Error("handlers_gen.go should NOT contain Echo (already exists)")
	}

	// Should have the generated code header
	if !strings.Contains(content, "Code generated by forge.") {
		t.Error("handlers_gen.go should contain generated code header")
	}
}

func TestGenerateMissingHandlerStubs_AllUpToDate(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "echoservice")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	// All methods already implemented
	existingHandlers := `package echo

func (s *Service) Echo() {}
func (s *Service) Ping() {}
`
	if err := os.WriteFile(filepath.Join(targetDir, "handlers.go"), []byte(existingHandlers), 0644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:      "EchoService",
		Package:   "echo.v1",
		GoPackage: "github.com/test/proj/gen/proto/services/echo/v1",
		PkgName:   "echov1",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
			{Name: "Ping", InputType: "PingRequest", OutputType: "PingResponse"},
		},
		ProtoFile:  "proto/services/echo/v1/echo.proto",
		ModulePath: "github.com/test/proj",
	}

	result, err := GenerateMissingHandlerStubs(svc, t.TempDir(), targetDir, nil, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}

	if !result.AllUpToDate {
		t.Fatalf("expected AllUpToDate, got new methods: %v", result.NewMethods)
	}

	// Should NOT create handlers_gen.go
	if _, err := os.Stat(filepath.Join(targetDir, "handlers_gen.go")); !os.IsNotExist(err) {
		t.Error("handlers_gen.go should not be created when all methods exist")
	}
}

func TestGenerateMissingHandlerStubs_SkipsTestFiles(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "echoservice")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Method implemented only in test file — should NOT count as existing
	testFile := `package echo

func (s *Service) Echo() {}
`
	if err := os.WriteFile(filepath.Join(targetDir, "handlers_test.go"), []byte(testFile), 0644); err != nil {
		t.Fatal(err)
	}
	// Real handler dirs always carry a service.go; the disk-first
	// resolver reads the package clause from it.
	if err := os.WriteFile(filepath.Join(targetDir, "service.go"), []byte("package echo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:      "EchoService",
		Package:   "echo.v1",
		GoPackage: "github.com/test/proj/gen/proto/services/echo/v1",
		PkgName:   "echov1",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
		},
		ProtoFile:  "proto/services/echo/v1/echo.proto",
		ModulePath: "github.com/test/proj",
	}

	result, err := GenerateMissingHandlerStubs(svc, t.TempDir(), targetDir, nil, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}

	if result.AllUpToDate {
		t.Fatal("expected Echo to be generated since it's only in test files")
	}
	if len(result.NewMethods) != 1 || result.NewMethods[0] != "Echo" {
		t.Fatalf("expected [Echo], got %v", result.NewMethods)
	}
}

func TestGenerateMissingHandlerStubs_RemovesStaleGeneratedFileWhenAllMethodsImplemented(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "echoservice")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	existingHandlers := `package echo

func (s *Service) Echo() {}
func (s *Service) Ping() {}
`
	if err := os.WriteFile(filepath.Join(targetDir, "handlers.go"), []byte(existingHandlers), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "handlers_gen.go"), []byte("package echo\nfunc (s *Service) Echo() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:      "EchoService",
		Package:   "echo.v1",
		GoPackage: "github.com/test/proj/gen/proto/services/echo/v1",
		PkgName:   "echov1",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
			{Name: "Ping", InputType: "PingRequest", OutputType: "PingResponse"},
		},
		ProtoFile:  "proto/services/echo/v1/echo.proto",
		ModulePath: "github.com/test/proj",
	}

	result, err := GenerateMissingHandlerStubs(svc, t.TempDir(), targetDir, nil, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}
	if !result.AllUpToDate {
		t.Fatalf("expected AllUpToDate, got new methods: %v", result.NewMethods)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "handlers_gen.go")); !os.IsNotExist(err) {
		t.Fatal("handlers_gen.go should be removed when all methods are implemented elsewhere")
	}
}

func TestGenerateMissingHandlerStubs_IgnoresGeneratedStubsWhenDetectingMissing(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "echoservice")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(targetDir, "handlers_gen.go"), []byte("package echo\nfunc (s *Service) Echo() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:      "EchoService",
		Package:   "echo.v1",
		GoPackage: "github.com/test/proj/gen/proto/services/echo/v1",
		PkgName:   "echov1",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
			{Name: "Ping", InputType: "PingRequest", OutputType: "PingResponse"},
		},
		ProtoFile:  "proto/services/echo/v1/echo.proto",
		ModulePath: "github.com/test/proj",
	}

	result, err := GenerateMissingHandlerStubs(svc, t.TempDir(), targetDir, nil, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}
	if result.AllUpToDate {
		t.Fatal("expected missing methods to be regenerated when only handlers_gen.go exists")
	}
	if len(result.NewMethods) != 2 {
		t.Fatalf("expected 2 regenerated methods, got %v", result.NewMethods)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "handlers_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(handlers_gen.go) error = %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "func (s *Service) Echo(") || !strings.Contains(content, "func (s *Service) Ping(") {
		t.Fatal("handlers_gen.go should be rewritten with all still-missing methods")
	}
}

func TestGenerateSetup_CreatesFile(t *testing.T) {
	targetDir := t.TempDir()

	if err := GenerateSetup("example.com/proj", "", false, targetDir); err != nil {
		t.Fatalf("GenerateSetup() error = %v", err)
	}

	setupPath := filepath.Join(targetDir, "pkg", "app", "setup.go")
	data, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("ReadFile(setup.go) error = %v", err)
	}

	content := string(data)

	if !strings.Contains(content, "func Setup(app *App, cfg *config.Config)") {
		t.Error("setup.go should contain Setup function")
	}
	if !strings.Contains(content, "example.com/proj/pkg/config") {
		t.Error("setup.go should import the project config package")
	}
	if !strings.Contains(content, "never overwrite") {
		t.Error("setup.go should document that it's never overwritten")
	}
}

func TestGenerateSetup_DoesNotOverwrite(t *testing.T) {
	targetDir := t.TempDir()
	appDir := filepath.Join(targetDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}

	customContent := "package app\n// my custom setup\n"
	if err := os.WriteFile(filepath.Join(appDir, "setup.go"), []byte(customContent), 0644); err != nil {
		t.Fatal(err)
	}

	if err := GenerateSetup("example.com/proj", "", false, targetDir); err != nil {
		t.Fatalf("GenerateSetup() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(appDir, "setup.go"))
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != customContent {
		t.Errorf("setup.go was overwritten, expected %q, got %q", customContent, string(data))
	}
}

func TestGenerateBootstrap_IncludesSetupCall(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil, nil, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	if !strings.Contains(content, "Setup(app, cfg)") {
		t.Error("bootstrap.go should call Setup(app, cfg)")
	}
	if !strings.Contains(content, "pkg/app/setup.go") {
		t.Error("bootstrap.go should reference setup.go in a comment")
	}
}

func TestGenerateSetup_WithPostgres(t *testing.T) {
	targetDir := t.TempDir()

	// Pass ormEnabled=true so the setup file includes ORM wiring — the test
	// asserts on ORM-related output.
	if err := GenerateSetup("example.com/proj", "postgres", true, targetDir); err != nil {
		t.Fatalf("GenerateSetup() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "setup.go"))
	if err != nil {
		t.Fatalf("ReadFile(setup.go) error = %v", err)
	}

	content := string(data)

	if !strings.Contains(content, "database/sql") {
		t.Error("setup.go should import database/sql when database is configured")
	}
	if !strings.Contains(content, "pgx/v5/stdlib") {
		t.Error("setup.go should import pgx driver for postgres")
	}
	if !strings.Contains(content, "sql.Open") {
		t.Error("setup.go should call sql.Open")
	}
	if !strings.Contains(content, "db.Ping()") {
		t.Error("setup.go should call db.Ping()")
	}
	if !strings.Contains(content, "app.DB = db") {
		t.Error("setup.go should assign db to app.DB")
	}
	if !strings.Contains(content, "SetMaxOpenConns") {
		t.Error("setup.go should set connection pool settings")
	}
}

func TestGeneratePostBootstrap_CreatesFile(t *testing.T) {
	targetDir := t.TempDir()

	if err := GeneratePostBootstrap(targetDir); err != nil {
		t.Fatalf("GeneratePostBootstrap() error = %v", err)
	}

	hookPath := filepath.Join(targetDir, "pkg", "app", "post_bootstrap.go")
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(post_bootstrap.go) error = %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "func PostBootstrap(app *App) error") {
		t.Error("post_bootstrap.go must declare PostBootstrap(app *App) error")
	}
	if !strings.Contains(content, "return nil") {
		t.Error("post_bootstrap.go default body must be a no-op (return nil)")
	}
	if !strings.Contains(content, "never overwrite") {
		t.Error("post_bootstrap.go should document that it's never overwritten")
	}
}

func TestGeneratePostBootstrap_DoesNotOverwrite(t *testing.T) {
	targetDir := t.TempDir()
	appDir := filepath.Join(targetDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}

	custom := "package app\n// my custom post-bootstrap wiring\n"
	hookPath := filepath.Join(appDir, "post_bootstrap.go")
	if err := os.WriteFile(hookPath, []byte(custom), 0644); err != nil {
		t.Fatal(err)
	}

	if err := GeneratePostBootstrap(targetDir); err != nil {
		t.Fatalf("GeneratePostBootstrap() error = %v", err)
	}

	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != custom {
		t.Errorf("post_bootstrap.go was overwritten: want %q got %q", custom, string(got))
	}
}

func TestGenerateSetup_WithoutDatabase(t *testing.T) {
	targetDir := t.TempDir()

	if err := GenerateSetup("example.com/proj", "", false, targetDir); err != nil {
		t.Fatalf("GenerateSetup() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "setup.go"))
	if err != nil {
		t.Fatalf("ReadFile(setup.go) error = %v", err)
	}

	content := string(data)

	if strings.Contains(content, "database/sql") {
		t.Error("setup.go should not import database/sql when no database configured")
	}
	if strings.Contains(content, "sql.Open") {
		t.Error("setup.go should not call sql.Open when no database configured")
	}
}

func TestGenerateBootstrap_WithDatabase(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", true, true, targetDir, nil, nil, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}
	// App struct (with DB field + database/sql import) now lives in
	// app_gen.go after the wire-gen migration follow-up.
	if err := GenerateAppGen(true, true, len(services) > 0, false, false, false, targetDir, nil); err != nil {
		t.Fatalf("GenerateAppGen() error = %v", err)
	}

	appGenData, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "app_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(app_gen.go) error = %v", err)
	}
	content := string(appGenData)

	if !strings.Contains(content, "DB") || !strings.Contains(content, "*sql.DB") {
		t.Error("app_gen.go should include DB field when database is configured")
	}
	if !strings.Contains(content, `"database/sql"`) {
		t.Error("app_gen.go should import database/sql when database is configured")
	}
}

// TestToServicePackage_MovedToNaming notes the canonical test moved to
// internal/naming/naming_test.go (TestServicePackage). The codegen
// helper now delegates to naming.ServicePackage; the table-driven cases
// live with the canonical implementation.
// TestGenerateServiceStub_HandlersTestMatchesBootstrapTestingHelper covers
// the cross-role collision case: when an internal/<svc> directory exists,
// GenerateBootstrapTesting emits NewTestSvc<Pascal> rather than NewTest<Pascal>.
// The scaffolded handlers_scaffold_test.go must reference the same identifier.
func TestGenerateServiceStub_HandlersTestMatchesBootstrapTestingHelper(t *testing.T) {
	projectDir := t.TempDir()
	// Simulate the colliding internal package — its presence is what flips
	// the disambiguation in ComputeTestHelperName / GenerateBootstrapTesting.
	if err := os.MkdirAll(filepath.Join(projectDir, "internal", "billing"), 0755); err != nil {
		t.Fatalf("setup internal/billing: %v", err)
	}
	targetDir := filepath.Join(projectDir, "handlers", "billing")

	svc := ServiceDef{
		Name:      "BillingService",
		Package:   "billing.v1",
		GoPackage: "example.com/proj/gen/services/billing/v1",
		PkgName:   "billingv1",
		Methods: []Method{
			{Name: "GetBill", InputType: "GetBillRequest", OutputType: "GetBillResponse"},
		},
		ProtoFile:  "proto/services/billing/v1/billing.proto",
		ModulePath: "example.com/proj",
	}

	if err := GenerateServiceStub(svc, targetDir); err != nil {
		t.Fatalf("GenerateServiceStub: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(targetDir, "handlers_scaffold_test.go"))
	if err != nil {
		t.Fatalf("read handlers_scaffold_test.go: %v", err)
	}
	content := string(got)

	if !strings.Contains(content, "app.NewTestSvcBilling(t)") {
		t.Errorf("handlers_scaffold_test.go should reference app.NewTestSvcBilling on internal/billing collision, content:\n%s", content)
	}
	if strings.Contains(content, "app.NewTestBilling(t)") {
		t.Errorf("handlers_scaffold_test.go should NOT reference app.NewTestBilling on collision, content:\n%s", content)
	}

	// And the no-collision case: another service without an internal dir
	// keeps the simple form.
	noCollisionDir := filepath.Join(projectDir, "handlers", "echo")
	echoSvc := ServiceDef{
		Name:      "EchoService",
		Package:   "echo.v1",
		GoPackage: "example.com/proj/gen/services/echo/v1",
		PkgName:   "echov1",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
		},
		ProtoFile:  "proto/services/echo/v1/echo.proto",
		ModulePath: "example.com/proj",
	}
	if err := GenerateServiceStub(echoSvc, noCollisionDir); err != nil {
		t.Fatalf("GenerateServiceStub (echo): %v", err)
	}
	echoTest, err := os.ReadFile(filepath.Join(noCollisionDir, "handlers_scaffold_test.go"))
	if err != nil {
		t.Fatalf("read echo handlers_scaffold_test.go: %v", err)
	}
	if !strings.Contains(string(echoTest), "app.NewTestEcho(t)") {
		t.Errorf("echo handlers_scaffold_test.go should reference app.NewTestEcho (no collision)")
	}
}

func TestComputeTestHelperName(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, "internal", "billing"), 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cases := []struct {
		pkg, project, want string
	}{
		{"billing", projectDir, "SvcBilling"},
		{"users", projectDir, "Users"},
		{"admin_server", projectDir, "AdminServer"},
		{"billing", "", "Billing"}, // no project context -> no-collision form
	}
	for _, c := range cases {
		if got := ComputeTestHelperName(c.pkg, c.project); got != c.want {
			t.Errorf("ComputeTestHelperName(%q, %q) = %q, want %q", c.pkg, c.project, got, c.want)
		}
	}
}

// TestWorkerDataFromNames_PascalCaseFieldName locks in the
// snake_case-worker fix: `forge add worker calibrator_refit` must yield
// the idiomatic Go identifier `CalibratorRefit` for the exported
// Workers struct field + the `wireWorkerCalibratorRefitDeps` function name,
// not the underscore-preserving `Calibrator_refit` form that revive /
// staticcheck ST1003 would flag.
//
// Post-2026-06-08: the on-disk Package + Go package identifier is
// snake_case ("calibrator_refit" stays "calibrator_refit", "email-sender"
// becomes "email_sender"). Snake_case is a valid Go package identifier
// and matches the universal on-disk dir convention proto buf emits for
// multi-word proto packages. FieldName still derives from the original
// name via ToPascalCase so the exported Go identifier reads as multiple
// words.
func TestWorkerDataFromNames_PascalCaseFieldName(t *testing.T) {
	cases := []struct {
		name          string
		wantPackage   string
		wantFieldName string
		wantVarName   string
	}{
		// Snake-case → snake pkg; PascalCase via ToPascalCase from the
		// original name so word boundaries survive.
		{"calibrator_refit", "calibrator_refit", "CalibratorRefit", "calibratorRefit"},
		// Hyphenated → normalized to snake.
		{"email-sender", "email_sender", "EmailSender", "emailSender"},
		// Single-word stays as-is (just upper-cased first letter).
		{"refresh", "refresh", "Refresh", "refresh"},
		// Initialism — ToPascalCase recognizes API and uppercases it.
		{"api_poll", "api_poll", "APIPoll", "aPIPoll"},
	}
	for _, c := range cases {
		got, err := WorkerDataFromNames([]string{c.name}, "")
		if err != nil {
			t.Fatalf("WorkerDataFromNames(%q): %v", c.name, err)
		}
		if len(got) != 1 {
			t.Fatalf("WorkerDataFromNames(%q) returned %d entries, want 1", c.name, len(got))
		}
		w := got[0]
		if w.Package != c.wantPackage {
			t.Errorf("WorkerDataFromNames(%q).Package = %q, want %q", c.name, w.Package, c.wantPackage)
		}
		if w.FieldName != c.wantFieldName {
			t.Errorf("WorkerDataFromNames(%q).FieldName = %q, want %q", c.name, w.FieldName, c.wantFieldName)
		}
		if w.VarName != c.wantVarName {
			t.Errorf("WorkerDataFromNames(%q).VarName = %q, want %q", c.name, w.VarName, c.wantVarName)
		}
		// Sanity: FieldName must NOT contain an underscore.
		if strings.Contains(w.FieldName, "_") {
			t.Errorf("WorkerDataFromNames(%q).FieldName = %q must not contain '_'", c.name, w.FieldName)
		}
	}
}

// TestOperatorDataFromNames_PascalCaseFieldName mirrors the worker
// regression test — operators share the snake_case → PascalCase rule.
func TestOperatorDataFromNames_PascalCaseFieldName(t *testing.T) {
	got, err := OperatorDataFromNames([]string{"cert_rotator"}, "")
	if err != nil {
		t.Fatalf("OperatorDataFromNames: %v", err)
	}
	if len(got) != 1 || got[0].FieldName != "CertRotator" {
		t.Errorf("OperatorDataFromNames(\"cert_rotator\")[0].FieldName = %q, want \"CertRotator\"", got[0].FieldName)
	}
}

// TestWorkerDataFromSpecs_HonorsExplicitPath locks in the path-
// honoring rule: when forge.yaml declares
// `path: workers/climatology_refresh`, the generated bootstrap import must
// be `"<module>/workers/climatology_refresh"` (matching the on-disk dir).
// Same rule applies to the Alias — it must equal the `package X`
// declaration in the dir's .go file so call sites like `<Alias>.New(...)`
// resolve correctly.
//
// Coverage:
//   - Explicit snake_case path → ImportPath + Package + Alias all
//     preserve the underscore.
//   - Empty path (legacy entry point) → falls back to
//     `naming.GoPackage(name)` which canonicalises to snake_case.
//   - On-disk `package X` declaration overrides the path-derived alias —
//     ground truth wins when the user renamed the package after scaffolding
//     (e.g. legacy `package widgetv2` from the pre-2026-06-08 compact-form
//     interlude).
func TestWorkerDataFromSpecs_HonorsExplicitPath(t *testing.T) {
	projectDir := t.TempDir()
	// Seed an on-disk worker dir with the snake_case package declaration
	// so the ground-truth alias detection has something to read.
	workerDir := filepath.Join(projectDir, "workers", "climatology_refresh")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	src := "package climatology_refresh\n\ntype Worker struct{}\n\nfunc New(Deps) *Worker { return &Worker{} }\n\ntype Deps struct{}\n"
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Mismatched on-disk dir for the "ground truth overrides path leaf" case:
	// path says workers/widget_v2 but the actual `package X` is `widgetv2`.
	mismatchDir := filepath.Join(projectDir, "workers", "widget_v2")
	if err := os.MkdirAll(mismatchDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mismatchSrc := "package widgetv2\n\ntype Worker struct{}\n"
	if err := os.WriteFile(filepath.Join(mismatchDir, "worker.go"), []byte(mismatchSrc), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cases := []struct {
		desc           string
		spec           WorkerSpec
		projectDir     string
		wantPackage    string
		wantImportPath string
		wantAlias      string
		wantFieldName  string
	}{
		{
			desc:           "explicit snake_case path preserves underscore for import + alias",
			spec:           WorkerSpec{Name: "climatology_refresh", Path: "workers/climatology_refresh"},
			projectDir:     projectDir,
			wantPackage:    "climatology_refresh",
			wantImportPath: "climatology_refresh",
			wantAlias:      "climatology_refresh",
			wantFieldName:  "ClimatologyRefresh",
		},
		{
			desc:           "empty path falls back to snake_case Go-style form",
			spec:           WorkerSpec{Name: "calibrator_refit"},
			projectDir:     "",
			wantPackage:    "calibrator_refit",
			wantImportPath: "calibrator_refit",
			wantAlias:      "calibrator_refit",
			wantFieldName:  "CalibratorRefit",
		},
		{
			desc:           "on-disk package declaration overrides path-derived alias",
			spec:           WorkerSpec{Name: "widget_v2", Path: "workers/widget_v2"},
			projectDir:     projectDir,
			wantPackage:    "widgetv2", // overridden by ground truth
			wantImportPath: "widget_v2",
			wantAlias:      "widgetv2", // overridden by ground truth
			wantFieldName:  "WidgetV2",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got, err := WorkerDataFromSpecs([]WorkerSpec{c.spec}, c.projectDir)
			if err != nil {
				t.Fatalf("WorkerDataFromSpecs: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("WorkerDataFromSpecs returned %d entries, want 1", len(got))
			}
			w := got[0]
			if w.Package != c.wantPackage {
				t.Errorf("Package = %q, want %q", w.Package, c.wantPackage)
			}
			if w.ImportPath != c.wantImportPath {
				t.Errorf("ImportPath = %q, want %q", w.ImportPath, c.wantImportPath)
			}
			if w.Alias != c.wantAlias {
				t.Errorf("Alias = %q, want %q", w.Alias, c.wantAlias)
			}
			if w.FieldName != c.wantFieldName {
				t.Errorf("FieldName = %q, want %q", w.FieldName, c.wantFieldName)
			}
		})
	}
}

// TestOperatorDataFromSpecs_HonorsExplicitPath mirrors the worker test —
// the path-honoring rule applies equally to operators.
func TestOperatorDataFromSpecs_HonorsExplicitPath(t *testing.T) {
	projectDir := t.TempDir()
	opDir := filepath.Join(projectDir, "operators", "cert_rotator")
	if err := os.MkdirAll(opDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	src := "package cert_rotator\n\ntype Controller struct{}\n"
	if err := os.WriteFile(filepath.Join(opDir, "controller.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got, err := OperatorDataFromSpecs([]OperatorSpec{
		{Name: "cert_rotator", Path: "operators/cert_rotator"},
	}, projectDir)
	if err != nil {
		t.Fatalf("OperatorDataFromSpecs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("OperatorDataFromSpecs returned %d entries, want 1", len(got))
	}
	op := got[0]
	if op.ImportPath != "cert_rotator" {
		t.Errorf("ImportPath = %q, want %q", op.ImportPath, "cert_rotator")
	}
	if op.Alias != "cert_rotator" {
		t.Errorf("Alias = %q, want %q", op.Alias, "cert_rotator")
	}
	if op.FieldName != "CertRotator" {
		t.Errorf("FieldName = %q, want %q", op.FieldName, "CertRotator")
	}
}

// TestGenerateMissingHandlerStubs_UnitTestSkipsCRUDMethods pins the
// per-RPC test owner-rule: handlers_crud_gen_test.go owns CRUD-method
// rows (shape-aware: AIP-158 Id/PageSize/update_mask literals), so the
// regen path of handlers_scaffold_test.go must NOT also emit a
// Test<CRUDMethod>_Generated row for the same method. Without this
// filter the user sees two scaffold tests per CRUD RPC, one in each
// file, and any future shape change has to be applied twice.
//
// Regression guard: the previous implementation passed `fullData` (every
// RPC) into unit_test.go.tmpl, producing the overlap that this test
// pins against.
func TestGenerateMissingHandlerStubs_UnitTestSkipsCRUDMethods(t *testing.T) {
	projectDir := t.TempDir()
	targetDir := filepath.Join(projectDir, "handlers", "patients")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed a placeholder handlers_scaffold_test.go so the regen branch
	// (isPlaceholderUnitTest → true) actually fires.
	placeholder := `// Code generated by forge. DO NOT EDIT.
package patients_test

// forge-unit-test-placeholder: this file is regenerated once RPCs are
// defined in the proto file. Run 'forge generate' after adding RPCs.
`
	if err := os.WriteFile(filepath.Join(targetDir, "handlers_scaffold_test.go"), []byte(placeholder), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed a placeholder integration_test.go and a minimal handlers.go so
	// scanExistingMethods doesn't trip.
	intPlaceholder := `// Code generated by forge. DO NOT EDIT.
package patients_test

// forge-integration-test-placeholder
`
	if err := os.WriteFile(filepath.Join(targetDir, "integration_test.go"), []byte(intPlaceholder), 0o644); err != nil {
		t.Fatal(err)
	}
	// Real handler dirs always carry a service.go; the disk-first
	// resolver reads the package clause from it (test files are skipped).
	if err := os.WriteFile(filepath.Join(targetDir, "service.go"), []byte("package patients\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "PatientsService",
		Package:    "patients.v1",
		GoPackage:  "example.com/test/gen/proto/services/patients/v1",
		PkgName:    "patientsv1",
		ModulePath: "example.com/test",
		ProtoFile:  "proto/services/patients/v1/patients.proto",
		Methods: []Method{
			{Name: "CreatePatient", InputType: "CreatePatientRequest", OutputType: "CreatePatientResponse"},
			{Name: "GetPatient", InputType: "GetPatientRequest", OutputType: "GetPatientResponse"},
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
		},
	}

	crudMethodNames := map[string]bool{
		"CreatePatient": true,
		"GetPatient":    true,
	}

	if _, err := GenerateMissingHandlerStubs(svc, projectDir, targetDir, crudMethodNames, nil); err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(targetDir, "handlers_scaffold_test.go"))
	if err != nil {
		t.Fatalf("read handlers_scaffold_test.go: %v", err)
	}
	got := string(content)

	// CRUD methods must NOT appear — handlers_crud_gen_test.go owns them.
	for _, crud := range []string{"TestCreatePatient_Generated", "TestGetPatient_Generated"} {
		if strings.Contains(got, crud) {
			t.Errorf("handlers_scaffold_test.go should not contain %s (CRUD methods are owned by handlers_crud_gen_test.go); got:\n%s", crud, got)
		}
	}
	// Non-CRUD method must still appear — unit_test.go.tmpl owns it.
	if !strings.Contains(got, "TestEcho_Generated") {
		t.Errorf("handlers_scaffold_test.go should contain TestEcho_Generated (non-CRUD method); got:\n%s", got)
	}
}

// TestUnitTestScaffold_SelfDestructingRows pins the scaffold-test contract:
// every generated row must be able to FAIL. The scaffold row asserts
// WantErr: connect.CodeUnimplemented against the stub handler, so it goes
// red the moment the handler is implemented — forcing the row to be
// rewritten with a real assertion. The permissive AnyOutcome knob is gone
// from pkg/tdd entirely; a test that cannot fail teaches green-means-nothing.
//
// The scaffold must also:
//   - emit Ctx: app.AuthedContext(t) so handlers that read claims via
//     middleware.GetUser see an authenticated context (review F4),
//   - replace the assertion-free WiresPermissiveAuthorizer shim with a
//     real authorizer-chain test (deny-all denies, default allows).
func TestUnitTestScaffold_SelfDestructingRows(t *testing.T) {
	data := ServiceTemplateData{
		ServiceName:         "EchoService",
		ServicePackage:      "echo",
		Module:              "example.com/test",
		ProtoPackage:        "services/echo",
		ProtoImportPath:     "services/echo",
		ProtoConnectPackage: "echov1connect",
		HandlerName:         "EchoService",
		TestHelperName:      "Echo",
		ServiceImportPath:   "echo",
		Methods: []MethodTemplateData{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
		},
	}
	content, err := templates.ServiceTemplates().Render("unit_test.go.tmpl", data)
	if err != nil {
		t.Fatalf("render unit_test.go.tmpl: %v", err)
	}
	got := string(content)

	if strings.Contains(got, "AnyOutcome") {
		t.Errorf("scaffold must not reference AnyOutcome (deleted from pkg/tdd — permissive rows belong in no library); got:\n%s", got)
	}
	if !strings.Contains(got, "WantErr: connect.CodeUnimplemented") {
		t.Errorf("scaffold row must self-destruct via WantErr: connect.CodeUnimplemented; got:\n%s", got)
	}
	if !strings.Contains(got, "Ctx:") || !strings.Contains(got, "app.AuthedContext(t)") {
		t.Errorf("scaffold row must emit Ctx: app.AuthedContext(t); got:\n%s", got)
	}
	if strings.Contains(got, "WiresPermissiveAuthorizer") {
		t.Errorf("assertion-free WiresPermissiveAuthorizer test must be gone; got:\n%s", got)
	}
	if !strings.Contains(got, "AuthorizerChain") || !strings.Contains(got, "denyAllAuthorizer") {
		t.Errorf("scaffold must contain the authorizer-chain test (deny-all denied, default allowed); got:\n%s", got)
	}
}
