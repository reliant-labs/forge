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
	"unicode"

	"github.com/reliant-labs/forge/internal/checksums"
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
	ServiceName        string               // e.g. "EchoService" (or hyphenated CLI form)
	ServicePackage     string               // Go-package-safe form, e.g. "echo" / "admin_server"
	Module             string               // e.g. "github.com/demo-project"
	ProtoImportPath    string               // e.g. "proto/services/echo" (without /v1)
	ProtoPackage       string               // same as ProtoImportPath for handlers.go.tmpl
	ProtoConnectPackage string              // e.g. "echov1connect"
	HandlerName        string               // e.g. "EchoService"
	ProtoFileSymbol    string               // e.g. "File_services_echo_v1_echo_proto"
	Methods            []MethodTemplateData // method data for handlers.go.tmpl and test templates
	// TestHelperName is the disambiguated suffix that the bootstrap testing
	// generator uses for `app.NewTest<X>` and `app.NewTest<X>Server` helpers.
	// Equal to PascalCase(ServicePackage) when there's no cross-role
	// collision, else "Svc" + PascalCase(ServicePackage) — matching
	// AssignBootstrapAliases / GenerateBootstrapTesting's collision rule.
	// Test scaffold templates reference this rather than re-pascal-casing
	// ServiceName so the call site stays in sync with the actual factory.
	TestHelperName string
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
		ServicePackage:     pkgName,
		Module:             svc.ModulePath,
		ProtoImportPath:    protoImportPath,
		ProtoPackage:       protoImportPath,
		ProtoConnectPackage: connectPkg,
		HandlerName:        svc.Name,
		ProtoFileSymbol:    protoFileSymbol,
		Methods:            methods,
		TestHelperName:     ComputeTestHelperName(pkgName, pd),
	}
}

// ComputeTestHelperName returns the suffix used by the `app.NewTest<X>` and
// `app.NewTest<X>Server` factories generated into pkg/app/testing.go. When
// the service's Go-package name collides with an internal package directory
// of the same name (e.g. service `billing` + `internal/billing/`), the
// bootstrap testing generator disambiguates by prefixing "Svc"
// (NewTestSvcBilling). This helper mirrors that rule so test scaffolds emit
// the same identifier the factory actually has.
//
// projectDir may be empty (no project context); in that case there's no
// collision detection possible and the result is the no-collision form.
// The collision rule matches GenerateBootstrapTesting's pkgCount logic.
func ComputeTestHelperName(servicePkg, projectDir string) string {
	pascal := naming.ToPascalCase(servicePkg)
	if projectDir == "" {
		return pascal
	}
	internalDir := filepath.Join(projectDir, "internal", servicePkg)
	if info, err := os.Stat(internalDir); err == nil && info.IsDir() {
		return "Svc" + pascal
	}
	return pascal
}

