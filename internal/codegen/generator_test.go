package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/naming"
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

	// Must contain constructor calls. wire_gen owns the Deps literal
	// now; bootstrap calls wireXxxDeps then passes the result into
	// xxx.New (2026-05-07 wire-gen migration).
	if !strings.Contains(content, `api.New(apiDeps)`) {
		t.Error("bootstrap.go should construct api service with wire_gen-built Deps")
	}
	if !strings.Contains(content, `orders.New(ordersDeps)`) {
		t.Error("bootstrap.go should construct orders service with wire_gen-built Deps")
	}
	if !strings.Contains(content, `wireAPIDeps(app, cfg, logger`) {
		t.Error("bootstrap.go should call wireAPIDeps(app, cfg, logger, devMode)")
	}
	if !strings.Contains(content, `wireOrdersDeps(app, cfg, logger`) {
		t.Error("bootstrap.go should call wireOrdersDeps(app, cfg, logger, devMode)")
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
	if strings.Contains(content, "app.RESTHandler =") {
		t.Error("bootstrap.go should NOT assign app.RESTHandler when api.rest is off")
	}
}

// TestGenerateBootstrap_RESTEnabled_WrapsMux verifies that a project
// with `api.rest: true` in forge.yaml regenerates bootstrap.go with:
//   - a `connectrpc.com/vanguard` import,
//   - a `vanguard.NewTranscoder(...)` call inside both Bootstrap and
//     BootstrapOnly, populated with one entry per service keyed by the
//     Connect-generated `<X>ServiceName` constant, and
//   - the wrapped handler stored on `app.RESTHandler`.
//
// The generated app_gen.go grows a `RESTHandler http.Handler` field;
// this test confirms the field is always emitted so cmd-server.go can
// read it unconditionally without a templated branch.
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

	if !strings.Contains(bContent, `"connectrpc.com/vanguard"`) {
		t.Error("bootstrap.go should import connectrpc.com/vanguard when api.rest is on")
	}
	if !strings.Contains(bContent, "vanguard.NewTranscoder(vanguardSvcs)") {
		t.Errorf("bootstrap.go should call vanguard.NewTranscoder; got:\n%s", bContent)
	}
	// Per-service NewService call with the connect ServiceName constant.
	if !strings.Contains(bContent, "apiv1connect.APIServiceName") {
		t.Error("bootstrap.go should reference apiv1connect.APIServiceName for APIService")
	}
	if !strings.Contains(bContent, "ordersv1connect.OrdersServiceName") {
		t.Error("bootstrap.go should reference ordersv1connect.OrdersServiceName for OrdersService")
	}
	if !strings.Contains(bContent, "app.RESTHandler = transcoder") {
		t.Error("bootstrap.go should assign the transcoder to app.RESTHandler")
	}
	// Both Bootstrap and BootstrapOnly should wrap — count occurrences.
	if got := strings.Count(bContent, "vanguard.NewTranscoder"); got != 2 {
		t.Errorf("bootstrap.go should call vanguard.NewTranscoder twice (Bootstrap + BootstrapOnly); got %d", got)
	}
	// Connect imports should appear in the import block.
	if !strings.Contains(bContent, `"example.com/proj/gen/services/api/v1/apiv1connect"`) {
		t.Errorf("bootstrap.go should import the apiv1connect package; got:\n%s", bContent)
	}

	appGen, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "app_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(app_gen) error = %v", err)
	}
	if !strings.Contains(string(appGen), "RESTHandler http.Handler") {
		t.Error("app_gen.go should declare RESTHandler http.Handler on App")
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

	// Snake-case package names match codegen.toServicePackage output.
	webhookServices := map[string]bool{
		"admin_server": true,
	}

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil, webhookServices, BootstrapFeatures{}, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	// Auto-wired RegisterWebhookRoutes for the webhook-bearing service.
	// (2026-05-07 wire-gen migration: services now hang directly off
	// `app.Services.<Field>` instead of a local `svcs` var.)
	if !strings.Contains(content, "app.Services.AdminServer.RegisterWebhookRoutes(mux, middleware.HTTPStack(logger))") {
		t.Errorf("bootstrap.go should auto-wire RegisterWebhookRoutes for admin_server (has webhooks); got:\n%s", content)
	}

	// No auto-wire for the service without webhooks.
	if strings.Contains(content, "app.Services.Orders.RegisterWebhookRoutes(") {
		t.Errorf("bootstrap.go should NOT auto-wire RegisterWebhookRoutes for orders (no webhooks)")
	}

	// Both services still get RegisterHTTP — the auto-wire is additive.
	if !strings.Contains(content, "app.Services.AdminServer.RegisterHTTP(mux, middleware.HTTPStack(logger))") {
		t.Errorf("bootstrap.go should still call RegisterHTTP for admin_server")
	}
	if !strings.Contains(content, "app.Services.Orders.RegisterHTTP(mux, middleware.HTTPStack(logger))") {
		t.Errorf("bootstrap.go should still call RegisterHTTP for orders")
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

	// Must contain per-service dep override options
	if !strings.Contains(content, `func WithAPIDeps(deps api.Deps) TestOption`) {
		t.Error("testing.go should contain WithAPIDeps option")
	}
	if !strings.Contains(content, `func WithOrdersDeps(deps orders.Deps) TestOption`) {
		t.Error("testing.go should contain WithOrdersDeps option")
	}

	// Must contain NewTestXxx functions
	if !strings.Contains(content, `func NewTestAPI(t *testing.T, opts ...TestOption) *api.Service`) {
		t.Error("testing.go should contain NewTestAPI function")
	}
	if !strings.Contains(content, `func NewTestOrders(t *testing.T, opts ...TestOption) *orders.Service`) {
		t.Error("testing.go should contain NewTestOrders function")
	}

	// Must contain NewTestXxxServer functions
	if !strings.Contains(content, `func NewTestAPIServer(t *testing.T, opts ...TestOption)`) {
		t.Error("testing.go should contain NewTestAPIServer function")
	}
	if !strings.Contains(content, `func NewTestOrdersServer(t *testing.T, opts ...TestOption)`) {
		t.Error("testing.go should contain NewTestOrdersServer function")
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

	// Must contain WithCacheDeps option
	if !strings.Contains(content, `func WithCacheDeps(deps cache.Deps) TestOption`) {
		t.Error("testing.go should contain WithCacheDeps option")
	}

	// Must contain NewTestCache function returning interface type
	if !strings.Contains(content, `func NewTestCache(t *testing.T, opts ...TestOption) cache.Service`) {
		t.Error("testing.go should contain NewTestCache function")
	}
}

func TestPackageDataFromNames(t *testing.T) {
	pkgs := PackageDataFromNames([]string{"cache", "db", "notifications"}, t.TempDir())

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
	pkgs := PackageDataFromNames([]string{"mcp/database", "cache"}, t.TempDir())
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

func TestToServicePackage(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"EchoService", "echo"},
		{"OrdersService", "orders"},
		{"Service", "service"},
		{"notifications", "notifications"},
	}

	for _, tt := range tests {
		got := toServicePackage(tt.input)
		if got != tt.want {
			t.Errorf("toServicePackage(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
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
func TestWorkerDataFromNames_PascalCaseFieldName(t *testing.T) {
	cases := []struct {
		name              string
		wantPackage       string
		wantFieldName     string
		wantVarName       string
	}{
		// Snake-case → PascalCase via ToPascalCase. The directory + Go
		// package stay snake_case so they remain filesystem-friendly.
		{"calibrator_refit", "calibrator_refit", "CalibratorRefit", "calibratorRefit"},
		// Hyphenated → underscored Package; PascalCase FieldName.
		{"email-sender", "email_sender", "EmailSender", "emailSender"},
		// Single-word stays as-is (just upper-cased first letter).
		{"refresh", "refresh", "Refresh", "refresh"},
		// Initialism — ToPascalCase recognizes API and uppercases it.
		{"api_poll", "api_poll", "APIPoll", "aPIPoll"},
	}
	for _, c := range cases {
		got := WorkerDataFromNames([]string{c.name}, "")
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
	got := OperatorDataFromNames([]string{"cert_rotator"}, "")
	if len(got) != 1 || got[0].FieldName != "CertRotator" {
		t.Errorf("OperatorDataFromNames(\"cert_rotator\")[0].FieldName = %q, want \"CertRotator\"", got[0].FieldName)
	}
}
