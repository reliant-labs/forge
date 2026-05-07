package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// validGoPackageName matches a valid Go package name: lowercase letters, digits, underscores.
var validGoPackageName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validPackageKinds lists the supported --kind values.
var validPackageKinds = map[string]bool{
	"eventbus": true,
	"client":   true,
}

// validPackageTypes lists the supported --type values for `forge add package`.
//
//   - service:    default; classic Service/Deps/New(Deps) Service shape that
//                 codegen wires into bootstrap. The same scaffold used since
//                 the start of forge.
//   - adapter:    outbound boundary translator (third-party API client,
//                 queue producer, storage gateway). No RPC handlers; expects
//                 to be wired via Setup() and called by interactors/services.
//                 See `forge skill load adapter`.
//   - interactor: use-case orchestrator that composes 2+ adapters/services
//                 to fulfill a workflow. Deps are interfaces only. Designed
//                 to be unit-tested with all-mock deps.
//                 See `forge skill load interactor`.
var validPackageTypes = map[string]bool{
	"service":    true,
	"adapter":    true,
	"interactor": true,
}

// packageTypeHelp is the long-form help text shown under `--type`.
const packageTypeHelp = `package shape: service|adapter|interactor (default service)

  service     Standard internal/<name>/ with Service/Deps/New — wired into
              bootstrap, callable by handlers. The default.
  adapter     Outbound boundary (HTTP client, queue producer, storage
              gateway). No business logic; thin translation to a third-party
              system. Marked with '// forge:adapter' so lint can keep RPC
              handlers out. See: forge skill load adapter
  interactor  Use-case orchestrator composing >=2 adapters/services. Deps
              MUST be interfaces (lint-enforced) so the workflow is testable
              with all-mock deps. Marked with '// forge:interactor'.
              See: forge skill load interactor`

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

` + packageTypeHelp + `

