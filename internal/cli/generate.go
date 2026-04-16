package cli

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/database"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/packs"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateMu protects the generation pipeline from concurrent runs.
// It is legitimately package-level shared state used by generate, add, and new commands.
var generateMu sync.Mutex

func newGenerateCmd() *cobra.Command {
	var (
		watch bool
		force bool
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate code from proto files",
		Long: `Generate code from proto files based on project configuration or directory conventions.

When forge.project.yaml exists, generation is driven by the config:
  - buf generate for Go stubs (protoc-gen-go + protoc-gen-connect-go)
  - protoc-gen-forge-orm for entity protos in proto/db/
  - buf generate for TypeScript stubs for Next.js frontends
  - Service stubs and mocks for new services
  - pkg/app/bootstrap.go with explicit service bootstrapping
  - sqlc generate if sqlc.yaml exists
  - go mod tidy in gen/

Without forge.project.yaml, falls back to directory convention scanning:
  proto/           - Root proto directory (for buf generate)
  proto/services/  - Service definitions (stubs + mocks)
  proto/api/       - API messages
  proto/db/        - Database models (protoc-gen-forge-orm)

Examples:
  forge generate              # Generate all code
  forge generate --watch      # Watch mode for development
  forge generate --force      # Force regeneration of config files`,
		RunE: func(cmd *cobra.Command, args []string) error {
			generateMu.Lock()
			err := runGeneratePipeline(".", force)
			generateMu.Unlock()
			if err != nil {
				return err
			}

			if watch {
				fmt.Println("\n👀 Watching for changes... (Press Ctrl+C to stop)")
				return watchForChanges()
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Watch for changes and regenerate")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force regeneration of config files like buf.gen.yaml")

	return cmd
}

// runGeneratePipeline executes the unified generation pipeline.
// Both config-based and directory-scan modes converge on the same ordered steps.
// projectDir is the root of the project (contains go.mod, proto/, etc.).
// The caller must hold generateMu.
func runGeneratePipeline(projectDir string, force bool) error {
	// Step 0a: Load project config (nil when file doesn't exist — fallback to dir scan)
	cfg, err := loadProjectConfigFrom(filepath.Join(projectDir, defaultProjectConfigFile))
	if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
		return fmt.Errorf("failed to load project config: %w", err)
	}
	if errors.Is(err, ErrProjectConfigNotFound) {
		cfg = nil
	}

	// Step 0b: Load checksums for generated-file tracking
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("failed to resolve project dir: %w", err)
	}
	cs, err := generator.LoadChecksums(abs)
	if err != nil {
		return fmt.Errorf("failed to load checksums: %w", err)
	}
	// Ensure checksums are saved at the end of the pipeline
	defer func() {
		if saveErr := generator.SaveChecksums(abs, cs); saveErr != nil {
			log.Printf("Warning: failed to save checksums: %v", saveErr)
		}
	}()

	if cfg != nil {
		fmt.Printf("📦 Generating code for project: %s\n\n", cfg.Name)
	} else {
		// Verify we're in a forge project at all
		if _, err := os.Stat(filepath.Join(projectDir, "proto")); os.IsNotExist(err) {
			return fmt.Errorf("no 'proto' directory found. Are you in a forge project?")
		}
		fmt.Println("📦 Generating code (directory-scan mode)")
		fmt.Println()
	}

	// Detect proto directories
	hasServices := dirExists(filepath.Join(projectDir, "proto/services"))
	hasAPI := dirExists(filepath.Join(projectDir, "proto/api"))
	hasDB := dirExists(filepath.Join(projectDir, "proto/db"))
	hasConfig := dirExists(filepath.Join(projectDir, "proto/config"))

	if cfg == nil && !hasServices && !hasAPI && !hasDB && !hasConfig {
		return fmt.Errorf("no proto files found in proto/api, proto/services, proto/db, or proto/config")
	}

	if cfg == nil {
		fmt.Println("🔍 Detected proto directories:")
		if hasAPI {
			fmt.Println("  ✓ proto/api/ (API messages)")
		}
		if hasServices {
			fmt.Println("  ✓ proto/services/ (Service definitions)")
		}
		if hasDB {
			fmt.Println("  ✓ proto/db/ (Database models)")
		}
		if hasConfig {
			fmt.Println("  ✓ proto/config/ (Config definitions)")
		}
		fmt.Println()
	}

	// ── Step 1: buf generate for Go stubs ──
	if err := runBufGenerateGo(projectDir); err != nil {
		return fmt.Errorf("buf generate (Go) failed: %w", err)
	}

	// ── Step 2: ORM generation if proto/db/ exists ──
	if hasDB {
		if err := runOrmGenerate(projectDir); err != nil {
			return fmt.Errorf("ORM generation failed: %w", err)
		}
	}

	// ── Step 2b: Auto-generate initial migration if proto/db entities exist and no migrations yet ──
	if hasDB && !hasSQLMigrations(projectDir) {
		if err := maybeGenerateInitialMigration(projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: initial migration generation failed: %v\n", err)
		}
	}

	// ── Step 3: TypeScript generation for each Next.js frontend ──
	if cfg != nil {
		for _, fe := range cfg.Frontends {
			if strings.EqualFold(fe.Type, "nextjs") {
				if err := runBufGenerateTypeScript(fe, cfg, projectDir); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: TypeScript generation for %s failed: %v\n", fe.Name, err)
				}
			}
		}
	}

	// ── Step 3b: Config loader generation from proto/config/ ──
	if hasConfig {
		if err := generateConfigLoader(projectDir); err != nil {
			return fmt.Errorf("config loader generation failed: %w", err)
		}
	}

	// ── Parse services and module path once for steps 4-6 ──
	var services []codegen.ServiceDef
	var modulePath string
	if hasServices {
		services, err = codegen.ParseServicesFromProtos(filepath.Join(projectDir, "proto/services"), projectDir)
		if err != nil {
			return fmt.Errorf("failed to parse service protos: %w", err)
		}
		// ParseServicesFromProtos already reads the module path and sets it on each ServiceDef.
		// Extract it from the first service to avoid a redundant GetModulePath() call.
		if len(services) > 0 {
			modulePath = services[0].ModulePath
		} else {
			modulePath, err = codegen.GetModulePath(projectDir)
			if err != nil {
				return fmt.Errorf("failed to read module path: %w", err)
			}
		}
	}

	// Resolve module path for workers/operators if not already set (no proto services)
	hasWorkers := len(discoverWorkers(projectDir)) > 0
	hasOperators := len(discoverOperators(projectDir)) > 0
	if modulePath == "" && (hasWorkers || hasOperators) {
		modulePath, err = codegen.GetModulePath(projectDir)
		if err != nil {
			return fmt.Errorf("failed to read module path: %w", err)
		}
	}

	// ── Step 4: Service stubs (non-destructive) ──
	if hasServices {
		if err := generateServiceStubs(cfg, services, projectDir); err != nil {
			return fmt.Errorf("service stub generation failed: %w", err)
		}
	}

	// ── Step 5: Mocks (always regenerate) ──
	if hasServices {
		if err := generateServiceMocks(services, projectDir); err != nil {
			return fmt.Errorf("mock generation failed: %w", err)
		}
	}

	// ── Step 5b: Internal package contract generation ──
	if err := generateInternalPackageContracts(projectDir); err != nil {
		return fmt.Errorf("internal package contract generation failed: %w", err)
	}

	// ── Step 5c: Auth middleware generation ──
	if cfg != nil && cfg.Auth.Provider != "" && cfg.Auth.Provider != "none" {
		if err := generateAuthMiddleware(cfg, services, modulePath, projectDir); err != nil {
			return fmt.Errorf("auth middleware generation failed: %w", err)
		}
	}

	// ── Step 5d: Tenant middleware generation ──
	if cfg != nil && cfg.Auth.MultiTenant != nil && cfg.Auth.MultiTenant.Enabled {
		if err := generateTenantMiddleware(cfg, projectDir); err != nil {
			return fmt.Errorf("tenant middleware generation failed: %w", err)
		}
	}

	// ── Step 5e: Webhook route generation ──
	if cfg != nil {
		if err := generateWebhookRoutes(cfg, projectDir); err != nil {
			return fmt.Errorf("webhook route generation failed: %w", err)
		}
	}

	// ── Step 6: Generate pkg/app/bootstrap.go ──
	if hasServices || hasWorkers || hasOperators {
		var dbDriver string
		if cfg != nil {
			dbDriver = cfg.Database.Driver
		}
		if err := generateBootstrap(services, modulePath, dbDriver, projectDir); err != nil {
			return fmt.Errorf("bootstrap generation failed: %w", err)
		}
	}

	// ── Step 6b: Generate pkg/app/testing.go ──
	if hasServices || hasWorkers || hasOperators {
		mtEnabled := cfg != nil && cfg.Auth.MultiTenant != nil && cfg.Auth.MultiTenant.Enabled
		if err := generateBootstrapTesting(services, modulePath, mtEnabled, projectDir); err != nil {
			return fmt.Errorf("bootstrap testing generation failed: %w", err)
		}
	}

	// ── Step 6c: Generate pkg/app/migrate.go if database is configured ──
	if cfg != nil && cfg.Database.Driver != "" {
		if err := generateMigrate(projectDir, cfg.ModulePath); err != nil {
			return fmt.Errorf("migrate generation failed: %w", err)
		}
	}

	// ── Step 7: sqlc generate if sqlc.yaml exists ──
	if err := runSqlcGenerate(projectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: sqlc generate failed: %v\n", err)
	}

	// ── Step 8: go mod tidy in gen/ ──
	if err := runGoModTidyGen(projectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: go mod tidy in gen/ failed: %v\n", err)
	}

	// ── Step 8b: Generate CI/CD workflows ──
	if cfg != nil {
		fmt.Println("\n🔧 Generating CI/CD workflows...")
		if err := generateCIWorkflows(abs, cfg, cs, force); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  CI/CD generation warning: %v\n", err)
			// Non-fatal: don't fail the pipeline for CI generation issues
		}
	}

	// ── Step 8c: Re-render installed pack generate hooks ──
	if cfg != nil && len(cfg.Packs) > 0 {
		if err := runPackGenerateHooks(projectDir, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  Pack generate hooks warning: %v\n", err)
		}
	}

	// ── Step 8d: Regenerate infrastructure files (Tier 1) ──
	fmt.Println("\n── Regenerating infrastructure files ──")
	if err := generator.RegenerateInfraFiles(abs, cfg); err != nil {
		return fmt.Errorf("regenerate infrastructure files: %w", err)
	}

	// ── Step 8e: go mod tidy in project root ──
	if err := runGoModTidyRoot(projectDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: go mod tidy in project root failed: %v\n", err)
	}

	// ── Step 8f: Run goimports on generated Go files ──
	if modulePath == "" {
		modulePath, _ = codegen.GetModulePath(projectDir)
	}
	if modulePath != "" {
		if err := runGoimportsOnGenerated(projectDir, modulePath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: goimports failed: %v\n", err)
		}
	}

	// ── Step 9: Validate generated code compiles ──
	fmt.Println("\n🔨 Validating generated code...")
	validateCmd := exec.Command("go", "build", "./...")
	validateCmd.Dir = projectDir
	validateCmd.Stdout = os.Stdout
	validateCmd.Stderr = os.Stderr
	if err := validateCmd.Run(); err != nil {
		return fmt.Errorf("generated code failed to compile: %w", err)
	}

	fmt.Println()
	fmt.Println("✅ Code generation complete!")
	return nil
}

