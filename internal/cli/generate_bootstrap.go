package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// generateBootstrap regenerates pkg/app/bootstrap.go with explicit service construction.
func generateBootstrap(services []codegen.ServiceDef, modulePath string, databaseDriver string, ormEnabled bool, projectDir string) error {
	fmt.Println("🔧 Generating pkg/app/bootstrap.go...")

	workers := discoverWorkers(projectDir)
	operators := discoverOperators(projectDir)

	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	packages := discoverPackages(projectDir)

	hasDatabase := databaseDriver != ""
	if err := codegen.GenerateBootstrap(services, packages, workers, operators, modulePath, hasDatabase, ormEnabled, projectDir); err != nil {
		return fmt.Errorf("failed to generate bootstrap: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/bootstrap.go")

	// Generate setup.go (user-owned, never overwritten)
	if err := codegen.GenerateSetup(modulePath, databaseDriver, ormEnabled, projectDir); err != nil {
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
