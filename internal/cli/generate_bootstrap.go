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

// generateAppSubstrate scaffolds the user-owned pkg/app substrate
// (app_gen.go / app_extras.go / setup.go / post_bootstrap.go).
//
// FORGE_SHAPE_REDESIGN §2: the LIVE runtime DI composition is the
// internal/app layer (OpenInfra → Build → PostBuild → Inventory),
// emitted by stepInternalAppComposition. The old name-matched pkg/app
// DI unit (bootstrap.go, wire_gen.go, services_gen.go, services.go,
// diagnostics_gen.go, the appkit.Def/ServiceDef/Run table) is retired.
//
// What remains under pkg/app is the user-owned scaffold setup.go +
// post_bootstrap.go reference (a minimal *App carrier in app_gen.go +
// the AppExtras extension surface). These are kept so the user-owned
// setup.go — which forge never overwrites — still compiles. See the
// setup.go↔providers.go reconciliation note in the redesign doc.
func generateAppSubstrate(services []codegen.ServiceDef, modulePath string, databaseDriver string, ormEnabled bool, projectDir string, cs *checksums.FileChecksums) error {
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

	hasDatabase := databaseDriver != ""

	// app_gen.go owns the minimal *App carrier (DB / ORM + the embedded
	// *AppExtras) that the user-owned setup.go compiles against.
	if err := codegen.GenerateAppGen(hasDatabase, ormEnabled, len(services) > 0, len(workers) > 0, len(operators) > 0, false, projectDir, cs); err != nil {
		return fmt.Errorf("failed to generate app_gen.go: %w", err)
	}
	fmt.Println("  ✅ Generated pkg/app/app_gen.go")

	// app_extras.go (Tier-2 user-owned). Written ONCE — never overwritten.
	if err := codegen.GenerateAppExtras(projectDir); err != nil {
		return fmt.Errorf("failed to generate app_extras.go: %w", err)
	}

	// setup.go (user-owned, never overwritten).
	if err := codegen.GenerateSetup(modulePath, databaseDriver, ormEnabled, projectDir); err != nil {
		return fmt.Errorf("failed to generate setup.go: %w", err)
	}

	// post_bootstrap.go (user-owned, never overwritten).
	if err := codegen.GeneratePostBootstrap(projectDir); err != nil {
		return fmt.Errorf("failed to generate post_bootstrap.go: %w", err)
	}

	return nil
}