// ─── Step implementations ────────────────────────────────────────────────────

// runBufGenerateGo runs buf generate using the project's buf.gen.yaml for Go stubs.
func runBufGenerateGo(projectDir string) error {
	// Create a default buf.gen.yaml only if one doesn't exist
	if _, err := os.Stat(filepath.Join(projectDir, "buf.gen.yaml")); os.IsNotExist(err) {
		if err := writeDefaultBufGenYaml(projectDir); err != nil {
			return fmt.Errorf("failed to create buf.gen.yaml: %w", err)
		}
		fmt.Println("📝 Generated default buf.gen.yaml")
	}

	fmt.Println("🔨 Running buf generate (Go stubs)...")
	cmd := exec.Command("buf", "generate")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("buf generate failed: %w", err)
	}

	fmt.Println("  ✅ Go protobuf + Connect stubs generated")
	return nil
}

// writeDefaultBufGenYaml writes a standard buf.gen.yaml with Go plugins.
func writeDefaultBufGenYaml(projectDir string) error {
	config := `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt:
      - paths=source_relative
  - remote: buf.build/connectrpc/go
    out: gen
    opt:
      - paths=source_relative
`
	return os.WriteFile(filepath.Join(projectDir, "buf.gen.yaml"), []byte(config), 0644)
}