// GenerateServiceStub generates service.go and handlers.go for a new service
// using the embedded FS templates. crudMethodNames lists methods that CRUD gen
// will implement; these are excluded from the initial handlers.go stubs.
func GenerateServiceStub(svc ServiceDef, targetDir string, crudMethodNames ...map[string]bool) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	// Derive projectDir from targetDir's <projectDir>/handlers/<svc> shape so
	// the test-helper-name collision check can probe internal/<pkg>. Day-0,
	// no caller passes a non-conventional targetDir.
	projectDir := filepath.Dir(filepath.Dir(targetDir))
	data := mapServiceDefToTemplateData(svc, projectDir)

	// Render service.go from embedded template
	serviceContent, err := templates.ServiceTemplates().Render("service.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render service.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "service.go"), serviceContent, 0644); err != nil {
		return err
	}

	// For handlers.go, filter out methods that CRUD gen will implement.
	var crudNames map[string]bool
	if len(crudMethodNames) > 0 {
		crudNames = crudMethodNames[0]
	}
	handlersData := data
	if len(crudNames) > 0 {
		var nonCRUD []MethodTemplateData
		for _, m := range data.Methods {
			if !crudNames[m.Name] {
				nonCRUD = append(nonCRUD, m)
			}
		}
		handlersData.Methods = nonCRUD
	}

	// Render handlers.go from embedded template only when there are real methods
	// to implement. With zero methods, handlers.go would just be a placeholder
	// comment; skip it and let the user (or subsequent forge generate runs) create
	// it with actual content.
	if len(handlersData.Methods) > 0 {
		handlersContent, err := templates.ServiceTemplates().Render("handlers.go.tmpl", handlersData)
		if err != nil {
			return fmt.Errorf("render handlers.go.tmpl: %w", err)
		}
		if err := os.WriteFile(filepath.Join(targetDir, "handlers.go"), handlersContent, 0644); err != nil {
			return err
		}
	}

	// Render handlers_scaffold_test.go from embedded template (same filter as handlers.go — skip CRUD methods).
	// The qualified filename frees the canonical handlers_test.go slot for user-owned tests; forge never
	// touches handlers_test.go.
	unitTestContent, err := templates.ServiceTemplates().Render("unit_test.go.tmpl", handlersData)
	if err != nil {
		return fmt.Errorf("render unit_test.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "handlers_scaffold_test.go"), unitTestContent, 0644); err != nil {
		return err
	}

	// Render integration_test.go from embedded template
	integrationTestContent, err := templates.ServiceTemplates().Render("integration_test.go.tmpl", data)
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
	authzContent, err := templates.ServiceTemplates().Render("authorizer.go.tmpl", authzData)
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

	serviceContent, err := templates.ServiceTemplates().Render("service.go.tmpl", data)
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

// BootstrapComponentData represents a bootstrappable component (service, package, worker, operator).
//
// For nested internal packages (e.g. internal/mcp/database/contract.go), Package
// is the leaf Go-identifier ("database"), while ImportPath carries the full
// path under internal/ ("mcp/database") used to construct the import line. For
// top-level packages, ImportPath is the same as Package. FieldName must remain
// unique across all packages, so for nested entries it should encode the full
// path (e.g. "McpDatabase") to avoid collisions with sibling leaves. VarName is
// the lowerCamel form of FieldName and is used in the bootstrap template as a
// unique prefix for local Go variables (e.g. "mcpDatabaseImpl"); using FieldName
// avoids collisions when two nested packages share a leaf name.
type BootstrapComponentData struct {
	Name       string // e.g. "api", "cache", "email_sender"
	Package    string // e.g. "api" (Go package identifier — leaf of ImportPath for nested entries)
	ImportPath string // e.g. "api" or "mcp/database" (path under internal/, workers/, etc.)
	FieldName  string // e.g. "API" or "McpDatabase" (exported struct field, must be unique)
	VarName    string // e.g. "api" or "mcpDatabase" (unique lowerCamel local-var prefix in bootstrap)
	Fallible   bool   // true if New() returns (T, error)
	// Alias is the import alias used in bootstrap.go for this component's
	// Go package. Defaults to Package when there are no cross-role
	// collisions; gets a role-prefixed value (e.g. "svcBilling",
	// "pkgBilling") when a service Package matches an internal package
	// Package (or other cross-role pair). All bootstrap.go references
	// to the package's exported symbols must use Alias rather than
	// Package so the alias-rewrite is observed at every call site.
	Alias string
	// HasWebhooks is true when this service has webhooks declared in
	// forge.yaml. The bootstrap template uses this to emit a
	// `RegisterWebhookRoutes(mux, stack)` call after `RegisterHTTP(...)`,
	// so generated webhook routes get mounted on the mux without the user
	// having to hand-edit the user-owned `RegisterHTTP` body in
	// handlers/<svc>/service.go. Only populated for services; ignored
	// for packages, workers, and operators.
	HasWebhooks bool
}

// Type aliases for backward compatibility and readability.
type BootstrapServiceData = BootstrapComponentData
type BootstrapPackageData = BootstrapComponentData
type BootstrapWorkerData = BootstrapComponentData

// WorkerDataFromNames builds BootstrapWorkerData from worker names (e.g. from forge.yaml).
// projectDir is the root project directory; if non-empty, it is used to detect fallible constructors.
// Hyphens in the user-facing name are normalized to underscores for Package/FieldName so the
// generated bootstrap.go remains syntactically valid Go.
func WorkerDataFromNames(names []string, projectDir string) []BootstrapWorkerData {
	var workers []BootstrapWorkerData
	for _, name := range names {
		pkg := toGoPackage(name)
		fieldName := naming.ToExportedFieldName(pkg)
		fallible := false
		if projectDir != "" {
			fallible, _ = DetectFallibleConstructor(filepath.Join(projectDir, "workers", pkg))
		}
		workers = append(workers, BootstrapWorkerData{
			Name:       name,
			Package:    pkg,
			ImportPath: pkg,
			FieldName:  fieldName,
			VarName:    lowerFirst(fieldName),
			Fallible:   fallible,
			Alias:      pkg,
		})
	}
	return workers
}

type BootstrapOperatorData = BootstrapComponentData

// OperatorDataFromNames builds BootstrapOperatorData from operator names (e.g. from forge.yaml).
// projectDir is the root project directory; if non-empty, it is used to detect fallible constructors.
func OperatorDataFromNames(names []string, projectDir string) []BootstrapOperatorData {
	var operators []BootstrapOperatorData
	for _, name := range names {
		pkg := toGoPackage(name)
		fieldName := naming.ToExportedFieldName(pkg)
		fallible := false
		if projectDir != "" {
			fallible, _ = DetectFallibleConstructor(filepath.Join(projectDir, "operators", pkg))
		}
		operators = append(operators, BootstrapOperatorData{
			Name:       name,
			Package:    pkg,
			ImportPath: pkg,
			FieldName:  fieldName,
			VarName:    lowerFirst(fieldName),
			Fallible:   fallible,
			Alias:      pkg,
		})
	}
	return operators
}

// toGoPackage normalizes a CLI/forge.yaml-style name into a valid Go package
// identifier: lowercase with hyphens replaced by underscores. This mirrors
// generator.ServicePackageName but is duplicated here to keep codegen free of
// a generator dependency (the generator package already imports codegen).
func toGoPackage(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

// lowerFirst returns s with the first rune lowercased — used to derive a
// lowerCamel local-variable prefix from a PascalCase FieldName so generated
// bootstrap code avoids collisions when nested packages share a leaf name.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// upperFirst returns s with the first rune uppercased — used to build
// alias prefixes (e.g. "svc" + "Billing").
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// CollisionCounts returns a map of Go-package-name → occurrence count
// across services, packages, workers, and operators. A count > 1 means
// a cross-role collision (e.g. service "billing" + internal package
// "billing") and is the trigger for role-prefixed aliasing in
// AssignBootstrapAliases. Exposed so wire_gen and other generators can
// derive the SAME collision-aware FieldName that bootstrap uses without
// duplicating the bookkeeping.
func CollisionCounts(services []BootstrapServiceData, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData) map[string]int {
	count := map[string]int{}
	for _, s := range services {
		count[s.Package]++
	}
	for _, p := range packages {
		count[p.Package]++
	}
	for _, w := range workers {
		count[w.Package]++
	}
	for _, o := range operators {
		count[o.Package]++
	}
	return count
}

// ResolveCollisionNaming returns the (Alias, FieldName) pair for a
// component given its raw Package name, a fallback FieldName the caller
// already computed for the no-collision case, the cross-role collision
// counts, and the component's role-short-name prefix ("svc", "pkg",
// "wkr", "op"). When the package collides cross-role, the result is
// (rolePrefix + Package, RolePrefix + Package) — alias is lower-camel,
// field name is upper-camel. Otherwise (Package, fallbackFieldName) —
// preserving the caller's per-role naming convention (services use
// ToPascalCase; workers/operators use ToExportedFieldName which keeps
// underscores; nested packages use a path-encoded form).
//
// Single source of truth for the wire_gen ↔ bootstrap naming agreement:
// both files derive their `wireXxxDeps` function name + `Services.Xxx`
// field reference from this helper, so the two stay in lockstep when a
// service package collides with an internal-package import.
func ResolveCollisionNaming(pkg, fallbackFieldName, rolePrefix string, counts map[string]int) (alias, fieldName string) {
	if counts[pkg] > 1 {
		return rolePrefix + upperFirst(pkg), upperFirst(rolePrefix) + upperFirst(pkg)
	}
	return pkg, fallbackFieldName
}

// AssignBootstrapAliases populates the Alias field on every
// BootstrapComponentData across services, packages, workers, and
// operators. When the .Package fields are unique across all four roles,
// each Alias equals its Package (default Go import alias — preserves the
// original codegen output). When two roles share a .Package value (e.g.
// service "billing" + internal package "billing"), the conflicting
// component(s) get a role-prefixed alias ("svcBilling", "pkgBilling",
// "wkrBilling", "opBilling") so the import line in bootstrap.go can
// alias the import and every reference site can use the alias unambiguously.
//
// This is purely additive — when there's no collision the default
// alias matches Package and the rendered bootstrap is identical to
// pre-aliasing output.
//
// Internally delegates to ResolveCollisionNaming so wire_gen and
// bootstrap derive their function/field names from the same rule.
func AssignBootstrapAliases(services []BootstrapServiceData, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData) {
	count := CollisionCounts(services, packages, workers, operators)

	setAlias := func(c *BootstrapComponentData, rolePrefix string) {
		alias, fieldName := ResolveCollisionNaming(c.Package, c.FieldName, rolePrefix, count)
		c.Alias = alias
		// Only override FieldName/VarName on collision — the no-collision
		// branch keeps whatever the caller computed (services use
		// ToPascalCase, packages use ToExportedFieldName for nested
		// support; honoring those preserves nested-package field names
		// like "McpDatabase").
		if count[c.Package] > 1 {
			c.FieldName = fieldName
			c.VarName = lowerFirst(c.FieldName)
		}
	}
	for i := range services {
		setAlias(&services[i], "svc")
	}
	for i := range packages {
		setAlias(&packages[i], "pkg")
	}
	for i := range workers {
		setAlias(&workers[i], "wkr")
	}
	for i := range operators {
		setAlias(&operators[i], "op")
	}
}

// GenerateBootstrap generates pkg/app/bootstrap.go from the bootstrap.go.tmpl template.
//
// hasDatabase gates the DB field + setupDatabase wiring; ormEnabled gates the
// ORM field + generated ORM client construction. The two are separate
// concerns: a project may configure a DB driver (for migrations, raw SQL,
// sqlc) without opting into the generated forge ORM. The ORM field is
// dropped when no proto/db/ entity definitions exist so `App.ORM` can never
// be silently nil in user code.
//
// Reads forge.yaml to detect `binary: shared` mode; in that mode the rendered
// `BootstrapOnly` lazily constructs each service inside its name-gated block
// instead of constructing all up-front, so per-service cobra subcommands
// (`./<bin> api`) actually scope-down their dependency graph.
//
// cs is the project's checksum tracker — passing it keeps pkg/app/bootstrap.go
// out of `forge audit`'s tracked-files drift report. A nil cs is tolerated.
//
// webhookServices is keyed by snake-case service package name (e.g.
// "admin_server") and indicates which services have webhooks declared in
// forge.yaml. The bootstrap template emits `RegisterWebhookRoutes(mux,
// stack)` after `RegisterHTTP(...)` for those services so generated
// webhook routes get auto-mounted on the mux without the user having to
// hand-edit the user-owned `RegisterHTTP` body. Pass nil if no services
// have webhooks. (2026-04-30 LLM-port: introduced as part of the
// auto-wire fix — pre-fix, projects had to manually edit service.go to
// call s.RegisterWebhookRoutes(mux, stack).)
func GenerateBootstrap(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, hasDatabase bool, ormEnabled bool, projectDir string, configFields map[string]bool, webhookServices map[string]bool, cs *checksums.FileChecksums) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	var bootstrapSvcs []BootstrapServiceData
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		fallible, _ := DetectFallibleConstructor(filepath.Join(projectDir, "handlers", pkg))
		fieldName := naming.ToPascalCase(pkg)
		// Runtime name is the kebab form derived from the snake package
		// directory — matches what cobra subcommands pass to runServer
		// (cmd-shared-service.go.tmpl uses {{.ServiceName}} which is the
		// kebab form from forge.yaml). Pre-2026-04-30 this was set to
		// the snake-case package name, which silently dropped the cobra
		// invocation under shared-binary mode (admin-server vs admin_server).
		runtimeName := strings.ReplaceAll(pkg, "_", "-")
		bootstrapSvcs = append(bootstrapSvcs, BootstrapServiceData{
			Name:       runtimeName,
			Package:    pkg,
			ImportPath: pkg,
			// Use ToPascalCase so multi-word service packages produce the
			// same exported field name as the unit/integration test templates,
			// which call `app.NewTest{{.ServiceName | pascalCase}}` (e.g.
			// "admin_server" -> "AdminServer").
			FieldName:   fieldName,
			VarName:     lowerFirst(fieldName),
			Fallible:    fallible,
			Alias:       pkg,
			HasWebhooks: webhookServices[pkg],
		})
	}

	// Resolve cross-role import-alias collisions (e.g. service "billing"
	// vs internal package "billing"). When unique, Alias == Package and
	// the bootstrap import line + symbol references emit unchanged.
	AssignBootstrapAliases(bootstrapSvcs, packages, workers, operators)

	hasFallible := hasFallibleConstructor(bootstrapSvcs, packages, workers, operators)

	if configFields == nil {
		configFields = DefaultConfigFieldNames()
	}

	binaryShared := projectBinaryShared(projectDir)

	data := struct {
		Module       string
		Services     []BootstrapServiceData
		Packages     []BootstrapPackageData
		Workers      []BootstrapWorkerData
		Operators    []BootstrapOperatorData
		HasDatabase  bool
		OrmEnabled   bool
		HasFallible  bool
		BinaryShared bool
		ConfigFields map[string]bool
	}{
		Module:       modulePath,
		Services:     bootstrapSvcs,
		Packages:     packages,
		Workers:      workers,
		Operators:    operators,
		HasDatabase:  hasDatabase,
		OrmEnabled:   ormEnabled,
		HasFallible:  hasFallible,
		BinaryShared: binaryShared,
		ConfigFields: configFields,
	}

	content, err := templates.ProjectTemplates().Render("bootstrap.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(projectDir, filepath.Join("pkg", "app", "bootstrap.go"), content, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/bootstrap.go: %w", err)
	}
	return nil
}

