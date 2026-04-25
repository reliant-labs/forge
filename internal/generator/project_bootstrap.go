package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateBootstrap writes pkg/app/bootstrap.go with explicit service construction.
func (g *ProjectGenerator) generateBootstrap() error {
	type bootstrapService struct {
		Name      string
		Package   string
		FieldName string
		Fallible  bool
	}

	type bootstrapPackage struct {
		Name      string
		Package   string
		FieldName string
		Fallible  bool
	}

	type bootstrapWorker struct {
		Name      string
		Package   string
		FieldName string
		Fallible  bool
	}

	type bootstrapOperator struct {
		Name      string
		Package   string
		FieldName string
		Fallible  bool
	}

	var services []bootstrapService
	if g.ServiceName != "" {
		pkg := g.ServiceName
		fieldName := naming.ToExportedFieldName(pkg)
		services = []bootstrapService{
			{
				Name:      pkg,
				Package:   pkg,
				FieldName: fieldName,
			},
		}
	}

	data := struct {
		Module       string
		Services     []bootstrapService
		Packages     []bootstrapPackage
		Workers      []bootstrapWorker
		Operators    []bootstrapOperator
		HasDatabase  bool
		OrmEnabled   bool
		HasFallible  bool
		ConfigFields map[string]bool
	}{
		Module:    g.ModulePath,
		Services:  services,
		Packages:  nil, // No packages at initial project creation
		Workers:   nil, // No workers at initial project creation
		Operators: nil, // No operators at initial project creation
		// Initial scaffold has no proto/db entities; the post-scaffold
		// generate pipeline re-renders with the correct flags if the user
		// adds entities.
		HasDatabase:  false,
		OrmEnabled:   false,
		ConfigFields: codegen.DefaultConfigFieldNames(),
	}

	content, err := templates.ProjectTemplates.Render("bootstrap.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap.go.tmpl: %w", err)
	}

	destPath := filepath.Join(g.Path, "pkg", "app", "bootstrap.go")
	return os.WriteFile(destPath, content, 0644)
}

// generateBootstrapTesting writes pkg/app/testing.go with test helper functions.
func (g *ProjectGenerator) generateBootstrapTesting() error {
	type bootstrapTestService struct {
		Name             string
		Package          string
		FieldName        string
		ProtoServiceName string
		Fallible         bool
	}

	type bootstrapPackage struct {
		Name      string
		Package   string
		FieldName string
		Fallible  bool
	}

	var services []bootstrapTestService
	if g.ServiceName != "" {
		pkg := g.ServiceName
		fieldName := naming.ToExportedFieldName(pkg)
		protoServiceName := naming.ToPascalCase(pkg) + "Service"
		services = []bootstrapTestService{
			{
				Name:             pkg,
				Package:          pkg,
				FieldName:        fieldName,
				ProtoServiceName: protoServiceName,
			},
		}
	}

	data := struct {
		Module             string
		Services           []bootstrapTestService
		Packages           []bootstrapPackage
		MultiTenantEnabled bool
	}{
		Module:             g.ModulePath,
		Services:           services,
		Packages:           nil,   // No packages at initial project creation
		MultiTenantEnabled: false, // Multi-tenancy configured post-creation via forge generate
	}

	content, err := templates.ProjectTemplates.Render("bootstrap_testing.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap_testing.go.tmpl: %w", err)
	}

	destPath := filepath.Join(g.Path, "pkg", "app", "testing.go")
	return os.WriteFile(destPath, content, 0644)
}