// resolveOrmPluginBinary finds or installs the protoc-gen-forge-orm plugin.
// Returns the path to the binary (or empty string if unavailable).
func resolveOrmPluginBinary() (string, error) {
	// 1. Check PATH for pre-installed binary
	if path, err := exec.LookPath("protoc-gen-forge-orm"); err == nil {
		return path, nil
	}

	// 2. Check local bin directory
	localBin := filepath.Join("bin", "protoc-gen-forge-orm")
	if _, err := os.Stat(localBin); err == nil {
		return localBin, nil
	}

	// 3. Try to build from source if cmd/protoc-gen-forge-orm exists
	srcPath := filepath.Join("cmd", "protoc-gen-forge-orm", "main.go")
	if _, err := os.Stat(srcPath); err == nil {
		fmt.Println("Building protoc-gen-forge-orm from source...")
		if err := os.MkdirAll("bin", 0755); err == nil {
			buildCmd := exec.Command("go", "build", "-o", localBin, "./cmd/protoc-gen-forge-orm")
			buildCmd.Stdout = os.Stdout
			buildCmd.Stderr = os.Stderr
			if err := buildCmd.Run(); err == nil {
				fmt.Printf("Built %s\n", localBin)
				return localBin, nil
			}
			fmt.Println("Warning: failed to build from source, trying go install...")
		}
	}

	// 4. Try go install
	fmt.Println("Installing protoc-gen-forge-orm via go install...")
	installCmd := exec.Command("go", "install", "github.com/reliant-labs/forge/cmd/protoc-gen-forge-orm@latest")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err == nil {
		if path, err := exec.LookPath("protoc-gen-forge-orm"); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("protoc-gen-forge-orm not found: not on PATH, not at bin/protoc-gen-forge-orm, and go install failed")
}

// runOrmGenerate runs buf generate with the protoc-gen-forge-orm plugin for proto/db/ entities.
func runOrmGenerate(projectDir string) error {
	hasProtoFiles, err := hasProtoFilesInDir(filepath.Join(projectDir, "proto", "db"))
	if err != nil {
		return fmt.Errorf("scan proto/db for ORM protos: %w", err)
	}
	if !hasProtoFiles {
		fmt.Println("  ℹ️  No proto files found in proto/db - skipping ORM code generation")
		return nil
	}

	ormBinPath, err := resolveOrmPluginBinary()
	if err != nil {
		fmt.Println("  ⚠️  protoc-gen-forge-orm not available - skipping ORM code generation")
		fmt.Println("     Install with: go install github.com/reliant-labs/forge/cmd/protoc-gen-forge-orm@latest")
		return nil
	}

	fmt.Println("🔨 Running protoc-gen-forge-orm for entity protos...")

	// Use the resolved binary path in the buf config
	ormConfig := fmt.Sprintf(`version: v2
plugins:
  - local: %s
    out: gen
    opt:
      - paths=source_relative
`, ormBinPath)
	tmpFile := filepath.Join(projectDir, "buf.gen.orm.yaml")
	if err := os.WriteFile(tmpFile, []byte(ormConfig), 0644); err != nil {
		return fmt.Errorf("failed to write ORM buf config: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cmd := exec.Command("buf", "generate", "--template", "buf.gen.orm.yaml", "--path", "proto/db")
	cmd.Dir = projectDir
	// Add bin/ to PATH so buf can find the plugin if it's there
	cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s%c%s",
		filepath.Join(projectDir, "bin"), os.PathListSeparator, os.Getenv("PATH")))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ORM generation failed: %w", err)
	}

	fmt.Println("  ✅ ORM code generated from proto/db/")
	return nil
}

func hasProtoFilesInDir(root string) (bool, error) {
	if !dirExists(root) {
		return false, nil
	}

	hasProtoFiles := false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".proto" {
			hasProtoFiles = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return false, err
	}
	return hasProtoFiles, nil
}

