package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// MethodTemplateData holds per-method data for the embedded service templates.
type MethodTemplateData struct {
	Name           string // RPC method name, e.g. "GetItem"
	InputType      string // proto input message name, e.g. "GetItemRequest"
	OutputType     string // proto output message name, e.g. "GetItemResponse"
	ClientStreaming bool   // true if the client streams requests
	ServerStreaming bool   // true if the server streams responses
	AuthRequired   bool   // true if method_options.auth_required is set
}

// ServiceTemplateData holds the data shape expected by the embedded service templates.
type ServiceTemplateData struct {
	ServiceName        string               // e.g. "EchoService"
	Module             string               // e.g. "github.com/demo-project"
	ProtoImportPath    string               // e.g. "proto/services/echo" (without /v1)
	ProtoPackage       string               // same as ProtoImportPath for handlers.go.tmpl
	ProtoConnectPackage string              // e.g. "echov1connect"
	HandlerName        string               // e.g. "EchoService"
	ProtoFileSymbol    string               // e.g. "File_services_echo_v1_echo_proto"
	Methods            []MethodTemplateData // method data for handlers.go.tmpl and test templates
}

// mapServiceDefToTemplateData converts a ServiceDef to the data shape expected by embedded templates.
func mapServiceDefToTemplateData(svc ServiceDef) ServiceTemplateData {
	// GoPackage is like "github.com/project/gen/proto/services/echo/v1"
	// We need ProtoImportPath = "proto/services/echo" (relative, no /v1)
	protoImportPath := ""
	if svc.ModulePath != "" && svc.GoPackage != "" {
		// Strip module + "/gen/" prefix and "/v1" suffix
		prefix := svc.ModulePath + "/gen/"
		if strings.HasPrefix(svc.GoPackage, prefix) {
			protoImportPath = strings.TrimPrefix(svc.GoPackage, prefix)
			// Remove trailing /v1, /v2, etc.
			if idx := strings.LastIndex(protoImportPath, "/v"); idx >= 0 {
				protoImportPath = protoImportPath[:idx]
			}
		}
	}

	connectPkg := svc.PkgName + "connect"

	// Build proto file symbol: File_services_echo_v1_echo_proto
	// The buf module root is "proto/", so protoc sees paths relative to that.
	// Strip the leading "proto/" from the filesystem path before building the symbol.
	relProtoFile := strings.TrimPrefix(svc.ProtoFile, "proto/")
	protoFileSymbol := "File_" + strings.ReplaceAll(
		strings.ReplaceAll(relProtoFile, "/", "_"),
		".", "_",
	)

	var methods []MethodTemplateData
	for _, m := range svc.Methods {
		methods = append(methods, MethodTemplateData{
			Name:           m.Name,
			InputType:      m.InputType,
			OutputType:     m.OutputType,
			ClientStreaming: m.ClientStreaming,
			ServerStreaming: m.ServerStreaming,
			AuthRequired:   m.AuthRequired,
		})
	}

	pkgName := toServicePackage(svc.Name)

	return ServiceTemplateData{
		ServiceName:        pkgName,
		Module:             svc.ModulePath,
		ProtoImportPath:    protoImportPath,
		ProtoPackage:       protoImportPath,
		ProtoConnectPackage: connectPkg,
		HandlerName:        svc.Name,
		ProtoFileSymbol:    protoFileSymbol,
		Methods:            methods,
	}
}

