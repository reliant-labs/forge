package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/generator"
)

func newNewCmd() *cobra.Command {
	var (
		projectPath   string
		modulePath    string
		serviceNames  []string
		frontendNames []string
		goVersion     string
		inPlace       bool
		force         bool
		license       string
		licenseAuthor string
	)

	cmd := &cobra.Command{
		Use:   "new [project-name] --mod [module-path]",
		Short: "Create a new Connect RPC project",
		Long: `Create a new project with the Forge framework structure.

This command will create:
- Project directory structure
- Proto definitions directory
- Service scaffolding with initial service
- KCL deploy configuration for dev/staging/prod
- Docker & docker-compose configuration
- Basic .gitignore and .golangci.yml
- Git repository with initial commit
- forge.yaml project configuration

Example:
  forge new my-project --mod github.com/example/my-project
  forge new my-project --mod github.com/example/my-project --service gateway
  forge new my-project --mod github.com/example/my-project --frontend web
  forge new --in-place --mod github.com/example/my-project`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var projectName string
			if len(args) > 0 {
				projectName = args[0]
			}
			return runNew(projectName, projectPath, modulePath, serviceNames, frontendNames, goVersion, inPlace, force, license, licenseAuthor)
		},
	}

	cmd.Flags().StringVarP(&projectPath, "path", "p", ".", "Path where to create the project")
	cmd.Flags().StringVar(&modulePath, "mod", "", "Go module path (required, e.g., github.com/example/my-project)")
	cmd.Flags().StringSliceVar(&serviceNames, "service", nil, "Name(s) of initial Go services (can be repeated or comma-separated)")
	cmd.Flags().StringSliceVar(&frontendNames, "frontend", nil, "Name(s) of Next.js frontends (can be repeated or comma-separated)")
	cmd.Flags().StringVar(&goVersion, "go-version", "", "Go version to use in go.mod (e.g., 1.24); defaults to detected version")
	cmd.Flags().BoolVar(&inPlace, "in-place", false, "Create project in current directory instead of a new subdirectory")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing project configuration")
	cmd.Flags().StringVar(&license, "license", "MIT", "License to include (MIT, Apache-2.0, BSD-3-Clause, none)")
	cmd.Flags().StringVar(&licenseAuthor, "license-author", "", "Author/copyright holder for the LICENSE file (defaults to git config user.name)")
	_ = cmd.MarkFlagRequired("mod")

	return cmd
}

