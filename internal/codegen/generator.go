package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/reliant-labs/forge/internal/naming"
)

// MethodTemplateData holds per-method data for the embedded service templates.
type MethodTemplateData struct {
	Name            string // RPC method name, e.g. "GetItem"
	InputType       string // proto input message name, e.g. "GetItemRequest"
	OutputType      string // proto output message name, e.g. "GetItemResponse"
	ClientStreaming bool   // true if the client streams requests
	ServerStreaming bool   // true if the server streams responses
	AuthRequired    bool   // true if method_options.auth_required is set
}

// ServiceTemplateData holds the data shape expected by the embedded service templates.
type ServiceTemplateData struct {
	ServiceName    string // e.g. "EchoService" (or hyphenated CLI form)
	ServicePackage string // Go package CLAUSE, e.g. "echo" (disk-resolved for existing dirs)
	// ServiceImportPath is the internal/handlers/ directory leaf used in scaffolded
	// test imports (`{{.Module}}/internal/handlers/{{.ServiceImportPath}}`). Equals
	// ServicePackage for fresh scaffolds; for EXISTING dirs it is the real
	// directory name, which may legally differ from the package clause.
	ServiceImportPath   string
	Module              string               // e.g. "github.com/demo-project"
	ProtoImportPath     string               // e.g. "proto/services/echo" (without /v1)
	ProtoPackage        string               // same as ProtoImportPath for handlers.go.tmpl
	ProtoConnectPackage string               // e.g. "echov1connect"
	HandlerName         string               // e.g. "EchoService"
	ProtoFileSymbol     string               // e.g. "File_services_echo_v1_echo_proto"
	Methods             []MethodTemplateData // method data for handlers.go.tmpl and test templates
	// TestHelperName is the disambiguated suffix that the bootstrap testing
	// generator uses for `app.NewTest<X>` and `app.NewTest<X>Server` helpers.
	// Equal to PascalCase(ServicePackage) when there's no cross-role
	// collision, else "Svc" + PascalCase(ServicePackage) — matching
	// AssignBootstrapAliases / GenerateBootstrapTesting's collision rule.
	// Test scaffold templates reference this rather than re-pascal-casing
	// ServiceName so the call site stays in sync with the actual factory.
	TestHelperName string
}

// protoImportPath derives the relative proto import path for a service
// from its descriptor's GoPackage. GoPackage is like
// "github.com/project/gen/proto/services/echo/v1"; the result strips the
// module + "/gen/" prefix and the trailing "/v1" (or /v2, …) version
// suffix, yielding "proto/services/echo". Returns "" when either
// ModulePath or GoPackage is empty, or when GoPackage doesn't carry the
// expected module+/gen/ prefix. Single source of the derivation shared by
// mapServiceDefToTemplateData (ProtoImportPath/ProtoPackage),
// buildCRUDTemplateData, and buildCRUDTestTemplateData.
func protoImportPath(svc ServiceDef) string {
	if svc.ModulePath == "" || svc.GoPackage == "" {
		return ""
	}
	// Strip module + "/gen/" prefix and "/v1" suffix.
	prefix := svc.ModulePath + "/gen/"
	rest, ok := strings.CutPrefix(svc.GoPackage, prefix)
	if !ok {
		return ""
	}
	// Remove trailing /v1, /v2, etc.
	if idx := strings.LastIndex(rest, "/v"); idx >= 0 {
		rest = rest[:idx]
	}
	return rest
}