// GenerateServiceStub generates service.go and handlers.go for a new service
// using the embedded FS templates.
func GenerateServiceStub(svc ServiceDef, targetDir string) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	data := mapServiceDefToTemplateData(svc)

	// Render service.go from embedded template
	serviceContent, err := templates.RenderServiceTemplate("service/service.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render service.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "service.go"), serviceContent, 0644); err != nil {
		return err
	}

	// Render handlers.go from embedded template only when there are real methods
	// to implement. With zero methods, handlers.go would just be a placeholder
	// comment; skip it and let the user (or subsequent forge generate runs) create
	// it with actual content.
	if len(data.Methods) > 0 {
		handlersContent, err := templates.RenderServiceTemplate("service/handlers.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render handlers.go.tmpl: %w", err)
		}
		if err := os.WriteFile(filepath.Join(targetDir, "handlers.go"), handlersContent, 0644); err != nil {
			return err
		}
	}

	// Render handlers_test.go from embedded template
	unitTestContent, err := templates.RenderServiceTemplate("service/unit_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render unit_test.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "handlers_test.go"), unitTestContent, 0644); err != nil {
		return err
	}

	// Render integration_test.go from embedded template
	integrationTestContent, err := templates.RenderServiceTemplate("service/integration_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render integration_test.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "integration_test.go"), integrationTestContent, 0644); err != nil {
		return err
	}

	// Render authorizer.go from embedded template
	authzData := struct {
		Package     string
		ServiceName string
		Module      string
	}{
		Package:     data.ServiceName,
		ServiceName: data.HandlerName,
		Module:      data.Module,
	}
	authzContent, err := templates.RenderServiceTemplate("service/authorizer.go.tmpl", authzData)
	if err != nil {
		return fmt.Errorf("render authorizer.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "authorizer.go"), authzContent, 0644); err != nil {
		return err
	}

	return nil
}

// RegenerateServiceFile regenerates only service.go for an existing service
// directory, using the proto-derived HandlerName so that Connect RPC references
// (Unimplemented*Handler, New*Handler) match the actual proto service name.
func RegenerateServiceFile(svc ServiceDef, targetDir string) error {
	data := mapServiceDefToTemplateData(svc)

	serviceContent, err := templates.RenderServiceTemplate("service/service.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render service.go.tmpl: %w", err)
	}
	return os.WriteFile(filepath.Join(targetDir, "service.go"), serviceContent, 0644)
}