// runBufGenerateTypeScript runs buf generate for TypeScript stubs in a Next.js frontend.
// It runs buf from the project root to avoid picking up node_modules proto files,
// using --path flags to scope generation and --template to point at the frontend's buf.gen.yaml.
func runBufGenerateTypeScript(fe config.FrontendConfig, cfg *config.ProjectConfig, projectDir string) error {
	feDir := fe.Path
	if feDir == "" {
		feDir = filepath.Join("frontends", fe.Name)
	}

	absFeDir := filepath.Join(projectDir, feDir)
	if !dirExists(absFeDir) {
		return fmt.Errorf("frontend directory %s not found", feDir)
	}

	fmt.Printf("🔨 Generating TypeScript stubs for %s...\n", fe.Name)

	// Ensure the frontend has a buf.gen.yaml with out: relative to project root
	feBufGen := filepath.Join(absFeDir, "buf.gen.yaml")
	if _, err := os.Stat(feBufGen); os.IsNotExist(err) {
		tsConfig := fmt.Sprintf(`version: v2
plugins:
  - remote: buf.build/bufbuild/es
    out: %s/src/gen
    opt:
      - target=ts
`, filepath.ToSlash(feDir))
		if err := os.WriteFile(feBufGen, []byte(tsConfig), 0644); err != nil {
			return fmt.Errorf("failed to write TypeScript buf config: %w", err)
		}
	}

	// Build command: run from project root, use --template with relative path to frontend's buf.gen.yaml
	relativeTemplate := filepath.Join(feDir, "buf.gen.yaml")
	args := []string{"generate", "--template", relativeTemplate, "--path", "proto/services"}

	// Include proto/api if it exists
	if dirExists(filepath.Join(projectDir, "proto/api")) {
		args = append(args, "--path", "proto/api")
	}

	cmd := exec.Command("buf", args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("TypeScript generation failed for %s: %w", fe.Name, err)
	}

	fmt.Printf("  ✅ TypeScript stubs generated for %s\n", fe.Name)
	return nil
}

// generateServiceStubs creates service.go, handlers.go, wrapper.go for new services.
// For existing service directories, it generates stubs only for missing RPC handlers.
func generateServiceStubs(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string) error {
	fmt.Println("\n🔧 Generating service stubs...")

	if len(services) == 0 {
		fmt.Println("  No services found in proto/services/")
		return nil
	}

	hasNewStubs := false
	for _, svc := range services {
		relServiceDir := toServiceDir(svc.Name)
		absServiceDir := filepath.Join(projectDir, relServiceDir)

		if dirExists(absServiceDir) {
			// Incremental: generate stubs only for missing RPC methods
			result, err := codegen.GenerateMissingHandlerStubs(svc, absServiceDir)
			if err != nil {
				return fmt.Errorf("failed to generate missing stubs for %s: %w", svc.Name, err)
			}
			if result.AllUpToDate {
				fmt.Printf("  ⏭️  Skipped %s/ (all handlers up to date)\n", relServiceDir)
			} else {
				fmt.Printf("  ✅ Generated %d new handler stub(s) in %s/handlers_new.go: %s\n",
					len(result.NewMethods), relServiceDir, strings.Join(result.NewMethods, ", "))
				hasNewStubs = true
			}
			continue
		}

		if err := codegen.GenerateServiceStub(svc, absServiceDir); err != nil {
			return fmt.Errorf("failed to generate stub for %s: %w", svc.Name, err)
		}
		fmt.Printf("  ✅ Created %s/\n", relServiceDir)
	}

	if hasNewStubs {
		fmt.Println("  💡 Run 'go build ./...' to verify the new stubs compile")
	}

	return nil
}

// generateServiceMocks always regenerates mocks from proto definitions.
func generateServiceMocks(services []codegen.ServiceDef, projectDir string) error {
	fmt.Println("🔧 Generating service mocks...")

	if len(services) == 0 {
		return nil
	}

	for _, svc := range services {
		written, err := codegen.GenerateMock(svc, filepath.Join(projectDir, "handlers/mocks"))
		if err != nil {
			return fmt.Errorf("failed to generate mock for %s: %w", svc.Name, err)
		}
		mockName := strings.ToLower(strings.TrimSuffix(svc.Name, "Service"))
		if written {
			fmt.Printf("  ✅ Updated handlers/mocks/%s_mock.go\n", mockName)
		} else {
			fmt.Printf("  ⏭️  Skipped handlers/mocks/%s_mock.go (no RPCs)\n", mockName)
		}
	}

	return nil
}

// generateInternalPackageContracts scans internal/*/contract.go files and generates
// mock_gen.go and middleware_gen.go for each using the contract AST generator.
func generateInternalPackageContracts(projectDir string) error {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil
	}

	entries, err := os.ReadDir(internalDir)
	if err != nil {
		return fmt.Errorf("read internal/ directory: %w", err)
	}

	generated := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		contractPath := filepath.Join(internalDir, entry.Name(), "contract.go")
		if _, err := os.Stat(contractPath); os.IsNotExist(err) {
			continue
		}

		if err := contract.Generate(contractPath); err != nil {
			return fmt.Errorf("generate contract for %s: %w", entry.Name(), err)
		}
		fmt.Printf("  ✅ Generated mock + middleware for internal/%s/\n", entry.Name())
		generated++
	}

	if generated > 0 {
		fmt.Printf("🔧 Generated contracts for %d internal package(s)\n", generated)
	}

	return nil
}

// generateConfigLoader parses proto/config/ for config protos with
// ConfigFieldOptions annotations and generates pkg/config/config.go.
func generateConfigLoader(projectDir string) error {
	fmt.Println("🔧 Generating config loader from proto/config/...")

	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto/config"))
	if err != nil {
		return fmt.Errorf("failed to parse config protos: %w", err)
	}

	if len(messages) == 0 {
		fmt.Println("  ℹ️  No config fields with config_field annotations found")
		return nil
	}

	if err := codegen.GenerateConfigLoader(messages, projectDir); err != nil {
		return fmt.Errorf("failed to generate config loader: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/config/config.go")
	return nil
}

// discoverPackages returns BootstrapPackageData for internal packages.
// It scans internal/*/contract.go to find packages with Go interface contracts.
func discoverPackages(projectDir string) []codegen.BootstrapPackageData {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil
	}

	entries, err := os.ReadDir(internalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v\n", internalDir, err)
		return nil
	}

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		contractPath := filepath.Join(internalDir, entry.Name(), "contract.go")
		if _, err := os.Stat(contractPath); err == nil {
			names = append(names, entry.Name())
		}
	}

	return codegen.PackageDataFromNames(names, projectDir)
}