// mapServiceDefToTemplateData converts a ServiceDef to the data shape expected by embedded templates.
// projectDir is used to detect cross-role package-name collisions for the
// `app.NewTest<X>` helper name (when there's an internal/<pkg> matching the
// service package, the bootstrap testing generator emits `NewTestSvc<X>`).
// Pass an empty projectDir when the caller has no project context (test-only
// helpers); the helper falls back to the no-collision form.
func mapServiceDefToTemplateData(svc ServiceDef, projectDir ...string) ServiceTemplateData {
	pd := ""
	if len(projectDir) > 0 {
		pd = projectDir[0]
	}
	// GoPackage is like "github.com/project/gen/proto/services/echo/v1"
	// We need ProtoImportPath = "proto/services/echo" (relative, no /v1)
	importPath := protoImportPath(svc)

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
			Name:            m.Name,
			InputType:       m.InputType,
			OutputType:      m.OutputType,
			ClientStreaming: m.ClientStreaming,
			ServerStreaming: m.ServerStreaming,
			AuthRequired:    m.AuthRequired,
		})
	}

	// Scaffold-time synthesis: this data shape is consumed when CREATING a
	// brand-new handler dir (GenerateServiceStub), where forge picks the
	// name. Callers that re-render into an EXISTING dir must override
	// ServicePackage / ServiceImportPath / TestHelperName from disk truth
	// — see GenerateMissingHandlerStubs's applyDiskIdentity.
	pkgName := naming.ServicePackage(svc.Name)

	return ServiceTemplateData{
		ServiceName:         pkgName,
		ServicePackage:      pkgName,
		ServiceImportPath:   pkgName,
		Module:              svc.ModulePath,
		ProtoImportPath:     importPath,
		ProtoPackage:        importPath,
		ProtoConnectPackage: connectPkg,
		HandlerName:         svc.Name,
		ProtoFileSymbol:     protoFileSymbol,
		Methods:             methods,
		TestHelperName:      ComputeTestHelperName(pkgName, pd),
	}
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

	// Synthesis is safe here: naming.ServicePackage only picks the FILENAME
	// inside the shared mocks dir. The file's package clause is the
	// constant "mocks" and its imports come from the proto descriptor's
	// GoPackage/PkgName — no handler-dir identity is referenced, so the
	// disk-first resolver isn't needed.
	mockFile := filepath.Join(mockDir, naming.ServicePackage(svc.Name)+"_mock.go")
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

// ServiceRowPrefix is the name prefix of the generated per-service row
// constructors in pkg/app/services_gen.go ("serviceRow" + FieldName,
// e.g. serviceRowBilling). The cli layer's registration parser matches
// identifiers in the user-owned pkg/app/services.go by this prefix, so
// the template (services_gen.go.tmpl / services.go.tmpl) and the parser
// must agree on it.
const ServiceRowPrefix = "serviceRow"

// ServiceRowFuncName returns the canonical (no-collision) row
// constructor name for a service, accepting any spelling the codebase
// uses (proto "AdminServerService", forge.yaml "admin-server", snake
// "admin_server"). Used for user-facing messages ("add this line:");
// when a cross-role package collision renames the FieldName (rare),
// the emitted constructor carries the collision-aware name instead and
// the registration parser's normalized matching still resolves it.
func ServiceRowFuncName(svcName string) string {
	fieldName := naming.ToPascalCase(strings.TrimSuffix(svcName, "Service"))
	if fieldName == "" {
		fieldName = naming.ToPascalCase(svcName)
	}
	return ServiceRowPrefix + fieldName
}

// prepareServiceData prepares template data for service generation
func prepareServiceData(svc ServiceDef) map[string]any {
	var methods []map[string]any
	needsEmptypb := false
	needsPb := false

	for _, method := range svc.Methods {
		if method.IsInputEmpty() || method.IsOutputEmpty() {
			needsEmptypb = true
		}
		if !method.IsInputEmpty() || !method.IsOutputEmpty() {
			needsPb = true
		}
		methods = append(methods, map[string]any{
			"Name":       method.Name,
			"Signature":  buildMethodSignature(method),
			"ReturnType": buildReturnType(method),
			"ReturnStub": buildReturnStub(method),
			"CallArgs":   buildCallArgs(method),
		})
	}

	return map[string]any{
		"ServiceName":    svc.Name,
		"ServicePackage": naming.ServicePackage(svc.Name),
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

// DISK-FIRST RULE: name synthesis (naming.ServicePackage / naming.GoPackage)
// is only authoritative for components that don't exist on disk yet (fresh
// scaffolds) and for stable lookup KEYS (e.g. the webhookServices map). Any
// generator referencing an EXISTING handler/worker/operator directory must
// resolve identity via ResolveComponentDir / ResolveServiceComponent
// (disk_resolver.go) instead — re-synthesizing names for existing artifacts
// is the broken-imports bug class the resolver eliminates.
