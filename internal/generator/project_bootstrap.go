package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateBootstrap scaffolds the minimal pkg/app substrate
// (app_gen.go + app_extras.go) at `forge new` time.
//
// FORGE_SHAPE_REDESIGN §2: the LIVE runtime DI composition is the
// internal/app layer (OpenInfra -> Build -> PostBuild -> Inventory),
// emitted by the post-scaffold `forge generate`. The old name-matched
// pkg/app DI unit (bootstrap.go / wire_gen.go / services_gen.go /
// services.go) is retired. All this scaffold step needs to emit is the
// minimal *App carrier the user-owned setup.go compiles against;
// app_extras.go is the (user-owned, never-overwritten) extension shell.
func (g *ProjectGenerator) generateBootstrap() error {
	hasServices := g.ServiceName != "" || len(g.AdditionalServices) > 0
	if err := g.generateAppGen(hasServices, false, false, false); err != nil {
		return fmt.Errorf("generate app_gen.go: %w", err)
	}
	if err := g.generateAppExtras(); err != nil {
		return fmt.Errorf("generate app_extras.go: %w", err)
	}
	return nil
}

// generateBootstrapTesting writes pkg/app/testing.go with test helper functions.
func (g *ProjectGenerator) generateBootstrapTesting() error {
	type autoStub struct {
		FieldName          string
		StubType           string
		InterfaceQualified string
		Methods            []struct {
			Name            string
			Params          string
			Results         string
			ReturnStatement string
		}
	}
	type unresolvedStub struct {
		FieldName string
		TypeExpr  string
	}
	type bootstrapTestService struct {
		Name                   string
		Package                string
		ImportPath             string // handlers/ dir leaf; see generateBootstrap's bootstrapService
		FieldName              string
		ProtoServiceName       string
		ProtoConnectImportPath string
		ProtoConnectPkg        string
		Fallible               bool
		HasDB                  bool
		// HasAuthorizer mirrors codegen.BootstrapTestServiceData: a freshly
		// scaffolded service's Deps declares an Authorizer (see
		// templates/service/service.go.tmpl), so the authz-aware test harness
		// wires it. The post-codegen GenerateBootstrapTesting pass re-derives
		// this from the on-disk Deps once the service has grown/shrunk fields.
		HasAuthorizer bool
		Alias         string
		VarName       string
		// AutoStubs is always empty at the project-scaffold step; the
		// service has no Deps fields beyond the bare-Deps trio at this
		// point. The post-codegen GenerateBootstrapTesting pass populates
		// it once handlers/<svc>/service.go exists.
		AutoStubs       []autoStub
		UnresolvedStubs []unresolvedStub
	}

	type bootstrapPackage struct {
		Name       string
		Package    string
		ImportPath string
		FieldName  string
		Fallible   bool
		Alias      string
		VarName    string
	}

	var services []bootstrapTestService
	var connectImports []string
	if g.ServiceName != "" {
		pkg := naming.ServicePackage(g.ServiceName)
		fieldName := naming.ToPascalCase(g.ServiceName)
		// ProtoServiceName matches what the proto template emits:
		// `service {{.ServiceName | pascalCase}}Service` (PascalCase handles hyphens).
		protoServiceName := naming.ToPascalCase(g.ServiceName) + "Service"
		// Project bootstrap is the first scaffold pass before any descriptor
		// exists; use the convention path. The codegen-pass regenerator will
		// later replace this file with descriptor-derived imports.
		connectPkg := pkg + "v1connect"
		connectImport := g.ModulePath + "/gen/services/" + pkg + "/v1/" + connectPkg
		services = []bootstrapTestService{
			{
				Name:                   g.ServiceName,
				Package:                pkg,
				ImportPath:             pkg,
				FieldName:              fieldName,
				ProtoServiceName:       protoServiceName,
				ProtoConnectImportPath: connectImport,
				ProtoConnectPkg:        connectPkg,
				// A scaffolded service includes an Authorizer dep by default.
				HasAuthorizer: true,
				Alias:         pkg,
				VarName:       lowerFirstRune(fieldName),
			},
		}
		connectImports = []string{connectImport}
	}

	// extraImport mirrors codegen.ExtraImport. We can't pull the codegen
	// type directly (the generator package is upstream of codegen in the
	// build graph), but the template only reads .Alias / .Path so a
	// structurally-identical local type works. The initial-scaffold
	// pass never has cross-package auto-stubs, so this stays nil here.
	type extraImport struct {
		Alias string
		Path  string
	}

	data := struct {
		Module             string
		Services           []bootstrapTestService
		ConnectImports     []string
		Packages           []bootstrapPackage
		MultiTenantEnabled bool
		AnyServiceHasDB    bool
		ExtraImports       []extraImport
	}{
		Module:             g.ModulePath,
		Services:           services,
		ConnectImports:     connectImports,
		Packages:           nil,   // No packages at initial project creation
		MultiTenantEnabled: false, // Multi-tenancy configured post-creation via forge generate
		AnyServiceHasDB:    false, // DB deps are added later by forge generate
		ExtraImports:       nil,   // No cross-package auto-stubs at initial scaffold time
	}

	content, err := templates.ProjectTemplates().Render("bootstrap_testing.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap_testing.go.tmpl: %w", err)
	}

	destPath := filepath.Join(g.Path, "pkg", "app", "testing.go")
	return os.WriteFile(destPath, content, 0644)
}