// GenerateAppGen writes pkg/app/app_gen.go — the forge-owned canonical
// *App struct shape (Services, Workers, Operators, Packages, DB, ORM)
// with `*AppExtras` embedded so the user can extend App by appending
// fields to AppExtras in the user-owned pkg/app/app_extras.go file.
//
// Splitting the App struct out of bootstrap.go is what made the
// user-extension story tractable: bootstrap.go is regenerated every
// run, so users couldn't append fields there. With the struct hoisted
// here + embedded extras, the user-side workflow is a clean two-step:
//   1. Add field to AppExtras in pkg/app/app_extras.go (Tier-2).
//   2. Assign `app.<Field> = ...` in pkg/app/setup.go (Tier-2).
//
// app_gen.go itself is regenerated as forge.yaml's component list
// changes (services/workers/operators/packages added or removed); the
// AppExtras embedding stays stable across regenerates.
func GenerateAppGen(hasDatabase bool, ormEnabled bool, hasServices bool, hasWorkers bool, hasOperators bool, hasPackages bool, projectDir string, cs *checksums.FileChecksums) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	data := struct {
		HasDatabase bool
		OrmEnabled  bool
		Services    bool
		Workers     bool
		Operators   bool
		Packages    bool
	}{
		HasDatabase: hasDatabase,
		OrmEnabled:  ormEnabled,
		Services:    hasServices,
		Workers:     hasWorkers,
		Operators:   hasOperators,
		Packages:    hasPackages,
	}

	content, err := templates.ProjectTemplates().Render("app_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render app_gen.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(projectDir, filepath.Join("pkg", "app", "app_gen.go"), content, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/app_gen.go: %w", err)
	}
	return nil
}

