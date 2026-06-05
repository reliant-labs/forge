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
	// HasLogger / HasConfig record whether this component's Deps struct
	// declares a Logger / Config field. The bootstrap template gates the
	// emission of those Deps-literal fields on these flags so a package
	// that doesn't consume them isn't forced to carry vestigial Logger /
	// Config fields just to keep the generated New(Deps{...}) call site
	// type-checking. Populated by inspectComponentDepsShape before
	// rendering; defaults to false (skip) when the source dir can't be
	// parsed (e.g. just-scaffolded component with no Deps yet).
	HasLogger bool
	HasConfig bool
	// AppFieldRefs lists Deps fields (other than Logger/Config) whose
	// names AND types match an AppExtras field. Bootstrap emits one
	// assignment per entry like `<DepsField>: app.<DepsField>`. Without
	// this, audit.New got only {Logger} even when audit.Deps.Repo and
	// app.Repo both existed — the package silently degraded (Log warn-
	// and-drops) until the next forge generate cycle. wire_gen has had
	// this logic for services; this brings packages to parity.
	AppFieldRefs []AppFieldRef
	// ConnectPkg is the import alias of the generated Connect package for
	// this service (e.g. "echov1connect") — used by the bootstrap template
	// to reference the `<X>ServiceName` constant when building vanguard
	// REST services. Only populated for services and only when
	// `api.rest: true`; empty for non-services or when REST is off.
	ConnectPkg string
	// ProtoServiceName is the PascalCase proto service identifier (e.g.
	// "EchoService") used to look up the `<ProtoServiceName>Name` constant
	// in the connect-generated package. Combined with ConnectPkg, the
	// bootstrap template emits `<connectPkg>.<ProtoServiceName>Name` as
	// the Connect path passed to vanguard.NewService.
	ProtoServiceName string
}

// AppFieldRef pairs a package Deps field name with the app.<name>
// expression bootstrap should emit for it. Only emitted when the
// AppExtras field type EXACTLY matches the Deps field type (otherwise
// the compile fails with funding.Repository vs *db.PostgresRepository
// style mismatches).
type AppFieldRef struct {
	DepsField string // e.g. "Repo"
}

// Type aliases for backward compatibility and readability.
type BootstrapServiceData = BootstrapComponentData
type BootstrapPackageData = BootstrapComponentData
type BootstrapWorkerData = BootstrapComponentData

// inspectComponentDepsShape walks each component's source directory under
// roleRoot (e.g. "internal", "workers", "operators") and populates
// HasLogger / HasConfig / AppFieldRefs from the parsed Deps struct +
// AppExtras AST. A missing source dir or unparseable file falls through
// to defaults: best-effort, errors-as-default.
//
// Mutates each component in place so callers don't have to thread a
// result slice through bootstrap data assembly.
func inspectComponentDepsShape(components []BootstrapComponentData, projectDir, roleRoot string) {
	// Resolve AppExtras field types once for the whole batch so each
	// component can name-and-type-match without re-parsing pkg/app.
	appFields, _ := ParseAppFields(filepath.Join(projectDir, "pkg", "app"))
	appFieldTypes := map[string]string{}
	for _, f := range appFields {
		appFieldTypes[f.Name] = f.Type
	}

	for i := range components {
		dir := filepath.Join(projectDir, roleRoot, components[i].ImportPath)
		fields, err := ParseServiceDeps(dir)
		if err != nil || len(fields) == 0 {
			continue
		}
		for _, f := range fields {
			switch f.Name {
			case "Logger":
				// Logger is the project's *slog.Logger. Gate emission
				// on the declared type matching to avoid stomping on a
				// package-local Logger type.
				if f.Type == "" || f.Type == "*slog.Logger" {
					components[i].HasLogger = true
				}
			case "Config":
				// HasConfig gates emission of `Config: cfg` in the
				// bootstrap template — but `cfg` is the project's
				// `*config.Config`. If the package declares a
				// domain-local Config (e.g. enforcement.Config) the
				// emit would produce a hard type-mismatch at codegen
				// time. Gate on the type-string matching the project
				// config so a domain Config field gets the typed-zero
				// default and the user wires it manually in setup.go
				// (or via an AppExtras field that matches the type).
				// FRICTION 2026-06-02: cp-forge layer-2 enforcement.
				if f.Type == "" || f.Type == "*config.Config" {
					components[i].HasConfig = true
				} else {
					// Domain-local Config — treat like any other field
					// and let the name+exact-type matcher decide.
					appType, hasName := appFieldTypes[f.Name]
					if hasName && appType == f.Type {
						components[i].AppFieldRefs = append(components[i].AppFieldRefs, AppFieldRef{DepsField: f.Name})
					}
				}
			default:
				// Name-match AND exact type-match. Without strict type
				// comparison the wire would emit `Repo: app.Repo` even
				// when funding.Deps.Repo is funding.Repository (narrow
				// domain interface) and app.Repo is *db.PostgresRepository
				// — type-fails at compile time. Skipping unmatched-type
				// fields preserves the typed-zero default the Deps zero
				// value provides; the user can either change app.Repo's
				// declared type to match the narrow interface OR write a
				// setup.go adapter — both surfaced clearly by the
				// bootstrap-deps-coverage lint when name matches but
				// type doesn't.
				appType, hasName := appFieldTypes[f.Name]
				if hasName && appType == f.Type {
					components[i].AppFieldRefs = append(components[i].AppFieldRefs, AppFieldRef{DepsField: f.Name})
				}
			}
		}
	}
}

