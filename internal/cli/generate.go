package cli

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
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

When forge.yaml exists, generation is driven by the config:
  - buf generate for Go stubs (protoc-gen-go + protoc-gen-connect-go)
  - protoc-gen-forge for entity protos in proto/db/
  - buf generate for TypeScript stubs for Next.js frontends
  - Service stubs and mocks for new services
  - pkg/app/bootstrap.go with explicit service bootstrapping
  - sqlc generate if sqlc.yaml exists
  - go mod tidy in gen/

Without forge.yaml, falls back to directory convention scanning:
  proto/           - Root proto directory (for buf generate)
  proto/services/  - Service definitions (stubs + mocks)
  proto/api/       - API messages
  proto/db/        - Database models (protoc-gen-forge)

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
	// Acquire cross-process file lock (complements the in-process generateMu)
	release, err := acquireGenerateLock(projectDir)
	if err != nil {
		return err
	}
	defer release()

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
	// Stamp the forge version that produced this generation cycle. CI pins
	// its forge install to this version for reproducible `verify-generated`.
	cs.ForgeVersion = buildinfo.Version()
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
	if (cfg == nil || cfg.Features.CodegenEnabled()) {
		if err := runBufGenerateGo(projectDir); err != nil {
			return fmt.Errorf("buf generate (Go) failed: %w", err)
		}
	}

	// ── Step 1b: Descriptor extraction (services, entities, configs → forge_descriptor.json) ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) {
		if err := runDescriptorGenerate(projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: descriptor generation failed: %v\n", err)
		}
	}

	// ── Step 2: ORM generation if proto/db/ exists ──
	if (cfg == nil || cfg.Features.ORMEnabled()) && hasDB {
		if err := runOrmGenerate(projectDir); err != nil {
			return fmt.Errorf("ORM generation failed: %w", err)
		}
	}

	// ── Step 2b: Auto-generate initial migration if proto/db entities exist and no migrations yet ──
	if (cfg == nil || cfg.Features.MigrationsEnabled()) && hasDB && !hasSQLMigrations(projectDir) {
		if err := maybeGenerateInitialMigration(projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: initial migration generation failed: %v\n", err)
		}
	}

	// ── Step 2c: Replace boilerplate migration with entity-aware migration ──
	if (cfg == nil || cfg.Features.MigrationsEnabled()) && hasServices {
		entityDefs, parseErr := codegen.ParseEntityProtos(projectDir)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: entity proto parsing for migrations failed: %v\n", parseErr)
		} else if len(entityDefs) > 0 && isBoilerplateMigration(projectDir) {
			migDir := filepath.Join(projectDir, "db", "migrations")
			removeBoilerplateMigrations(migDir)
			planEntities := codegen.EntityDefsToPlanEntities(entityDefs)
			if err := generator.GeneratePlanMigrations(projectDir, planEntities); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: entity migration generation failed: %v\n", err)
			} else {
				fmt.Printf("  ✅ Generated entity-aware migration (%d tables)\n", len(entityDefs))
			}
		}
	}

	// ── Step 3: TypeScript generation for frontends with Connect clients ──
	if (cfg == nil || cfg.Features.FrontendEnabled()) && cfg != nil {
		for _, fe := range cfg.Frontends {
			if strings.EqualFold(fe.Type, "nextjs") || strings.EqualFold(fe.Type, "react-native") {
				if err := runBufGenerateTypeScript(fe, cfg, projectDir); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: TypeScript generation for %s failed: %v\n", fe.Name, err)
				}
			}
		}
	}

	// ── Step 3b: Config loader generation from proto/config/ ──
	var configFields map[string]bool
	if (cfg == nil || cfg.Features.CodegenEnabled()) && hasConfig {
		var cfgErr error
		var features config.FeaturesConfig
		if cfg != nil {
			features = cfg.Features
		}
		configFields, cfgErr = generateConfigLoader(projectDir, features)
		if cfgErr != nil {
			return fmt.Errorf("config loader generation failed: %w", cfgErr)
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

	// ── Step 3c: Frontend React Query hooks for each service ──
	if (cfg == nil || cfg.Features.FrontendEnabled()) && cfg != nil && hasServices && len(services) > 0 {
		if err := generateFrontendHooks(cfg, services, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: frontend hooks generation failed: %v\n", err)
		}
	}

	// ── Step 3d: Ensure core UI components are installed ──
	if (cfg == nil || cfg.Features.FrontendEnabled()) && cfg != nil && len(cfg.Frontends) > 0 {
		ensureFrontendComponents(cfg, projectDir)
	}

	// ── Step 3e: Frontend CRUD pages for each service ──
	if (cfg == nil || cfg.Features.FrontendEnabled()) && cfg != nil && hasServices && len(services) > 0 {
		if err := generateFrontendPages(cfg, services, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: frontend page generation failed: %v\n", err)
		}
	}

	// ── Step 3f: Frontend nav and dashboard (re-render with entity data) ──
	if (cfg == nil || cfg.Features.FrontendEnabled()) && cfg != nil && len(cfg.Frontends) > 0 {
		if err := generateFrontendNav(cfg, services, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: frontend nav generation failed: %v\n", err)
		}
	}

	// ── Step 4: Service stubs (non-destructive) ──
	// Compute CRUD method names upfront so the stub generator can skip them.
	// Entities may live in proto/db/ (legacy) or proto/services/ (new arch).
	var crudMethodNames map[string]bool
	if hasServices {
		crudMethodNames = collectCRUDMethodNames(services, projectDir)
	}
	if (cfg == nil || cfg.Features.CodegenEnabled()) && hasServices {
		if err := generateServiceStubs(cfg, services, projectDir, crudMethodNames); err != nil {
			return fmt.Errorf("service stub generation failed: %w", err)
		}
	}

	// ── Step 4a: Generate internal/db/ (type aliases + ORM CRUD) ──
	if (cfg == nil || cfg.Features.ORMEnabled()) && hasServices {
		entities, entErr := codegen.ParseEntityProtos(projectDir)
		if entErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: entity parsing for ORM generation failed: %v\n", entErr)
		} else if len(entities) > 0 {
			// Derive service name from the first entity's proto file path.
			svcName := codegen.ServiceNameFromProtoFile(entities[0].ProtoFile)
			if svcName != "" {
				planEntities := codegen.EntityDefsToPlanEntities(entities)
				entityNames := make([]string, len(entities))
				for i, e := range entities {
					entityNames[i] = e.Name
				}

				if err := generator.GeneratePlanDBTypes(projectDir, modulePath, svcName, entityNames); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: db types generation failed: %v\n", err)
				}

				if err := generator.GeneratePlanORM(projectDir, modulePath, svcName, planEntities); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: ORM generation failed: %v\n", err)
				} else {
					fmt.Printf("  ✅ Generated internal/db/ (%d entity ORM files)\n", len(entities))
				}
			}
		}
	}

	// ── Step 4b: CRUD handler generation ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) && hasServices {
		if err := generateCRUDHandlers(services, modulePath, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: CRUD handler generation failed: %v\n", err)
		}
	}

	// ── Step 4c: Authorizer generation (role mappings from proto annotations) ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) && hasServices {
		if err := codegen.GenerateAuthorizer(services, modulePath, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: authorizer generation failed: %v\n", err)
		}
	}

	// ── Step 5: Mocks (always regenerate) ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) && hasServices {
		if err := generateServiceMocks(services, projectDir); err != nil {
			return fmt.Errorf("mock generation failed: %w", err)
		}
	}

	// ── Step 5b: Internal package contract generation ──
	if (cfg == nil || cfg.Features.ContractsEnabled()) {
		if err := generateInternalPackageContracts(projectDir); err != nil {
			return fmt.Errorf("internal package contract generation failed: %w", err)
		}
	}

	// ── Step 5c: Auth middleware generation ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) && cfg != nil && cfg.Auth.Provider != "" && cfg.Auth.Provider != "none" {
		if err := generateAuthMiddleware(cfg, services, modulePath, projectDir); err != nil {
			return fmt.Errorf("auth middleware generation failed: %w", err)
		}
	}

	// ── Step 5d: Tenant middleware generation ──
	// Auto-enable multi-tenant if any entity has a tenant key field.
	if (cfg == nil || cfg.Features.CodegenEnabled()) && cfg != nil && hasServices {
		entities, _ := codegen.ParseEntityProtos(projectDir)
		hasTenantEntities := false
		for _, e := range entities {
			if e.HasTenant {
				hasTenantEntities = true
				break
			}
		}
		if hasTenantEntities {
			if cfg.Auth.MultiTenant == nil {
				cfg.Auth.MultiTenant = &config.MultiTenantConfig{}
			}
			if !cfg.Auth.MultiTenant.Enabled {
				cfg.Auth.MultiTenant.Enabled = true
				configPath := filepath.Join(projectDir, defaultProjectConfigFile)
				if writeErr := generator.WriteProjectConfigFile(cfg, configPath); writeErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to persist multi-tenant config: %v\n", writeErr)
				} else {
					fmt.Println("  ✅ Auto-enabled multi-tenant config (entities use tenant_key)")
				}
			}
			if err := generateTenantMiddleware(cfg, projectDir); err != nil {
				return fmt.Errorf("tenant middleware generation failed: %w", err)
			}
		}
	}

	// ── Step 5e: Webhook route generation ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) && cfg != nil {
		if err := generateWebhookRoutes(cfg, projectDir); err != nil {
			return fmt.Errorf("webhook route generation failed: %w", err)
		}
	}

	// ── Step 6: Generate pkg/app/bootstrap.go ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) && (hasServices || hasWorkers || hasOperators) {
		var dbDriver string
		if cfg != nil {
			dbDriver = cfg.Database.Driver
		}
		// ORM is enabled when entity definitions exist. They may live in:
		// - proto/db/ (legacy architecture)
		// - internal/db/types.go (new architecture — entities inline in service protos)
		ormEnabled := false
		if hasDB {
			ok, perr := hasProtoFilesInDir(filepath.Join(projectDir, "proto", "db"))
			if perr != nil {
				return fmt.Errorf("scan proto/db for ORM protos: %w", perr)
			}
			ormEnabled = ok
		}
		if !ormEnabled {
			// New architecture: check for internal/db/types.go (generated by plan_db_types_gen)
			if _, err := os.Stat(filepath.Join(projectDir, "internal", "db", "types.go")); err == nil {
				ormEnabled = true
			}
		}
		if err := generateBootstrap(services, modulePath, dbDriver, ormEnabled, projectDir, configFields); err != nil {
			return fmt.Errorf("bootstrap generation failed: %w", err)
		}
	}

	// ── Step 6b: Generate pkg/app/testing.go ──
	if (cfg == nil || cfg.Features.CodegenEnabled()) && (hasServices || hasWorkers || hasOperators) {
		mtEnabled := cfg != nil && cfg.Auth.MultiTenant != nil && cfg.Auth.MultiTenant.Enabled
		if err := generateBootstrapTesting(services, modulePath, mtEnabled, projectDir); err != nil {
			return fmt.Errorf("bootstrap testing generation failed: %w", err)
		}
	}

	// ── Step 6c: Generate pkg/app/migrate.go if database is configured ──
	if (cfg == nil || cfg.Features.MigrationsEnabled()) && cfg != nil && cfg.Database.Driver != "" {
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
	if (cfg == nil || cfg.Features.CIEnabled()) && cfg != nil {
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
	if (cfg == nil || cfg.Features.DeployEnabled()) {
		fmt.Println("\n── Regenerating infrastructure files ──")
		if err := generator.RegenerateInfraFiles(abs, cfg); err != nil {
			return fmt.Errorf("regenerate infrastructure files: %w", err)
		}
	}

	// ── Step 8d-i: Generate Grafana dashboards ──
	if (cfg == nil || cfg.Features.ObservabilityEnabled()) && cfg != nil {
		if err := generator.GenerateGrafanaDashboards(cfg.Name, abs); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Grafana dashboard generation failed: %v\n", err)
		}
	}

	// ── Step 8d-ii: Generate entity-aware seed data ──
	var entityDefs []codegen.EntityDef
	if (cfg == nil || cfg.Features.MigrationsEnabled()) && (hasDB || hasServices) {
		var parseErr error
		entityDefs, parseErr = codegen.ParseEntityProtos(projectDir)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: entity proto parsing for seeds failed: %v\n", parseErr)
		} else if len(entityDefs) > 0 {
			seedEntities := generator.EntityDefsToSeedEntities(entityDefs)
			if err := generator.GenerateEntitySeeds(seedEntities, abs); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: entity seed generation failed: %v\n", err)
			}
		}
	}

	// ── Step 8d-iii: Generate frontend mock data + transport ──
	if (cfg == nil || cfg.Features.FrontendEnabled()) && cfg != nil && len(cfg.Frontends) > 0 && len(entityDefs) > 0 && len(services) > 0 {
		if err := generateFrontendMocks(cfg, services, entityDefs, projectDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: frontend mock generation failed: %v\n", err)
		}
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

	// ── Step 8g: Post-generation validation ──
	if warnings := validateGeneratedProject(projectDir); len(warnings) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Post-generation warnings:\n")
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "  • %s\n", w)
		}
	}

	// ── Step 9: Validate generated code compiles ──
	fmt.Println("\n🔨 Validating generated code...")
	validateCmd := exec.Command("go", "build", "./...")
	validateCmd.Dir = projectDir
	var buildStderr strings.Builder
	validateCmd.Stdout = os.Stdout
	validateCmd.Stderr = io.MultiWriter(os.Stderr, &buildStderr)
	if err := validateCmd.Run(); err != nil {
		errOutput := buildStderr.String()
		if errOutput != "" {
			fmt.Fprintf(os.Stderr, "\n💡 Build failed. Common fixes:\n")
			if strings.Contains(errOutput, "pkg/config") {
				fmt.Fprintf(os.Stderr, "  • Ensure proto/config/ has annotated config fields and re-run 'forge generate'\n")
			}
			if strings.Contains(errOutput, "GeneratedAuthorizer") || strings.Contains(errOutput, "authorizer_gen") {
				fmt.Fprintf(os.Stderr, "  • authorizer_gen.go may be missing — re-run 'forge generate'\n")
			}
		}
		return fmt.Errorf("generated code failed to compile: %w", err)
	}

	fmt.Println()
	fmt.Println("✅ Code generation complete!")
	return nil
}