// GenerateAppExtras writes pkg/app/app_extras.go ONCE — it's the
// Tier-2 user-extension scaffold that holds the empty AppExtras struct
// + a comment block explaining how to add fields. Never overwritten on
// subsequent generates (mirrors GenerateSetup's never-overwrite rule).
func GenerateAppExtras(projectDir string) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	extrasPath := filepath.Join(appDir, "app_extras.go")

	if _, err := os.Stat(extrasPath); err == nil {
		// User-owned file already exists — leave it alone.
		return nil
	}

	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	content, err := templates.ProjectTemplates().Render("app_extras.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render app_extras.go.tmpl: %w", err)
	}

	return os.WriteFile(extrasPath, content, 0644)
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
//
// ProtoConnectImportPath and ProtoConnectPkg are derived from the proto file's
// declared `go_package` rather than from the service name. This makes the
// generated testing.go correct even when the proto file's go_package doesn't
// follow the convention `<module>/gen/services/<svc>/v1/<svc>v1` (for example
// when multiple proto services live in a single proto file and share one go
// package, or when the package name has a custom alias).
type BootstrapTestServiceData struct {
	Name                   string // e.g. "api"
	Package                string // e.g. "api" (Go package name)
	FieldName              string // e.g. "API" (exported struct field)
	ProtoServiceName       string // e.g. "ApiService" (proto service name for connect client)
	ProtoConnectImportPath string // e.g. "github.com/foo/bar/gen/services/api/v1/apiv1connect"
	ProtoConnectPkg        string // e.g. "apiv1connect" (Go identifier used at call sites)
	Fallible               bool   // true if New() returns (T, error)
	HasDB                  bool   // true if Deps struct has a DB orm.Context field
	// Alias mirrors BootstrapComponentData.Alias — when an internal package
	// shares its leaf-name with this service's package, both get role-prefixed
	// aliases ("svcBilling" vs "pkgBilling") so the generated testing.go imports
	// don't collide.
	Alias string
	// VarName is the lowerCamel form of FieldName, used as the testConfig
	// field name (e.g. `c.billingDeps`). Defaults to lowerFirst(Package);
	// becomes "svcBilling" when there's a cross-role collision so the
	// services-range and packages-range testConfig fields stay distinct.
	VarName string
}

