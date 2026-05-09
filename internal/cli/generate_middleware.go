package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateAuthMiddleware generates pkg/middleware/auth_gen.go from the auth config.
func generateAuthMiddleware(cfg *config.ProjectConfig, services []codegen.ServiceDef, modulePath string, projectDir string, cs *generator.FileChecksums) error {
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
	// Default is fail-closed (auth required); only methods that explicitly
	// set `(forge.v1.method).auth_required = false` join the skip list and
	// bypass the auth interceptor.
	var skipMethods []string
	for _, svc := range services {
		for _, m := range svc.Methods {
			if !m.AuthRequired {
				procedure := fmt.Sprintf("/%s.%s/%s", svc.Package, svc.Name, m.Name)
				skipMethods = append(skipMethods, procedure)
			}
		}
	}

	if err := codegen.GenerateAuthMiddleware(&cfg.Auth, modPath, skipMethods, projectDir, cs); err != nil {
		return err
	}

	fmt.Println("  ✅ Generated pkg/middleware/auth_gen.go")
	if cfg.Auth.Provider == "api_key" || cfg.Auth.Provider == "both" {
		fmt.Println("  ✅ Generated pkg/middleware/auth_validator.go (if not exists)")
	}

	return nil
}

// generateTenantMiddleware generates pkg/middleware/tenant_gen.go from the multi-tenant config.
func generateTenantMiddleware(cfg *config.ProjectConfig, projectDir string, cs *generator.FileChecksums) error {
	fmt.Println("🔧 Generating tenant middleware...")

	if err := codegen.GenerateTenantMiddleware(cfg.Auth.MultiTenant, projectDir, cs); err != nil {
		return err
	}

	fmt.Println("  ✅ Generated pkg/middleware/tenant_gen.go")
	return nil
}

// generateWebhookRoutes generates webhook_routes_gen.go for each service that has webhooks.
//
// cs is the project's checksum tracker — passing it ensures the rendered
// webhook_routes_gen.go is recorded so it doesn't show up as an orphan
// in `forge audit`. A nil cs is tolerated.
func generateWebhookRoutes(cfg *config.ProjectConfig, projectDir string, cs *generator.FileChecksums) error {
	for _, svc := range cfg.Services {
		if len(svc.Webhooks) == 0 {
			continue
		}

		svcPkg := generator.ServicePackageName(svc.Name)
		svcDir := filepath.Join(projectDir, "handlers", svcPkg)
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
			Package:  svcPkg,
			Webhooks: entries,
		}

		content, err := templates.WebhookTemplates().Render("webhook_routes_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render webhook routes for %s: %w", svc.Name, err)
		}

		relPath := filepath.Join("handlers", svcPkg, "webhook_routes_gen.go")
		if _, err := generator.WriteGeneratedFile(projectDir, relPath, content, cs, true); err != nil {
			return fmt.Errorf("write webhook routes for %s: %w", svc.Name, err)
		}

		// Print the actual landing path (snake-case package dir), not svc.Name
		// (which can be kebab-case). Pre-fix: kebab/snake mismatch made the
		// log line untrustworthy when a service name contained hyphens.
		fmt.Printf("  ✅ Generated %s\n", relPath)
	}

	return nil
}