// discoverWorkers returns BootstrapWorkerData for all worker-type services in the project config.
func discoverWorkers(projectDir string) []codegen.BootstrapWorkerData {
	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	cfg, err := loadProjectConfigFrom(cfgPath)
	if err != nil || cfg == nil {
		return nil
	}

	var names []string
	for _, svc := range cfg.Services {
		if strings.EqualFold(svc.Type, "worker") {
			names = append(names, svc.Name)
		}
	}
	return codegen.WorkerDataFromNames(names, projectDir)
}

// discoverOperators returns BootstrapOperatorData for all operator-type services in the project config.
func discoverOperators(projectDir string) []codegen.BootstrapOperatorData {
	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	cfg, err := loadProjectConfigFrom(cfgPath)
	if err != nil || cfg == nil {
		return nil
	}

	var names []string
	for _, svc := range cfg.Services {
		if strings.EqualFold(svc.Type, "operator") {
			names = append(names, svc.Name)
		}
	}
	return codegen.OperatorDataFromNames(names, projectDir)
}

// generateAuthMiddleware generates pkg/middleware/auth_gen.go from the auth config.
func generateAuthMiddleware(cfg *config.ProjectConfig, services []codegen.ServiceDef, modulePath string, projectDir string) error {
	fmt.Println("🔧 Generating auth middleware...")

	// Resolve module path if not already set
	modPath := modulePath
	if modPath == "" {
		var err error
		modPath, err = codegen.GetModulePath(projectDir)
		if err != nil {
			return fmt.Errorf("failed to read module path: %w", err)
		}
	}

	// Build the skip list from proto method options.
	// Methods annotated with auth_required: false in proto options are skipped.
	// For now we use an empty list — proto option parsing can be added later.
	var skipMethods []string

	if err := codegen.GenerateAuthMiddleware(&cfg.Auth, modPath, skipMethods, projectDir); err != nil {
		return err
	}

	fmt.Println("  ✅ Generated pkg/middleware/auth_gen.go")
	if cfg.Auth.Provider == "api_key" || cfg.Auth.Provider == "both" {
		fmt.Println("  ✅ Generated pkg/middleware/auth_validator.go (if not exists)")
	}

	return nil
}

// generateTenantMiddleware generates pkg/middleware/tenant_gen.go from the multi-tenant config.
func generateTenantMiddleware(cfg *config.ProjectConfig, projectDir string) error {
	fmt.Println("🔧 Generating tenant middleware...")

	if err := codegen.GenerateTenantMiddleware(cfg.Auth.MultiTenant, projectDir); err != nil {
		return err
	}

	fmt.Println("  ✅ Generated pkg/middleware/tenant_gen.go")
	return nil
}

// generateWebhookRoutes generates webhook_routes_gen.go for each service that has webhooks.
func generateWebhookRoutes(cfg *config.ProjectConfig, projectDir string) error {
	for _, svc := range cfg.Services {
		if len(svc.Webhooks) == 0 {
			continue
		}

		svcDir := filepath.Join(projectDir, "handlers", svc.Name)
		if _, err := os.Stat(svcDir); os.IsNotExist(err) {
			continue // service directory doesn't exist yet
		}

		var entries []templates.WebhookRouteEntryData
		for _, wh := range svc.Webhooks {
			entries = append(entries, templates.WebhookRouteEntryData{
				Name:       strings.ReplaceAll(wh.Name, "_", "-"),
				PascalName: naming.ToPascalCase(wh.Name),
			})
		}

		data := templates.WebhookRoutesTemplateData{
			Package:  svc.Name,
			Webhooks: entries,
		}

		content, err := templates.RenderWebhookTemplate("webhook/webhook_routes_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render webhook routes for %s: %w", svc.Name, err)
		}

		outPath := filepath.Join(svcDir, "webhook_routes_gen.go")
		if err := os.WriteFile(outPath, content, 0644); err != nil {
			return fmt.Errorf("write webhook routes for %s: %w", svc.Name, err)
		}

		fmt.Printf("  ✅ Generated handlers/%s/webhook_routes_gen.go\n", svc.Name)
	}

	return nil
}