Example:
  forge package new cache
  forge package new notifications
  forge package new events --kind eventbus
  forge package new stripe-adapter --type adapter
  forge package new billing-flow --type interactor`,
		Args: cobra.ExactArgs(1),
		RunE: runPackageNew,
	}

	packageNewCmd.Flags().String("kind", "", "package kind template (e.g. eventbus, client)")
	packageNewCmd.Flags().String("type", "service", "package shape: service|adapter|interactor (see --help for details)")

	cmd.AddCommand(packageNewCmd)

	return cmd
}

func runPackageNew(cmd *cobra.Command, args []string) error {
	name := args[0]

	kind, _ := cmd.Flags().GetString("kind")
	kind = strings.TrimSpace(kind)

	// --type defaults to "service" when the flag isn't registered (e.g.
	// older callers or test commands that don't wire it). Treat empty
	// the same as the default so the call site always gets a valid value.
	pkgType, _ := cmd.Flags().GetString("type")
	pkgType = strings.TrimSpace(pkgType)
	if pkgType == "" {
		pkgType = "service"
	}
	if !validPackageTypes[pkgType] {
		// Sort the names so the error message is stable across runs (map
		// iteration order is unspecified).
		valid := make([]string, 0, len(validPackageTypes))
		for k := range validPackageTypes {
			valid = append(valid, k)
		}
		sort.Strings(valid)
		return fmt.Errorf("invalid package type %q: valid types are %s", pkgType, strings.Join(valid, ", "))
	}

	// --kind and --type compose only on the default "service" type.
	// Adapters and interactors get a fixed scaffold; layering an
	// eventbus/client kind on top would silently overwrite the type
	// scaffold.
	if pkgType != "service" && kind != "" {
		return fmt.Errorf("--kind cannot be combined with --type=%s; the type owns the scaffold shape", pkgType)
	}

	// Validate --kind if provided
	if kind != "" && !validPackageKinds[kind] {
		valid := make([]string, 0, len(validPackageKinds))
		for k := range validPackageKinds {
			valid = append(valid, k)
		}
		return fmt.Errorf("invalid package kind %q: valid kinds are %s", kind, strings.Join(valid, ", "))
	}

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

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Check for name conflict in config
	for _, pkg := range cfg.Packages {
		if pkg.Name == name {
			return fmt.Errorf("package %q already exists in forge.yaml", name)
		}
	}

	fmt.Printf("Creating internal package '%s'...\n", name)

	// Create directory
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		return fmt.Errorf("create package directory: %w", err)
	}

	// Template data. ImportPath mirrors Name for now because `forge package
	// new` only accepts flat names (validGoPackageName rejects "/"). When
	// nested-path support lands, ImportPath should carry the full
	// module-relative path under internal/ (e.g. "mcp/database").
	data := struct {
		Name       string
		ImportPath string
		Module     string
	}{
		Name:       name,
		ImportPath: name,
		Module:     cfg.ModulePath,
	}

	// --type=adapter|interactor renders an entire scaffold tree from a
	// dedicated subdir under internal-package/. The shape mirrors --kind
	// (full set of files), but routed by --type so users get the
	// hexagonal-architecture vocabulary rather than the codegen-internal
	// "kind" language. See packageTypeHelp above.
	if pkgType == "adapter" || pkgType == "interactor" {
		tmplFiles, err := templates.InternalPkgKindTemplates(pkgType).ListFlat("")
		if err != nil {
			return fmt.Errorf("list %s templates: %w", pkgType, err)
		}
		if len(tmplFiles) == 0 {
			return fmt.Errorf("no templates found for --type=%s (this is a forge bug — please report)", pkgType)
		}

		for _, tmplFile := range tmplFiles {
			content, err := templates.InternalPkgKindTemplates(pkgType).Render(tmplFile, data)
			if err != nil {
				return fmt.Errorf("render %s: %w", tmplFile, err)
			}
			outName := strings.TrimSuffix(tmplFile, ".tmpl")
			if err := os.WriteFile(filepath.Join(pkgDir, outName), content, 0644); err != nil {
				return fmt.Errorf("write %s: %w", outName, err)
			}
		}
	} else if kind != "" {
		// Kind-specific: discover and render all templates from the kind subdirectory.
		tmplFiles, err := templates.InternalPkgKindTemplates(kind).ListFlat("")
		if err != nil {
			return fmt.Errorf("list %s templates: %w", kind, err)
		}

		for _, tmplFile := range tmplFiles {
			content, err := templates.InternalPkgKindTemplates(kind).Render(tmplFile, data)
			if err != nil {
				return fmt.Errorf("render %s: %w", tmplFile, err)
			}

			// Strip .tmpl suffix for the output filename.
			outName := strings.TrimSuffix(tmplFile, ".tmpl")
			if err := os.WriteFile(filepath.Join(pkgDir, outName), content, 0644); err != nil {
				return fmt.Errorf("write %s: %w", outName, err)
			}
		}
	} else {
		// Default: render the generic contract.go and service.go templates.
		contractContent, err := templates.InternalPkgTemplates().Render("contract.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render contract.go: %w", err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "contract.go"), contractContent, 0644); err != nil {
			return fmt.Errorf("write contract.go: %w", err)
		}

		serviceContent, err := templates.InternalPkgTemplates().Render("service.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render service.go: %w", err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "service.go"), serviceContent, 0644); err != nil {
			return fmt.Errorf("write service.go: %w", err)
		}

		// Scaffold contract_test.go using tdd.TableContract. Once-only:
		// the user owns this file after the first scaffold. We render
		// only on package creation; subsequent forge generate runs do
		// not touch it.
		contractTestContent, err := templates.InternalPkgTemplates().Render("contract_test.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render contract_test.go: %w", err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "contract_test.go"), contractTestContent, 0644); err != nil {
			return fmt.Errorf("write contract_test.go: %w", err)
		}
	}

	// Update forge.yaml. Type is recorded only when non-default so existing
	// projects don't churn forge.yaml on regenerate (omitempty drops "service").
	pkgCfg := config.PackageConfig{
		Name: name,
		Kind: kind,
	}
	if pkgType != "service" {
		pkgCfg.Type = pkgType
	}
	cfg.Packages = append(cfg.Packages, pkgCfg)
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Internal package '%s' created!\n", name)

	return nil
}