func runNew(projectName, projectPath, modulePath string, serviceNames []string, frontendNames []string, goVersion string, inPlace bool, force bool, license, licenseAuthor string) error {
	var targetPath string

	if inPlace {
		// In-place mode: scaffold into the current (or --path) directory directly
		var err error
		targetPath, err = filepath.Abs(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve path: %w", err)
		}

		// Derive project name from directory if not provided
		if projectName == "" {
			projectName = filepath.Base(targetPath)
		}

		// Validate project name (hyphens allowed for directory/module paths)
		if err := validateProjectName(projectName); err != nil {
			return fmt.Errorf("invalid project name %q: %w", projectName, err)
		}

		// Check that we're not scaffolding over an existing project
		if _, err := os.Stat(filepath.Join(targetPath, defaultProjectConfigFile)); err == nil {
			if !force {
				return fmt.Errorf("%s already exists in %s; this directory already contains a Forge project", defaultProjectConfigFile, targetPath)
			}
			fmt.Printf("  --force: overwriting existing %s\n", defaultProjectConfigFile)
		}
	} else {
		if projectName == "" {
			return fmt.Errorf("project name is required (or use --in-place to scaffold in the current directory)")
		}

		targetPath = filepath.Join(projectPath, projectName)

		// Validate project name (hyphens allowed for directory/module paths)
		if err := validateProjectName(projectName); err != nil {
			return fmt.Errorf("invalid project name %q: %w", projectName, err)
		}

		// Check if directory already exists
		if _, err := os.Stat(targetPath); err == nil {
			return fmt.Errorf("directory %s already exists", targetPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat %s: %w", targetPath, err)
		}
	}

	// Validate service names
	for _, svcName := range serviceNames {
		if err := validateServiceName(svcName); err != nil {
			return fmt.Errorf("invalid service name %q: %w", svcName, err)
		}
	}

	// Validate frontend names
	for _, feName := range frontendNames {
		if err := validateFrontendName(feName); err != nil {
			return fmt.Errorf("invalid frontend name %q: %w", feName, err)
		}
	}

	fmt.Printf("Creating new project '%s' at %s\n", projectName, targetPath)
	if len(serviceNames) > 0 {
		if len(serviceNames) == 1 {
			fmt.Printf("  Service: %s\n", serviceNames[0])
		} else {
			fmt.Printf("  Services: %s\n", strings.Join(serviceNames, ", "))
		}
	}
	if len(frontendNames) > 0 {
		fmt.Printf("  Frontend: %s\n", strings.Join(frontendNames, ", "))
	}

	// Clean up on failure. To guard against TOCTOU where another process
	// might have created files at targetPath in the meantime, we drop a
	// marker file immediately after creating the directory and only run
	// cleanup when that marker is still present.
	// In --in-place mode, we never RemoveAll the target directory since it
	// is an existing directory the user owns. We only remove the marker.
	var success bool
	markerPath := filepath.Join(targetPath, ".forge", ".scaffold-in-progress")
	defer func() {
		if success {
			return
		}
		if _, err := os.Stat(markerPath); err != nil {
			return
		}
		if inPlace {
			// In --in-place mode, only remove the marker — don't nuke the user's directory.
			if rmErr := os.Remove(markerPath); rmErr != nil && !os.IsNotExist(rmErr) {
				fmt.Fprintf(os.Stderr, "warning: failed to remove scaffold marker: %v\n", rmErr)
			}
			return
		}
		if rmErr := os.RemoveAll(targetPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to clean up %s: %v\n", targetPath, rmErr)
		}
	}()

	// Create the target directory and drop the in-progress marker before
	// invoking the generator. The generator is expected to populate the
	// directory; creating it up-front is safe because the generator uses
	// MkdirAll internally.
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}
	if err := os.WriteFile(markerPath, []byte("forge scaffold in progress\n"), 0o644); err != nil {
		return fmt.Errorf("failed to write scaffold marker: %w", err)
	}

	// Create project generator
	gen := generator.NewProjectGenerator(projectName, targetPath, modulePath)
	if len(serviceNames) > 0 {
		gen.ServiceName = serviceNames[0]
	}
	gen.GoVersionOverride = goVersion
	if len(frontendNames) > 0 {
		gen.FrontendName = frontendNames[0]
	}

	// Generate project structure
	if err := gen.Generate(); err != nil {
		return fmt.Errorf("failed to generate project: %w", err)
	}

	// Write LICENSE file if requested
	if err := writeLicenseFile(targetPath, license, licenseAuthor); err != nil {
		return fmt.Errorf("failed to write LICENSE: %w", err)
	}

	// Generate additional services beyond the first (if any)
	if len(serviceNames) > 1 {
		for i, svcName := range serviceNames[1:] {
			port := gen.ServicePort + i + 1
			fmt.Printf("\n🔧 Adding additional service '%s' (port %d)...\n", svcName, port)
			if err := generator.GenerateServiceFiles(targetPath, modulePath, svcName, projectName, port); err != nil {
				return fmt.Errorf("failed to generate service %s: %w", svcName, err)
			}
			// Update project config with additional service
			if err := generator.AppendServiceToConfig(targetPath, svcName, port); err != nil {
				return fmt.Errorf("failed to update config for service %s: %w", svcName, err)
			}
		}
	}

	// Generate additional frontends beyond the first
	for i, feName := range frontendNames[min(1, len(frontendNames)):] {
		fePort := gen.FrontendPort + i + 1
		fmt.Printf("\n🔧 Adding additional frontend '%s' (port %d)...\n", feName, fePort)
		if err := generator.GenerateFrontendFiles(targetPath, modulePath, projectName, feName, gen.ServicePort); err != nil {
			return fmt.Errorf("failed to generate frontend %s: %w", feName, err)
		}
		if err := generator.AppendFrontendToConfig(targetPath, feName, fePort); err != nil {
			return fmt.Errorf("failed to update config for frontend %s: %w", feName, err)
		}
	}

	fmt.Println("\n🔧 Bootstrapping generated proto code...")
	if err := bootstrapGeneratedCode(targetPath); err != nil {
		fmt.Fprintf(os.Stderr, "\n⚠️  Project scaffolded but initial code generation failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "    Run '%s generate && %s build' to retry.\n", CLIName(), CLIName())
	}

	// Initialize git repository
	fmt.Println("\n🔧 Initializing git repository...")
	if err := initGitRepository(targetPath); err != nil {
		fmt.Printf("Warning: failed to initialize git repository: %v\n", err)
	}

	fmt.Println("🔧 Running go mod tidy...")
	if err := runGoModTidy(targetPath); err != nil {
		fmt.Printf("Warning: go mod tidy failed: %v\n", err)
		fmt.Println("You can run 'go mod tidy' manually later")
	}

	var frontendInstallFailed bool
	if len(frontendNames) > 0 {
		fmt.Println("🔧 Installing frontend dependencies (this generates package-lock.json)...")
		if err := runNpmInstall(targetPath, frontendNames); err != nil {
			frontendInstallFailed = true
			fmt.Printf("Warning: npm install failed: %v\n", err)
			fmt.Println("You can run 'npm install' manually later — note that CI requires package-lock.json to exist.")
		}
	}

	// Scaffold finished — remove the in-progress marker so a later failure
	// (if any were ever added) wouldn't delete a completed project.
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: failed to remove scaffold marker: %v\n", err)
	}

	success = true
	fmt.Printf("\n✅ Project '%s' created successfully!\n", projectName)
	fmt.Println("\nNext steps:")
	if !inPlace {
		fmt.Printf("  cd %s\n", projectName)
		fmt.Println("")
	}
	fmt.Println("  # Download dependencies:")
	fmt.Println("  go mod download")
	fmt.Println("")
	if len(serviceNames) > 0 {
		for _, svcName := range serviceNames {
			fmt.Printf("  # Add RPCs to proto/services/%s/v1/%s.proto\n", svcName, svcName)
		}
	} else {
		fmt.Printf("  # Add a service:\n")
		fmt.Printf("  %s add service <name>\n", CLIName())
	}
	fmt.Println("  # Then generate code from protos:")
	fmt.Printf("  %s generate\n", CLIName())
	fmt.Println("")
	if frontendInstallFailed {
		fmt.Printf("  # Frontend dependency install failed above — re-run manually:\n")
		for _, feName := range frontendNames {
			fmt.Printf("  cd frontends/%s && npm install\n", feName)
		}
		fmt.Println("")
	}
	fmt.Println("  # Build and run:")
	fmt.Println("  task run")

	return nil
}