// GenerateMock generates a mock file for a service.
// Services with zero RPCs are skipped — there is nothing to mock.
// Returns (true, nil) if a file was written, (false, nil) if skipped.
func GenerateMock(svc ServiceDef, mockDir string) (written bool, err error) {
	if len(svc.Methods) == 0 {
		return false, nil
	}

	// Create mocks directory
	if err := os.MkdirAll(mockDir, 0755); err != nil {
		return false, err
	}

	// Prepare template data
	data := prepareServiceData(svc)

	// Parse and execute template
	tmpl, err := template.New("mock").Parse(mockTemplate)
	if err != nil {
		return false, err
	}

	mockFile := filepath.Join(mockDir, toServicePackage(svc.Name)+"_mock.go")
	f, err := os.Create(mockFile)
	if err != nil {
		return false, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	if err := tmpl.Execute(f, data); err != nil {
		return false, err
	}
	return true, nil
}

// BootstrapServiceData holds data for a single service in the bootstrap template.
type BootstrapServiceData struct {
	Name      string // e.g. "api"
	Package   string // e.g. "api" (Go package name)
	FieldName string // e.g. "API" (exported struct field)
	Fallible  bool   // true if New() returns (T, error)
}

// BootstrapPackageData holds data for a single internal package in the bootstrap template.
type BootstrapPackageData struct {
	Name      string // e.g. "cache"
	Package   string // e.g. "cache" (Go package name)
	FieldName string // e.g. "Cache" (exported struct field)
	Fallible  bool   // true if New() returns (T, error)
}

// BootstrapWorkerData holds data for a single worker in the bootstrap template.
type BootstrapWorkerData struct {
	Name      string // e.g. "email_sender"
	Package   string // e.g. "email_sender" (Go package name)
	FieldName string // e.g. "EmailSender" (exported struct field)
	Fallible  bool   // true if New() returns (T, error)
}

// WorkerDataFromNames builds BootstrapWorkerData from worker names (e.g. from forge.project.yaml).
// projectDir is the root project directory; if non-empty, it is used to detect fallible constructors.
func WorkerDataFromNames(names []string, projectDir string) []BootstrapWorkerData {
	var workers []BootstrapWorkerData
	for _, name := range names {
		fallible := false
		if projectDir != "" {
			fallible, _ = DetectFallibleConstructor(filepath.Join(projectDir, "workers", name))
		}
		workers = append(workers, BootstrapWorkerData{
			Name:      name,
			Package:   name,
			FieldName: naming.ToExportedFieldName(name),
			Fallible:  fallible,
		})
	}
	return workers
}

// BootstrapOperatorData holds data for a single operator in the bootstrap template.
type BootstrapOperatorData struct {
	Name      string // e.g. "workspace"
	Package   string // e.g. "workspace" (Go package name)
	FieldName string // e.g. "Workspace" (exported struct field)
	Fallible  bool   // true if New() returns (T, error)
}

// OperatorDataFromNames builds BootstrapOperatorData from operator names (e.g. from forge.project.yaml).
// projectDir is the root project directory; if non-empty, it is used to detect fallible constructors.
func OperatorDataFromNames(names []string, projectDir string) []BootstrapOperatorData {
	var operators []BootstrapOperatorData
	for _, name := range names {
		fallible := false
		if projectDir != "" {
			fallible, _ = DetectFallibleConstructor(filepath.Join(projectDir, "operators", name))
		}
		operators = append(operators, BootstrapOperatorData{
			Name:      name,
			Package:   name,
			FieldName: naming.ToExportedFieldName(name),
			Fallible:  fallible,
		})
	}
	return operators
}

// GenerateBootstrap generates pkg/app/bootstrap.go from the bootstrap.go.tmpl template.
//
// hasDatabase gates the DB field + setupDatabase wiring; ormEnabled gates the
// ORM field + generated ORM client construction. The two are separate
// concerns: a project may configure a DB driver (for migrations, raw SQL,
// sqlc) without opting into the generated forge ORM. The ORM field is
// dropped when no proto/db/ entity definitions exist so `App.ORM` can never
// be silently nil in user code.
func GenerateBootstrap(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, hasDatabase bool, ormEnabled bool, projectDir string) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	var bootstrapSvcs []BootstrapServiceData
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		fallible, _ := DetectFallibleConstructor(filepath.Join(projectDir, "handlers", pkg))
		bootstrapSvcs = append(bootstrapSvcs, BootstrapServiceData{
			Name:      pkg,
			Package:   pkg,
			FieldName: naming.ToExportedFieldName(pkg),
			Fallible:  fallible,
		})
	}

	hasFallible := hasFallibleConstructor(bootstrapSvcs, packages, workers, operators)

	data := struct {
		Module      string
		Services    []BootstrapServiceData
		Packages    []BootstrapPackageData
		Workers     []BootstrapWorkerData
		Operators   []BootstrapOperatorData
		HasDatabase bool
		OrmEnabled  bool
		HasFallible bool
	}{
		Module:      modulePath,
		Services:    bootstrapSvcs,
		Packages:    packages,
		Workers:     workers,
		Operators:   operators,
		HasDatabase: hasDatabase,
		OrmEnabled:  ormEnabled,
		HasFallible: hasFallible,
	}

	content, err := templates.RenderProjectTemplate("bootstrap.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap.go.tmpl: %w", err)
	}

	outPath := filepath.Join(appDir, "bootstrap.go")
	return os.WriteFile(outPath, content, 0644)
}

// prepareServiceData prepares template data for service generation
func prepareServiceData(svc ServiceDef) map[string]interface{} {
	var methods []map[string]interface{}
	needsEmptypb := false
	needsPb := false

	for _, method := range svc.Methods {
		if method.IsInputEmpty() || method.IsOutputEmpty() {
			needsEmptypb = true
		}
		if !method.IsInputEmpty() || !method.IsOutputEmpty() {
			needsPb = true
		}
		methods = append(methods, map[string]interface{}{
			"Name":       method.Name,
			"Signature":  buildMethodSignature(method),
			"ReturnType": buildReturnType(method),
			"ReturnStub": buildReturnStub(method),
			"CallArgs":   buildCallArgs(method),
		})
	}

	return map[string]interface{}{
		"ServiceName":    svc.Name,
		"ServicePackage": toServicePackage(svc.Name),
		"GoPackage":      svc.GoPackage,
		"PkgName":        svc.PkgName,
		"ModulePath":     svc.ModulePath,
		"Methods":        methods,
		"HasMethods":     len(svc.Methods) > 0,
		"NeedsEmptypb":   needsEmptypb,
		"NeedsPb":        needsPb,
	}
}