// generateBootstrap regenerates pkg/app/bootstrap.go with explicit service construction.
func generateBootstrap(services []codegen.ServiceDef, modulePath string, databaseDriver string, projectDir string) error {
	fmt.Println("🔧 Generating pkg/app/bootstrap.go...")

	workers := discoverWorkers(projectDir)
	operators := discoverOperators(projectDir)

	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	packages := discoverPackages(projectDir)

	hasDatabase := databaseDriver != ""
	if err := codegen.GenerateBootstrap(services, packages, workers, operators, modulePath, hasDatabase, projectDir); err != nil {
		return fmt.Errorf("failed to generate bootstrap: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/bootstrap.go")

	// Generate setup.go (user-owned, never overwritten)
	if err := codegen.GenerateSetup(modulePath, databaseDriver, projectDir); err != nil {
		return fmt.Errorf("failed to generate setup.go: %w", err)
	}

	return nil
}

// generateBootstrapTesting regenerates pkg/app/testing.go with test helpers.
func generateBootstrapTesting(services []codegen.ServiceDef, modulePath string, multiTenantEnabled bool, projectDir string) error {
	fmt.Println("🔧 Generating pkg/app/testing.go...")

	workers := discoverWorkers(projectDir)
	operators := discoverOperators(projectDir)

	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	packages := discoverPackages(projectDir)

	if err := codegen.GenerateBootstrapTesting(services, packages, workers, operators, modulePath, multiTenantEnabled, projectDir); err != nil {
		return fmt.Errorf("failed to generate bootstrap testing: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/testing.go")
	return nil
}

// hasSQLMigrations returns true if db/migrations/ contains at least one .sql file.
func hasSQLMigrations(projectDir string) bool {
	migDir := filepath.Join(projectDir, "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			return true
		}
	}
	return false
}

// maybeGenerateInitialMigration auto-generates an initial migration from proto/db entities
// when db/migrations/ is empty and proto/db has .proto files.
func maybeGenerateInitialMigration(projectDir string) error {
	hasProtos, err := hasProtoFilesInDir(filepath.Join(projectDir, "proto", "db"))
	if err != nil || !hasProtos {
		return nil
	}

	migDir := filepath.Join(projectDir, "db", "migrations")
	fmt.Println("🔧 Auto-generating initial migration from proto/db entities...")
	opts := &database.MigrationOptions{
		FromProto: true,
	}
	if err := database.CreateMigration("init", migDir, opts); err != nil {
		return fmt.Errorf("auto-generate initial migration: %w", err)
	}
	return nil
}

// generateMigrate writes pkg/app/migrate.go with embedded migration support.
func generateMigrate(projectDir, modulePath string) error {
	fmt.Println("🔧 Generating pkg/app/migrate.go...")

	has := hasSQLMigrations(projectDir)
	if err := codegen.GenerateMigrate(projectDir, modulePath, has); err != nil {
		return fmt.Errorf("failed to generate migrate.go: %w", err)
	}

	if has {
		fmt.Println("  ✅ Generated pkg/app/migrate.go (with embedded migrations)")
	} else {
		fmt.Println("  ✅ Generated pkg/app/migrate.go (no migrations yet)")
	}
	return nil
}

// runSqlcGenerate runs sqlc generate if sqlc.yaml exists.
func runSqlcGenerate(projectDir string) error {
	if _, err := os.Stat(filepath.Join(projectDir, "sqlc.yaml")); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(projectDir, "sqlc.yml")); os.IsNotExist(err) {
			// No sqlc config found, skip silently
			return nil
		}
	}

	if _, err := exec.LookPath("sqlc"); err != nil {
		fmt.Println("  ⚠️  sqlc not found on PATH - skipping sqlc generate")
		return nil
	}

	fmt.Println("🔨 Running sqlc generate...")
	cmd := exec.Command("sqlc", "generate")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sqlc generate failed: %w", err)
	}

	fmt.Println("  ✅ sqlc queries generated")
	return nil
}

// runGoModTidyGen runs `go mod tidy` inside the gen/ directory to keep deps fresh.
func runGoModTidyGen(projectDir string) error {
	genDir := filepath.Join(projectDir, "gen")
	goMod := filepath.Join(genDir, "go.mod")
	if _, err := os.Stat(goMod); os.IsNotExist(err) {
		// No go.mod in gen/, nothing to tidy
		return nil
	}

	fmt.Println("🔨 Running go mod tidy in gen/...")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = genDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy in gen/ failed: %w", err)
	}

	fmt.Println("  ✅ gen/go.mod tidied")
	return nil
}

func runGoModTidyRoot(projectDir string) error {
	goMod := filepath.Join(projectDir, "go.mod")
	if _, err := os.Stat(goMod); os.IsNotExist(err) {
		return nil
	}

	fmt.Println("🔨 Running go mod tidy in project root...")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy in project root failed: %w", err)
	}

	fmt.Println("  ✅ go.mod tidied")
	return nil
}

