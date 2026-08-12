package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateAuthSetup scaffolds the OWNED internal/app/auth.go (SetupAuth)
// ONCE. Authentication is code now, not forge.yaml config: SetupAuth picks
// the validator (default JWT from env) and the generated cmd serve wiring
// calls it. Write-if-absent — the user owns the file after first emit.
func generateAuthSetup(modulePath string, projectDir string) error {
	fmt.Println("🔧 Scaffolding auth setup (internal/app/auth.go)...")

	modPath := modulePath
	if modPath == "" {
		var err error
		modPath, err = codegen.GetModulePath(projectDir)
		if err != nil {
			return fmt.Errorf("failed to read module path: %w", err)
		}
	}

	if err := codegen.GenerateAuthSetup(modPath, projectDir); err != nil {
		return err
	}

	fmt.Println("  ✅ Scaffolded internal/app/auth.go (SetupAuth — yours to edit)")
	return nil
}

// generateWebhookRoutes generates webhook_routes_gen.go for each service that has webhooks.
//
// cs is the project's checksum tracker — passing it ensures the rendered
// webhook_routes_gen.go is recorded so it doesn't show up as an orphan
// in `forge project audit`. A nil cs is tolerated.
//
// reg is the parsed pkg/app/services.go registration view: webhook
// routes mount on the serving binary's mux, so declaring webhooks on a
// service this binary does not register is a hard generate-time error
// naming the registration file — the declaration could never take
// effect, and skipping it silently would hide a real misconfiguration.
func generateWebhookRoutes(reg *serviceRegistry, projectDir string, cs *generator.FileChecksums) error {
	// Webhooks are discovered from the real source — the webhook_<name>.go
	// files under each internal/handlers/<svc>/ dir — not a declared config
	// list. Every dir under internal/handlers/ is a Connect service handler
	// (workers/operators live elsewhere), so scanning it directly needs no
	// components manifest.
	handlersRoot := filepath.Join(projectDir, "internal", "handlers")
	dirs, err := os.ReadDir(handlersRoot)
	if err != nil {
		return nil // no handlers dir → no services → no webhooks
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		svcDirLeaf := d.Name()
		handlerDir := filepath.Join(handlersRoot, svcDirLeaf)
		webhookNames := codegen.WebhookNamesForService(handlerDir)
		if len(webhookNames) == 0 {
			continue
		}
		// Webhook routes mount on the serving binary's mux, so a service with
		// webhook handlers that this binary does not register is a hard
		// generate-time error naming the registration file — the routes could
		// never mount, and skipping silently would hide a misconfiguration.
		if !reg.registered(svcDirLeaf) {
			return fmt.Errorf("service %q has webhooks (%s) but is not registered in %s — webhooks require a serving binary; add `%s(app, cfg, logger, opts...),` to RegisteredServices there, or move the webhooks to the binary that serves the service",
				svcDirLeaf, strings.Join(webhookNames, ", "), serviceRegistryRelPath, codegen.ServiceRowFuncName(svcDirLeaf))
		}

		// Resolve the dir's real Go package clause (disk-first; the spelling
		// can legally differ from naming.ServicePackage synthesis).
		svcPkg := svcDirLeaf
		if res, resErr := codegen.ResolveServiceComponent(projectDir, svcDirLeaf); resErr == nil && res.FromDisk {
			svcPkg = res.PackageName
		}

		var entries []templates.WebhookRouteEntryData
		for _, name := range webhookNames {
			entries = append(entries, templates.WebhookRouteEntryData{
				Name:       strings.ReplaceAll(name, "_", "-"),
				PascalName: naming.ToPascalCase(name),
			})
		}

		data := templates.WebhookRoutesTemplateData{
			Package:  svcPkg,
			Webhooks: entries,
		}

		content, err := templates.WebhookTemplates().Render("webhook_routes_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render webhook routes for %s: %w", svcDirLeaf, err)
		}

		relPath := filepath.Join("internal", "handlers", svcDirLeaf, "webhook_routes_gen.go")
		if _, err := generator.WriteGeneratedFile(projectDir, relPath, content, cs, true); err != nil {
			return fmt.Errorf("write webhook routes for %s: %w", svcDirLeaf, err)
		}
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
//
// cs is the project's checksum tracker. Passing it threads every emitted
// mock_gen.go through the WriteGeneratedFile chokepoint so the path lands
// in checksums.WrittenThisRun — without it, the stale-artifact sweep
// flagged every manifest-tracked mock_gen.go as a deletion candidate on
// every run (kalshi FORGE_BACKLOG #15). A nil cs is tolerated (the file
// is still written; no checksum is recorded — and with no manifest there
// is correspondingly no sweep that could flag it).
func generateInternalPackageContracts(projectDir string, cfg *config.ProjectConfig, cs *generator.FileChecksums) error {
	internalDir := filepath.Join(projectDir, "internal")
	if !dirExists(internalDir) {
		return nil
	}

	// Lift the project's extra interface-type allow-list from forge.yaml
	// into the shape the contract generator wants. A type listed here
	// joins the built-in cross-package allow-list when the mock template
	// decides whether to emit "nil" or "T{}" for a return type. nil/empty
	// is the no-op default.
	var extraIfaceTypes map[string]bool
	if cfg != nil && len(cfg.Contracts.InterfaceTypes) > 0 {
		extraIfaceTypes = make(map[string]bool, len(cfg.Contracts.InterfaceTypes))
		for _, t := range cfg.Contracts.InterfaceTypes {
			extraIfaceTypes[t] = true
		}
	}
	contractOpts := contract.Options{
		ExtraInterfaceTypes: extraIfaceTypes,
		// Route the mock_gen.go write through the manifest chokepoint —
		// see the cs param doc above for the stale-sweep rationale.
		ProjectRoot: projectDir,
		Checksums:   cs,
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
		//
		// Skipping is only half the job: retire anything forge emitted here
		// before the exclude landed, so the exclude is an ownership exit
		// rather than an abandonment (see contract/retire.go). The whole
		// subtree is swept because SkipDir means the walk will not visit
		// the descendants that the exclude also covers.
		if cfg != nil && cfg.Contracts.IsExcluded(rel) {
			if retErr := retireExcludedSubtree(path, contractOpts); retErr != nil {
				return retErr
			}
			return filepath.SkipDir
		}

		contractPath := filepath.Join(path, "contract.go")
		if _, statErr := os.Stat(contractPath); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return statErr
		}

		// Per-package opt-out: `//forge:exclude-contract` in this package's
		// source is an alternative to listing it in forge.yaml
		// contracts.exclude — same effect (no mock/observability scaffold),
		// expressed locally next to the code. Union with the central list:
		// either source excludes. The directive lives ON a contract-shaped
		// package, so it is only consulted once a contract.go is present (a
		// package with no contract.go is skipped above regardless). We do NOT
		// SkipDir here — descendants may still want codegen and carry their
		// own directive; only THIS package opts out.
		if codegen.HasExcludeContractDirective(path) {
			fmt.Printf("  ⏭️  Skipped contract codegen for %s/ (//forge:exclude-contract)\n", rel)
			// Retire this package only — the directive opts out exactly the
			// package that carries it, matching the no-SkipDir note above.
			return reportRetirement(contract.RetireExcludedArtifacts(path, contractOpts))
		}

		if genErr := contract.GenerateWithOptions(contractPath, contractOpts); genErr != nil {
			return fmt.Errorf("generate contract for %s: %w", rel, genErr)
		}
		fmt.Printf("  ✅ Generated mock + middleware for %s/\n", rel)

		// Observability decorator. A package opts in via the `// forge:constructor`
		// marker on its constructor (the Phase-2 signal) OR the legacy owned
		// observe_chain.go seam — and NOT via a package-level `// forge:no-observe`
		// opt-out; paired with a New that returns the Service-contract interface,
		// forge (re)generates the slim middleware_gen.go wrapper from that
		// interface — the same ParseContract that drove the mock, so the two
		// never drift. Otherwise remove any stale decorator, so opting out (the
		// no-observe marker, or deleting both marker and seam) leaves nothing
		// behind. Handler packages return a concrete *Service and are excluded by
		// the ctorType==ifaceName gate inside ShouldInstrumentComponent
		// (otelconnect owns the edge). Method-level `// forge:no-observe` still
		// generates the decorator; those methods delegate directly (SkipObserve).
		obsCF, obsErr := contract.ParseContract(contractPath)
		if obsErr != nil {
			return fmt.Errorf("parse contract for observability decorator %s: %w", rel, obsErr)
		}
		ifaceName := codegen.DetectServiceInterfaceName(path)
		if ifaceName == "" {
			ifaceName = "Service"
		}
		ctorType, _ := codegen.DetectConstructorType(path)
		if codegen.ShouldInstrumentComponent(path, ctorType, ifaceName) {
			// Resolve the wrapper's names off the constructor's concrete return
			// type (the SAME resolver the compose assembler uses), so the
			// generated constructor matches the emitted call exactly. opNamespace
			// stays "<pkg>" for the single-impl package; a multi-wrapped-ctor
			// package would carry the concrete-type segment.
			mw := codegen.ResolveMiddlewareWrapper(path, ifaceName)
			opNamespace := obsCF.Package
			if mw.OpSegment != "" {
				opNamespace += "." + mw.OpSegment
			}
			if _, decErr := contract.WriteObservedDecorator(obsCF, path, ifaceName, mw.Constructor, mw.Struct, opNamespace, contractOpts); decErr != nil {
				return fmt.Errorf("generate observability decorator for %s: %w", rel, decErr)
			}
		} else if remErr := contract.RemoveObservedDecorator(path); remErr != nil {
			return fmt.Errorf("remove stale observability decorator for %s: %w", rel, remErr)
		}

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
		// Scaffold-once, and that includes the user's right to delete it: a
		// recorded birth with no file on disk is a removal to respect, not
		// an absence to fill. An existing file is adopted so its LATER
		// deletion is recognizable as one.
		testRoot, testRel, testLedger := checksums.SplitScaffoldPath(testPath)
		if _, statErr := os.Stat(testPath); statErr == nil {
			if testLedger {
				checksums.RecordScaffold(testRoot, testRel)
			}
		} else if testLedger && checksums.ScaffoldRecorded(testRoot, testRel) {
			// Deliberately deleted — leave it deleted.
		} else if os.IsNotExist(statErr) {
			if err := birthContractTest(contractTestBirth{
				ProjectDir:   projectDir,
				PkgDir:       path,
				Rel:          rel,
				ContractPath: contractPath,
				TestPath:     testPath,
				TestRoot:     testRoot,
				TestRel:      testRel,
				TestLedger:   testLedger,
			}); err != nil {
				return err
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

// contractTestBirth is everything the one-time contract_test.go scaffold
// needs to decide whether to write the file, and where.
type contractTestBirth struct {
	ProjectDir   string
	PkgDir       string
	Rel          string
	ContractPath string
	TestPath     string
	TestRoot     string
	TestRel      string
	TestLedger   bool
}

// birthContractTest scaffolds contract_test.go for a package that has none.
//
// Three package shapes are declined rather than scaffolded, because
// contract_test.go.tmpl emits exactly one call — `_, err := pkg.New(pkg.Deps{})`
// — and that call only compiles for the canonical single-`Service`,
// two-result-`New` package. Writing it anyway would hand back a package that
// does not build, so each declined shape prints why and what to change to opt
// back in.
func birthContractTest(b contractTestBirth) error {
	cf, parseErr := contract.ParseContract(b.ContractPath)
	if parseErr != nil {
		return fmt.Errorf("parse contract for %s: %w", b.Rel, parseErr)
	}

	switch {
	case len(cf.Interfaces) > 1:
		fmt.Printf("  ℹ️  Skipped contract_test.go scaffold for %s/ (multi-interface package; write tests manually)\n", b.Rel)
		return nil
	case len(cf.Interfaces) == 1 && cf.Interfaces[0].Name != "Service":
		fmt.Printf("  ℹ️  Skipped contract_test.go scaffold for %s/ (interface %q is not the canonical Service shape; write tests manually)\n", b.Rel, cf.Interfaces[0].Name)
		return nil
	case !packageHasTwoResultNew(b.PkgDir):
		fmt.Printf("  ℹ️  Skipped contract_test.go scaffold for %s/ (New is single-result; polish to `func New(Deps) (Service, error)` to enable auto-scaffold)\n", b.Rel)
		return nil
	}

	// importPath is "mcp/database" for internal/mcp/database, just
	// "database" for internal/database. Strip the leading "internal/"
	// segment so the template can emit `{{.Module}}/internal/{{.ImportPath}}`.
	importPath := strings.TrimPrefix(b.Rel, "internal/")
	modPath, modErr := codegen.GetModulePath(b.ProjectDir)
	if modErr != nil {
		return fmt.Errorf("read module path for contract_test scaffold: %w", modErr)
	}
	data := struct {
		Name       string
		ImportPath string
		Module     string
	}{Name: filepath.Base(b.PkgDir), ImportPath: importPath, Module: modPath}
	content, renderErr := templates.InternalPkgTemplates().Render("contract_test.go.tmpl", data)
	if renderErr != nil {
		return fmt.Errorf("render contract_test.go for %s: %w", b.Rel, renderErr)
	}
	if writeErr := os.WriteFile(b.TestPath, content, 0644); writeErr != nil {
		return fmt.Errorf("write contract_test.go for %s: %w", b.Rel, writeErr)
	}
	if b.TestLedger {
		checksums.RecordScaffold(b.TestRoot, b.TestRel)
	}
	fmt.Printf("  ✅ Scaffolded contract_test.go for %s/\n", b.Rel)
	return nil
}

// retireExcludedSubtree retires the contract-codegen artifacts under an
// excluded directory AND every package beneath it. A central
// `contracts.exclude` entry covers the whole subtree, and the caller
// SkipDirs immediately afterwards, so this is the only pass that will
// ever see those descendants.
func retireExcludedSubtree(dir string, opts contract.Options) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir // fixture packages, not real output
		}
		return reportRetirement(contract.RetireExcludedArtifacts(path, opts))
	})
}