func buildMethodSignature(m Method) string {
	switch {
	case m.ClientStreaming && m.ServerStreaming:
		// Bidirectional streaming
		return fmt.Sprintf("ctx context.Context, stream *connect.BidiStream[%s, %s]", m.GoInputType(), m.GoOutputType())
	case m.ClientStreaming:
		// Client streaming
		return fmt.Sprintf("ctx context.Context, stream *connect.ClientStream[%s]", m.GoInputType())
	case m.ServerStreaming:
		// Server streaming
		return fmt.Sprintf("ctx context.Context, req *connect.Request[%s], stream *connect.ServerStream[%s]", m.GoInputType(), m.GoOutputType())
	default:
		// Unary
		return fmt.Sprintf("ctx context.Context, req *connect.Request[%s]", m.GoInputType())
	}
}

func buildReturnType(m Method) string {
	switch {
	case m.ClientStreaming && m.ServerStreaming:
		return "error"
	case m.ClientStreaming:
		return fmt.Sprintf("(*connect.Response[%s], error)", m.GoOutputType())
	case m.ServerStreaming:
		return "error"
	default:
		return fmt.Sprintf("(*connect.Response[%s], error)", m.GoOutputType())
	}
}

func buildReturnStub(m Method) string {
	// Go convention: error strings should not start with a capital letter and
	// should not end with punctuation. RPC method names are pascal-cased, so
	// we format them as a value rather than using them as the first word.
	errExpr := fmt.Sprintf("connect.NewError(connect.CodeUnimplemented, fmt.Errorf(\"handler for %%s not yet implemented\", %q))", m.Name)
	switch {
	case m.ClientStreaming && m.ServerStreaming, m.ServerStreaming:
		return errExpr
	default:
		return "nil, " + errExpr
	}
}

func buildCallArgs(m Method) string {
	switch {
	case m.ClientStreaming && m.ServerStreaming:
		return "ctx, stream"
	case m.ClientStreaming:
		return "ctx, stream"
	case m.ServerStreaming:
		return "ctx, req, stream"
	default:
		return "ctx, req"
	}
}

// BootstrapTestServiceData holds data for a single service in the bootstrap testing template.
type BootstrapTestServiceData struct {
	Name             string // e.g. "api"
	Package          string // e.g. "api" (Go package name)
	FieldName        string // e.g. "API" (exported struct field)
	ProtoServiceName string // e.g. "ApiService" (proto service name for connect client)
	Fallible         bool   // true if New() returns (T, error)
}

// GenerateBootstrapTesting generates pkg/app/testing.go from the bootstrap_testing.go.tmpl template.
func GenerateBootstrapTesting(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, multiTenantEnabled bool, projectDir string) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	var testSvcs []BootstrapTestServiceData
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		fallible, _ := DetectFallibleConstructor(filepath.Join(projectDir, "handlers", pkg))
		testSvcs = append(testSvcs, BootstrapTestServiceData{
			Name:             pkg,
			Package:          pkg,
			FieldName:        naming.ToExportedFieldName(pkg),
			ProtoServiceName: svc.Name,
			Fallible:         fallible,
		})
	}

	data := struct {
		Module             string
		Services           []BootstrapTestServiceData
		Packages           []BootstrapPackageData
		MultiTenantEnabled bool
	}{
		Module:             modulePath,
		Services:           testSvcs,
		Packages:           packages,
		MultiTenantEnabled: multiTenantEnabled,
	}

	content, err := templates.RenderProjectTemplate("bootstrap_testing.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap_testing.go.tmpl: %w", err)
	}

	outPath := filepath.Join(appDir, "testing.go")
	return os.WriteFile(outPath, content, 0644)
}