// generateHybridComposition emits the internal/app composition layer
// (PASS 1, additive). Scaffold-once owned files (providers.go, post_build.go)
// are written before the generated injector so its Infra-field resolution
// sees the Infra struct on the first pass too.
func generateHybridComposition(services []codegen.ServiceDef, packages []codegen.BootstrapPackageData, workers []codegen.BootstrapWorkerData, operators []codegen.BootstrapOperatorData, modulePath, databaseDriver string, ormEnabled bool, projectDir string, webhookServices map[string]bool, cs *checksums.FileChecksums) error {
	// NO len()==0 early-return: the generated cmd/server.go imports
	// internal/app unconditionally (OpenInfra → Build → PostBuild → mount via
	// Inventory → serverkit.Run), so internal/app must be a non-empty,
	// compilable package even when no component is discovered (degenerate
	// trees with no parseable proto service / no descriptor). The generators
	// + templates below emit valid empty Build/Inventory/Services/lifecycle so
	// `go mod tidy` resolves internal/app LOCALLY instead of 404ing.

	// Owned, scaffold-once (never overwritten after first emit).
	if err := codegen.GenerateProviders(modulePath, databaseDriver, ormEnabled, projectDir); err != nil {
		return fmt.Errorf("failed to scaffold internal/app/providers.go: %w", err)
	}
	if err := codegen.GeneratePostBuild(projectDir); err != nil {
		return fmt.Errorf("failed to scaffold internal/app/post_build.go: %w", err)
	}

	// Generated injector + Services registry (regenerated every run). A
	// MissingProvider here is a LOUD generate-time error naming the type +
	// component + field the user must add to Infra.
	if err := codegen.GenerateInject(codegen.InjectGenInput{
		GenContext: codegen.GenContext{ProjectDir: projectDir, ModulePath: modulePath, Checksums: cs},
		Services:   services,
		Packages:   packages,
		Workers:    workers,
		Operators:  operators,
	}); err != nil {
		return fmt.Errorf("failed to generate internal/app/inject_gen.go: %w", err)
	}

	// Supervised-component surface (workers/operators) over *Services.
	if err := codegen.GenerateLifecycle(codegen.InjectGenInput{
		GenContext: codegen.GenContext{ProjectDir: projectDir, ModulePath: modulePath, Checksums: cs},
		Services:   services,
		Packages:   packages,
		Workers:    workers,
		Operators:  operators,
	}); err != nil {
		return fmt.Errorf("failed to generate internal/app/lifecycle_gen.go: %w", err)
	}

	// Data-only mount inventory (services only).
	if err := codegen.GenerateInventory(codegen.InventoryGenInput{
		GenContext:      codegen.GenContext{ProjectDir: projectDir, ModulePath: modulePath, Checksums: cs},
		Services:        services,
		Packages:        packages,
		Workers:         workers,
		Operators:       operators,
		WebhookServices: webhookServices,
	}); err != nil {
		return fmt.Errorf("failed to generate internal/app/inventory_gen.go: %w", err)
	}

	// NOTE: the REAL per-component cmd-group subcommands (dir-nested under
	// cmd/<bin>/cmd/{services,workers,operators}) are NOT emitted here. They
	// are anchored by the dedicated stepCmdGroups pipeline step, which runs
	// AFTER stepRegenerateInfra has (re)created cmd/<bin>/cmd/serve.go +
	// cmd/<bin>/main.go. Doing it here would silently no-op on a flat→nested
	// migration: serve.go doesn't exist yet at composition time, so the group
	// subpackages would never get anchored — yet the freshly-regenerated
	// main.go blank-imports them, and the next `go mod tidy` / `go build`
	// would 404 the empty (Go-file-less) local group dirs. See
	// generateCmdGroups + stepCmdGroups.

	fmt.Println("  ✅ Generated internal/app composition layer (Build + Inventory + Infra)")
	return nil
}