// WorkerDataFromNames builds BootstrapWorkerData from worker names (e.g. from forge.yaml).
// projectDir is the root project directory; if non-empty, it is used to detect fallible constructors.
// Hyphens and underscores in the user-facing name are stripped for Package (the on-disk
// directory + Go package name) so `calibrator_refit` becomes `calibratorrefit`, matching
// Go style. FieldName is derived from the ORIGINAL name (which retains its separators)
// via ToPascalCase so snake_case worker names still produce idiomatic exported identifiers
// (`Workers.CalibratorRefit`, `wireWorkerCalibratorRefitDeps`) rather than the
// run-together `Workers.Calibratorrefit` shape ToPascalCase would otherwise emit.
func WorkerDataFromNames(names []string, projectDir string) []BootstrapWorkerData {
	var workers []BootstrapWorkerData
	for _, name := range names {
		pkg := toGoPackage(name)
		fieldName := naming.ToPascalCase(name)
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
// See WorkerDataFromNames for the FieldName naming rationale (same snake_case → PascalCase rule).
func OperatorDataFromNames(names []string, projectDir string) []BootstrapOperatorData {
	var operators []BootstrapOperatorData
	for _, name := range names {
		pkg := toGoPackage(name)
		fieldName := naming.ToPascalCase(name)
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
// identifier: lowercase with both hyphen and underscore separators stripped
// (so "calibrator_refit" becomes "calibratorrefit", matching Go style). This
// mirrors generator.ServicePackageName but is duplicated here to keep codegen
// free of a generator dependency (the generator package already imports codegen).
func toGoPackage(name string) string {
	return toGoPackageReplacer.Replace(strings.ToLower(name))
}

// toGoPackageReplacer mirrors generator.servicePackageReplacer — see that
// var's comment for why both separators are stripped.
var toGoPackageReplacer = strings.NewReplacer("-", "", "_", "")

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
// ToPascalCase; workers/operators also use ToPascalCase so snake_case
// names produce idiomatic exported identifiers (`Workers.CalibratorRefit`
// rather than `Workers.Calibrator_refit`); nested packages use a
// path-encoded form via ToExportedFieldName).
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
		// branch keeps whatever the caller computed (services and
		// workers/operators use ToPascalCase; packages use
		// ToExportedFieldName for nested support; honoring those preserves
		// nested-package field names like "McpDatabase").
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
// BootstrapFeatures carries the per-project feature toggles that
// influence what bootstrap.go's body emits. Added as a struct (not yet
// another bool) so future feature flags can land additively without
// re-shaping the GenerateBootstrap signature; today the struct only
// carries diagnostics + strict-wiring, but the same pattern absorbs
// later flags cleanly.
type BootstrapFeatures struct {
	// DiagnosticsEnabled is true when the project opts in to
	// `features.diagnostics: true`. Drives whether the template emits
	// the diagnostics.Default.Boot(emitter) call after Setup. Default
	// false so existing projects don't suddenly start logging warns on
	// regen.
	DiagnosticsEnabled bool

	// StrictWiringEnabled is true when the project opts in to
	// `features.strict_wiring: true`. Implies DiagnosticsEnabled at the
	// wire site — strict-mode wraps the LogEmitter in StrictEmitter so
	// any registered diagnostic exits the process after the summary
	// line. Default false.
	StrictWiringEnabled bool
}

func GenerateBootstrap(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, hasDatabase bool, ormEnabled bool, projectDir string, configFields map[string]bool, webhookServices map[string]bool, features BootstrapFeatures, cs *checksums.FileChecksums) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	restEnabled := projectAPIRESTEnabled(projectDir)

	var bootstrapSvcs []BootstrapServiceData
	var connectImports []string
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		fallible, _ := DetectFallibleConstructor(filepath.Join(projectDir, "handlers", pkg))
		// FieldName mirrors the PascalCase proto-service name without the
		// "Service" suffix ("AdminServerService" -> "AdminServer"). We derive
		// it from svc.Name (which retains separators / PascalCase boundaries)
		// rather than from pkg, because pkg is the compact form
		// ("adminserver") that ToPascalCase can't split back into words.
		fieldName := naming.ToPascalCase(strings.TrimSuffix(svc.Name, "Service"))
		if fieldName == "" {
			fieldName = naming.ToPascalCase(svc.Name)
		}
		// Runtime name is the kebab form of the original svc.Name (proto
		// PascalCase) — matches what cobra subcommands pass to runServer
		// (cmd-shared-service.go.tmpl uses {{.ServiceName}} which is the
		// kebab form from forge.yaml). Pre-2026-04-30 this was derived
		// from the snake-case package name, which silently dropped the cobra
		// invocation under shared-binary mode (admin-server vs admin_server).
		// The kebab form preserves the original word boundaries that the
		// compact pkg form loses.
		runtimeName := naming.ToKebabCase(strings.TrimSuffix(svc.Name, "Service"))
		// ConnectPkg + ProtoServiceName drive the vanguard.NewService call
		// site when REST is enabled. The proto service name is the proto
		// handler name (e.g. "EchoService") which matches the `<X>Name`
		// constant exposed by connect-generated packages.
		//
		// Prefer the proto's declared go_package + PkgName so a service
		// whose proto lives outside the `services/<svc>/v1` convention
		// (e.g. several services collected in a single shared.proto under
		// gen/reliant/v1) still emits the correct *v1connect import. Fall
		// back to the convention only when both descriptor fields are
		// empty — synthetic test fixtures and pre-descriptor scaffolds.
		var connectPkg, connectImport string
		if svc.GoPackage != "" && svc.PkgName != "" {
			connectPkg = svc.PkgName + "connect"
			connectImport = svc.GoPackage + "/" + connectPkg
		} else {
			connectPkg = pkg + "v1connect"
			connectImport = modulePath + "/gen/services/" + pkg + "/v1/" + connectPkg
		}
		protoServiceName := fieldName + "Service"
		if restEnabled {
			connectImports = append(connectImports, connectImport)
		}
		bootstrapSvcs = append(bootstrapSvcs, BootstrapServiceData{
			Name:       runtimeName,
			Package:    pkg,
			ImportPath: pkg,
			// Use ToPascalCase so multi-word service packages produce the
			// same exported field name as the unit/integration test templates,
			// which call `app.NewTest{{.ServiceName | pascalCase}}` (e.g.
			// "admin_server" -> "AdminServer").
			FieldName:        fieldName,
			VarName:          lowerFirst(fieldName),
			Fallible:         fallible,
			Alias:            pkg,
			HasWebhooks:      webhookServices[pkg],
			ConnectPkg:       connectPkg,
			ProtoServiceName: protoServiceName,
		})
	}

	// Resolve cross-role import-alias collisions (e.g. service "billing"
	// vs internal package "billing"). When unique, Alias == Package and
	// the bootstrap import line + symbol references emit unchanged.
	AssignBootstrapAliases(bootstrapSvcs, packages, workers, operators)

	// Probe each component's Deps struct so the bootstrap template can
	// emit only the Logger / Config fields that actually exist, and
	// auto-wire AppExtras-name-and-type-matching fields. Without this,
	// the template's hardcoded `Logger: ..., Config: cfg` lines force
	// every internal package to declare those fields even when the
	// package doesn't read them (the "Deps shape coupling" friction from
	// the control-plane migration), and the audit-no-op silent-drop fires
	// when audit.Deps.Repo and AppExtras.Repo both exist but bootstrap
	// emits only Logger.
	inspectComponentDepsShape(packages, projectDir, "internal")
	inspectComponentDepsShape(workers, projectDir, "workers")
	inspectComponentDepsShape(operators, projectDir, "operators")

	hasFallible := hasFallibleConstructor(bootstrapSvcs, packages, workers, operators)

	if configFields == nil {
		configFields = DefaultConfigFieldNames()
	}

	binaryShared := projectBinaryShared(projectDir)

	data := struct {
		Module              string
		Services            []BootstrapServiceData
		Packages            []BootstrapPackageData
		Workers             []BootstrapWorkerData
		Operators           []BootstrapOperatorData
		HasDatabase         bool
		OrmEnabled          bool
		HasFallible         bool
		BinaryShared        bool
		ConfigFields        map[string]bool
		RESTEnabled         bool
		ConnectImports      []string
		DiagnosticsEnabled  bool
		StrictWiringEnabled bool
	}{
		Module:              modulePath,
		Services:            bootstrapSvcs,
		Packages:            packages,
		Workers:             workers,
		Operators:           operators,
		HasDatabase:         hasDatabase,
		OrmEnabled:          ormEnabled,
		HasFallible:         hasFallible,
		BinaryShared:        binaryShared,
		ConfigFields:        configFields,
		RESTEnabled:         restEnabled,
		ConnectImports:      connectImports,
		DiagnosticsEnabled:  features.DiagnosticsEnabled,
		StrictWiringEnabled: features.StrictWiringEnabled,
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
		RESTEnabled bool
	}{
		HasDatabase: hasDatabase,
		OrmEnabled:  ormEnabled,
		Services:    hasServices,
		Workers:     hasWorkers,
		Operators:   hasOperators,
		Packages:    hasPackages,
		RESTEnabled: hasServices && projectAPIRESTEnabled(projectDir),
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
	// AutoStubs lists the per-Deps-field synthesized interface stubs the
	// template should emit and inject as defaults inside NewTest<Svc>.
	// Each entry corresponds to a Deps field whose Go type is an interface
	// declared locally in the handler package (so the testing.go file can
	// reference the interface via the imported handler package alias).
	// Optional-dep fields are excluded — those stay nil to preserve the
	// "graceful dev-mode degrade" semantics. See ParseLocalInterfaces +
	// HasOptionalDepMarker for the detection rules.
	AutoStubs []DepsAutoStub
	// UnresolvedStubs lists Deps fields whose type is a cross-package
	// selector forge couldn't resolve (alias not in imports, package
	// can't load, type isn't an interface). The template emits a
	// `// TODO: stub <type>` line next to NewTest<Svc> so the user
	// sees a visible reminder to hand-roll an override via
	// With<Svc>Deps(...). Empty when every selector resolved cleanly.
	UnresolvedStubs []UnresolvedAutoStub
}

// UnresolvedAutoStub is one Deps field whose cross-package selector
// type couldn't be turned into a synthesized stub. Surfaces in the
// generated testing.go as a TODO comment so the user knows to
// hand-roll an override.
type UnresolvedAutoStub struct {
	// FieldName is the Deps field name as declared.
	FieldName string
	// TypeExpr is the unresolved type expression as written in the
	// Deps struct (e.g. "external.Client").
	TypeExpr string
}

// DepsAutoStub describes one synthesized interface implementation
// emitted into the generated pkg/app/testing.go for a service-owned
// Deps field. The stub satisfies the field's interface with zero-value
// returns; it exists so NewTest<Svc>(t) can construct the Service even
// when the field is required by validateDeps. Tests that exercise real
// behavior continue to override via With<Svc>Deps(...).
type DepsAutoStub struct {
	// FieldName is the Deps field name as declared (e.g. "Repo").
	FieldName string
	// StubType is the unqualified Go identifier the template should use
	// for the synthesized stub struct (e.g. "stubApiRepo"). Generated
	// from the service alias + field name so two services with the same
	// Deps-field name don't collide at the package level.
	StubType string
	// InterfaceQualified is the package-qualified type expression used
	// when injecting the stub into Deps in NewTest<Svc>.
	//
	// For locally-declared interfaces this carries the literal "<alias>."
	// placeholder so the caller can substitute the post-collision
	// service alias ("svcBilling" vs "billing"). For cross-package
	// interfaces (CrossPackage = true) the prefix is already the
	// declaring package's alias (e.g. "repo.Repository") and must NOT
	// be re-aliased — the service alias is irrelevant to it.
	InterfaceQualified string
	// Methods are the interface's flattened method set rendered for
	// the template's stub-emit loop.
	Methods []InterfaceMethod
	// CrossPackage flags stubs whose interface lives in a package
	// other than the handler's. The caller uses this to skip the
	// "<alias>." rewrite and to fold the stub's ExtraImports into
	// the file's import block.
	CrossPackage bool
	// ExtraImports lists every package the stub's method signatures
	// reference (including the interface's own package). Only populated
	// when CrossPackage = true. The bootstrap_testing assembler
	// deduplicates these across stubs into the top-level
	// ExtraImports field on the template data.
	ExtraImports []ExtraImport
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
		// Auto-stub: walk the service's Deps fields, find any interface-
		// typed required fields and queue a synthesized stub. Handles
		// both locally-declared interfaces AND cross-package selector
		// types (e.g. repo.Repository) — the selector path loads the
		// imported package via go/types and walks its method set. The
		// bare-Deps trio (Logger / Config / Authorizer) and the DB
		// orm.Context field are handled by the existing default fill
		// so they're excluded from auto-stubbing here.
		//
		// unresolvedStubs surfaces selector types we couldn't resolve
		// (package failed to load, alias not in imports, type isn't
		// an interface) so the template can emit a TODO comment.
		autoStubs, unresolvedStubs := computeAutoStubs(handlerDir, pkg)
		// Derive Connect package path/name from the proto's declared
		// go_package + PkgName instead of guessing from the service name.
		// This is what lets a service whose proto moved (e.g. from
		// services/daemon_token/v1 to reliant/v1) regenerate testing.go
		// with the correct *v1connect import — the convention path
		// would still point at the old gen/services/<svc>/v1 location.
		//
		// Falls back to the convention path only when BOTH descriptor
		// fields are empty (synthetic test fixtures, pre-descriptor
		// scaffolds). Falling back on GoPackage alone left connectPkg as
		// the literal "connect" when PkgName happened to be empty.
		connectPkg := svc.PkgName + "connect"
		connectImport := svc.GoPackage + "/" + connectPkg
		if svc.GoPackage == "" || svc.PkgName == "" {
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
			AutoStubs:              autoStubs,
			UnresolvedStubs:        unresolvedStubs,
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
		// Rewrite the AutoStubs' qualified interface refs to use the
		// post-collision alias. The unqualified stub-type identifier
		// already carries an UpperCamel(Package) prefix so collisions
		// across services are impossible by construction.
		//
		// CrossPackage stubs skip this rewrite — their interface lives
		// in a different package whose alias has nothing to do with
		// this service's alias. Their InterfaceQualified is already
		// the resolved "<pkg>.<TypeName>" form from
		// ResolveCrossPkgInterface.
		alias := testSvcs[i].Alias
		for j, stub := range testSvcs[i].AutoStubs {
			if stub.CrossPackage {
				continue
			}
			// Replace the placeholder "<alias>." prefix with the resolved
			// alias. computeAutoStubs writes "<alias>." literally so the
			// rewrite is a single string-replace per field.
			testSvcs[i].AutoStubs[j].InterfaceQualified = strings.Replace(stub.InterfaceQualified,
				"<alias>.", alias+".", 1)
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
	// Probe each package's Deps AST so testing.go emits the same Deps
	// shape as bootstrap.go. Without this call, removing Logger from a
	// package's Deps regenerates bootstrap.go cleanly but breaks
	// testing.go (the v2 migration of control-plane reproduced this).
	inspectComponentDepsShape(pkgsCopy, projectDir, "internal")
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

	// Collect the union of every cross-package import any auto-stub
	// needs. The bootstrap_testing template emits these in a dedicated
	// import block so the rendered file can reference cross-package
	// interface types and their method-signature dependencies without
	// the user wiring up the import by hand. Deterministic ordering is
	// preserved by SortedNeededImports — the union is then re-sorted
	// here so the final file stays diff-stable.
	extraImports := mergeExtraImports(testSvcs)

	data := struct {
		Module             string
		Services           []BootstrapTestServiceData
		ConnectImports     []string
		Packages           []BootstrapPackageData
		MultiTenantEnabled bool
		AnyServiceHasDB    bool
		ExtraImports       []ExtraImport
	}{
		Module:             modulePath,
		Services:           testSvcs,
		ConnectImports:     connectImports,
		Packages:           packages,
		MultiTenantEnabled: multiTenantEnabled,
		AnyServiceHasDB:    anyServiceHasDB,
		ExtraImports:       extraImports,
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

// mergeExtraImports folds every cross-package stub's ExtraImports
// into one deterministic deduplicated slice. Path is the dedupe key —
// two stubs that both depend on "x/y/z" produce a single import line.
// Conflict on the alias (same path with different aliases across two
// stubs) is resolved first-wins, matching the order computeAutoStubs
// returns and the import-collection inside ResolveCrossPkgInterface
// (which uses the imported package's declared name, so a conflict
// would already be a real package-rename collision the user would
// have to resolve regardless).
func mergeExtraImports(services []BootstrapTestServiceData) []ExtraImport {
	seen := map[string]string{}
	for _, s := range services {
		for _, stub := range s.AutoStubs {
			if !stub.CrossPackage {
				continue
			}
			for _, imp := range stub.ExtraImports {
				if _, ok := seen[imp.Path]; ok {
					continue
				}
				seen[imp.Path] = imp.Alias
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]ExtraImport, 0, len(seen))
	for path, alias := range seen {
		out = append(out, ExtraImport{Path: path, Alias: alias})
	}
	// Deterministic order — codegen must be reproducible for the
	// checksums-based "no spurious diff" guarantee. SortedNeededImports
	// sorts by Path; we do the same here so the merged slice stays
	// in lockstep with the per-stub view.
	sortExtraImports(out)
	return out
}

// sortExtraImports puts an ExtraImport slice in canonical Path order.
// Factored out of mergeExtraImports so the tests can reuse it on
// hand-built slices without re-running the merge.
func sortExtraImports(s []ExtraImport) {
	// import "sort" once; the codegen package already imports it.
	if len(s) < 2 {
		return
	}
	// Trivial insertion sort: codegen runs are dominated by I/O and
	// template execution, the import list is typically <10 elements,
	// and we want to avoid pulling another import for this one call.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Path > s[j].Path; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// bareDepsFieldNames is the set of Deps field names the bootstrap_testing
// template fills in by default — these never need an auto-stub. Keep
// the list narrow: anything else (Repo, Audit, Stripe, Email, ...)
// flows through the auto-stub path when its type is a local interface.
var bareDepsFieldNames = map[string]bool{
	"Logger":     true,
	"Config":     true,
	"Authorizer": true,
	"DB":         true,
}

// computeAutoStubs walks a service's handler directory, parses its
// Deps struct + locally-declared interfaces, and returns one
// DepsAutoStub for every required Deps field whose type is an
// interface forge can satisfy with a zero-value stub. The bare-Deps
// trio (Logger / Config / Authorizer) and the optional-dep set are
// excluded — those are either filled in by the template's existing
// default-merge step or deliberately left nil.
//
// Two type shapes are handled:
//
//  1. Bare-identifier types ("Repository") that resolve to an
//     interface declared in the handler package. InterfaceQualified
//     is "<alias>." + name so the caller can substitute the
//     post-collision service alias.
//
//  2. Selector types ("repo.Repository") that resolve to an
//     interface in an imported package. ResolveCrossPkgInterface
//     does the heavy lifting (alias → import path → go/packages
//     load → types.Interface walk). On success, the stub carries
//     CrossPackage = true and ExtraImports listing every package
//     its method signatures reference.
//
// Unresolvable selector types (alias mismatch, package can't load,
// type isn't an interface) fall through to the existing
// "field stays nil" behavior. This is the soft-fail design: tests
// that hit a nil dependency see the usual nil-deref, the user
// either overrides the field via With<Svc>Deps or hand-rolls a
// stub — both are existing escape valves.
func computeAutoStubs(handlerDir, _ string) ([]DepsAutoStub, []UnresolvedAutoStub) {
	fields, err := ParseServiceDeps(handlerDir)
	if err != nil || len(fields) == 0 {
		return nil, nil
	}
	locals, _ := ParseLocalInterfaces(handlerDir)
	var stubs []DepsAutoStub
	var unresolved []UnresolvedAutoStub
	for _, f := range fields {
		if bareDepsFieldNames[f.Name] {
			continue
		}
		if f.Optional {
			continue
		}
		t := strings.TrimSpace(f.Type)

		// (1) Bare-identifier interface declared in this package.
		if iface, ok := locals[t]; ok {
			stubs = append(stubs, DepsAutoStub{
				FieldName:          f.Name,
				StubType:           "", // resolved below from handlerDir's package
				InterfaceQualified: "<alias>." + iface.Name,
				Methods:            iface.Methods,
			})
			continue
		}

		// (2) Selector type — resolve across the import boundary.
		// We only handle the simple `<pkg>.<TypeName>` shape; pointer
		// (`*pkg.T`), slice, map, and chan decorations on an interface
		// are not idiomatic and stay on the hand-roll path.
		if dot := strings.IndexByte(t, '.'); dot > 0 && !strings.ContainsAny(t, "*[]<>(){}") {
			pkgAlias := t[:dot]
			typeName := t[dot+1:]
			res, ok := ResolveCrossPkgInterface(handlerDir, pkgAlias, typeName)
			if !ok {
				// Soft-fail: record the selector so the template can
				// surface a TODO comment. The field still stays nil
				// at construction time — the comment exists purely to
				// nudge the user toward With<Svc>Deps overrides or
				// a hand-rolled stub.
				unresolved = append(unresolved, UnresolvedAutoStub{
					FieldName: f.Name,
					TypeExpr:  t,
				})
				continue
			}
			stubs = append(stubs, DepsAutoStub{
				FieldName:          f.Name,
				StubType:           "", // resolved below
				InterfaceQualified: res.PackageName + "." + typeName,
				Methods:            res.Methods,
				CrossPackage:       true,
				ExtraImports:       SortedNeededImports(res.NeededImports),
			})
		}
	}
	// Make stub-type names predictable + collision-free across the
	// file: stub<UpperPackage><FieldName>. handlerDir's last segment
	// IS the service Go-package name (toServicePackage), so use it
	// directly. The CrossPackage flag does not affect the stub-type
	// identifier — it lives in pkg/app, not in the imported package,
	// so the service-name prefix still gives us per-service uniqueness.
	pkg := filepath.Base(handlerDir)
	for i := range stubs {
		stubs[i].StubType = "stub" + upperFirst(pkg) + stubs[i].FieldName
	}
	return stubs, unresolved
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

	// Filter CRUD methods out of the unit-test scaffold so per-RPC rows
	// don't overlap with handlers_crud_gen_test.go (which owns shape-aware
	// per-CRUD-RPC rows). Same filter rule as the initial-gen path in
	// GenerateServiceStub — one source of truth per method, no duplication.
	unitTestData := fullData
	if len(crudMethodNames) > 0 {
		var nonCRUD []MethodTemplateData
		for _, m := range fullData.Methods {
			if !crudMethodNames[m.Name] {
				nonCRUD = append(nonCRUD, m)
			}
		}
		unitTestData.Methods = nonCRUD
	}

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
		testContent, err := templates.ServiceTemplates().Render("unit_test.go.tmpl", unitTestData)
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

// scanExistingMethods reads all .go files in dir and returns a set of
// method names that are already implemented on *Service. It uses
// go/parser so that multi-line receivers, comments, and strings
// containing "*Service" are handled correctly.
//
// This is the dedup that lets a user's `handlers.go` claim a method
// (e.g. `func (s *Service) CreateUser(...) ...`) and have the next
// `forge generate` automatically drop the matching stub from
// `handlers_gen.go`. Same shape closes the FORGE_REVIEW_PROCESS.md §2.3
// git_credential drift class — gen-files and user-files share the
// `*Service` receiver, so a method declared in either is sufficient
// signal that the proto RPC is implemented.
//
// An individual file that fails to parse is skipped with a warning
// rather than failing the whole pass: a transient syntax error in a
// sibling file must not brick the dedup for the entire package, since
// losing dedup means the user's just-written `CreateUser` would be
// re-stubbed in handlers_gen.go and the package would fail to compile
// (duplicate method).
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
			// Soft-skip on per-file parse errors so a transient syntax
			// error elsewhere in the package doesn't unwind the dedup
			// — see func doc.
			fmt.Fprintf(os.Stderr, "Warning: scanExistingMethods skipping %s (parse error): %v\n", path, err)
			continue
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

// GeneratePostBootstrap writes pkg/app/post_bootstrap.go ONCE — it's a
// Tier-3 user-owned scaffold whose default body is a no-op. Users own
// the file after first emit and forge generate never overwrites it
// (same rule as GenerateSetup and GenerateAppExtras).
//
// The hook exists so projects can run wiring that depends on a
// constructed component (e.g. setting a snapshot saver onto a
// concrete worker singleton); wire_gen only resolves Deps fields, so
// post-construct registrations can't live in Setup.
//
// cmd/server.go.tmpl calls `app.PostBootstrap(application)` after
// Bootstrap returns and propagates any returned error as a fatal boot
// failure.
func GeneratePostBootstrap(targetDir string) error {
	appDir := filepath.Join(targetDir, "pkg", "app")
	hookPath := filepath.Join(appDir, "post_bootstrap.go")

	// Never overwrite — this is user-owned code.
	if _, err := os.Stat(hookPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	content, err := templates.ProjectTemplates().Render("post_bootstrap.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render post_bootstrap.go.tmpl: %w", err)
	}

	return os.WriteFile(hookPath, content, 0644)
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
// Go-package form ("echo"). Multi-word PascalCase names compact to a single
// lowercase identifier matching the dir created at scaffold time
// (e.g. "AdminServerService" -> "adminserver" matches handlers/adminserver/).
// Hyphens / underscores (which can appear when downstream callers feed in the
// CLI form directly) are also stripped.
//
// This must agree with generator.ServicePackageName for any single service so
// that bootstrap/codegen keys and the on-disk handler directory match. See the
// matching toGoPackageReplacer for the separator policy.
func toServicePackage(name string) string {
	trimmed := strings.TrimSuffix(name, "Service")
	if trimmed == "" {
		trimmed = name
	}
	// ToSnakeCase handles PascalCase ("AdminServer" -> "admin_server"); strip
	// the resulting separators so we match ServicePackageName's compact form.
	return toGoPackage(naming.ToSnakeCase(trimmed))
}