// GenerateMigrate writes pkg/app/migrate.go with embedded migration support.
// When hasMigrations is true, the generated file includes go:embed directives
// and golang-migrate logic. When false, AutoMigrate is a no-op stub so that
// cmd/server.go always compiles.
func GenerateMigrate(targetDir string, modulePath string, hasMigrations bool) error {
	appDir := filepath.Join(targetDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	data := struct {
		HasMigrations bool
		ModulePath    string
	}{
		HasMigrations: hasMigrations,
		ModulePath:    modulePath,
	}

	content, err := templates.RenderProjectTemplate("migrate.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render migrate.go.tmpl: %w", err)
	}

	outPath := filepath.Join(appDir, "migrate.go")
	if err := os.WriteFile(outPath, content, 0644); err != nil {
		return err
	}

	// Generate db/embed.go with the go:embed directive (must be in the db/ dir)
	if hasMigrations {
		dbDir := filepath.Join(targetDir, "db")
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return err
		}
		embedContent := []byte(`// Code generated by forge generate. DO NOT EDIT.
package db

import "embed"

//go:embed migrations/*.sql
var MigrationsFS embed.FS
`)
		embedPath := filepath.Join(dbDir, "embed.go")
		if err := os.WriteFile(embedPath, embedContent, 0644); err != nil {
			return err
		}
	}

	return nil
}

// PackageDataFromNames builds BootstrapPackageData from package names (e.g. from forge.project.yaml).
// projectDir is the root project directory; if non-empty, it is used to detect fallible constructors
// by inspecting the Go source in internal/<name>/.
func PackageDataFromNames(names []string, projectDir string) []BootstrapPackageData {
	var pkgs []BootstrapPackageData
	for _, name := range names {
		fallible := false
		if projectDir != "" {
			fallible, _ = DetectFallibleConstructor(filepath.Join(projectDir, "internal", name))
		}
		pkgs = append(pkgs, BootstrapPackageData{
			Name:      name,
			Package:   name,
			FieldName: naming.ToExportedFieldName(name),
			Fallible:  fallible,
		})
	}
	return pkgs
}

// MissingHandlerResult holds the result of scanning for missing handler stubs.
type MissingHandlerResult struct {
	NewMethods  []string // names of methods that were generated
	AllUpToDate bool     // true if no new methods were needed
}

// GenerateMissingHandlerStubs scans the existing service directory for implemented
// methods on *Service, compares against the proto ServiceDef, and generates stubs
// only for missing methods into handlers_gen.go.
// If all methods are already implemented, it returns AllUpToDate=true.
// If handlers_gen.go already exists, it is overwritten (it's generated code).
func GenerateMissingHandlerStubs(svc ServiceDef, targetDir string) (*MissingHandlerResult, error) {
	existing, err := scanExistingMethods(targetDir, false)
	if err != nil {
		return nil, fmt.Errorf("scan existing methods: %w", err)
	}

	var missing []Method
	for _, m := range svc.Methods {
		if !existing[m.Name] {
			missing = append(missing, m)
		}
	}

	handlersGenPath := filepath.Join(targetDir, "handlers_gen.go")
	if len(missing) == 0 {
		if err := os.Remove(handlersGenPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale handlers_gen.go: %w", err)
		}
		return &MissingHandlerResult{AllUpToDate: true}, nil
	}

	// Build a ServiceDef with only the missing methods for template rendering
	missingSvc := svc
	missingSvc.Methods = missing
	data := mapServiceDefToTemplateData(missingSvc)

	content, err := templates.RenderServiceTemplate("service/handlers_gen.go.tmpl", data)
	if err != nil {
		return nil, fmt.Errorf("render handlers_gen.go.tmpl: %w", err)
	}

	if err := os.WriteFile(handlersGenPath, content, 0644); err != nil {
		return nil, err
	}

	// If integration_test.go / handlers_test.go are still placeholders (no RPCs when
	// first generated), regenerate them with actual test scaffolding now that RPCs exist.
	fullData := mapServiceDefToTemplateData(svc)

	integrationTestPath := filepath.Join(targetDir, "integration_test.go")
	if isPlaceholderIntegrationTest(integrationTestPath) {
		testContent, err := templates.RenderServiceTemplate("service/integration_test.go.tmpl", fullData)
		if err != nil {
			return nil, fmt.Errorf("render integration_test.go.tmpl: %w", err)
		}
		if err := os.WriteFile(integrationTestPath, testContent, 0644); err != nil {
			return nil, fmt.Errorf("write integration_test.go: %w", err)
		}
	}

	handlersTestPath := filepath.Join(targetDir, "handlers_test.go")
	if isPlaceholderUnitTest(handlersTestPath) {
		testContent, err := templates.RenderServiceTemplate("service/unit_test.go.tmpl", fullData)
		if err != nil {
			return nil, fmt.Errorf("render unit_test.go.tmpl: %w", err)
		}
		if err := os.WriteFile(handlersTestPath, testContent, 0644); err != nil {
			return nil, fmt.Errorf("write handlers_test.go: %w", err)
		}
	}

	var names []string
	for _, m := range missing {
		names = append(names, m.Name)
	}

	return &MissingHandlerResult{NewMethods: names}, nil
}

