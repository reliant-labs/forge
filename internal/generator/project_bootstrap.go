package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

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
