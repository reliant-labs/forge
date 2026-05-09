package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/generator"
)

// generateBootstrap regenerates pkg/app/bootstrap.go with explicit service construction.
func generateBootstrap(services []codegen.ServiceDef, modulePath string, databaseDriver string, ormEnabled bool, projectDir string, configFields map[string]bool, cs *checksums.FileChecksums) error {
	fmt.Println("🔧 Generating pkg/app/bootstrap.go...")

	workers := discoverWorkers(projectDir)
	operators := discoverOperators(projectDir)

	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	packages := discoverPackages(projectDir)

	// Build the webhook-services map keyed by snake-case service package
	// name. The bootstrap template uses this to emit
	// `RegisterWebhookRoutes(mux, stack)` after `RegisterHTTP(...)` for
	// services that have webhooks declared in forge.yaml — auto-wiring
	// the generated webhook routes onto the mux instead of forcing the
	// user to hand-edit the user-owned `RegisterHTTP` body.
	// (2026-04-30 LLM-port webhook auto-wire fix.)
	webhookServices := discoverWebhookServices(projectDir)

	hasDatabase := databaseDriver != ""

	// Generate app_gen.go FIRST — this owns the canonical *App struct
	// shape with `*AppExtras` embedded. wire_gen later parses pkg/app/
	// looking for the App struct + AppExtras fields, so app_gen.go must
	// be on disk before wire_gen runs.
	if err := codegen.GenerateAppGen(hasDatabase, ormEnabled, len(services) > 0, len(workers) > 0, len(operators) > 0, len(packages) > 0, projectDir, cs); err != nil {
		return fmt.Errorf("failed to generate app_gen.go: %w", err)
	}
	fmt.Println("  ✅ Generated pkg/app/app_gen.go")

	// Generate app_extras.go (Tier-2 user-owned scaffold). Written
	// ONCE — never overwritten on subsequent generates. Holds the
	// empty AppExtras struct that wire_gen consults for user-extension
	// fields.
	if err := codegen.GenerateAppExtras(projectDir); err != nil {
		return fmt.Errorf("failed to generate app_extras.go: %w", err)
	}

	if err := codegen.GenerateBootstrap(services, packages, workers, operators, modulePath, hasDatabase, ormEnabled, projectDir, configFields, webhookServices, cs); err != nil {
		return fmt.Errorf("failed to generate bootstrap: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/bootstrap.go")

	// Generate setup.go (user-owned, never overwritten). Must happen
	// before wire_gen so the App struct in pkg/app is parseable when
	// wire_gen scans it for unconventional Deps-field producers.
	if err := codegen.GenerateSetup(modulePath, databaseDriver, ormEnabled, projectDir); err != nil {
		return fmt.Errorf("failed to generate setup.go: %w", err)
	}

	// Generate wire_gen.go after bootstrap + app_gen + app_extras.
	// wire_gen parses each service/worker/operator Deps struct + the
	// live *App struct (from pkg/app/app_gen.go) + the user's AppExtras
	// (from pkg/app/app_extras.go) to emit one wireXxxDeps function per
	// component. Bootstrap.go calls those functions to assemble the full
	// Deps before component.New(deps), eliminating the pre-2026-05-07
	// ApplyDeps two-phase init.
	//
	// packages/workers/operators are passed alongside services so wire_gen
	// uses the SAME collision-aware FieldName as bootstrap when a service
	// package collides with an internal-package import (svc Billing +
	// internal/billing → wireSvcBillingDeps on both sides). Bug-1 of the
	// 2026-05-07 wire_gen rollout.
	if err := codegen.GenerateWireGen(services, packages, workers, operators, modulePath, projectDir, ormEnabled, cs); err != nil {
		return fmt.Errorf("failed to generate wire_gen.go: %w", err)
	}
	if len(services) > 0 || len(workers) > 0 || len(operators) > 0 {
		fmt.Println("  ✅ Generated pkg/app/wire_gen.go")
	}

	return nil
}

// generateBootstrapTesting regenerates pkg/app/testing.go with test helpers.
func generateBootstrapTesting(services []codegen.ServiceDef, modulePath string, multiTenantEnabled bool, projectDir string, cs *checksums.FileChecksums) error {
	fmt.Println("🔧 Generating pkg/app/testing.go...")

	workers := discoverWorkers(projectDir)
	operators := discoverOperators(projectDir)

	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	packages := discoverPackages(projectDir)

	if err := codegen.GenerateBootstrapTesting(services, packages, workers, operators, modulePath, multiTenantEnabled, projectDir, cs); err != nil {
		return fmt.Errorf("failed to generate bootstrap testing: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/testing.go")
	return nil
}

// generateMigrate writes pkg/app/migrate.go with embedded migration support.
func generateMigrate(projectDir, modulePath string, cs *checksums.FileChecksums) error {
	fmt.Println("🔧 Generating pkg/app/migrate.go...")

	has := hasSQLMigrations(projectDir)
	if err := codegen.GenerateMigrate(projectDir, modulePath, has, cs); err != nil {
		return fmt.Errorf("failed to generate migrate.go: %w", err)
	}

	if has {
		fmt.Println("  ✅ Generated pkg/app/migrate.go (with embedded migrations)")
	} else {
		fmt.Println("  ✅ Generated pkg/app/migrate.go (no migrations yet)")
	}
	return nil
}

// discoverPackages returns BootstrapPackageData for internal packages.
// It walks internal/ recursively to find every directory containing a
// contract.go (Go interface contract). Names are returned in nested form
// (e.g. "mcp/database") so PackageDataFromNames can derive a unique
// FieldName/VarName and the bootstrap template can emit the correct import
// path. Directories listed in cfg.Contracts.Exclude are skipped wholesale,
// matching the behavior of generate_middleware.go's contract walk.
func discoverPackages(projectDir string) []codegen.BootstrapPackageData {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil
	}

	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	cfg, _ := loadProjectConfigFrom(cfgPath) // best-effort; nil cfg means no excludes

	var names []string
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
		// Compute module-relative path (e.g. "internal/mcp/database") for
		// exclude-matching. Use forward slashes regardless of OS so patterns
		// in forge.yaml stay portable.
		rel, relErr := filepath.Rel(projectDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if cfg != nil && cfg.Contracts.IsExcluded(rel) {
			return filepath.SkipDir
		}
		contractPath := filepath.Join(path, "contract.go")
		if _, statErr := os.Stat(contractPath); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return statErr
		}
		// Name is the path under internal/, e.g. "cache" or "mcp/database".
		name := strings.TrimPrefix(rel, "internal/")
		names = append(names, name)
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		fmt.Fprintf(os.Stderr, "Warning: walking %s: %v\n", internalDir, walkErr)
		return nil
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

// discoverWebhookServices returns a set of snake-case service package
// names for which forge.yaml declares one or more webhooks. The bootstrap
// template uses this to emit `RegisterWebhookRoutes(mux, stack)` after
// `RegisterHTTP(...)` so generated webhook routes get auto-mounted on the
// mux without the user having to hand-edit the user-owned `RegisterHTTP`
// body in handlers/<svc>/service.go.
//
// Keying matches `codegen.toServicePackage(svc.Name)` for proto-derived
// services: forge.yaml's hyphenated CLI name ("admin-server") and the
// proto's PascalCase form ("AdminServerService") both normalize to
// "admin_server", which is also the directory leaf under handlers/.
func discoverWebhookServices(projectDir string) map[string]bool {
	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	cfg, err := loadProjectConfigFrom(cfgPath)
	if err != nil || cfg == nil {
		return nil
	}

	out := map[string]bool{}
	for _, svc := range cfg.Services {
		if len(svc.Webhooks) == 0 {
			continue
		}
		out[generator.ServicePackageName(svc.Name)] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
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