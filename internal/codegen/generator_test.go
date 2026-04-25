package codegen

import (
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

	// Must contain new New() signature accepting Deps
	if !strings.Contains(content, `func New(deps Deps) *Service`) {
		t.Error("generated stub should have New(deps Deps) *Service signature")
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

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil); err != nil {
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

	// Must contain constructor calls with Deps
	if !strings.Contains(content, `api.New(api.Deps{`) {
		t.Error("bootstrap.go should construct api service with Deps")
	}
	if !strings.Contains(content, `orders.New(orders.Deps{`) {
		t.Error("bootstrap.go should construct orders service with Deps")
	}

	// Must contain generated file header
	if !strings.Contains(content, `Code generated by forge generate. DO NOT EDIT.`) {
		t.Error("bootstrap.go should contain generated file header")
	}

	// Must return (*App, error)
	if !strings.Contains(content, `func Bootstrap(mux *http.ServeMux, logger *slog.Logger, cfg *config.Config, opts ...connect.HandlerOption) (*App, error)`) {
		t.Error("bootstrap.go Bootstrap() should return (*App, error)")
	}

	// Must contain App struct
	if !strings.Contains(content, `type App struct`) {
		t.Error("bootstrap.go should contain App struct")
	}

	// Without packages, should not contain Packages struct
	if strings.Contains(content, `type Packages struct`) {
		t.Error("bootstrap.go without packages should not contain Packages struct")
	}
}

func TestGenerateBootstrap_WithPackages(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}

	packages := []BootstrapPackageData{
		{Name: "cache", Package: "cache", FieldName: "Cache"},
		{Name: "notifications", Package: "notifications", FieldName: "Notifications"},
	}

	if err := GenerateBootstrap(services, packages, nil, nil, "example.com/proj", false, false, targetDir, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
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

	// App struct must have Packages field
	if !strings.Contains(content, `Packages *Packages`) {
		t.Error("bootstrap.go App struct should contain Packages field")
	}
}

func TestGenerateBootstrapTesting_MultipleServices(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
		{Name: "OrdersService", ModulePath: "example.com/proj"},
	}

	if err := GenerateBootstrapTesting(services, nil, nil, nil, "example.com/proj", false, targetDir); err != nil {
		t.Fatalf("GenerateBootstrapTesting() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	// Must contain generated file header
	if !strings.Contains(content, `Code generated by forge generate. DO NOT EDIT.`) {
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

	// Must contain defaultTestConfig with discard logger
	if !strings.Contains(content, `io.Discard`) {
		t.Error("testing.go should use io.Discard for default logger")
	}
}

func TestGenerateBootstrapTesting_WithPackages(t *testing.T) {
	targetDir := t.TempDir()

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
	}

	packages := []BootstrapPackageData{
		{Name: "cache", Package: "cache", FieldName: "Cache"},
	}

	if err := GenerateBootstrapTesting(services, packages, nil, nil, "example.com/proj", false, targetDir); err != nil {
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

	result, err := GenerateMissingHandlerStubs(svc, targetDir, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}

	if result.AllUpToDate {
		t.Fatal("expected new methods to be generated, got AllUpToDate")
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
	if !strings.Contains(content, "Code generated by forge generate") {
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

	result, err := GenerateMissingHandlerStubs(svc, targetDir, nil)
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

	result, err := GenerateMissingHandlerStubs(svc, targetDir, nil)
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

	result, err := GenerateMissingHandlerStubs(svc, targetDir, nil)
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

	result, err := GenerateMissingHandlerStubs(svc, targetDir, nil)
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

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", false, false, targetDir, nil); err != nil {
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

	if err := GenerateBootstrap(services, nil, nil, nil, "example.com/proj", true, true, targetDir, nil); err != nil {
		t.Fatalf("GenerateBootstrap() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "bootstrap.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)

	if !strings.Contains(content, "DB") || !strings.Contains(content, "*sql.DB") {
		t.Error("bootstrap.go should include DB field when database is configured")
	}
	if !strings.Contains(content, `"database/sql"`) {
		t.Error("bootstrap.go should import database/sql when database is configured")
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