// reportRetirement prints one line per artifact the retirement sweep
// acted on, and returns its error unchanged so call sites stay a single
// expression.
//
// The kept files get the louder treatment on purpose: forge no longer
// emits them, so `forge generate` will never resolve them and the drift
// gate would otherwise name them on every run with no remedy attached.
func reportRetirement(r contract.Retirement, err error) error {
	if err != nil {
		return err
	}
	for _, p := range r.Retired {
		fmt.Printf("  🧹 Retired %s (package opted out of contract codegen)\n", p)
	}
	for _, p := range r.Kept {
		fmt.Fprintf(os.Stderr, "  ⚠️  %s is left over from contract codegen the package has since opted out of, but its bytes differ from forge's render — forge won't delete your edits. Remove it by hand, or `%s project disown %s --reason \"<why>\"` to keep it deliberately.\n", p, Name(), p)
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

	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto", "config"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse config protos: %w", err)
	}

	if len(messages) == 0 {
		fmt.Println("  ℹ️  No config fields with config_field annotations found; using default scaffold config fields")
		messages = codegen.DefaultConfigMessages()
	}

	// A `sensitive` field bound to a FRONTEND is refused before anything is
	// written. This runs at the head of config generation on purpose: a
	// secret that reaches a bundle, a config.js, or even a rendered KCL
	// plan has already been published, so the only useful moment to refuse
	// is before the first artifact exists.
	if err := codegen.ValidateFrontendConfigs(messages); err != nil {
		return nil, err
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

	// Re-render the primary binary's cmd/<bin>/cmd/serve.go so it stays in
	// sync with the config fields.
	if err := codegen.GenerateCmdServerWithFields(configFields, projectDir, cs); err != nil {
		return nil, fmt.Errorf("failed to regenerate cmd/<bin>/cmd/serve.go: %w", err)
	}

	fmt.Println("  ✅ Regenerated cmd/<bin>/cmd/serve.go")
	return configFields, nil
}