// generateAppGen writes pkg/app/app_gen.go at scaffold time. The App
// struct definition lives here (forge-owned, regenerated as components
// are added/removed) instead of bootstrap.go so the user-extension
// scaffold (app_extras.go) can embed AppExtras into it cleanly.
//
// At initial scaffold time we always pass false for HasDatabase /
// OrmEnabled — the project hasn't grown a db driver yet. The codegen
// pipeline re-emits app_gen.go with the right values once forge.yaml
// and proto/db/ have entities.
func (g *ProjectGenerator) generateAppGen(hasServices, hasWorkers, hasOperators, hasPackages bool) error {
	data := struct {
		HasDatabase bool
		OrmEnabled  bool
		Services    bool
		Workers     bool
		Operators   bool
		Packages    bool
		RESTEnabled bool
	}{
		HasDatabase: false,
		OrmEnabled:  false,
		Services:    hasServices,
		Workers:     hasWorkers,
		Operators:   hasOperators,
		Packages:    hasPackages,
		// Initial scaffold: REST is off by default. Users opt-in by setting
		// `api.rest: true` in forge.yaml; the post-scaffold `forge generate`
		// then re-renders app_gen.go and bootstrap.go with the vanguard wrap.
		RESTEnabled: false,
	}
	content, err := templates.ProjectTemplates().Render("app_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render app_gen.go.tmpl: %w", err)
	}
	destPath := filepath.Join(g.Path, "pkg", "app", "app_gen.go")
	return os.WriteFile(destPath, content, 0644)
}

// generateAppExtras writes pkg/app/app_extras.go ONCE at scaffold time
// — the Tier-2 user-extension scaffold with an empty AppExtras struct.
// Subsequent `forge generate` runs leave it alone (matches setup.go's
// never-overwrite rule).
func (g *ProjectGenerator) generateAppExtras() error {
	destPath := filepath.Join(g.Path, "pkg", "app", "app_extras.go")
	if _, err := os.Stat(destPath); err == nil {
		// User-owned file already exists — leave it alone.
		return nil
	}
	content, err := templates.ProjectTemplates().Render("app_extras.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render app_extras.go.tmpl: %w", err)
	}
	return os.WriteFile(destPath, content, 0644)
}

// lowerFirstRune returns s with the first rune lowercased — used to
// derive a lowerCamel local-var prefix from a PascalCase FieldName.
// Mirrors codegen.lowerFirst (kept private here to avoid an import
// cycle from generator → codegen).
func lowerFirstRune(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] = r[0] + ('a' - 'A')
	}
	return string(r)
}
