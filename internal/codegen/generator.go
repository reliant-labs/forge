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

	// Render handlers.go from embedded template
	handlersContent, err := templates.RenderServiceTemplate("service/handlers.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "handlers.go"), handlersContent, 0644); err != nil {
		return err
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
		PackageName string
		ServiceName string
		Module      string
	}{
		PackageName: data.ServiceName,
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
}

// BootstrapPackageData holds data for a single internal package in the bootstrap template.
type BootstrapPackageData struct {
	Name      string // e.g. "cache"
	Package   string // e.g. "cache" (Go package name)
	FieldName string // e.g. "Cache" (exported struct field)
}

// GenerateBootstrap generates pkg/app/bootstrap.go from the bootstrap.go.tmpl template.
func GenerateBootstrap(services []ServiceDef, packages []BootstrapPackageData, modulePath string, targetDir string) error {
	appDir := filepath.Join(targetDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	var bootstrapSvcs []BootstrapServiceData
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		bootstrapSvcs = append(bootstrapSvcs, BootstrapServiceData{
			Name:      pkg,
			Package:   pkg,
			FieldName: naming.ToExportedFieldName(pkg),
		})
	}

	data := struct {
		Module   string
		Services []BootstrapServiceData
		Packages []BootstrapPackageData
	}{
		Module:   modulePath,
		Services: bootstrapSvcs,
		Packages: packages,
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
	switch {
	case m.ClientStreaming && m.ServerStreaming, m.ServerStreaming:
		return "connect.NewError(connect.CodeUnimplemented, nil)"
	default:
		return "nil, connect.NewError(connect.CodeUnimplemented, nil)"
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
}

// GenerateBootstrapTesting generates pkg/app/testing.go from the bootstrap_testing.go.tmpl template.
func GenerateBootstrapTesting(services []ServiceDef, packages []BootstrapPackageData, modulePath string, targetDir string) error {
	appDir := filepath.Join(targetDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	var testSvcs []BootstrapTestServiceData
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		testSvcs = append(testSvcs, BootstrapTestServiceData{
			Name:             pkg,
			Package:          pkg,
			FieldName:        naming.ToExportedFieldName(pkg),
			ProtoServiceName: svc.Name,
		})
	}

	data := struct {
		Module   string
		Services []BootstrapTestServiceData
		Packages []BootstrapPackageData
	}{
		Module:   modulePath,
		Services: testSvcs,
		Packages: packages,
	}

	content, err := templates.RenderProjectTemplate("bootstrap_testing.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap_testing.go.tmpl: %w", err)
	}

	outPath := filepath.Join(appDir, "testing.go")
	return os.WriteFile(outPath, content, 0644)
}

// PackageDataFromNames builds BootstrapPackageData from package names (e.g. from forge.project.yaml).
func PackageDataFromNames(names []string) []BootstrapPackageData {
	var pkgs []BootstrapPackageData
	for _, name := range names {
		pkgs = append(pkgs, BootstrapPackageData{
			Name:      name,
			Package:   name,
			FieldName: naming.ToExportedFieldName(name),
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
// only for missing methods into handlers_new.go.
// If all methods are already implemented, it returns AllUpToDate=true.
// If handlers_new.go already exists, it is overwritten (it's generated code).
func GenerateMissingHandlerStubs(svc ServiceDef, targetDir string) (*MissingHandlerResult, error) {
	existing, err := scanExistingMethods(targetDir)
	if err != nil {
		return nil, fmt.Errorf("scan existing methods: %w", err)
	}

	var missing []Method
	for _, m := range svc.Methods {
		if !existing[m.Name] {
			missing = append(missing, m)
		}
	}

	if len(missing) == 0 {
		return &MissingHandlerResult{AllUpToDate: true}, nil
	}

	// Build a ServiceDef with only the missing methods for template rendering
	missingSvc := svc
	missingSvc.Methods = missing
	data := mapServiceDefToTemplateData(missingSvc)

	content, err := templates.RenderServiceTemplate("service/handlers_new.go.tmpl", data)
	if err != nil {
		return nil, fmt.Errorf("render handlers_new.go.tmpl: %w", err)
	}

	if err := os.WriteFile(filepath.Join(targetDir, "handlers_new.go"), content, 0644); err != nil {
		return nil, err
	}

	var names []string
	for _, m := range missing {
		names = append(names, m.Name)
	}

	return &MissingHandlerResult{NewMethods: names}, nil
}

// scanExistingMethods reads all .go files in dir and returns a set of method names
// that are already implemented on *Service. It uses go/parser so that multi-line
// receivers, comments, and strings containing "*Service" are handled correctly.
func scanExistingMethods(dir string) (map[string]bool, error) {
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
func GenerateSetup(modulePath string, targetDir string) error {
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
		Module string
	}{
		Module: modulePath,
	}

	content, err := templates.RenderProjectTemplate("setup.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render setup.go.tmpl: %w", err)
	}

	return os.WriteFile(setupPath, content, 0644)
}

// toServicePackage converts "EchoService" -> "echo"
func toServicePackage(name string) string {
	trimmed := strings.TrimSuffix(name, "Service")
	if trimmed == "" {
		return strings.ToLower(name)
	}
	return strings.ToLower(trimmed)
}