// initGitRepository initializes a git repository and makes initial commit
func initGitRepository(path string) error {
	cmd := exec.Command("git", "init")
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init failed: %s", string(output))
	}

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add failed: %s", string(output))
	}

	cmd = exec.Command("git", "commit", "-m", "Initial commit from forge")
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit failed: %s", string(output))
	}

	return nil
}

// runNpmInstall runs `npm install` in each frontend directory so that a
// package-lock.json exists before first commit. CI relies on `npm ci` which
// requires the lockfile.
func runNpmInstall(root string, frontends []string) error {
	if _, err := exec.LookPath("npm"); err != nil {
		return fmt.Errorf("npm not found on PATH: %w", err)
	}
	for _, name := range frontends {
		feDir := filepath.Join(root, "frontends", name)
		if _, err := os.Stat(filepath.Join(feDir, "package.json")); err != nil {
			continue
		}
		cmd := exec.Command("npm", "install", "--no-audit", "--no-fund")
		cmd.Dir = feDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("npm install (%s) failed: %w", name, err)
		}
	}
	return nil
}

// runGoModTidy runs go mod tidy in the project root and gen/ directories when safe.
func runGoModTidy(path string) error {
	shouldTidyRoot, err := shouldRunRootGoModTidy(path)
	if err != nil {
		return err
	}

	if shouldTidyRoot {
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = path
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go mod tidy (root) failed: %w", err)
		}
	} else {
		fmt.Printf("ℹ️  Skipping root go mod tidy until generated proto code exists. Run '%s generate' first.\n", CLIName())
	}

	genDir := filepath.Join(path, "gen")
	if _, err := os.Stat(filepath.Join(genDir, "go.mod")); err == nil {
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = genDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go mod tidy (gen) failed: %w", err)
		}
	}

	return nil
}

func bootstrapGeneratedCode(path string) error {
	generateMu.Lock()
	defer generateMu.Unlock()

	return runGeneratePipeline(path, false)
}

func shouldRunRootGoModTidy(path string) (bool, error) {
	moduleName, err := readModuleName(path)
	if err != nil {
		return false, err
	}

	serviceRoot := filepath.Join(path, "handlers")
	if _, err := os.Stat(serviceRoot); os.IsNotExist(err) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect handlers directory: %w", err)
	}

	missingGeneratedImports := false
	err = filepath.Walk(serviceRoot, func(currentPath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || filepath.Ext(currentPath) != ".go" {
			return nil
		}

		contents, err := os.ReadFile(currentPath)
		if err != nil {
			return err
		}

		for _, line := range strings.Split(string(contents), "\n") {
			importPath, ok := extractQuotedImportPath(line)
			if !ok || !strings.HasPrefix(importPath, moduleName+"/gen/") {
				continue
			}

			relativeImportPath := strings.TrimPrefix(importPath, moduleName+"/")
			generatedPath := filepath.Join(path, filepath.FromSlash(relativeImportPath))
			if _, err := os.Stat(generatedPath); os.IsNotExist(err) {
				missingGeneratedImports = true
				return filepath.SkipAll
			} else if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return false, fmt.Errorf("inspect generated imports before go mod tidy: %w", err)
	}

	return !missingGeneratedImports, nil
}

func extractQuotedImportPath(line string) (string, bool) {
	firstQuote := strings.Index(line, "\"")
	if firstQuote == -1 {
		return "", false
	}

	remaining := line[firstQuote+1:]
	secondQuote := strings.Index(remaining, "\"")
	if secondQuote == -1 {
		return "", false
	}

	return remaining[:secondQuote], true
}

func readModuleName(path string) (string, error) {
	contents, err := os.ReadFile(filepath.Join(path, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}

	for _, line := range strings.Split(string(contents), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "module ")), nil
		}
	}

	return "", fmt.Errorf("module directive not found in go.mod")
}