package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// goKeywords is the set of Go reserved keywords.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// goPredeclaredIdentifiers is the set of Go predeclared types, constants, zero value, and builtin functions.
var goPredeclaredIdentifiers = map[string]bool{
	// Types
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"any": true, "comparable": true,
	// Constants
	"true": true, "false": true, "iota": true,
	// Zero value
	"nil": true,
	// Builtin functions
	"append": true, "cap": true, "close": true, "complex": true, "copy": true,
	"delete": true, "imag": true, "len": true, "make": true, "new": true,
	"panic": true, "print": true, "println": true, "real": true, "recover": true,
	"min": true, "max": true, "clear": true,
}

// validateIdentifier checks that a name is a valid Go identifier and not a keyword.
func validateIdentifier(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	first, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsLetter(first) && first != '_' {
		return fmt.Errorf("name must start with a letter or underscore")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return fmt.Errorf("name contains invalid character: %c", r)
		}
	}
	if goKeywords[name] {
		return fmt.Errorf("%q is a Go keyword", name)
	}
	if goPredeclaredIdentifiers[name] {
		return fmt.Errorf("%q is a Go predeclared identifier", name)
	}
	return nil
}

// validateProjectName checks that a project name is valid for use as a directory
// name and in Go module paths. Hyphens are allowed since they are valid in
// module paths and directory names; templates use snakeCase/pascalCase helpers
// to convert when a Go identifier is needed.
func validateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	first, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsLetter(first) {
		return fmt.Errorf("name must start with a letter")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return fmt.Errorf("name contains invalid character: %c", r)
		}
	}
	if goKeywords[name] {
		return fmt.Errorf("%q is a Go keyword", name)
	}
	if goPredeclaredIdentifiers[name] {
		return fmt.Errorf("%q is a Go predeclared identifier", name)
	}
	return nil
}

func newAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a service or frontend to an existing project",
		Long: `Add a new service or frontend to an existing Forge project.

Subcommands:
  forge add service <name>    Add a new Go service
  forge add frontend <name>   Add a new Next.js frontend`,
	}

	cmd.AddCommand(newAddServiceCmd())
	cmd.AddCommand(newAddFrontendCmd())

	return cmd
}

// projectRoot finds the project root by looking for forge.project.yaml.
func projectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	configPath := filepath.Join(cwd, "forge.project.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return "", fmt.Errorf("forge.project.yaml not found in current directory; run this from the project root")
	}
	return cwd, nil
}

// --- add service ---

func newAddServiceCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "service <name>",
		Short: "Add a new Go service to the project",
		Long: `Add a new Go service to an existing Forge project.

This creates the service directory structure, proto file, Dockerfile,
hot-reload config, and updates the project configuration.

Example:
  forge add service users
  forge add service orders --port 8082`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddService(args[0], port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Service port (default: auto-increment from 8080)")

	return cmd
}

