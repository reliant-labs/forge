package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

// generateBootstrap regenerates pkg/app/bootstrap.go with explicit service construction.
func generateBootstrap(services []codegen.ServiceDef, modulePath string, databaseDriver string, ormEnabled bool, projectDir string, configFields map[string]bool, bootstrapFeatures codegen.BootstrapFeatures, cs *checksums.FileChecksums) error {
	fmt.Println("🔧 Generating pkg/app/bootstrap.go...")

	workers, err := discoverWorkers(projectDir)
	if err != nil {
		return err
	}
	operators, err := discoverOperators(projectDir)
	if err != nil {
		return err
	}

	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	packages, err := discoverPackages(projectDir)
	if err != nil {
		return fmt.Errorf("discover internal packages: %w", err)
	}

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

	if err := codegen.GenerateBootstrap(services, packages, workers, operators, modulePath, hasDatabase, ormEnabled, projectDir, configFields, webhookServices, bootstrapFeatures, cs); err != nil {
		return fmt.Errorf("failed to generate bootstrap: %w", err)
	}

	fmt.Println("  ✅ Generated pkg/app/bootstrap.go")

	// Generate setup.go (user-owned, never overwritten). Must happen
	// before wire_gen so the App struct in pkg/app is parseable when
	// wire_gen scans it for unconventional Deps-field producers.
	if err := codegen.GenerateSetup(modulePath, databaseDriver, ormEnabled, projectDir); err != nil {
		return fmt.Errorf("failed to generate setup.go: %w", err)
	}

	// Generate post_bootstrap.go (user-owned, never overwritten). The
	// generated cmd/server.go calls `app.PostBootstrap(application)`
	// after Bootstrap, so the file must exist before downstream
	// compilation. Default body is a no-op; users own it after first
	// emit.
	if err := codegen.GeneratePostBootstrap(projectDir); err != nil {
		return fmt.Errorf("failed to generate post_bootstrap.go: %w", err)
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
	wireData, err := codegen.GenerateWireGenData(services, packages, workers, operators, modulePath, projectDir, ormEnabled, cs)
	if err != nil {
		return fmt.Errorf("failed to generate wire_gen.go: %w", err)
	}
	if len(services) > 0 || len(workers) > 0 || len(operators) > 0 {
		fmt.Println("  ✅ Generated pkg/app/wire_gen.go")
	}

	// Diagnostics codegen runs AFTER wire_gen so it can name the
	// component / dep that landed at nil. Each kind of wire function gets
	// its own prefix ("wire" for services, "wireWorker" for workers,
	// "wireOperator" for operators) so the registered Component matches
	// the symbol the wire function actually emits at runtime — operators
	// can grep boot logs by component name and find the function in
	// wire_gen.go directly. Stubs are scanned separately by the
	// diagnostics codegen itself from handlers/<svc>/*.go for the
	// `// forge:gen unwired-stub` marker the handler templates emit.
	nilDeps := codegen.NilDepEntriesFromWireData(wireData.Services, "wire")
	nilDeps = append(nilDeps, codegen.NilDepEntriesFromWireData(wireData.Workers, "wireWorker")...)
	nilDeps = append(nilDeps, codegen.NilDepEntriesFromWireData(wireData.Operators, "wireOperator")...)
	if err := codegen.GenerateDiagnostics(services, workers, operators, modulePath, projectDir, nilDeps, cs); err != nil {
		return fmt.Errorf("failed to generate diagnostics_gen.go: %w", err)
	}
	if len(services) > 0 || len(workers) > 0 || len(operators) > 0 {
		fmt.Println("  ✅ Generated pkg/app/diagnostics_gen.go")
	}

	return nil
}

// generateBootstrapTesting regenerates pkg/app/testing.go with test helpers.
func generateBootstrapTesting(services []codegen.ServiceDef, modulePath string, multiTenantEnabled bool, projectDir string, cs *checksums.FileChecksums) error {
	fmt.Println("🔧 Generating pkg/app/testing.go...")

	workers, err := discoverWorkers(projectDir)
	if err != nil {
		return err
	}
	operators, err := discoverOperators(projectDir)
	if err != nil {
		return err
	}

	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	packages, err := discoverPackages(projectDir)
	if err != nil {
		return fmt.Errorf("discover internal packages: %w", err)
	}

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
//
// A walk error is returned so the caller can fail the pipeline rather
// than silently emit a partial bootstrap (which would surface later as
// a mysterious "undefined: pkg" go build error in pkg/app/bootstrap.go).
func discoverPackages(projectDir string) ([]codegen.BootstrapPackageData, error) {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil, nil
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
		return nil, fmt.Errorf("walking %s: %w", internalDir, walkErr)
	}

	return codegen.PackageDataFromNames(names, projectDir)
}

// discoverWorkers returns BootstrapWorkerData for all worker-type services in
// the project config. Passes each service's explicit `path:` field through so
// snake_case dir layouts (e.g. workers/climatology_refresh) produce the
// correct import line — without `path:`, the legacy compaction would emit
// workers/climatologyrefresh and the generated bootstrap would fail to
// compile. The returned error is a disk-first resolution failure (worker dir
// exists but its package clause is unparseable/conflicting) — see
// codegen.ResolveComponentDir.
func discoverWorkers(projectDir string) ([]codegen.BootstrapWorkerData, error) {
	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	cfg, err := loadProjectConfigFrom(cfgPath)
	if err != nil || cfg == nil {
		return nil, nil
	}

	var specs []codegen.WorkerSpec
	for _, svc := range cfg.Services {
		if strings.EqualFold(svc.Type, "worker") {
			specs = append(specs, codegen.WorkerSpec{Name: svc.Name, Path: svc.Path})
		}
	}
	return codegen.WorkerDataFromSpecs(specs, projectDir)
}

// discoverWebhookServices returns a set of snake-case service package
// names for which forge.yaml declares one or more webhooks. The bootstrap
// template uses this to emit `RegisterWebhookRoutes(mux, stack)` after
// `RegisterHTTP(...)` so generated webhook routes get auto-mounted on the
// mux without the user having to hand-edit the user-owned `RegisterHTTP`
// body in handlers/<svc>/service.go.
//
// Keying matches `naming.ServicePackage(svc.Name)` for proto-derived
// services: forge.yaml's hyphenated CLI name ("admin-server") and the
// proto's PascalCase form ("AdminServerService") both normalize to
// "admin_server" (post-2026-06-08 snake-canonicalisation), which is
// also the directory leaf under handlers/.
func discoverWebhookServices(projectDir string) map[string]bool {
	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	cfg, err := loadProjectConfigFrom(cfgPath)
	if err != nil || cfg == nil {
		return nil
	}
	// Best-effort registration view: webhooks on an unregistered service
	// are a hard error earlier in the pipeline (generateWebhookRoutes),
	// but this map is also built on standalone paths, so filter here too
	// rather than emitting a RegisterWebhookRoutes call into a row
	// constructor whose service the binary doesn't serve. A parse error
	// falls open to "registered" — the build/pipeline reports it.
	reg, regErr := loadServiceRegistry(projectDir)
	if regErr != nil {
		reg = &serviceRegistry{Exists: false}
	}

	out := map[string]bool{}
	for _, svc := range cfg.Services {
		if len(svc.Webhooks) == 0 {
			continue
		}
		if isConnectServiceConfig(svc) && !reg.registered(svc.Name) {
			continue
		}
		out[naming.ServicePackage(svc.Name)] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// discoverOperators returns BootstrapOperatorData for all operator-type
// services in the project config. Honors the `path:` field for the same
// reason as discoverWorkers; error semantics match discoverWorkers.
func discoverOperators(projectDir string) ([]codegen.BootstrapOperatorData, error) {
	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	cfg, err := loadProjectConfigFrom(cfgPath)
	if err != nil || cfg == nil {
		return nil, nil
	}

	var specs []codegen.OperatorSpec
	for _, svc := range cfg.Services {
		if strings.EqualFold(svc.Type, "operator") {
			specs = append(specs, codegen.OperatorSpec{Name: svc.Name, Path: svc.Path})
		}
	}
	return codegen.OperatorDataFromSpecs(specs, projectDir)
}
