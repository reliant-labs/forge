package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// validGoPackageName matches a valid Go package name: lowercase letters, digits, underscores.
var validGoPackageName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func newPackageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "package",
		Short: "Manage internal packages",
		Long: `Manage internal packages with Go interface contracts.

Internal packages live under internal/<name>/ and define their boundary
through a Go interface in contract.go. Unlike proto API services, internal
package contracts use native Go interfaces — supporting channels, complex
types, factories, and other constructs that proto cannot express.

Subcommands:
  forge package new <name>   Create a new internal package`,
	}

	packageNewCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new internal package with contract interface",
		Long: `Create a new internal package under internal/<name>/ with:
  - contract.go  — Go interface that IS the package contract
  - service.go   — Implementation with unexported concrete type

After creation, define your interface methods in contract.go, then run
'forge generate' to produce mock_gen.go and middleware_gen.go.

Example:
  forge package new cache
  forge package new notifications`,
		Args: cobra.ExactArgs(1),
		RunE: runPackageNew,
	}

	cmd.AddCommand(packageNewCmd)

	return cmd
}

func runPackageNew(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate name is a valid Go package name
	if !validGoPackageName.MatchString(name) {
		return fmt.Errorf("invalid package name %q: must be lowercase, start with a letter, and contain only letters, digits, and underscores", name)
	}
	if goKeywords[name] {
		return fmt.Errorf("%q is a Go keyword", name)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	// Check it doesn't already exist
	pkgDir := filepath.Join(root, "internal", name)
	if dirExists(pkgDir) {
		return fmt.Errorf("internal package %q already exists at %s", name, pkgDir)
	}

	configPath := filepath.Join(root, "forge.project.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Check for name conflict in config
	for _, pkg := range cfg.Packages {
		if pkg.Name == name {
			return fmt.Errorf("package %q already exists in forge.project.yaml", name)
		}
	}

	fmt.Printf("Creating internal package '%s'...\n", name)

	// Create directory
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		return fmt.Errorf("create package directory: %w", err)
	}

	// Template data
	data := struct {
		Name   string
		Module string
	}{
		Name:   name,
		Module: cfg.ModulePath,
	}

	// Render and write contract.go
	contractContent, err := templates.RenderInternalPackageTemplate("contract.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render contract.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "contract.go"), contractContent, 0644); err != nil {
		return fmt.Errorf("write contract.go: %w", err)
	}

	// Render and write service.go
	serviceContent, err := templates.RenderInternalPackageTemplate("service.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render service.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "service.go"), serviceContent, 0644); err != nil {
		return fmt.Errorf("write service.go: %w", err)
	}

	// Update forge.project.yaml
	cfg.Packages = append(cfg.Packages, config.PackageConfig{
		Name: name,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Internal package '%s' created!\n", name)
	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Define interface methods in internal/%s/contract.go\n", name)
	fmt.Printf("  2. Implement them in internal/%s/service.go\n", name)
	fmt.Println("  3. Run: forge generate  (generates mock_gen.go and middleware_gen.go)")

	return nil
}