// GenerateBootstrapTesting generates pkg/app/testing.go from the bootstrap_testing.go.tmpl template.
//
// cs is the project's checksum tracker — passing it keeps pkg/app/testing.go
// recorded so `forge audit` doesn't flag stale state on it. A nil cs is tolerated.
func GenerateBootstrapTesting(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, multiTenantEnabled bool, projectDir string, cs *checksums.FileChecksums) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	var testSvcs []BootstrapTestServiceData
	anyServiceHasDB := false
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		handlerDir := filepath.Join(projectDir, "handlers", pkg)
		fallible, _ := DetectFallibleConstructor(handlerDir)
		hasDB, _ := DetectDepsDBField(handlerDir)
		if hasDB {
			anyServiceHasDB = true
		}
		// Derive Connect package path/name from the proto's declared
		// go_package + PkgName instead of guessing from the service name.
		// Falls back to the convention path when GoPackage is empty (covers
		// older descriptors and synthetic test fixtures).
		connectPkg := svc.PkgName + "connect"
		connectImport := svc.GoPackage + "/" + connectPkg
		if svc.GoPackage == "" {
			connectImport = modulePath + "/gen/services/" + pkg + "/v1/" + pkg + "v1connect"
			connectPkg = pkg + "v1connect"
		}
		testSvcs = append(testSvcs, BootstrapTestServiceData{
			Name:                   pkg,
			Package:                pkg,
			FieldName:              naming.ToPascalCase(pkg),
			ProtoServiceName:       svc.Name,
			ProtoConnectImportPath: connectImport,
			ProtoConnectPkg:        connectPkg,
			Fallible:               fallible,
			HasDB:                  hasDB,
			Alias:                  pkg, // overwritten below if there's a cross-role collision
			VarName:                pkg, // overwritten below if there's a cross-role collision
		})
	}

	// Resolve cross-role import-alias collisions. Build a count over the
	// services + packages namespace (workers/operators don't appear in
	// testing.go imports, but we still pass them so the alias derivation
	// matches bootstrap.go's exactly). When colliding, also rewrite
	// FieldName so the generated `NewTest<FieldName>` and `With<FieldName>Deps`
	// helpers don't collide on the same Go identifier.
	pkgCount := map[string]int{}
	for _, s := range testSvcs {
		pkgCount[s.Package]++
	}
	for _, p := range packages {
		pkgCount[p.Package]++
	}
	for i := range testSvcs {
		if pkgCount[testSvcs[i].Package] > 1 {
			testSvcs[i].Alias = "svc" + upperFirst(testSvcs[i].Package)
			testSvcs[i].FieldName = "Svc" + upperFirst(testSvcs[i].Package)
			testSvcs[i].VarName = lowerFirst(testSvcs[i].FieldName)
		}
	}
	// Apply the same collision rule to packages so the testing.go import
	// alias for a colliding internal package matches the bootstrap.go alias.
	// Take a defensive copy so we don't mutate the caller's slice.
	pkgsCopy := append([]BootstrapPackageData(nil), packages...)
	for i := range pkgsCopy {
		if pkgCount[pkgsCopy[i].Package] > 1 {
			pkgsCopy[i].Alias = "pkg" + upperFirst(pkgsCopy[i].Package)
			pkgsCopy[i].FieldName = "Pkg" + upperFirst(pkgsCopy[i].Package)
			pkgsCopy[i].VarName = lowerFirst(pkgsCopy[i].FieldName)
		} else if pkgsCopy[i].Alias == "" {
			pkgsCopy[i].Alias = pkgsCopy[i].Package
		}
	}
	packages = pkgsCopy

	// Dedupe Connect package imports: when multiple proto services share one
	// proto file (and thus one go_package), they share one connect import.
	// Without dedupe the generated testing.go would contain duplicate imports
	// and fail to compile.
	connectImportSet := make(map[string]struct{}, len(testSvcs))
	var connectImports []string
	for _, s := range testSvcs {
		if _, seen := connectImportSet[s.ProtoConnectImportPath]; seen {
			continue
		}
		connectImportSet[s.ProtoConnectImportPath] = struct{}{}
		connectImports = append(connectImports, s.ProtoConnectImportPath)
	}

	data := struct {
		Module             string
		Services           []BootstrapTestServiceData
		ConnectImports     []string
		Packages           []BootstrapPackageData
		MultiTenantEnabled bool
		AnyServiceHasDB    bool
	}{
		Module:             modulePath,
		Services:           testSvcs,
		ConnectImports:     connectImports,
		Packages:           packages,
		MultiTenantEnabled: multiTenantEnabled,
		AnyServiceHasDB:    anyServiceHasDB,
	}

	content, err := templates.ProjectTemplates().Render("bootstrap_testing.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap_testing.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(projectDir, filepath.Join("pkg", "app", "testing.go"), content, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/testing.go: %w", err)
	}
	return nil
}