// generatePerEnvDeployConfig emits the KCL config files. It writes the ONE
// project-level, forge-owned file — config_gen.k, carrying both the config
// TYPE and the projection BEHAVIOR (appConfigEnvMap) — from the proto config
// annotations, then scaffolds a user-owned per-env config.k
// (write-if-absent) from the proto's own defaults for every environment
// declared on the filesystem (deploy/kcl/<env>/main.k).
//
// The env's main.k imports config_gen + its own config.k and projects
// the typed AppConfig into every workload's env via appConfigEnvMap — the one
// source both host-run and cluster deploy read.
// configFieldsExcludingBinaries flattens the config fields of every message
// NOT bound to a binary — the project-global AppConfig, when one still
// exists alongside per-binary configs.
//
// Shared blocks (BaseConfig) are excluded too: their leaves already reach
// the projection through each binary that composes them, and emitting them
// again at the top level would put a binary's fields back into a shared
// surface, which is what per-binary config exists to prevent.
func configFieldsExcludingBinaries(messages []codegen.ConfigMessage) []codegen.ConfigField {
	composed := map[string]bool{}
	for _, m := range messages {
		for _, f := range m.Fields {
			if f.MessageType != "" {
				composed[f.MessageType] = true
			}
		}
	}
	var out []codegen.ConfigField
	for _, m := range messages {
		if m.Binary != "" || composed[m.Name] {
			continue
		}
		for _, f := range m.Fields {
			if f.MessageType != "" {
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

func generatePerEnvDeployConfig(projectDir string, cfg *config.ProjectConfig, cs *generator.FileChecksums) error {
	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto", "config"))
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

	// Project-level, forge-owned file: the config TYPE + the projection
	// BEHAVIOR (appConfigEnvMap) in ONE module, config_gen.k. Regenerated
	// from proto on every run.
	// Per-binary configs, when the project declares any: each annotated
	// message projects its OWN schema + env-projection lambda, so a
	// workload's env carries only that binary's fields. A project that
	// annotates nothing takes the single-AppConfig path, unchanged.
	perBinary := codegen.BinaryConfigFieldsFrom(messages)
	var rootFields []codegen.ConfigField
	if len(perBinary) > 0 {
		rootFields = configFieldsExcludingBinaries(messages)
	}

	if len(perBinary) > 0 {
		if err := codegen.GenerateConfigNativeSharedPerBinary(rootFields, perBinary, cfg.Name, projectDir, kclDirAbs, cs); err != nil {
			return fmt.Errorf("emit per-binary KCL config module: %w", err)
		}
	} else if err := codegen.GenerateConfigNativeShared(fields, cfg.Name, projectDir, kclDirAbs, cs); err != nil {
		return fmt.Errorf("emit KCL config module: %w", err)
	}

	// Frontend configs are a SEPARATE module (frontend_config_gen.k)
	// because they are public by construction: nothing in that file may
	// carry a secret reference, and keeping the two apart makes that
	// boundary visible in the file tree rather than a rule to remember.
	frontendConfigs := codegen.FrontendConfigsFromMessages(messages)
	if err := codegen.GenerateFrontendConfigNative(frontendConfigs, cfg.Name, projectDir, kclDirAbs, cs); err != nil {
		return fmt.Errorf("emit KCL frontend config module: %w", err)
	}

	// Transition for projects generated before the merge: their user-owned
	// config.k / main.k still import the two modules that just went away, so
	// point them at config_gen before anything tries to render. Runs before
	// the per-env scaffold below so a freshly reconciled env is consistent
	// in the same pass.
	if rewrote, rerr := codegen.ReconcileConfigModuleImports(kclDirAbs); rerr != nil {
		return fmt.Errorf("reconcile KCL config imports: %w", rerr)
	} else if len(rewrote) > 0 {
		fmt.Printf("  ♻️  Repointed %d env file(s) at deploy/kcl/%s.k (config_schema + config_projection merged): %s\n",
			len(rewrote), codegen.ConfigSchemaModule, strings.Join(rewrote, ", "))
	}

	envs, lerr := ListEnvs(projectDir)
	if lerr != nil {
		return fmt.Errorf("list envs: %w", lerr)
	}
	scaffolded := 0
	for _, envName := range envs {
		// Per-env user-owned config.k (write-if-absent) — the typed AppConfig
		// VALUES instance, scaffolded from proto defaults. Never clobbers an
		// existing (user-edited) file.
		// With per-binary configs it carries one typed instance per binary
		// instead of a single app_config.
		var (
			wrote bool
			cerr  error
		)
		if len(perBinary) > 0 {
			wrote, cerr = codegen.GenerateConfigKScaffoldPerBinary(rootFields, perBinary, cfg.Name, kclDirAbs, envName)
		} else {
			wrote, cerr = codegen.GenerateConfigKScaffold(fields, cfg.Name, kclDirAbs, envName)
		}
		if cerr != nil {
			return fmt.Errorf("scaffold %s config.k: %w", envName, cerr)
		}
		if wrote {
			scaffolded++
		}
		// The frontend half of the same file. Appended separately (rather
		// than folded into the write-if-absent body above) because a
		// frontend config is usually annotated LONG AFTER the env's
		// config.k was first written, and a file-granularity
		// write-if-absent would never revisit it — leaving the runtime
		// projection with no instance to project and the environment
		// silently on proto defaults.
		// The dev identity block rides along for the DEV env of a project
		// that declares an IdP. Both halves of the gate matter: a deployed
		// environment's issuer is a real one whose applications are
		// registered out of band, and a project scaffolded without a
		// frontend declares no IdP-convergence workload to seed a block for.
		devIdentity := envName == "dev" && projectDeclaresDevIDP(projectDir)
		if devIdentity {
			// The stub this config.k's import unconditionally reads from —
			// see EnsureIDPIdentityStub. Seeded BEFORE the instance below,
			// so the very first render after the identity fields are added
			// has something to import.
			if serr := codegen.EnsureIDPIdentityStub(kclDirAbs, envName); serr != nil {
				return fmt.Errorf("seed %s idp identity stub: %w", envName, serr)
			}
		}
		if added, ferr := codegen.EnsureFrontendConfigInstances(
			frontendConfigs, kclDirAbs, envName, cfg.Name, devIdentity, fields); ferr != nil {
			return fmt.Errorf("scaffold %s frontend config instances: %w", envName, ferr)
		} else if len(added) > 0 {
			fmt.Printf("  ✅ deploy/kcl/%s/config.k: added runtime config instance(s) for frontend(s) %s\n",
				envName, strings.Join(added, ", "))
		}
		// Per-env, gitignored `.env.<env>` — the VALUES half of every
		// `sensitive` config field, which config.k deliberately does not
		// carry. Local envs only (a cloud env's provider is external);
		// write-if-absent, so a developer's real values are never clobbered.
		if _, serr := codegen.GenerateEnvSecretsScaffold(fields, cfg.Name, projectDir, envName); serr != nil {
			return fmt.Errorf("scaffold %s secrets dotenv: %w", envName, serr)
		}
	}
	fmt.Printf("  ✅ Generated deploy/kcl/%s.k (scaffolded %d new config.k)\n", codegen.ConfigSchemaModule, scaffolded)
	return nil
}
