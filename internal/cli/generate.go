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
	"github.com/reliant-labs/forge/internal/generator/contract"
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
			_ = force // reserved for future use
			generateMu.Lock()
			err := runGeneratePipeline(".")
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
func runGeneratePipeline(projectDir string) error {
	// Step 0: Load project config (nil when file doesn't exist — fallback to dir scan)
	cfg, err := loadProjectConfigFrom(filepath.Join(projectDir, defaultProjectConfigFile))
	if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
		return fmt.Errorf("failed to load project config: %w", err)
	}
	if errors.Is(err, ErrProjectConfigNotFound) {
		cfg = nil
	}

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

	// ── Step 6: Generate pkg/app/bootstrap.go ──
	if hasServices {
		if err := generateBootstrap(services, modulePath, projectDir); err != nil {
			return fmt.Errorf("bootstrap generation failed: %w", err)
		}
	}

	// ── Step 6b: Generate pkg/app/testing.go ──
	if hasServices {
		if err := generateBootstrapTesting(services, modulePath, projectDir); err != nil {
			return fmt.Errorf("bootstrap testing generation failed: %w", err)
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

	if !isPluginAvailable("protoc-gen-forge-orm") {
		fmt.Println("  ⚠️  protoc-gen-forge-orm not found - skipping ORM code generation")
		fmt.Println("     Install with: go install github.com/reliant-labs/forge/cmd/protoc-gen-forge-orm@latest")
		return nil
	}

	fmt.Println("🔨 Running protoc-gen-forge-orm for entity protos...")

	ormConfig := `version: v2
plugins:
  - local: protoc-gen-forge-orm
    out: gen
    opt:
      - paths=source_relative
`
	tmpFile := filepath.Join(projectDir, "buf.gen.orm.yaml")
	if err := os.WriteFile(tmpFile, []byte(ormConfig), 0644); err != nil {
		return fmt.Errorf("failed to write ORM buf config: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cmd := exec.Command("buf", "generate", "--template", "buf.gen.orm.yaml", "--path", "proto/db")
	cmd.Dir = projectDir
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

	// Check if the frontend has its own buf.gen.yaml
	feBufGen := filepath.Join(absFeDir, "buf.gen.yaml")
	if _, err := os.Stat(feBufGen); os.IsNotExist(err) {
		// Create a buf.gen.yaml for TypeScript generation
		tsConfig := `version: v2
plugins:
  - remote: buf.build/bufbuild/es
    out: src/gen
    opt:
      - target=ts
  - remote: buf.build/connectrpc/es
    out: src/gen
    opt:
      - target=ts
inputs:
  - directory: ../../proto
`
		if err := os.WriteFile(feBufGen, []byte(tsConfig), 0644); err != nil {
			return fmt.Errorf("failed to write TypeScript buf config: %w", err)
		}
	}

	cmd := exec.Command("buf", "generate", "--template", "buf.gen.yaml")
	cmd.Dir = absFeDir
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

	return codegen.PackageDataFromNames(names)
}

// generateBootstrap regenerates pkg/app/bootstrap.go with explicit service construction.
func generateBootstrap(services []codegen.ServiceDef, modulePath string, projectDir string) error {
	fmt.Println("🔧 Generating pkg/app/bootstrap.go...")

	if len(services) == 0 {
		return nil
	}

	packages := discoverPackages(projectDir)

	if err := codegen.GenerateBootstrap(services, packages, modulePath, projectDir); err != nil {
		return fmt.Errorf("failed to generate bootstrap: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/bootstrap.go")

	// Generate setup.go (user-owned, never overwritten)
	if err := codegen.GenerateSetup(modulePath, projectDir); err != nil {
		return fmt.Errorf("failed to generate setup.go: %w", err)
	}

	return nil
}

// generateBootstrapTesting regenerates pkg/app/testing.go with test helpers.
func generateBootstrapTesting(services []codegen.ServiceDef, modulePath string, projectDir string) error {
	fmt.Println("🔧 Generating pkg/app/testing.go...")

	if len(services) == 0 {
		return nil
	}

	packages := discoverPackages(projectDir)

	if err := codegen.GenerateBootstrapTesting(services, packages, modulePath, projectDir); err != nil {
		return fmt.Errorf("failed to generate bootstrap testing: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/testing.go")
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
						if err := runGeneratePipeline("."); err != nil {
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