// GenerateMigrate writes pkg/app/migrate.go with embedded migration support.
// When hasMigrations is true, the generated file includes go:embed directives
// and golang-migrate logic. When false, AutoMigrate is a no-op stub so that
// cmd/server.go always compiles.
//
// cs is the project's checksum tracker — both pkg/app/migrate.go and
// db/embed.go are recorded so `forge audit` doesn't flag drift on them.
// A nil cs is tolerated.
func GenerateMigrate(targetDir string, modulePath string, hasMigrations bool, cs *checksums.FileChecksums) error {
	data := struct {
		HasMigrations bool
		ModulePath    string
	}{
		HasMigrations: hasMigrations,
		ModulePath:    modulePath,
	}

	content, err := templates.ProjectTemplates().Render("migrate.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render migrate.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(targetDir, filepath.Join("pkg", "app", "migrate.go"), content, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/migrate.go: %w", err)
	}

	// Generate db/embed.go with the go:embed directive (must be in the db/ dir)
	if hasMigrations {
		embedContent := []byte(`// Code generated by forge generate. DO NOT EDIT.
package db

import "embed"

//go:embed migrations/*.sql
var MigrationsFS embed.FS
`)
		if _, err := checksums.WriteGeneratedFile(targetDir, filepath.Join("db", "embed.go"), embedContent, cs, true); err != nil {
			return fmt.Errorf("write db/embed.go: %w", err)
		}
	}

	return nil
}