func runAddService(name string, port int) error {
	if err := validateIdentifier(name); err != nil {
		return fmt.Errorf("invalid service name: %w", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.project.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Check for name conflict
	for _, svc := range cfg.Services {
		if svc.Name == name {
			return fmt.Errorf("service %q already exists in the project", name)
		}
	}

	// Auto-assign port if not specified
	if port == 0 {
		port = 8080
		for _, svc := range cfg.Services {
			if svc.Port >= port {
				port = svc.Port + 1
			}
		}
	}

	fmt.Printf("Adding service '%s' (port %d)...\n", name, port)

	// Generate service files (service.go, handlers.go, proto)
	if err := generator.GenerateServiceFiles(root, cfg.ModulePath, name, cfg.Name, port); err != nil {
		return fmt.Errorf("generate service files: %w", err)
	}

	// Snapshot the existing project config so we can roll back to it if the
	// generation pipeline fails — otherwise the on-disk config would claim a
	// service that has no generated stubs, proto, or wiring.
	originalConfigBytes, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read project config for rollback snapshot: %w", err)
	}

	// Update forge.project.yaml (must happen before the generation pipeline
	// so the pipeline sees the new service in the config)
	cfg.Services = append(cfg.Services, config.ServiceConfig{
		Name: name,
		Type: "GO_SERVICE",
		Path: fmt.Sprintf("handlers/%s", name),
		Port: port,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	// Run the full generation pipeline: buf generate, service stubs, mocks,
	// bootstrap.go, testing.go, go mod tidy, etc.
	fmt.Println("\n🔧 Running generation pipeline...")
	generateMu.Lock()
	err = runGeneratePipeline(root)
	generateMu.Unlock()
	if err != nil {
		if restoreErr := os.WriteFile(configPath, originalConfigBytes, 0o644); restoreErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to restore original project config after pipeline failure: %v\n", restoreErr)
		}
		return fmt.Errorf("generation pipeline failed for service %q (project config restored): %w", name, err)
	}

	// Generate E2E test harness
	fmt.Println("Generating E2E test harness...")
	e2eMethods := generator.MethodsFromProtoStub(name)
	if err := generator.GenerateE2ETests(root, name, cfg.ModulePath, cfg.Name, e2eMethods); err != nil {
		fmt.Printf("  warning: failed to generate E2E tests: %v\n", err)
		// Non-fatal: the service was created successfully
	}

	fmt.Printf("\n✅ Service '%s' added successfully!\n", name)
	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Add RPCs to proto/services/%s/v1/%s.proto\n", name, name)
	fmt.Printf("  2. Implement handlers in handlers/%s/handlers.go\n", name)
	fmt.Printf("  3. E2E tests generated in e2e/%s/ — update after adding RPCs\n", name)

	return nil
}


// --- add frontend ---

func newAddFrontendCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "frontend <name>",
		Short: "Add a new Next.js frontend to the project",
		Long: `Add a new Next.js frontend to an existing forge project.

This creates the frontend directory with Next.js scaffolding, Connect RPC
client setup, and updates the project configuration.

Example:
  forge add frontend web
  forge add frontend dashboard --port 3001`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddFrontend(args[0], port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Frontend dev server port (default: auto-increment from 3000)")

	return cmd
}

// validateFrontendName checks that a frontend name is filesystem-safe.
func validateFrontendName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return fmt.Errorf("name contains invalid filesystem characters")
	}
	if strings.Contains(name, " ") {
		return fmt.Errorf("name cannot contain spaces")
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("name cannot start with . or -")
	}
	return nil
}

func runAddFrontend(name string, port int) error {
	if err := validateFrontendName(name); err != nil {
		return fmt.Errorf("invalid frontend name: %w", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.project.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Check for name conflict
	for _, frontend := range cfg.Frontends {
		if frontend.Name == name {
			return fmt.Errorf("frontend %q already exists in the project", name)
		}
	}

	// Auto-assign port if not specified
	if port == 0 {
		port = 3000
		for _, frontend := range cfg.Frontends {
			if frontend.Port >= port {
				port = frontend.Port + 1
			}
		}
	}

	fmt.Printf("Adding frontend '%s' (port %d)...\n", name, port)

	// Determine the API port from the first service
	apiPort := 8080
	if len(cfg.Services) > 0 {
		apiPort = cfg.Services[0].Port
	}

	// Generate frontend files
	if err := generator.GenerateFrontendFiles(root, cfg.ModulePath, cfg.Name, name, apiPort); err != nil {
		return fmt.Errorf("generate frontend files: %w", err)
	}

	// Update forge.project.yaml
	cfg.Frontends = append(cfg.Frontends, config.FrontendConfig{
		Name: name,
		Type: "nextjs",
		Path: fmt.Sprintf("frontends/%s", name),
		Port: port,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Frontend '%s' added successfully!\n", name)
	fmt.Println("\nNext steps:")
	fmt.Printf("  cd frontends/%s\n", name)
	fmt.Println("  npm install")
	fmt.Println("  npm run dev")

	return nil
}