// generateInternalPackageContracts walks internal/ recursively and, for every
// directory containing a contract.go, generates mock_gen.go, middleware_gen.go,
// tracing_gen.go and metrics_gen.go via the contract AST generator.
//
// The walk descends into sub-packages (e.g. internal/mcp/database/contract.go)
// because the original os.ReadDir-based implementation only saw immediate
// children of internal/ and silently skipped nested contracts.
//
// Directories listed in cfg.Contracts.Exclude (matched against the module-relative
// path, e.g. "internal/linter/contract") are skipped wholesale — the walk does
// not descend into them. testdata/ subtrees are also skipped because they hold
// linter fixtures whose contract.go files are not real packages.
func generateInternalPackageContracts(projectDir string, cfg *config.ProjectConfig) error {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil
	}

	generated := 0
	walkErr := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}

		// Skip testdata/ subtrees — fixture contracts, not real packages.
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}

		// Compute the module-relative path (e.g. "internal/mcp/database") so it
		// can be matched against the exclude list. Use forward slashes regardless
		// of OS so the patterns in forge.yaml stay portable.
		rel, relErr := filepath.Rel(projectDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		// Honor cfg.Contracts.Exclude — skip the directory entirely so neither
		// it nor its descendants get generated. Excludes apply to nested paths
		// too (e.g. "internal/linter/contract").
		if cfg != nil && cfg.Contracts.IsExcluded(rel) {
			return filepath.SkipDir
		}

		contractPath := filepath.Join(path, "contract.go")
		if _, statErr := os.Stat(contractPath); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return statErr
		}

		if genErr := contract.Generate(contractPath); genErr != nil {
			return fmt.Errorf("generate contract for %s: %w", rel, genErr)
		}
		fmt.Printf("  ✅ Generated mock + middleware for %s/\n", rel)

		// Scaffold contract_test.go once. The user owns the file after
		// the first scaffold — never overwrite.
		//
		// Two correctness considerations:
		//
		//  1. Nested package paths (e.g. internal/mcp/database) need the
		//     full module-relative path in the import, not just the leaf
		//     directory. Pass ImportPath = "mcp/database" so the template
		//     emits `{{.Module}}/internal/{{.ImportPath}}`.
		//
		//  2. Multi-interface packages have ambiguous "which constructor?"
		//     semantics. The template emits `pkg.New(pkg.Deps{})` which only
		//     compiles for the canonical single-Service shape. Skip the
		//     scaffold and let the user write tests by hand (the testing
		//     skill template covers the multi-interface pattern).
		testPath := filepath.Join(path, "contract_test.go")
		if _, statErr := os.Stat(testPath); os.IsNotExist(statErr) {
			cf, parseErr := contract.ParseContract(contractPath)
			if parseErr != nil {
				return fmt.Errorf("parse contract for %s: %w", rel, parseErr)
			}
			if len(cf.Interfaces) > 1 {
				fmt.Printf("  ℹ️  Skipped contract_test.go scaffold for %s/ (multi-interface package; write tests manually)\n", rel)
			} else if len(cf.Interfaces) == 1 && cf.Interfaces[0].Name != "Service" {
				// Single-interface, non-canonical name (e.g. Manager,
				// Handler). The template scaffolds `pkg.New(pkg.Deps{})`
				// which won't match a non-Service shape — skip and let
				// the user write the test manually.
				fmt.Printf("  ℹ️  Skipped contract_test.go scaffold for %s/ (interface %q is not the canonical Service shape; write tests manually)\n", rel, cf.Interfaces[0].Name)
			} else if !packageHasTwoResultNew(path) {
				// The contract_test.go.tmpl emits the canonical two-result
				// form `_, err := pkg.New(pkg.Deps{})`. Packages whose
				// `New` is still the legacy single-result form
				// (`func New(Deps) Service`) would get a non-compiling
				// scaffold — skip and leave breadcrumb. Polish New to
				// `(Service, error)` to opt back in.
				fmt.Printf("  ℹ️  Skipped contract_test.go scaffold for %s/ (New is single-result; polish to `func New(Deps) (Service, error)` to enable auto-scaffold)\n", rel)
			} else {
				pkgName := filepath.Base(path)
				// importPath is "mcp/database" for internal/mcp/database, just
				// "database" for internal/database. Strip the leading
				// "internal/" segment from the module-relative path.
				importPath := strings.TrimPrefix(rel, "internal/")
				modPath, modErr := codegen.GetModulePath(projectDir)
				if modErr != nil {
					return fmt.Errorf("read module path for contract_test scaffold: %w", modErr)
				}
				data := struct {
					Name       string
					ImportPath string
					Module     string
				}{Name: pkgName, ImportPath: importPath, Module: modPath}
				content, renderErr := templates.InternalPkgTemplates().Render("contract_test.go.tmpl", data)
				if renderErr != nil {
					return fmt.Errorf("render contract_test.go for %s: %w", rel, renderErr)
				}
				if writeErr := os.WriteFile(testPath, content, 0644); writeErr != nil {
					return fmt.Errorf("write contract_test.go for %s: %w", rel, writeErr)
				}
				fmt.Printf("  ✅ Scaffolded contract_test.go for %s/\n", rel)
			}
		}

		generated++
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk internal/ directory: %w", walkErr)
	}

	if generated > 0 {
		fmt.Printf("🔧 Generated contracts for %d internal package(s)\n", generated)
	}

	return nil
}

// packageHasTwoResultNew reports whether the package at dir defines a
// constructor with the canonical two-result `func New(deps Deps) (Service, error)`
// shape. Returns true if any *.go file in dir contains that signature.
// Returns false on read errors so the caller falls back to "skip
// auto-scaffold" — the safe direction.
//
// Source-text scan (not AST) is intentional: this runs in the inner
// loop of `forge generate` and the scaffold gate only needs a coarse
// match. False negatives (signatures spread oddly across lines) just
// suppress the auto-scaffold; the user can re-run after polishing.
func packageHasTwoResultNew(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	// Common variations of whitespace / receiver-less New decl.
	needles := []string{
		"func New(deps Deps) (Service, error)",
		"func New(d Deps) (Service, error)",
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		s := string(data)
		for _, n := range needles {
			if strings.Contains(s, n) {
				return true
			}
		}
	}
	return false
}