// runGoimportsOnGenerated runs goimports on generated Go files to fix import grouping.
func runGoimportsOnGenerated(projectDir, modulePath string) error {
	goimportsPath, err := exec.LookPath("goimports")
	if err != nil {
		fmt.Println("  ⚠️  goimports not found — skipping import formatting")
		fmt.Println("     Install with: go install golang.org/x/tools/cmd/goimports@latest")
		return nil
	}

	dirs := []string{"cmd", "pkg", "gen", "handlers"}
	var targets []string
	for _, d := range dirs {
		if dirExists(filepath.Join(projectDir, d)) {
			targets = append(targets, d)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	fmt.Println("🔨 Running goimports on generated code...")
	args := []string{"-local", modulePath, "-w"}
	args = append(args, targets...)
	cmd := exec.Command(goimportsPath, args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("goimports failed: %w", err)
	}

	fmt.Println("  ✅ Imports formatted")
	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func isPluginAvailable(pluginName string) bool {
	_, err := exec.LookPath(pluginName)
	return err == nil
}

func toServiceDir(serviceName string) string {
	// EchoService -> handlers/echo
	name := strings.ToLower(strings.TrimSuffix(serviceName, "Service"))
	return fmt.Sprintf("handlers/%s", name)
}

// ─── CI/CD Workflow Generation ───────────────────────────────────────────────

// generateCIWorkflows generates GitHub Actions workflow files from the project config.
// It uses the checksum system to detect user modifications and skip overwriting them.
func generateCIWorkflows(root string, cfg *config.ProjectConfig, cs *generator.FileChecksums, force bool) error {
	if cfg.CI.Provider != "" && cfg.CI.Provider != "github" {
		return nil // only github supported for now
	}

	provider := "github"

	// Build template data from config
	ciData := buildCIWorkflowData(cfg)
	deployData := buildDeployWorkflowData(cfg)
	buildData := buildBuildImagesWorkflowData(cfg)

	// ── ci.yml ──
	ciContent, err := templates.RenderCITemplate(provider, "ci.yml.tmpl", ciData)
	if err != nil {
		return fmt.Errorf("render ci.yml: %w", err)
	}
	written, err := generator.WriteGeneratedFile(root, ".github/workflows/ci.yml", ciContent, cs, force)
	if err != nil {
		return fmt.Errorf("write ci.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/workflows/ci.yml")
	} else {
		fmt.Println("  ⚠️  .github/workflows/ci.yml has local modifications, skipping (use --force to overwrite)")
	}

	// ── build-images.yml ──
	buildContent, err := templates.RenderCITemplate(provider, "build-images.yml.tmpl", buildData)
	if err != nil {
		return fmt.Errorf("render build-images.yml: %w", err)
	}
	written, err = generator.WriteGeneratedFile(root, ".github/workflows/build-images.yml", buildContent, cs, force)
	if err != nil {
		return fmt.Errorf("write build-images.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/workflows/build-images.yml")
	} else {
		fmt.Println("  ⚠️  .github/workflows/build-images.yml has local modifications, skipping (use --force to overwrite)")
	}

	// ── deploy.yml ──
	deployContent, err := templates.RenderCITemplate(provider, "deploy.yml.tmpl", deployData)
	if err != nil {
		return fmt.Errorf("render deploy.yml: %w", err)
	}
	written, err = generator.WriteGeneratedFile(root, ".github/workflows/deploy.yml", deployContent, cs, force)
	if err != nil {
		return fmt.Errorf("write deploy.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/workflows/deploy.yml")
	} else {
		fmt.Println("  ⚠️  .github/workflows/deploy.yml has local modifications, skipping (use --force to overwrite)")
	}

	// ── e2e.yml (only if E2E enabled) ──
	if cfg.CI.E2E.Enabled {
		e2eData := buildE2EWorkflowData(cfg)
		e2eContent, err := templates.RenderCITemplate(provider, "e2e.yml.tmpl", e2eData)
		if err != nil {
			return fmt.Errorf("render e2e.yml: %w", err)
		}
		written, err = generator.WriteGeneratedFile(root, ".github/workflows/e2e.yml", e2eContent, cs, force)
		if err != nil {
			return fmt.Errorf("write e2e.yml: %w", err)
		}
		if written {
			fmt.Println("  ✅ Generated .github/workflows/e2e.yml")
		} else {
			fmt.Println("  ⚠️  .github/workflows/e2e.yml has local modifications, skipping (use --force to overwrite)")
		}
	}

	// ── dependabot.yml ──
	depData := buildDependabotData(cfg)
	depContent, err := templates.RenderCITemplate(provider, "dependabot.yml.tmpl", depData)
	if err != nil {
		return fmt.Errorf("render dependabot.yml: %w", err)
	}
	written, err = generator.WriteGeneratedFile(root, ".github/dependabot.yml", depContent, cs, force)
	if err != nil {
		return fmt.Errorf("write dependabot.yml: %w", err)
	}
	if written {
		fmt.Println("  ✅ Generated .github/dependabot.yml")
	} else {
		fmt.Println("  ⚠️  .github/dependabot.yml has local modifications, skipping (use --force to overwrite)")
	}

	return nil
}

// buildCIWorkflowData maps a ProjectConfig to the CI workflow template data.
func buildCIWorkflowData(cfg *config.ProjectConfig) templates.CIWorkflowData {
	goVersion := cfg.CI.EffectiveGoVersion()
	hasFrontends := len(cfg.Frontends) > 0
	hasServices := len(cfg.Services) > 0

	var frontends []templates.FrontendCIConfig
	for _, fe := range cfg.Frontends {
		p := fe.Path
		if p == "" {
			p = "frontends/" + fe.Name
		}
		frontends = append(frontends, templates.FrontendCIConfig{Name: fe.Name, Path: p})
	}

	// Zero-value CILintConfig means "all enabled" (sensible default)
	lintCfg := cfg.CI.Lint
	allLintDefault := lintCfg == (config.CILintConfig{})

	vulnCfg := cfg.CI.VulnScan
	allVulnDefault := vulnCfg == (config.CIVulnConfig{})

	testCfg := cfg.CI.Test
	allTestDefault := testCfg == (config.CITestConfig{})

	// Collect environments for KCL validation
	var envs []string
	for _, e := range cfg.Envs {
		envs = append(envs, e.Name)
	}

	return templates.CIWorkflowData{
		ProjectName:  cfg.Name,
		GoVersion:    goVersion,
		HasFrontends: hasFrontends,
		Frontends:    frontends,
		HasServices:  hasServices,

		LintGolangci: allLintDefault || lintCfg.Golangci,
		LintBuf:      allLintDefault || lintCfg.Buf,
		LintFrontend: allLintDefault || lintCfg.Frontend,

		TestRace:     allTestDefault || testCfg.Race,
		TestCoverage: testCfg.Coverage,

		VulnGo:     allVulnDefault || vulnCfg.Go,
		VulnDocker:  allVulnDefault || vulnCfg.Docker,
		VulnNPM:     allVulnDefault || vulnCfg.NPM,

		E2EEnabled:  cfg.CI.E2E.Enabled,
		E2ERuntime:  effectiveE2ERuntime(cfg),

		PermContents: cfg.CI.EffectivePermContents(),

		HasKCL:       len(envs) > 0,
		Environments: envs,
	}
}

// buildDeployWorkflowData maps a ProjectConfig to the deploy workflow template data.
func buildDeployWorkflowData(cfg *config.ProjectConfig) templates.DeployWorkflowData {
	var envs []templates.DeployEnv
	for _, e := range cfg.Deploy.Environments {
		envs = append(envs, templates.DeployEnv{
			Name:       e.Name,
			Auto:       e.Auto,
			Protection: e.Protection,
			URL:        e.URL,
		})
	}
	// If no deploy environments configured, use sensible defaults from project envs
	if len(envs) == 0 {
		for _, e := range cfg.Envs {
			if e.Type == "cloud" {
				envs = append(envs, templates.DeployEnv{
					Name: e.Name,
				})
			}
		}
	}

	return templates.DeployWorkflowData{
		ProjectName:      cfg.Name,
		Environments:     envs,
		Registry:         cfg.Deploy.EffectiveRegistry(),
		HasFrontends:     len(cfg.Frontends) > 0,
		FrontendDeploy:   cfg.Deploy.FrontendDeploy,
		MigrationTest:    cfg.Deploy.MigrationTest,
		Concurrency:      cfg.Deploy.IsConcurrencyEnabled(),
		CancelInProgress: cfg.Deploy.Concurrency.CancelInProgress,
	}
}

// buildBuildImagesWorkflowData maps a ProjectConfig to the build-images workflow template data.
func buildBuildImagesWorkflowData(cfg *config.ProjectConfig) templates.BuildImagesWorkflowData {
	vulnCfg := cfg.CI.VulnScan
	allVulnDefault := vulnCfg == (config.CIVulnConfig{})

	return templates.BuildImagesWorkflowData{
		ProjectName:  cfg.Name,
		Registry:     cfg.Deploy.EffectiveRegistry(),
		HasFrontends: len(cfg.Frontends) > 0,
		VulnDocker:   allVulnDefault || vulnCfg.Docker,
	}
}

// buildE2EWorkflowData maps a ProjectConfig to the E2E workflow template data.
func buildE2EWorkflowData(cfg *config.ProjectConfig) templates.E2EWorkflowData {
	return templates.E2EWorkflowData{
		ProjectName:  cfg.Name,
		GoVersion:    cfg.CI.EffectiveGoVersion(),
		Runtime:      effectiveE2ERuntime(cfg),
		HasFrontends: len(cfg.Frontends) > 0,
	}
}

// buildDependabotData builds template data for the dependabot config.
// The dependabot template uses FrontendName (singular) for the npm directory.
func buildDependabotData(cfg *config.ProjectConfig) struct{ FrontendName string } {
	var feName string
	if len(cfg.Frontends) > 0 {
		feName = cfg.Frontends[0].Name
	}
	return struct{ FrontendName string }{FrontendName: feName}
}

// effectiveE2ERuntime returns the E2E runtime, defaulting to "docker-compose".
func effectiveE2ERuntime(cfg *config.ProjectConfig) string {
	if cfg.CI.E2E.Runtime != "" {
		return cfg.CI.E2E.Runtime
	}
	return "docker-compose"
}

// ─── Watch mode ──────────────────────────────────────────────────────────────

// watchForChanges watches the proto/ directory for changes and re-runs the
// generate pipeline on each change. Uses fsnotify with a debounce to coalesce
// rapid successive writes (e.g. an editor writing + renaming).
func watchForChanges() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	// Recursively add all directories under proto/
	if err := addWatchDirs(watcher, "proto"); err != nil {
		return fmt.Errorf("failed to watch proto/: %w", err)
	}

	// Debounce timer — wait 500ms after the last event before regenerating
	var debounce *time.Timer
	const debounceDelay = 500 * time.Millisecond

	// Track whether additional changes arrived while a regen is in-flight
	// so we don't lose events during the (potentially lengthy) regen.
	var pendingMu sync.Mutex
	var pending bool
	var lastEvent string

	// Listen for OS interrupt to exit cleanly
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			// Only care about .proto file changes
			if !strings.HasSuffix(event.Name, ".proto") {
				// But if a new directory is created, watch it too
				if event.Has(fsnotify.Create) {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						_ = watcher.Add(event.Name)
					}
				}
				continue
			}

			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) {
				pendingMu.Lock()
				pending = true
				lastEvent = event.Name
				pendingMu.Unlock()

				// Reset debounce timer
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(debounceDelay, func() {
					for {
						pendingMu.Lock()
						if !pending {
							pendingMu.Unlock()
							break
						}
						pending = false
						eventName := lastEvent
						pendingMu.Unlock()

						generateMu.Lock()
						fmt.Printf("\n🔄 Change detected: %s\n", eventName)
						if err := runGeneratePipeline(".", false); err != nil {
							log.Printf("Generation failed: %v", err)
						}
						generateMu.Unlock()
					}
					fmt.Println("\n👀 Watching for changes... (Press Ctrl+C to stop)")
				})
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Printf("Watcher error: %v", err)

		case <-sigCh:
			fmt.Println("\nStopping watch mode...")
			return nil
		}
	}
}

// runPackGenerateHooks processes generate hooks for all installed packs.
// This re-renders pack generate templates so pack-generated code stays
// up to date with the current project config.
func runPackGenerateHooks(projectDir string, cfg *config.ProjectConfig) error {
	installed, err := packs.InstalledPacks(cfg)
	if err != nil {
		return err
	}

	for _, p := range installed {
		if len(p.Generate) == 0 {
			continue
		}
		fmt.Printf("\n🔌 Running generate hooks for pack '%s'...\n", p.Name)
		if err := p.RenderGenerateFiles(projectDir, cfg); err != nil {
			return fmt.Errorf("pack %s generate hooks: %w", p.Name, err)
		}
	}
	return nil
}

// addWatchDirs recursively adds all directories under root to the watcher.
func addWatchDirs(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := watcher.Add(path); err != nil {
				return fmt.Errorf("failed to watch %s: %w", path, err)
			}
		}
		return nil
	})
}