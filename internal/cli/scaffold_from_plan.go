package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

func newScaffoldFromPlanCmd() *cobra.Command {
	var inPlace bool

	cmd := &cobra.Command{
		Use:   "scaffold-from-plan <plan-file>",
		Short: "Scaffold a complete project from a plan file",
		Long: `Scaffold a complete project from a YAML plan file.

The plan file specifies the project name, Go module, services, packages,
and frontends. All are scaffolded in a single batch — the generation
pipeline runs exactly once at the end.

Use "-" as the plan file to read from stdin.

Example:
  forge scaffold-from-plan plan.yaml --in-place
  cat plan.yaml | forge scaffold-from-plan - --in-place`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScaffoldFromPlan(args[0], inPlace)
		},
	}

	cmd.Flags().BoolVar(&inPlace, "in-place", false, "Scaffold in current directory")
	return cmd
}

func runScaffoldFromPlan(planPath string, inPlace bool) error {
	// 1. Read plan file
	var data []byte
	var err error
	if planPath == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(planPath)
	}
	if err != nil {
		return fmt.Errorf("read plan file: %w", err)
	}

	// 2. Parse YAML
	var plan config.PlanFile
	if err := yaml.Unmarshal(data, &plan); err != nil {
		return fmt.Errorf("parse plan file: %w", err)
	}

	// 3. Validate required fields
	if plan.ProjectName == "" {
		return fmt.Errorf("plan file: project_name is required")
	}
	if plan.GoModule == "" {
		return fmt.Errorf("plan file: go_module is required")
	}
	if err := validateProjectName(plan.ProjectName); err != nil {
		return fmt.Errorf("plan file: invalid project_name %q: %w", plan.ProjectName, err)
	}
	for _, svc := range plan.Services {
		if err := validateServiceName(svc.Name); err != nil {
			return fmt.Errorf("plan file: invalid service name %q: %w", svc.Name, err)
		}
	}
	for _, pkg := range plan.Packages {
		if !validGoPackageName.MatchString(pkg.Name) {
			return fmt.Errorf("plan file: invalid package name %q: must be lowercase, start with a letter, and contain only letters, digits, and underscores", pkg.Name)
		}
		if goKeywords[pkg.Name] {
			return fmt.Errorf("plan file: package name %q is a Go keyword", pkg.Name)
		}
		if pkg.Kind != "" && !validPackageKinds[pkg.Kind] {
			valid := make([]string, 0, len(validPackageKinds))
			for k := range validPackageKinds {
				valid = append(valid, k)
			}
			return fmt.Errorf("plan file: invalid package kind %q for package %q: valid kinds are %s", pkg.Kind, pkg.Name, strings.Join(valid, ", "))
		}
	}
	for _, fe := range plan.Frontends {
		if err := validateFrontendName(fe.Name); err != nil {
			return fmt.Errorf("plan file: invalid frontend name %q: %w", fe.Name, err)
		}
	}

	// 4. Determine target path
	var targetPath string
	if inPlace {
		targetPath, err = filepath.Abs(".")
		if err != nil {
			return fmt.Errorf("resolve current directory: %w", err)
		}
	} else {
		targetPath, err = filepath.Abs(plan.ProjectName)
		if err != nil {
			return fmt.Errorf("resolve project path: %w", err)
		}
	}

	// 5. Build first service/frontend for the base project generator
	var firstService, firstFrontend string
	if len(plan.Services) > 0 {
		firstService = plan.Services[0].Name
	}
	if len(plan.Frontends) > 0 {
		firstFrontend = plan.Frontends[0].Name
	}

	fmt.Printf("Scaffolding project '%s' from plan...\n", plan.ProjectName)

	// 6. Create project using ProjectGenerator (mirrors runNew but batched)
	gen := generator.NewProjectGenerator(plan.ProjectName, targetPath, plan.GoModule)
	gen.ServiceName = firstService
	gen.GoVersionOverride = plan.GoVersion
	gen.FrontendName = firstFrontend

	if err := gen.Generate(); err != nil {
		return fmt.Errorf("generate base project: %w", err)
	}

	// 7. Write LICENSE
	license := plan.License
	if license == "" {
		license = "MIT"
	}
	if err := writeLicenseFile(targetPath, license, ""); err != nil {
		return fmt.Errorf("write LICENSE: %w", err)
	}

	// 8. Add remaining services (skip first — already handled by generator)
	for i, svc := range plan.Services[min(1, len(plan.Services)):] {
		port := gen.ServicePort + i + 1
		fmt.Printf("  Adding service '%s' (port %d)...\n", svc.Name, port)
		if err := generator.GenerateServiceFiles(targetPath, plan.GoModule, svc.Name, plan.ProjectName, port); err != nil {
			return fmt.Errorf("generate service %s: %w", svc.Name, err)
		}
		if err := generator.AppendServiceToConfig(targetPath, svc.Name, port); err != nil {
			return fmt.Errorf("update config for service %s: %w", svc.Name, err)
		}
	}

	// 9. Add packages
	configPath := filepath.Join(targetPath, defaultProjectConfigFile)
	for _, pkg := range plan.Packages {
		fmt.Printf("  Adding package '%s'...\n", pkg.Name)
		pkgDir := filepath.Join(targetPath, "internal", pkg.Name)
		if err := os.MkdirAll(pkgDir, 0755); err != nil {
			return fmt.Errorf("create package directory %s: %w", pkg.Name, err)
		}

		tmplData := struct {
			Name   string
			Module string
		}{
			Name:   pkg.Name,
			Module: plan.GoModule,
		}

		if pkg.Kind != "" {
			tmplFiles, err := templates.ListInternalPackageKindTemplates(pkg.Kind)
			if err != nil {
				return fmt.Errorf("list %s templates: %w", pkg.Kind, err)
			}
			for _, tmplFile := range tmplFiles {
				content, err := templates.RenderInternalPackageKindTemplate(pkg.Kind, tmplFile, tmplData)
				if err != nil {
					return fmt.Errorf("render %s/%s: %w", pkg.Kind, tmplFile, err)
				}
				outName := strings.TrimSuffix(tmplFile, ".tmpl")
				if err := os.WriteFile(filepath.Join(pkgDir, outName), content, 0644); err != nil {
					return fmt.Errorf("write %s/%s: %w", pkg.Name, outName, err)
				}
			}
		} else {
			contractContent, err := templates.RenderInternalPackageTemplate("contract.go.tmpl", tmplData)
			if err != nil {
				return fmt.Errorf("render contract.go for %s: %w", pkg.Name, err)
			}
			if err := os.WriteFile(filepath.Join(pkgDir, "contract.go"), contractContent, 0644); err != nil {
				return fmt.Errorf("write contract.go for %s: %w", pkg.Name, err)
			}

			serviceContent, err := templates.RenderInternalPackageTemplate("service.go.tmpl", tmplData)
			if err != nil {
				return fmt.Errorf("render service.go for %s: %w", pkg.Name, err)
			}
			if err := os.WriteFile(filepath.Join(pkgDir, "service.go"), serviceContent, 0644); err != nil {
				return fmt.Errorf("write service.go for %s: %w", pkg.Name, err)
			}
		}

		// Append to forge.yaml
		cfg, err := generator.ReadProjectConfig(configPath)
		if err != nil {
			return fmt.Errorf("read config for package %s: %w", pkg.Name, err)
		}
		cfg.Packages = append(cfg.Packages, config.PackageConfig{
			Name: pkg.Name,
			Kind: pkg.Kind,
		})
		if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
			return fmt.Errorf("update config for package %s: %w", pkg.Name, err)
		}
	}

	// 10. Add remaining frontends (skip first — already handled by generator)
	for i, fe := range plan.Frontends[min(1, len(plan.Frontends)):] {
		fePort := gen.FrontendPort + i + 1
		fmt.Printf("  Adding frontend '%s' (port %d)...\n", fe.Name, fePort)
		if err := generator.GenerateFrontendFiles(targetPath, plan.GoModule, plan.ProjectName, fe.Name, gen.ServicePort); err != nil {
			return fmt.Errorf("generate frontend %s: %w", fe.Name, err)
		}
		if err := generator.AppendFrontendToConfig(targetPath, fe.Name, fePort); err != nil {
			return fmt.Errorf("update config for frontend %s: %w", fe.Name, err)
		}
	}

	// 11. Run generation pipeline exactly once
	fmt.Println("\n🔧 Bootstrapping generated code...")
	generateMu.Lock()
	err = runGeneratePipeline(targetPath, false)
	generateMu.Unlock()
	if err != nil {
		return fmt.Errorf("generation pipeline failed: %w", err)
	}

	// 12. Git init
	fmt.Println("🔧 Initializing git repository...")
	if err := initGitRepository(targetPath); err != nil {
		fmt.Printf("Warning: failed to initialize git repository: %v\n", err)
	}

	// 13. Go mod tidy
	fmt.Println("🔧 Running go mod tidy...")
	if err := runGoModTidy(targetPath); err != nil {
		fmt.Printf("Warning: go mod tidy failed: %v\n", err)
	}

	// 14. npm install for frontends
	if len(plan.Frontends) > 0 {
		feNames := make([]string, len(plan.Frontends))
		for i, fe := range plan.Frontends {
			feNames[i] = fe.Name
		}
		fmt.Println("🔧 Installing frontend dependencies...")
		if err := runNpmInstall(targetPath, feNames); err != nil {
			fmt.Printf("Warning: npm install failed: %v\n", err)
		}
	}

	fmt.Printf("\n✅ Project '%s' scaffolded from plan!\n", plan.ProjectName)
	fmt.Println("\nNext steps:")
	if !inPlace {
		fmt.Printf("  cd %s\n\n", plan.ProjectName)
	}
	fmt.Println("  # Download dependencies:")
	fmt.Println("  go mod download")
	fmt.Println("")
	if len(plan.Services) > 0 {
		for _, svc := range plan.Services {
			fmt.Printf("  # Add RPCs to proto/services/%s/v1/%s.proto\n", svc.Name, svc.Name)
		}
	}
	fmt.Println("")
	fmt.Printf("  # Generate code: %s generate\n", CLIName())
	fmt.Println("  # Build and run: task run")

	return nil
}