// generateCmdGroups anchors the dir-nested per-component command-group
// subpackages under cmd/<bin>/cmd/{services,workers,operators}: one
// services/<name>.go per service whose RunE calls cmd.Serve() with the TYPED
// mount method expression (*app.Services).Mount<Svc> (no string selection);
// one workers/<name>.go and operators/<name>.go per worker/operator
// (cmd.MountNone + a named supervised subset). Each group also gets a
// register_gen.go anchor so the subpackage compiles (and main.go's blank
// import resolves) even with ZERO items.
//
// Driven by the SAME `services`/`workers`/`operators` rows the composition
// layer is, so each subcommand lines up with a typed mount / WorkerList /
// OperatorList entry.
//
// Emitted only when the primary binary's cmd/<bin>/cmd/serve.go exists —
// CLI/library kinds and codegen-less trees have no serve pipeline to delegate
// to. The caller (stepCmdGroups) sequences this AFTER stepRegenerateInfra so
// that on a flat→nested migration — where serve.go does not exist until infra
// regen creates it — the group subpackages still get anchored before any
// `go mod tidy` / build-validate that imports them. Idempotent: re-running on
// an already-nested project rewrites byte-identical content.
func generateCmdGroups(services []codegen.ServiceDef, workers []codegen.BootstrapWorkerData, operators []codegen.BootstrapOperatorData, projectDir string, cs *checksums.FileChecksums) error {
	bin := bootstrapBinaryName(projectDir)
	if _, statErr := os.Stat(filepath.Join(projectDir, "cmd", bin, "cmd", "serve.go")); statErr != nil {
		return nil
	}
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	if err := codegen.GenerateCmdGroups(codegen.CmdServiceGroupInput{
		Bin:       bin,
		Services:  names,
		Workers:   workers,
		Operators: operators,
	}, projectDir, cs); err != nil {
		return fmt.Errorf("failed to generate cmd/%s command-group subcommands: %w", bin, err)
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

	if err := codegen.GenerateBootstrapTesting(codegen.BootstrapTestingGenInput{
		GenContext: codegen.GenContext{
			ProjectDir: projectDir,
			ModulePath: modulePath,
			Checksums:  cs,
		},
		Services:           services,
		Packages:           packages,
		Workers:            workers,
		Operators:          operators,
		MultiTenantEnabled: multiTenantEnabled,
	}); err != nil {
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
// bootstrapBinaryName resolves the primary binary name — the cmd/<bin>/
// directory leaf the command tree lives under. It is the forge.yaml project
// name; falls back to the project directory's base name when the config is
// unreadable (degenerate/standalone trees), mirroring the generator's
// binaryName().
func bootstrapBinaryName(projectDir string) string {
	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	if store, err := loadProjectStoreFrom(cfgPath); err == nil && store != nil {
		if name := store.Config().Name; name != "" {
			return name
		}
	}
	return filepath.Base(projectDir)
}

func discoverPackages(projectDir string) ([]codegen.BootstrapPackageData, error) {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil, nil
	}

	cfgPath := filepath.Join(projectDir, defaultProjectConfigFile)
	store, _ := loadProjectStoreFrom(cfgPath) // best-effort; nil store means no excludes

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
		if store != nil && store.Contracts().IsExcluded(rel) {
			return filepath.SkipDir
		}
		contractPath := filepath.Join(path, "contract.go")
		if _, statErr := os.Stat(contractPath); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return statErr
		}
		// Per-package opt-out: `//forge:exclude-contract` in this package's
		// source is the local-header equivalent of listing it in forge.yaml
		// contracts.exclude — same effect (the package is NOT a Build
		// component, so the injector emits no New(Deps) node for it).
		// Union with the central list above: either source excludes. This
		// MUST match generate_middleware.go's contract walk so the mock /
		// middleware walk and the bootstrap/injector walk agree on the
		// excluded set — otherwise a header-only exclude would drop the
		// mock yet still feed a non-Service-shaped package into the
		// type-topological injector (which would emit an uncompilable
		// pkg.New(pkg.Deps{}) node). Do NOT SkipDir: descendants may still
		// be Build components and carry their own directive; only THIS
		// package opts out.
		if codegen.HasExcludeContractDirective(path) {
			return nil
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
	store, err := loadProjectStoreFrom(cfgPath)
	if err != nil || store == nil {
		return nil, nil
	}

	var specs []codegen.WorkerSpec
	for _, comp := range store.Components() {
		// Both long-running workers and scheduled crons register a
		// Worker bootstrap row (cron-ness lives in the scaffolded
		// worker.go body, not the bootstrap wiring).
		if comp.IsWorker() || comp.IsCron() {
			specs = append(specs, codegen.WorkerSpec{Name: comp.Name, Path: comp.Path})
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
	store, err := loadProjectStoreFrom(cfgPath)
	if err != nil || store == nil {
		return nil
	}
	cfg := store.Config()
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
	for _, comp := range cfg.Components {
		if len(comp.Webhooks) == 0 {
			continue
		}
		if isConnectServiceConfig(comp) && !reg.registered(comp.Name) {
			continue
		}
		out[naming.ServicePackage(comp.Name)] = true
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
	store, err := loadProjectStoreFrom(cfgPath)
	if err != nil || store == nil {
		return nil, nil
	}

	var specs []codegen.OperatorSpec
	for _, comp := range store.Components() {
		if comp.IsOperator() {
			specs = append(specs, codegen.OperatorSpec{Name: comp.Name, Path: comp.Path})
		}
	}
	return codegen.OperatorDataFromSpecs(specs, projectDir)
}
