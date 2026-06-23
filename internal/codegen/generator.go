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

// foreignImport records one non-`pb` protobuf package the mock must import,
// with the alias used to qualify its message types.
type foreignImport struct {
	Alias string // e.g. "reliantv1"
	Path  string // e.g. "github.com/proj/gen/reliant/v1"
}

// goPackageForProtoFile derives the generated Go import path for a proto
// file under the buf module root. The buf module root is "proto/", and the
// go_package convention forge projects use is `<module>/gen/<dir>` where
// <dir> is the proto file's directory relative to "proto/" (e.g.
// "reliant/v1/daemon_registry.proto" → "<module>/gen/reliant/v1"). Returns
// "" when modulePath is empty or the path has no directory component.
func goPackageForProtoFile(protoFile, modulePath string) string {
	if modulePath == "" || protoFile == "" {
		return ""
	}
	rel := strings.TrimPrefix(protoFile, "proto/")
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "" || dir == "." {
		return ""
	}
	return modulePath + "/gen/" + dir
}

// mockTypeResolver maps a method's input/output message to the qualified Go
// type the mock should reference. Messages declared in the service's own
// proto file (the common case) keep the `pb.` qualifier — the package the
// connect stub itself lives in. Messages pulled in from an IMPORTED proto
// file (proto-split: a thin service surface that reuses request/response
// messages from a shared package) live in a DIFFERENT generated Go package,
// so the mock must import that package under a distinct alias and qualify
// the type with it. Before this resolver every type was hardcoded to `pb.`,
// which produced `undefined: pb.X` build failures for every cross-package
// reference.
type mockTypeResolver struct {
	svc      ServiceDef
	aliases  map[string]string // go import path -> alias
	imports  []foreignImport
	needsPb  bool
	needsEmp bool
}

func newMockTypeResolver(svc ServiceDef) *mockTypeResolver {
	return &mockTypeResolver{svc: svc, aliases: map[string]string{}}
}

// qualify returns the Go type reference (e.g. "pb.GetItemRequest",
// "reliantv1.CreateDaemonTokenRequest", "emptypb.Empty") for one message,
// registering any foreign import it implies as a side effect.
func (r *mockTypeResolver) qualify(msgType, fqType, protoFile string) string {
	if msgType == "google.protobuf.Empty" {
		r.needsEmp = true
		return "emptypb.Empty"
	}
	// Short name: drop any "pkg." qualifier the descriptor may carry on
	// InputType/OutputType (it is the proto short name in practice).
	short := msgType
	if i := strings.LastIndex(short, "."); i >= 0 {
		short = short[i+1:]
	}
	goPkg := goPackageForProtoFile(protoFile, r.svc.ModulePath)
	// Same-file (or unknown provenance) → the service's own package, `pb`.
	if protoFile == "" || goPkg == "" || protoFile == r.svc.ProtoFile || goPkg == r.svc.GoPackage {
		r.needsPb = true
		return "pb." + short
	}
	alias, ok := r.aliases[goPkg]
	if !ok {
		alias = mockImportAlias(goPkg, len(r.aliases))
		r.aliases[goPkg] = alias
		r.imports = append(r.imports, foreignImport{Alias: alias, Path: goPkg})
	}
	return alias + "." + short
}

// mockImportAlias builds a deterministic, collision-free import alias for a
// generated proto package path. It uses the last two path segments
// (dir + version, e.g. "reliant/v1" → "reliantv1") which is unique across a
// project's gen/ tree; the seq suffix is a defensive tiebreaker only used if
// two distinct paths ever normalize to the same alias.
func mockImportAlias(goPkg string, seq int) string {
	parts := strings.Split(strings.Trim(goPkg, "/"), "/")
	base := goPkg
	if len(parts) >= 2 {
		base = parts[len(parts)-2] + parts[len(parts)-1]
	} else if len(parts) == 1 {
		base = parts[0]
	}
	base = naming.GoPackage(base)
	if base == "" {
		base = fmt.Sprintf("pbx%d", seq)
	}
	return base
}

// prepareServiceData prepares template data for service generation
func prepareServiceData(svc ServiceDef) map[string]any {
	var methods []map[string]any
	res := newMockTypeResolver(svc)

	for _, method := range svc.Methods {
		methods = append(methods, map[string]any{
			"Name":       method.Name,
			"Signature":  buildMethodSignature(method, res),
			"ReturnType": buildReturnType(method, res),
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
		"NeedsEmptypb":   res.needsEmp,
		"NeedsPb":        res.needsPb,
		"ForeignImports": res.imports,
	}
}

// inputGoType / outputGoType resolve a method's input/output message to its
// qualified Go type via the resolver, replacing Method.GoInputType's
// hardcoded `pb.` assumption for the mock path.
func inputGoType(m Method, r *mockTypeResolver) string {
	return r.qualify(m.InputType, m.InputTypeFQ, m.InputProtoFile)
}

func outputGoType(m Method, r *mockTypeResolver) string {
	return r.qualify(m.OutputType, m.OutputTypeFQ, m.OutputProtoFile)
}

func buildMethodSignature(m Method, r *mockTypeResolver) string {
	switch {
	case m.ClientStreaming && m.ServerStreaming:
		// Bidirectional streaming
		return fmt.Sprintf("ctx context.Context, stream *connect.BidiStream[%s, %s]", inputGoType(m, r), outputGoType(m, r))
	case m.ClientStreaming:
		// Client streaming
		return fmt.Sprintf("ctx context.Context, stream *connect.ClientStream[%s]", inputGoType(m, r))
	case m.ServerStreaming:
		// Server streaming
		return fmt.Sprintf("ctx context.Context, req *connect.Request[%s], stream *connect.ServerStream[%s]", inputGoType(m, r), outputGoType(m, r))
	default:
		// Unary
		return fmt.Sprintf("ctx context.Context, req *connect.Request[%s]", inputGoType(m, r))
	}
}

func buildReturnType(m Method, r *mockTypeResolver) string {
	switch {
	case m.ClientStreaming && m.ServerStreaming:
		return "error"
	case m.ClientStreaming:
		return fmt.Sprintf("(*connect.Response[%s], error)", outputGoType(m, r))
	case m.ServerStreaming:
		return "error"
	default:
		return fmt.Sprintf("(*connect.Response[%s], error)", outputGoType(m, r))
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