// PackageDataFromNames builds BootstrapPackageData from package names (e.g. from
// forge.yaml or discoverPackages). Names may be flat ("cache") or nested using
// forward slashes ("mcp/database"). For nested names the leaf segment is the Go
// package identifier (used at call sites like `database.New(...)`), while the
// full path is preserved for the import line and for deriving a unique
// FieldName/VarName so two leaves with the same name (e.g. "mcp/database" and
// "foo/database") don't collide in generated code.
//
// projectDir is the root project directory; if non-empty, it is used to detect
// fallible constructors by inspecting the Go source in internal/<importPath>/.
func PackageDataFromNames(names []string, projectDir string) []BootstrapPackageData {
	var pkgs []BootstrapPackageData
	for _, name := range names {
		// Nested paths use forward slashes; flat names work the same way (no
		// slash → leaf == importPath).
		importPath := strings.TrimPrefix(filepath.ToSlash(name), "/")
		// The Go package identifier is always the leaf — that's what `package X`
		// declares in the contract.go and what call sites use as a qualifier.
		leaf := importPath
		if idx := strings.LastIndex(leaf, "/"); idx >= 0 {
			leaf = leaf[idx+1:]
		}
		leaf = toGoPackage(leaf)
		// FieldName encodes the full path (PascalCase concatenation) so nested
		// packages with the same leaf name don't share an exported struct
		// field. ToPascalCase already treats '/' as a separator-ish via the
		// underscore replacement we apply first.
		fieldNameSrc := strings.ReplaceAll(importPath, "/", "_")
		fieldName := naming.ToExportedFieldName(toGoPackage(fieldNameSrc))
		if strings.Contains(importPath, "/") {
			// For nested paths, ToExportedFieldName only uppercases the first
			// rune. ToPascalCase capitalizes every segment, which is what we
			// want when there are multiple path components.
			fieldName = naming.ToPascalCase(fieldNameSrc)
		}
		fallible := false
		if projectDir != "" {
			fallible, _ = DetectFallibleConstructor(filepath.Join(projectDir, "internal", filepath.FromSlash(importPath)))
		}
		pkgs = append(pkgs, BootstrapPackageData{
			Name:       name,
			Package:    leaf,
			ImportPath: importPath,
			FieldName:  fieldName,
			VarName:    lowerFirst(fieldName),
			Fallible:   fallible,
			Alias:      leaf,
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
// crudMethodNames optionally lists method names that CRUD gen will implement;
// stubs are skipped for these even if they don't exist yet in the package.
//
// cs is the project's checksum tracker. Passing it ensures the generated
// handlers_gen.go is recorded so it doesn't show up as an orphan in `forge
// audit`. The placeholder-replacement of integration_test.go /
// handlers_scaffold_test.go does not record a checksum: those files become
// user-owned after the placeholder is filled in. The canonical
// handlers_test.go filename is reserved for the user. A nil cs is tolerated.
func GenerateMissingHandlerStubs(svc ServiceDef, projectDir, targetDir string, crudMethodNames map[string]bool, cs *checksums.FileChecksums) (*MissingHandlerResult, error) {
	existing, err := scanExistingMethods(targetDir, false)
	if err != nil {
		return nil, fmt.Errorf("scan existing methods: %w", err)
	}

	var missing []Method
	for _, m := range svc.Methods {
		if !existing[m.Name] && !crudMethodNames[m.Name] {
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
	data := mapServiceDefToTemplateData(missingSvc, projectDir)

	content, err := templates.ServiceTemplates().Render("handlers_gen.go.tmpl", data)
	if err != nil {
		return nil, fmt.Errorf("render handlers_gen.go.tmpl: %w", err)
	}

	relHandlersGen, err := filepath.Rel(projectDir, handlersGenPath)
	if err != nil {
		return nil, fmt.Errorf("compute relative path for handlers_gen.go: %w", err)
	}
	if _, err := checksums.WriteGeneratedFile(projectDir, relHandlersGen, content, cs, true); err != nil {
		return nil, err
	}

	// If integration_test.go / handlers_scaffold_test.go are still placeholders (no RPCs when
	// first generated), regenerate them with actual test scaffolding now that RPCs exist.
	// These files become user-owned after the placeholder is filled in, so we
	// don't checksum them — we want forge audit to leave them alone.
	fullData := mapServiceDefToTemplateData(svc, projectDir)

	integrationTestPath := filepath.Join(targetDir, "integration_test.go")
	if isPlaceholderIntegrationTest(integrationTestPath) {
		testContent, err := templates.ServiceTemplates().Render("integration_test.go.tmpl", fullData)
		if err != nil {
			return nil, fmt.Errorf("render integration_test.go.tmpl: %w", err)
		}
		if err := os.WriteFile(integrationTestPath, testContent, 0644); err != nil {
			return nil, fmt.Errorf("write integration_test.go: %w", err)
		}
	}

	handlersTestPath := filepath.Join(targetDir, "handlers_scaffold_test.go")
	if isPlaceholderUnitTest(handlersTestPath) {
		testContent, err := templates.ServiceTemplates().Render("unit_test.go.tmpl", fullData)
		if err != nil {
			return nil, fmt.Errorf("render unit_test.go.tmpl: %w", err)
		}
		if err := os.WriteFile(handlersTestPath, testContent, 0644); err != nil {
			return nil, fmt.Errorf("write handlers_scaffold_test.go: %w", err)
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

// isPlaceholderUnitTest checks if handlers_scaffold_test.go is still the auto-generated
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
		if !includeGeneratedStubs && (entry.Name() == "handlers_gen.go" || entry.Name() == "handlers_crud_gen.go") {
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

	content, err := templates.ProjectTemplates().Render("setup.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render setup.go.tmpl: %w", err)
	}

	return os.WriteFile(setupPath, content, 0644)
}

// hasFallibleConstructor returns true if any service, package, worker, operator, or function has a fallible constructor.
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

// toServicePackage converts a proto service name like "EchoService" to its
// Go-package form ("echo"). Multi-word PascalCase names are snake-cased so the
// result is a valid Go identifier matching the dir created at scaffold time
// (e.g. "AdminServerService" -> "admin_server" matches handlers/admin_server/).
// Hyphens (which can appear when downstream callers feed in the CLI form
// directly) are also normalized to underscores.
func toServicePackage(name string) string {
	trimmed := strings.TrimSuffix(name, "Service")
	if trimmed == "" {
		trimmed = name
	}
	// ToSnakeCase handles PascalCase ("AdminServer" -> "admin_server") and
	// preserves all-lowercase input ("orders" -> "orders"). Replace any
	// stray hyphens defensively in case a caller passed in a CLI name.
	return strings.ReplaceAll(naming.ToSnakeCase(trimmed), "-", "_")
}