// isPlaceholderIntegrationTest checks if the integration test file is still
// the auto-generated placeholder with no real tests.
func isPlaceholderIntegrationTest(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), `forge-integration-test-placeholder`)
}

// isPlaceholderUnitTest checks if handlers_test.go is still the auto-generated
// placeholder with no real tests.
func isPlaceholderUnitTest(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), `forge-unit-test-placeholder`)
}

// scanExistingMethods reads all .go files in dir and returns a set of method names
// that are already implemented on *Service. It uses go/parser so that multi-line
// receivers, comments, and strings containing "*Service" are handled correctly.
func scanExistingMethods(dir string, includeGeneratedStubs bool) (map[string]bool, error) {
	existing := make(map[string]bool)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		// Skip test files
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if !includeGeneratedStubs && entry.Name() == "handlers_gen.go" {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			// Receiver must be a pointer: *Service
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok || ident.Name != "Service" {
				continue
			}
			if fn.Name != nil && fn.Name.Name != "" {
				existing[fn.Name.Name] = true
			}
		}
	}

	return existing, nil
}

// GenerateSetup generates pkg/app/setup.go if it does not already exist.
// This file is user-owned and never overwritten.
func GenerateSetup(modulePath string, databaseDriver string, ormEnabled bool, targetDir string) error {
	appDir := filepath.Join(targetDir, "pkg", "app")
	setupPath := filepath.Join(appDir, "setup.go")

	// Never overwrite — this is user-owned code
	if _, err := os.Stat(setupPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	data := struct {
		Module         string
		HasDatabase    bool
		OrmEnabled     bool
		DatabaseDriver string
	}{
		Module:         modulePath,
		HasDatabase:    databaseDriver != "",
		OrmEnabled:     ormEnabled,
		DatabaseDriver: databaseDriver,
	}

	content, err := templates.RenderProjectTemplate("setup.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render setup.go.tmpl: %w", err)
	}

	return os.WriteFile(setupPath, content, 0644)
}

// hasFallibleConstructor returns true if any service, package, worker, or operator has a fallible constructor.
func hasFallibleConstructor(services []BootstrapServiceData, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData) bool {
	for _, s := range services {
		if s.Fallible {
			return true
		}
	}
	for _, p := range packages {
		if p.Fallible {
			return true
		}
	}
	for _, w := range workers {
		if w.Fallible {
			return true
		}
	}
	for _, o := range operators {
		if o.Fallible {
			return true
		}
	}
	return false
}

// toServicePackage converts "EchoService" -> "echo"
func toServicePackage(name string) string {
	trimmed := strings.TrimSuffix(name, "Service")
	if trimmed == "" {
		return strings.ToLower(name)
	}
	return strings.ToLower(trimmed)
}