// generateConfigLoader parses proto/config/ for config protos with
// ConfigFieldOptions annotations and generates pkg/config/config.go.
// The features parameter controls which config fields make it into
// cmd/server.go — when migrations are disabled, migration-related
// fields (AutoMigrate, DatabaseUrl, pool tuning) are excluded from
// the server template so it doesn't reference app.AutoMigrate().
func generateConfigLoader(projectDir string, features config.FeaturesConfig, cs *generator.FileChecksums) (map[string]bool, error) {
	fmt.Println("🔧 Generating config loader from proto/config/...")

	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto/config"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse config protos: %w", err)
	}

	if len(messages) == 0 {
		fmt.Println("  ℹ️  No config fields with config_field annotations found; using default scaffold config fields")
		messages = codegen.DefaultConfigMessages()
	}

	if err := codegen.GenerateConfigLoader(messages, projectDir, cs); err != nil {
		return nil, fmt.Errorf("failed to generate config loader: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/config/config.go")

	// Build the config field map and strip migration-related fields when
	// migrations are disabled. The server template conditionally includes
	// migration code based on ConfigFields["AutoMigrate"]; removing it
	// prevents the template from emitting app.AutoMigrate() calls that
	// reference the ungenerated pkg/app/migrate.go.
	configFields := codegen.ConfigFieldNamesFromMessages(messages)
	if !features.MigrationsEnabled() {
		delete(configFields, "AutoMigrate")
		delete(configFields, "DatabaseUrl")
		delete(configFields, "MaxOpenConns")
		delete(configFields, "MaxIdleConns")
		delete(configFields, "ConnMaxIdleTime")
		delete(configFields, "ConnMaxLifetime")
	}

	// Re-render cmd/server.go so it stays in sync with the config fields.
	if err := codegen.GenerateCmdServerWithFields(configFields, projectDir, cs); err != nil {
		return nil, fmt.Errorf("failed to regenerate cmd/server.go: %w", err)
	}

	fmt.Println("  ✅ Regenerated cmd/server.go")
	return configFields, nil
}

// generatePerEnvDeployConfig renders deploy/kcl/<env>/config_gen.k for
// every environment declared in forge.yaml. It folds together:
//
//   - the proto annotations (sensitive, category, env_var) parsed from
//     proto/config/, and
//   - the merged per-env config map (forge.yaml inline + optional
//     config.<env>.yaml sibling).
//
// The result is one file per env that the env's hand-edited main.k can
// import to wire the rendered EnvVar lists into the application's env_vars.
//
// This step replaces the hand-curated `<NAME>_ENV` lambdas in
// deploy/kcl/base.k that projects accumulate as soon as they grow more
// than a couple of secret-backed knobs.
func generatePerEnvDeployConfig(projectDir string, cfg *config.ProjectConfig, cs *generator.FileChecksums) error {
	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto/config"))
	if err != nil {
		return fmt.Errorf("parse config protos: %w", err)
	}
	if len(messages) == 0 {
		// No annotated config — fall back to scaffold defaults. Same
		// behavior as generateConfigLoader so the two stay in sync.
		messages = codegen.DefaultConfigMessages()
	}

	// Flatten all proto config fields. Most projects have a single
	// AppConfig message; multiple are supported but rendered as one set.
	var fields []codegen.ConfigField
	for _, m := range messages {
		fields = append(fields, m.Fields...)
	}

	kclDir := cfg.K8s.KCLDir
	if kclDir == "" {
		kclDir = "deploy/kcl"
	}
	kclDirAbs := filepath.Join(projectDir, kclDir)

	for _, env := range cfg.Envs {
		envCfg, err := config.LoadEnvironmentConfig(cfg, projectDir, env.Name)
		if err != nil {
			// An env with no inline config + no sibling file is fine —
			// just emit the file with secret-only fields and skip
			// non-sensitive ones (no values to inline).
			envCfg = map[string]any{}
		}
		if err := codegen.GenerateDeployConfig(codegen.DeployConfigGenInput{
			ProjectName: cfg.Name,
			EnvName:     env.Name,
			KCLDir:      kclDirAbs,
			ProjectDir:  projectDir,
			Fields:      fields,
			EnvConfig:   envCfg,
			Envs:        cfg.Envs,
			Checksums:   cs,
		}); err != nil {
			return fmt.Errorf("emit %s config_gen.k: %w", env.Name, err)
		}
	}
	fmt.Printf("  ✅ Generated deploy/kcl/<env>/config_gen.k for %d environments\n", len(cfg.Envs))
	return nil
}