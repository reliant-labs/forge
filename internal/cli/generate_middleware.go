package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

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
	// Methods where auth_required is not set (false) get their full procedure
	// name added so the auth interceptor skips them.
	var skipMethods []string
	for _, svc := range services {
		for _, m := range svc.Methods {
			if !m.AuthRequired {
				procedure := fmt.Sprintf("/%s.%s/%s", svc.Package, svc.Name, m.Name)
				skipMethods = append(skipMethods, procedure)
			}
		}
	}

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

		content, err := templates.WebhookTemplates.Render("webhook_routes_gen.go.tmpl", data)
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
// The features parameter controls which config fields make it into
// cmd/server.go — when migrations are disabled, migration-related
// fields (AutoMigrate, DatabaseUrl, pool tuning) are excluded from
// the server template so it doesn't reference app.AutoMigrate().
func generateConfigLoader(projectDir string, features config.FeaturesConfig) (map[string]bool, error) {
	fmt.Println("🔧 Generating config loader from proto/config/...")

	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto/config"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse config protos: %w", err)
	}

	if len(messages) == 0 {
		fmt.Println("  ℹ️  No config fields with config_field annotations found; using default scaffold config fields")
		messages = codegen.DefaultConfigMessages()
	}

	if err := codegen.GenerateConfigLoader(messages, projectDir); err != nil {
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
	if err := codegen.GenerateCmdServerWithFields(configFields, projectDir); err != nil {
		return nil, fmt.Errorf("failed to regenerate cmd/server.go: %w", err)
	}

	fmt.Println("  ✅ Regenerated cmd/server.go")
	return configFields, nil
}