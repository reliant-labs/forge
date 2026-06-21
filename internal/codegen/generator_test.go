package codegen

import (
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

func TestGenerateBootstrapTesting_MultipleServices(t *testing.T) {
	targetDir := t.TempDir()

	// Scaffold real handler dirs whose Deps declare an Authorizer — the normal
	// forge service shape. The authz-aware test harness only emits the
	// AuthzInterceptor / permissive-default wiring for services that carry that
	// dep, so the assertions below need a service that actually has one.
	authedSvc := func(pkg string) string {
		return `package ` + pkg + `

import "log/slog"

type Authorizer interface{ Can(string) bool }

type Deps struct {
	Logger     *slog.Logger
	Authorizer Authorizer
}

type Service struct{ deps Deps }

func New(deps Deps) (*Service, error) { return &Service{deps: deps}, nil }
`
	}
	writeFileT(t, filepath.Join(targetDir, "internal", "handlers", "api", "service.go"), authedSvc("api"))
	writeFileT(t, filepath.Join(targetDir, "internal", "handlers", "orders", "service.go"), authedSvc("orders"))

	services := []ServiceDef{
		{Name: "APIService", ModulePath: "example.com/proj"},
		{Name: "OrdersService", ModulePath: "example.com/proj"},
	}

	if err := GenerateBootstrapTesting(BootstrapTestingGenInput{
		GenContext:         GenContext{ProjectDir: targetDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:           services,
		Packages:           nil,
		Workers:            nil,
		Operators:          nil,
		MultiTenantEnabled: false,
	}); err != nil {
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
	if !strings.Contains(content, `"example.com/proj/internal/handlers/api"`) {
		t.Error("testing.go should import api service package")
	}
	if !strings.Contains(content, `"example.com/proj/internal/handlers/orders"`) {
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

	if err := GenerateBootstrapTesting(BootstrapTestingGenInput{
		GenContext:         GenContext{ProjectDir: targetDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:           services,
		Packages:           packages,
		Workers:            nil,
		Operators:          nil,
		MultiTenantEnabled: false,
	}); err != nil {
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

// TestGenerateBootstrapTesting_ExternalComponentPackageExcluded is the
// FIX #3 regression: an `//forge:external-component` domain package that
// shares its package clause with a handler service (domain
// internal/billing + handler internal/handlers/billing, both `package
// billing`) must NOT get a NewTest<Pkg> factory, must NOT drive a
// Svc-prefix rename on the HANDLER service's factory, and must NOT
// duplicate-declare. testing.go must emit the plain NewTestBilling for the
// handler service and import the domain billing only if a stub needs it.
func TestGenerateBootstrapTesting_ExternalComponentPackageExcluded(t *testing.T) {
	targetDir := t.TempDir()

	// Handler service billing on disk (package billing).
	handlerDir := filepath.Join(targetDir, "internal", "handlers", "billing")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatalf("mkdir handler: %v", err)
	}
	handlerSrc := "package billing\n\ntype Deps struct{}\n\ntype Service struct{}\n\nfunc New(d Deps) (*Service, error) { return &Service{}, nil }\n"
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(handlerSrc), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}

	// External-component domain billing on disk (package billing) — has a
	// contract.go so discoverPackages would pick it up, marked external.
	domainDir := filepath.Join(targetDir, "internal", "billing")
	if err := os.MkdirAll(domainDir, 0o755); err != nil {
		t.Fatalf("mkdir domain: %v", err)
	}
	domainSrc := "//forge:external-component\npackage billing\n\ntype Service interface{ Charge() error }\n\ntype Deps struct{}\n\nfunc New(d Deps) Service { return nil }\n"
	if err := os.WriteFile(filepath.Join(domainDir, "contract.go"), []byte(domainSrc), 0o644); err != nil {
		t.Fatalf("write domain: %v", err)
	}

	// The domain billing is passed in Packages (as discoverPackages would
	// supply it) so the generator must filter it out itself.
	packages := []BootstrapPackageData{
		{Name: "billing", Package: "billing", ImportPath: "billing", FieldName: "Billing", VarName: "billing"},
	}
	if err := GenerateBootstrapTesting(BootstrapTestingGenInput{
		GenContext: GenContext{ProjectDir: targetDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:   []ServiceDef{{Name: "BillingService", ModulePath: "example.com/proj"}},
		Packages:   packages,
	}); err != nil {
		t.Fatalf("GenerateBootstrapTesting() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	// Handler service gets the PLAIN factory (no Svc prefix — the external
	// domain pkg doesn't count as a collision).
	if !strings.Contains(content, "func NewTestBilling(t *testing.T") {
		t.Errorf("testing.go should emit plain NewTestBilling for the handler service:\n%s", content)
	}
	// No factory for the external-component domain package.
	if strings.Contains(content, "func NewTestPkgBilling(") || strings.Contains(content, "func NewTestSvcBilling(") {
		t.Errorf("testing.go must not emit a factory keyed off the external-component domain pkg:\n%s", content)
	}
	// The external domain billing must not be imported as a package factory
	// (no With/Deps option for it).
	if strings.Contains(content, "WithPkgBillingDeps(") {
		t.Errorf("testing.go must not emit a Deps option for the external-component domain pkg:\n%s", content)
	}
}

// TestGenerateBootstrapTesting_MigratedDBOptIn pins the DB harness
// contract for projects with embedded migrations: the DEFAULT test DB is
// a bare (schema-less) real-postgres database, and a NewMigratedTestDB
// helper is emitted so tests opt in to the real schema via
// WithDB(NewMigratedTestDB(t)).
func TestGenerateBootstrapTesting_MigratedDBOptIn(t *testing.T) {
	projectDir := t.TempDir()

	// A service whose Deps carry a DB field (AnyServiceHasDB → true).
	handlerDir := filepath.Join(projectDir, "internal", "handlers", "api")
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
	if err := GenerateBootstrapTesting(BootstrapTestingGenInput{
		GenContext:         GenContext{ProjectDir: projectDir, ModulePath: "example.com/proj", Checksums: nil},
		Services:           services,
		Packages:           nil,
		Workers:            nil,
		Operators:          nil,
		MultiTenantEnabled: false,
	}); err != nil {
		t.Fatalf("GenerateBootstrapTesting() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `db:     testkit.NewPostgresDB(t)`) {
		t.Error("default test DB must be the BARE real-postgres DB (migrations are opt-in)")
	}
	if !strings.Contains(content, `func NewMigratedTestDB(t *testing.T) orm.Context`) {
		t.Error("testing.go should emit the NewMigratedTestDB opt-in helper when migrations exist")
	}
	if !strings.Contains(content, `testkit.NewMigratedPostgresDB(t, forgedb.MigrationsFS)`) {
		t.Error("NewMigratedTestDB should delegate to testkit.NewMigratedPostgresDB over forgedb.MigrationsFS")
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

// TestGenerateMissingHandlerStubs_HandwrittenImplInCrudFile reproduces
// kalshi fr-fba0c4be8d: a user hand-implements a non-CRUD RPC inside the
// user-owned handlers_crud.go (the scaffold header says it's their file).
// scanExistingMethods skips handlers_crud.go wholesale so its delegating
// CRUD shims don't suppress ops regen — but that also hid the hand impl,
// so GenerateMissingHandlerStubs re-emitted a DUPLICATE stub into
// handlers_gen.go and the package failed to compile. The fix scans
// handlers_crud.go for methods whose name is NOT a CRUD method (i.e. a
// hand impl) and treats those as already implemented.
func TestGenerateMissingHandlerStubs_HandwrittenImplInCrudFile(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "settlements")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}

	// handlers_crud.go: a forge-scaffolded shim that DELEGATES for the
	// CRUD-shaped GetSettlement, plus a HAND-WRITTEN custom-shape
	// ListSettlements (no entity behind it — not a CRUD method).
	crudFile := `package settlements

import (
	"context"
	"connectrpc.com/connect"
)

// GetSettlement is a generated CRUD shim that delegates to the ops layer.
func (s *Service) GetSettlement(
	ctx context.Context,
	req *connect.Request[any],
) (*connect.Response[any], error) {
	return s.getSettlementOp(ctx, req)
}

// ListSettlements is HAND-WRITTEN: a custom read shape with no entity.
func (s *Service) ListSettlements(
	ctx context.Context,
	req *connect.Request[any],
) (*connect.Response[any], error) {
	// custom query, hand-rolled
	return nil, nil
}
`
	if err := os.WriteFile(filepath.Join(targetDir, "handlers_crud.go"), []byte(crudFile), 0644); err != nil {
		t.Fatal(err)
	}

	svc := ServiceDef{
		Name:       "SettlementsService",
		Package:    "settlements.v1",
		GoPackage:  "github.com/test/proj/gen/proto/services/settlements/v1",
		PkgName:    "settlementsv1",
		ProtoFile:  "proto/services/settlements/v1/settlements.proto",
		ModulePath: "github.com/test/proj",
		Methods: []Method{
			{Name: "GetSettlement", InputType: "GetSettlementRequest", OutputType: "GetSettlementResponse"},
			{Name: "ListSettlements", InputType: "ListSettlementsRequest", OutputType: "ListSettlementsResponse"},
			{Name: "GetTradeable", InputType: "GetTradeableRequest", OutputType: "GetTradeableResponse"},
		},
	}

	// GetSettlement is a CRUD method (owned by CRUD gen); ListSettlements is
	// NOT — it's the hand impl. GetTradeable is a genuinely-missing RPC.
	crudMethodNames := map[string]bool{"GetSettlement": true}

	result, err := GenerateMissingHandlerStubs(svc, t.TempDir(), targetDir, crudMethodNames, nil)
	if err != nil {
		t.Fatalf("GenerateMissingHandlerStubs() error = %v", err)
	}

	// Only GetTradeable should be stubbed — NOT ListSettlements (hand impl)
	// and NOT GetSettlement (CRUD-owned).
	if len(result.NewMethods) != 1 || result.NewMethods[0] != "GetTradeable" {
		t.Fatalf("expected only GetTradeable stubbed, got %v", result.NewMethods)
	}

	data, err := os.ReadFile(filepath.Join(targetDir, "handlers_gen.go"))
	if err != nil {
		t.Fatalf("ReadFile(handlers_gen.go) error = %v", err)
	}
	content := string(data)
	if strings.Contains(content, "func (s *Service) ListSettlements(") {
		t.Error("handlers_gen.go must NOT re-stub the hand-written ListSettlements (duplicate method → compile error)")
	}
	if strings.Contains(content, "func (s *Service) GetSettlement(") {
		t.Error("handlers_gen.go must NOT stub the CRUD-owned GetSettlement")
	}
	if !strings.Contains(content, "func (s *Service) GetTradeable(") {
		t.Error("handlers_gen.go should stub the genuinely-missing GetTradeable")
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
	if !strings.Contains(content, "yours: scaffolded once, never touched again — forge will not overwrite this file") {
		t.Error("setup.go should carry the canonical Tier-2 'yours:' banner")
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
	if !strings.Contains(content, "yours: scaffolded once, never touched again — forge will not overwrite this file") {
		t.Error("post_bootstrap.go should carry the canonical Tier-2 'yours:' banner")
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
	targetDir := filepath.Join(projectDir, "internal", "handlers", "billing")

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
	noCollisionDir := filepath.Join(projectDir, "internal", "handlers", "echo")
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

	// An external-component domain dir must NOT trigger the Svc prefix: it
	// is not a forge-wired component, so the handler service keeps its plain
	// factory name (FIX #3). internal/user/ carries the directive.
	if err := os.MkdirAll(filepath.Join(projectDir, "internal", "user"), 0755); err != nil {
		t.Fatalf("setup user: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "internal", "user", "contract.go"),
		[]byte("//forge:external-component\npackage user\n"), 0644); err != nil {
		t.Fatalf("setup user contract: %v", err)
	}

	cases := []struct {
		pkg, project, want string
	}{
		{"billing", projectDir, "SvcBilling"}, // plain internal/billing dir -> collision
		{"users", projectDir, "Users"},
		{"admin_server", projectDir, "AdminServer"},
		{"billing", "", "Billing"},   // no project context -> no-collision form
		{"user", projectDir, "User"}, // external-component domain dir -> NOT a collision
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
	workerDir := filepath.Join(projectDir, "internal", "workers", "climatology_refresh")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	src := "package climatology_refresh\n\ntype Worker struct{}\n\nfunc New(Deps) *Worker { return &Worker{} }\n\ntype Deps struct{}\n"
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Mismatched on-disk dir for the "ground truth overrides path leaf" case:
	// path says workers/widget_v2 but the actual `package X` is `widgetv2`.
	mismatchDir := filepath.Join(projectDir, "internal", "workers", "widget_v2")
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
	opDir := filepath.Join(projectDir, "internal", "operators", "cert_rotator")
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
	targetDir := filepath.Join(projectDir, "internal", "handlers", "patients")
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
