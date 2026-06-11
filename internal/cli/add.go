package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/naming"
)

// goKeywords is the set of Go reserved keywords.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// goPredeclaredIdentifiers is the set of Go predeclared types, constants, zero value, and builtin functions.
var goPredeclaredIdentifiers = map[string]bool{
	// Types
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"any": true, "comparable": true,
	// Constants
	"true": true, "false": true, "iota": true,
	// Zero value
	"nil": true,
	// Builtin functions
	"append": true, "cap": true, "close": true, "complex": true, "copy": true,
	"delete": true, "imag": true, "len": true, "make": true, "new": true,
	"panic": true, "print": true, "println": true, "real": true, "recover": true,
	"min": true, "max": true, "clear": true,
}

// reservedServiceNames are names that conflict with forge's worker/scheduler
// subsystems. Using them as HTTP Connect service names causes confusion.
var reservedServiceNames = map[string]bool{
	"worker": true, "scheduler": true, "cron": true, "job": true,
}

// validateServiceName checks that a name is valid for a service and not a
// reserved service name. For background workers use 'forge add worker <name>'.
func validateServiceName(name string) error {
	if err := validateIdentifier(name); err != nil {
		return err
	}
	if reservedServiceNames[strings.ToLower(name)] {
		return fmt.Errorf("%q is reserved; for background workers use 'forge add worker <name>'", name)
	}
	return nil
}

// validateIdentifier checks that a name is valid for use as a service, worker,
// or operator name. Hyphens and underscores are allowed in the display name;
// templates use snakeCase/pascalCase helpers to convert when a Go identifier
// is needed (e.g. "admin-server" / "admin_server" -> package "admin_server"
// and field "AdminServer" — snake_case is the canonical on-disk form post-
// 2026-06-08). The leading-character and reserved-word rules match
// validateProjectName so all top-level scaffold names share one shape.
func validateIdentifier(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	first, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsLetter(first) {
		return fmt.Errorf("name must start with a letter")
	}
	last, _ := utf8.DecodeLastRuneInString(name)
	if last == '-' {
		return fmt.Errorf("name cannot end with a hyphen")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return fmt.Errorf("name contains invalid character: %c", r)
		}
	}
	if goKeywords[name] {
		return fmt.Errorf("%q is a Go keyword", name)
	}
	if goPredeclaredIdentifiers[name] {
		return fmt.Errorf("%q is a Go predeclared identifier", name)
	}
	return nil
}

// validateProjectName checks that a project name is valid for use as a directory
// name and in Go module paths. Hyphens are allowed since they are valid in
// module paths and directory names; templates use snakeCase/pascalCase helpers
// to convert when a Go identifier is needed.
func validateProjectName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	first, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsLetter(first) {
		return fmt.Errorf("name must start with a letter")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return fmt.Errorf("name contains invalid character: %c", r)
		}
	}
	if goKeywords[name] {
		return fmt.Errorf("%q is a Go keyword", name)
	}
	if goPredeclaredIdentifiers[name] {
		return fmt.Errorf("%q is a Go predeclared identifier", name)
	}
	return nil
}

func newAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a service, worker, operator, frontend, or binary to an existing project",
		Long: `Add a new service, worker, operator, frontend, or binary to an existing Forge project.

Subcommands:
  forge add service <name>                        Add a new Go service
  forge add worker <name>                         Add a new background worker
  forge add operator <name> [--group G] [--version V]  Add a Kubernetes operator
  forge add binary <name>                         Add a non-server long-running binary
  forge add frontend <name>                       Add a new Next.js frontend
  forge add scenario <name>                       Scaffold a frontend mock scenario
  forge add webhook <name> --service S            Add a webhook endpoint to a service
  forge add package <name>                        Add a new internal package (alias for package new)
  forge add adapter <name>                        Add an outbound adapter (HTTP/queue/storage gateway)
  forge add library <name>                        Scaffold a library-shaped package (no contract.go; pre-excluded)
  forge add handler-file <svc> <name>             Scaffold an additional RPC-group file in handlers/<svc>/
  forge add rpc <svc> <Name> [--stream M]         Scaffold a single hand-written RPC stub + proto snippet`,
	}

	cmd.AddCommand(newAddServiceCmd())
	cmd.AddCommand(newAddWorkerCmd())
	cmd.AddCommand(newAddOperatorCmd())
	cmd.AddCommand(newAddCRDCmd())
	cmd.AddCommand(newAddFrontendCmd())
	cmd.AddCommand(newAddScenarioCmd())
	cmd.AddCommand(newAddWebhookCmd())
	cmd.AddCommand(newAddPackageCmd())
	cmd.AddCommand(newAddAdapterCmd())
	cmd.AddCommand(newAddBinaryCmd())
	cmd.AddCommand(newAddLibraryCmd())
	cmd.AddCommand(newAddHandlerFileCmd())
	cmd.AddCommand(newAddRPCCmd())

	return cmd
}

// projectRoot finds the project root by looking for forge.yaml.
func projectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	configPath := filepath.Join(cwd, "forge.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return "", cliutil.UserErr("forge",
			"forge.yaml not found in current directory",
			"",
			"cd into your project root, or run 'forge new <name>' to scaffold a new project")
	}
	return cwd, nil
}

// requireServiceKind reads forge.yaml at root and returns an error if the
// project's kind is not "service". `forge add service/operator/worker/webhook`
// only makes sense for server-shaped projects — CLI and library kinds have
// no Connect-RPC server to attach handlers to.
func requireServiceKind(root, action string) error {
	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return cliutil.WrapUserErr(fmt.Sprintf("forge add %s", action),
			"read project config", configPath,
			"verify forge.yaml is valid YAML", err)
	}
	if !cfg.IsServiceKind() {
		return cliutil.UserErr(fmt.Sprintf("forge add %s", action),
			fmt.Sprintf("'forge add %s' is only available for service projects (this project's kind: %s)",
				action, cfg.EffectiveKind()),
			"",
			"re-run 'forge new' with --kind service to scaffold a server, or use 'forge add package' for internal Go packages")
	}
	return nil
}

// --- add service ---

func newAddServiceCmd() *cobra.Command {
	var (
		port   int
		resume bool
		force  bool
	)

	cmd := &cobra.Command{
		Use:   "service <name>",
		Short: "Add a new Go service to the project",
		Long: `Add a new Go service to an existing Forge project.

This creates the service directory structure, proto file, Dockerfile,
hot-reload config, and updates the project configuration.

Flags:
  --resume   Re-run a partial scaffold. Skips every output file that
             already exists on disk. Safe to invoke repeatedly.
  --force    Re-stamp the scaffold even when files exist. Overwrites
             service.go, authorizer.go, the test files, and the proto
             stub. Use after manually editing a scaffolded file and
             wanting to start over.

--resume and --force are mutually exclusive.

Example:
  forge add service users
  forge add service orders --port 8082
  forge add service users --resume   # recover from a partial failure
  forge add service users --force    # re-stamp every output file`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddService(args[0], port, resume, force)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Service port (default: auto-increment from 8080)")
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume a partial scaffold: skip files that already exist")
	cmd.Flags().BoolVar(&force, "force", false, "Force-overwrite every scaffold output file")

	return cmd
}

func runAddService(name string, port int, resume, force bool) error {
	ctxLabel := fmt.Sprintf("forge add service %s", name)

	// --resume and --force are mutually exclusive: one is "preserve user
	// edits", the other is "discard them". Combining them has no coherent
	// meaning, so reject early before we touch any files.
	if resume && force {
		return cliutil.UserErr(ctxLabel,
			"--resume and --force are mutually exclusive",
			"",
			"use --resume to recover from a partial failure (skips existing files), or --force to re-stamp every output file")
	}

	if err := validateServiceName(name); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid service name", "",
			"use a name starting with a letter, containing letters/digits/_/-; not a Go keyword or reserved (worker/scheduler/cron/job)",
			err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "service"); err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel, "read project config", configPath,
			"verify forge.yaml is valid YAML", err)
	}

	// Check for name conflict in the existing config. Under --resume or
	// --force we treat a matching name as "this is the partial scaffold I
	// am recovering / re-stamping", not as a hard error. We still skip
	// the forge.yaml append step in that case so we don't duplicate the
	// services: entry.
	existingIdx := -1
	for i, svc := range cfg.Services {
		if svc.Name == name {
			existingIdx = i
			break
		}
	}
	if existingIdx >= 0 && !resume && !force {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("service %q already exists in the project", name),
			"",
			"pass --resume to skip files that already exist, --force to overwrite them, or pick a different name")
	}

	// Port selection. If the service already exists in forge.yaml (resume
	// or force path) and the user did not pass --port, reuse the existing
	// port so the regenerated scaffold matches the recorded config.
	if port == 0 {
		if existingIdx >= 0 {
			port = cfg.Services[existingIdx].Port
		} else {
			port = 8080
			for _, svc := range cfg.Services {
				if svc.Port >= port {
					port = svc.Port + 1
				}
			}
		}
	}

	switch {
	case resume:
		fmt.Printf("Resuming service '%s' (port %d)...\n", name, port)
	case force:
		fmt.Printf("Force-stamping service '%s' (port %d)...\n", name, port)
	default:
		fmt.Printf("Adding service '%s' (port %d)...\n", name, port)
	}

	// Generate service files (service.go, handlers.go, proto). The mode
	// drives per-file overwrite/skip behavior; progress writes "✓ skipped"
	// and "⚠ overwriting" lines to stdout as it goes.
	mode := generator.ScaffoldFail
	switch {
	case resume:
		mode = generator.ScaffoldResume
	case force:
		mode = generator.ScaffoldForce
	}
	if err := generator.GenerateServiceFilesWithMode(root, cfg.ModulePath, name, cfg.Name, port, mode, os.Stdout); err != nil {
		return fmt.Errorf("generate service files: %w", err)
	}

	// Snapshot the existing project config so we can roll back to it if the
	// generation pipeline fails — otherwise the on-disk config would claim a
	// service that has no generated stubs, proto, or wiring.
	originalConfigBytes, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read project config for rollback snapshot: %w", err)
	}

	// Update forge.yaml (must happen before the generation pipeline
	// so the pipeline sees the new service in the config). The Path uses
	// the snake_case Go-package form so it matches the directory the
	// scaffolder actually creates ("admin-server" -> handlers/admin_server).
	//
	// Under --resume / --force the service entry may already exist in
	// forge.yaml; only append when this is a fresh add.
	if existingIdx < 0 {
		cfg.Services = append(cfg.Services, config.ServiceConfig{
			Name: name,
			Type: "go_service",
			Path: fmt.Sprintf("handlers/%s", naming.ServicePackage(name)),
			Port: port,
		})
		if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
			return fmt.Errorf("update project config: %w", err)
		}
	}

	// Run the full generation pipeline: buf generate, service stubs, mocks,
	// bootstrap.go, testing.go, go mod tidy, etc.
	fmt.Println("\n🔧 Running generation pipeline...")
	generateMu.Lock()
	err = runGeneratePipeline(root, false, false)
	generateMu.Unlock()
	if err != nil {
		// Only restore the config when we actually appended to it this
		// invocation; otherwise --resume would clobber a valid config
		// after a transient pipeline failure.
		if existingIdx < 0 {
			if restoreErr := os.WriteFile(configPath, originalConfigBytes, 0o644); restoreErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to restore original project config after pipeline failure: %v\n", restoreErr)
			}
			return fmt.Errorf("generation pipeline failed for service %q (project config restored): %w", name, err)
		}
		return fmt.Errorf("generation pipeline failed for service %q: %w", name, err)
	}

	// Generate E2E test harness. GenerateE2ETests already skips existing
	// files unconditionally, which gives the right behavior for both
	// --resume and default. --force is not threaded through here because
	// the E2E harness is the user's tests and clobbering them is rarely
	// what someone re-stamping a scaffold actually wants.
	fmt.Println("Generating E2E test harness...")
	e2eMethods := generator.MethodsFromProtoStub(name)
	if err := generator.GenerateE2ETests(root, name, cfg.ModulePath, cfg.Name, e2eMethods); err != nil {
		fmt.Printf("  warning: failed to generate E2E tests: %v\n", err)
		// Non-fatal: the service was created successfully
	}

	fmt.Printf("\n✅ Service '%s' added successfully!\n", name)

	// Registration: pkg/app/services.go is user-owned — forge never
	// edits it. When the file predates this service (the usual add-flow:
	// the registry was scaffolded with the project's earlier services),
	// the new row constructor is generated but unreferenced, so the
	// binary won't serve the service until the user adds the line. Print
	// the exact line; `forge audit` keeps the gap visible
	// (codegen.unregistered_services) until it's resolved. This is
	// deliberate: the registration line is the one decision the user (or
	// their agent) writes — forge generates the guardrails around it.
	if reg, regErr := loadServiceRegistry(root); regErr == nil && reg.Exists && !reg.registered(name) {
		fmt.Println()
		fmt.Printf("⚠️  %s is user-owned — forge does not edit it.\n", serviceRegistryRelPath)
		fmt.Printf("   To serve %q from this binary, add this row to RegisteredServices:\n\n", name)
		fmt.Printf("       %s(app, cfg, logger, devMode, opts...),\n\n", codegen.ServiceRowFuncName(name))
		fmt.Println("   Until then the service is generated but not served (forge audit: codegen.unregistered_services).")
	}

	return nil
}

// --- add package (alias for package new) ---

func newAddPackageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "package <name>",
		Short: "Add a new internal package (alias for 'forge package new')",
		Long: `Add a new internal package under internal/<name>/.

The --type flag picks the scaffold shape:

  service     (default) classic Service/Deps/New(Deps) Service. Wired into
              bootstrap; callable by handlers.
  adapter     Outbound boundary translator (HTTP client, queue producer,
              storage gateway). No business logic; thin translation to a
              third-party system. Marker: '// forge:adapter'.
              Skill: forge skill load adapter
  interactor  Use-case orchestrator composing >=2 adapters/services. Deps
              must be interfaces (lint-enforced). Marker:
              '// forge:interactor'.
              Skill: forge skill load interactor

Example:
  forge add package cache
  forge add package events --kind eventbus
  forge add package stripe-adapter --type adapter
  forge add package billing-flow --type interactor`,
		Args: cobra.ExactArgs(1),
		RunE: runPackageNew,
	}

	cmd.Flags().String("kind", "", "package kind template (e.g. eventbus, client)")
	cmd.Flags().String("type", "service", "package shape: service|adapter|interactor (see --help)")

	return cmd
}

// --- add worker ---

func newAddWorkerCmd() *cobra.Command {
	var kind string
	var schedule string
	var noGenerate bool

	cmd := &cobra.Command{
		Use:   "worker <name>",
		Short: "Add a new background worker to the project",
		Long: `Add a new background worker to an existing Forge project.

A worker is a long-running process that doesn't serve HTTP but participates
in the single-binary lifecycle. It has Start(ctx)/Stop(ctx) methods, health
reporting, and the same Deps injection as services.

The --no-generate flag suppresses the post-scaffold ` + "`forge generate`" + ` run.
The scaffold itself (workers/<name>/*.go + forge.yaml services append) is the
only step the verb promises; the pipeline run is a convenience that becomes
hostile under parallel-agent work (see kalshi-trader migration round friction
forge-add-worker-runs-full-pipeline). Pass --no-generate when staging
scaffold-only changes in a multi-lane round and follow up with an explicit
` + "`forge generate`" + ` at a coordination point.

Example:
  forge add worker email_sender
  forge add worker order_processor
  forge add worker cleanup --kind cron --schedule "*/5 * * * *"
  forge add worker engine_shadow --kind cron --schedule "0 3 * * *" --no-generate`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddWorker(args[0], kind, schedule, noGenerate)
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", "worker kind (use cron for scheduled workers)")
	cmd.Flags().StringVar(&schedule, "schedule", "", "cron schedule for --kind cron workers")
	cmd.Flags().BoolVar(&noGenerate, "no-generate", false, "skip the post-scaffold `forge generate` run (scaffold-only mode for parallel-agent rounds)")

	return cmd
}

func runAddWorker(name, kind, schedule string, noGenerate bool) error {
	ctxLabel := fmt.Sprintf("forge add worker %s", name)
	if err := validateIdentifier(name); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid worker name", "",
			"use a name starting with a letter, containing letters/digits/_/-", err)
	}

	kind = strings.ToLower(strings.TrimSpace(kind))
	schedule = strings.TrimSpace(schedule)
	switch kind {
	case "", "cron":
	default:
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid worker kind %q: valid kinds are cron", kind),
			"",
			"omit --kind for a long-running worker, or pass --kind=cron with --schedule for a scheduled worker")
	}
	if kind == "cron" {
		if schedule == "" {
			return cliutil.UserErr(ctxLabel, "--schedule is required when --kind cron", "",
				"pass --schedule with a 5-field cron expression (e.g. --schedule \"*/5 * * * *\")")
		}
		if _, err := cron.ParseStandard(schedule); err != nil {
			return cliutil.WrapUserErr(ctxLabel,
				fmt.Sprintf("invalid cron schedule %q", schedule), "",
				"use a 5-field cron expression (minute hour day-of-month month day-of-week), e.g. \"*/5 * * * *\"",
				err)
		}
	} else if schedule != "" {
		return cliutil.UserErr(ctxLabel, "--schedule requires --kind cron", "",
			"either drop --schedule (long-running worker) or add --kind cron")
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "worker"); err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Check for name conflict (workers are stored in Services with type "worker")
	for _, svc := range cfg.Services {
		if svc.Name == name {
			return fmt.Errorf("%q already exists in the project", name)
		}
	}

	if kind == "cron" {
		fmt.Printf("Adding cron worker '%s' (schedule %q)...\n", name, schedule)
	} else {
		fmt.Printf("Adding worker '%s'...\n", name)
	}

	// Generate worker files (worker.go, worker_test.go)
	if err := generator.GenerateWorkerFiles(root, cfg.ModulePath, name, kind, schedule); err != nil {
		return fmt.Errorf("generate worker files: %w", err)
	}

	// Update forge.yaml. Path uses the Go-package form so it matches the
	// directory the scaffolder creates ("email-sender" -> workers/email_sender).
	cfg.Services = append(cfg.Services, config.ServiceConfig{
		Name:     name,
		Type:     "worker",
		Kind:     kind,
		Path:     fmt.Sprintf("workers/%s", naming.ServicePackage(name)),
		Schedule: schedule,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	// --no-generate: scaffold-only mode. Skip the post-scaffold generate
	// pipeline so parallel-agent rounds can stage worker scaffolding
	// without triggering project-wide codegen churn (which races sibling
	// agents holding the api-service / mock_gen / wire_gen lanes). The
	// operator is responsible for running `forge generate` at a
	// coordination point. See friction
	// forge-add-worker-runs-full-pipeline (kalshi-trader migration
	// round, filed-not-fixed entry: "no --no-generate / --steps=worker
	// flag, so the worker-add path always runs the full codegen
	// pipeline").
	if noGenerate {
		fmt.Printf("\n⏩ --no-generate: skipping post-scaffold `forge generate` run.\n")
		fmt.Printf("    Worker '%s' scaffolded. Run `forge generate` at a coordination point\n", name)
		fmt.Printf("    to update pkg/app/{bootstrap,wire_gen,testing}.go for the new worker.\n")
		fmt.Printf("\n✅ Worker '%s' scaffold-only mode complete!\n", name)
		return nil
	}

	// Run the generation pipeline, narrowed to the bootstrap-only step
	// preset, so adding a worker regenerates
	// pkg/app/{bootstrap,testing,migrate}.go and nothing else. The full
	// pipeline would also rewrite every Tier-1 file in its catalog
	// (.github/workflows/ci.yml, cmd/server.go, frontend mocks,
	// pkg/config/config.go) — friction reported by the cp-forge
	// port-workers round where `forge add worker` × 7 rewrote 5
	// unrelated Tier-1 files per invocation. The step preset's allowed
	// step set lives in stepPresetAllowlist["bootstrap-only"]
	// (generate_pipeline.go).
	fmt.Println("\n🔧 Running generation pipeline (bootstrap-only step preset)...")
	generateMu.Lock()
	err = runGeneratePipelineFlags(root, pipelineFlags{Steps: "bootstrap-only"})
	generateMu.Unlock()
	if err != nil {
		// Non-fatal: the worker files were created successfully, but the
		// pipeline failure usually means the project doesn't compile (a
		// sibling-package issue or a stale generated file). Print a
		// distinct partial-success line so a user skimming the output
		// doesn't see the unconditional ✅ below and assume the build is
		// healthy. Friction reported by the kalshi-trader migration round:
		// the prior code printed the green check directly after the
		// "warning: generation pipeline failed" line, hiding the failure
		// in the visual noise.
		fmt.Fprintf(os.Stderr, "\nwarning: generation pipeline failed: %v\n", err)
		fmt.Printf("\n⚠️  Worker '%s' files written, but `forge generate` failed — fix the build before running it again.\n", name)
		fmt.Printf("    Tip: pass --no-generate to `forge add worker` to suppress the post-scaffold pipeline run.\n")
		return nil
	}

	fmt.Printf("\n✅ Worker '%s' added successfully!\n", name)

	return nil
}

// --- add operator ---

func newAddOperatorCmd() *cobra.Command {
	var (
		group              string
		version            string
		apiPackage         string
		crdType            string
		withPlaceholderCRD bool
	)

	cmd := &cobra.Command{
		Use:   "operator <name>",
		Short: "Add a new Kubernetes operator to the project",
		Long: `Add a new Kubernetes operator (manager binary) to an existing Forge project.

By default this scaffolds only the operator package + manager wiring; CRDs are
added with 'forge add crd <Name>' which produces a thin shim that delegates
to forge/pkg/controller.Reconciler[T].

Pass --with-placeholder-crd to keep the legacy combined types.go +
controller.go scaffold (kept for backward compatibility while users
migrate to the forge add crd workflow). When --with-placeholder-crd is
set, --api-package and --crd-type tune the legacy scaffold's CRD package
and type name.

Example:
  forge add operator manager
  forge add operator manager --group myapp.io --version v1alpha1
  forge add operator workspace --with-placeholder-crd
  forge add operator workspace-controller --with-placeholder-crd --api-package workspace --crd-type Workspace --group reliant.dev --version v1alpha1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddOperator(args[0], group, version, apiPackage, crdType, withPlaceholderCRD)
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "API group (default: <project-name>.io)")
	cmd.Flags().StringVar(&version, "version", "v1alpha1", "API version")
	cmd.Flags().StringVar(&apiPackage, "api-package", "", "(legacy) Go package name for the placeholder CRD types")
	cmd.Flags().StringVar(&crdType, "crd-type", "", "(legacy) Placeholder CRD Go type name")
	cmd.Flags().BoolVar(&withPlaceholderCRD, "with-placeholder-crd", false, "Emit legacy types.go + controller.go scaffold (use 'forge add crd' for new CRDs)")

	return cmd
}

func runAddOperator(name, group, version, apiPackage, crdType string, withPlaceholderCRD bool) error {
	if err := validateIdentifier(name); err != nil {
		return fmt.Errorf("invalid operator name: %w", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "operator"); err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}
	if !cfg.Features.OperatorsEnabled() {
		return config.DisabledFeatureError(config.FeatureOperators)
	}

	// Check for name conflict (operators are stored in Services with type "operator")
	for _, svc := range cfg.Services {
		if svc.Name == name {
			return fmt.Errorf("%q already exists in the project", name)
		}
	}

	// Default group from project name
	if group == "" {
		group = cfg.Name + ".io"
	}

	if !withPlaceholderCRD && (apiPackage != "" || crdType != "") {
		return fmt.Errorf("--api-package and --crd-type only apply with --with-placeholder-crd; for the new shape use 'forge add crd <Name>' after the operator is created")
	}

	fmt.Printf("Adding operator '%s' (group=%s, version=%s)...\n", name, group, version)

	if withPlaceholderCRD {
		// Legacy path: emit the combined scaffold.
		if err := generator.GenerateOperatorFilesWithAPI(root, cfg.ModulePath, name, group, version, apiPackage, crdType); err != nil {
			return fmt.Errorf("generate operator files: %w", err)
		}
	} else {
		// New default: only the operator package skeleton.
		if err := generator.GenerateOperatorBinaryOnly(root, cfg.ModulePath, name, group, version); err != nil {
			return fmt.Errorf("generate operator scaffold: %w", err)
		}
	}

	// Update forge.yaml. Path uses the Go-package form so it matches the
	// directory the scaffolder creates. Group/Version are persisted so
	// `forge add crd` can default from them.
	cfg.Services = append(cfg.Services, config.ServiceConfig{
		Name:    name,
		Type:    "operator",
		Path:    fmt.Sprintf("operators/%s", naming.ServicePackage(name)),
		Group:   group,
		Version: version,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	// Run the generation pipeline to update bootstrap.go and cmd-server.go
	fmt.Println("\n🔧 Running generation pipeline...")
	generateMu.Lock()
	err = runGeneratePipeline(root, false, false)
	generateMu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: generation pipeline failed: %v\n", err)
		// Non-fatal: the operator files were created successfully
	}

	fmt.Printf("\n✅ Operator '%s' added successfully!\n", name)
	if !withPlaceholderCRD {
		fmt.Printf("Next: 'forge add crd <Name> --operator %s' to scaffold a CRD.\n", name)
	}

	return nil
}

// --- add crd ---

func newAddCRDCmd() *cobra.Command {
	var (
		group    string
		version  string
		shape    string
		operator string
	)

	cmd := &cobra.Command{
		Use:   "crd <Name>",
		Short: "Add a Custom Resource Definition + reconciler to an operator",
		Long: `Add a Kubernetes Custom Resource Definition and its reconciler to an
existing operator.

Generates:
  - api/<version>/<name>_types.go              CRD spec + status types
  - operators/<operator>/<name>_controller.go  thin reconciler shim
  - operators/<operator>/<name>_controller_test.go fake-client unit test

The reconciler shim embeds forge/pkg/controller.Reconciler[T] which
provides fetch / NotFound / finalizer / dispatch lifecycle automatically.
You implement ReconcileSpec (and FinalizeSpec, when finalization needs
cleanup) for the domain logic.

Shapes:
  state-machine  Spec.State drives the loop through observable phases (default).
  config         Declarative-only — Spec describes a configuration to project.
  composite      Spec owns sub-resources whose lifetime is coupled to the parent.

Example:
  forge add crd Workspace
  forge add crd Database --shape config
  forge add crd Cluster --shape composite --operator manager`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddCRD(args[0], group, version, shape, operator)
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "API group (default: parent operator's group)")
	cmd.Flags().StringVar(&version, "version", "", "API version (default: parent operator's version)")
	cmd.Flags().StringVar(&shape, "shape", "state-machine", "Reconciler scaffold style: state-machine, config, composite")
	cmd.Flags().StringVar(&operator, "operator", "", "Target operator name (default: only operator in project)")

	return cmd
}

func runAddCRD(name, group, version, shape, operator string) error {
	if err := validateIdentifier(name); err != nil {
		return fmt.Errorf("invalid CRD name: %w", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "crd"); err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Resolve target operator: explicit flag > only-operator > error.
	operatorIdx := -1
	if operator != "" {
		for i, svc := range cfg.Services {
			if svc.Type == "operator" && svc.Name == operator {
				operatorIdx = i
				break
			}
		}
		if operatorIdx == -1 {
			return fmt.Errorf("operator %q not found in forge.yaml; run `forge add operator %s` first", operator, operator)
		}
	} else {
		operatorCount := 0
		for i, svc := range cfg.Services {
			if svc.Type == "operator" {
				operatorCount++
				operatorIdx = i
			}
		}
		switch operatorCount {
		case 0:
			return fmt.Errorf("no operators in this project; run `forge add operator <name>` first")
		case 1:
			operator = cfg.Services[operatorIdx].Name
		default:
			return fmt.Errorf("multiple operators in this project; pass --operator <name> to disambiguate")
		}
	}

	op := &cfg.Services[operatorIdx]
	if group == "" {
		group = op.Group
	}
	if group == "" {
		group = cfg.Name + ".io"
	}
	if version == "" {
		version = op.Version
	}
	if version == "" {
		version = "v1alpha1"
	}

	crdShape := generator.CRDShape(shape)
	if !crdShape.IsValid() {
		return fmt.Errorf("invalid --shape %q (valid: state-machine, config, composite)", shape)
	}

	for _, c := range op.CRDs {
		if c.Name == name {
			return fmt.Errorf("CRD %q already exists in operator %q", name, operator)
		}
	}

	fmt.Printf("Adding CRD '%s' to operator '%s' (group=%s, version=%s, shape=%s)...\n",
		name, operator, group, version, crdShape)

	if err := generator.GenerateCRDFiles(generator.CRDGenInput{
		Root:         root,
		ModulePath:   cfg.ModulePath,
		OperatorName: operator,
		TypeName:     name,
		Group:        group,
		Version:      version,
		Shape:        crdShape,
	}); err != nil {
		return fmt.Errorf("generate CRD files: %w", err)
	}

	op.CRDs = append(op.CRDs, config.CRDConfig{
		Name:    name,
		Group:   group,
		Version: version,
		Shape:   string(crdShape),
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ CRD '%s' added to operator '%s'!\n", name, operator)
	return nil
}

// --- add frontend ---

func newAddFrontendCmd() *cobra.Command {
	var port int
	var kind string
	var output string
	var basePath string

	cmd := &cobra.Command{
		Use:   "frontend <name>",
		Short: "Add a new frontend to the project",
		Long: `Add a new frontend to an existing forge project.

By default this creates a Next.js web frontend with Connect RPC client setup.
Use --kind mobile to scaffold a React Native app using Expo.
Use --kind vite-spa to scaffold a Vite + React + tanstack-router SPA.

For Next.js frontends (--kind web, the default), --output selects the
production build/runtime shape. "static" (the default) emits a static
export — the right shape when the frontend is a pure UI shell calling a
Go backend via Connect RPC. Opt into "standalone" when you need a Node
sidecar (server components, server actions, request-time redirect()).
Use "server" for full Next.js dev+prod (next start).

--base-path mounts the frontend under a URL prefix (e.g. /admin behind a
reverse proxy that blends several apps on one host). It is persisted as
frontends[].base_path in forge.yaml and rendered into next.config.ts
(basePath + assetPrefix) and the generated src/lib/basepath_gen.ts
helper. The single runtime override is NEXT_PUBLIC_BASE_PATH.

Example:
  forge add frontend web
  forge add frontend dashboard --port 3001
  forge add frontend mobile --kind mobile
  forge add frontend admin --kind vite-spa
  forge add frontend dashboard --output standalone
  forge add frontend admin --base-path /admin`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddFrontend(cmd.Context(), args[0], port, kind, output, basePath)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Frontend dev server port (default: auto-increment from 3000)")
	cmd.Flags().StringVar(&kind, "kind", "", "frontend kind (web, mobile, or vite-spa)")
	cmd.Flags().StringVar(&output, "output", "", "Next.js output shape: static (default), standalone, or server. Only applies to --kind web.")
	cmd.Flags().StringVar(&basePath, "base-path", "", `URL prefix the frontend is mounted under (e.g. "/admin"). Only applies to --kind web.`)

	return cmd
}

// validateFrontendName checks that a frontend name is filesystem-safe.
func validateFrontendName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return fmt.Errorf("name contains invalid filesystem characters")
	}
	if strings.Contains(name, " ") {
		return fmt.Errorf("name cannot contain spaces")
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("name cannot start with . or -")
	}
	return nil
}

func runAddFrontend(ctx context.Context, name string, port int, kind, output, basePath string) error {
	if err := validateFrontendName(name); err != nil {
		return fmt.Errorf("invalid frontend name: %w", err)
	}

	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "", "web", "mobile", "vite-spa":
	default:
		return fmt.Errorf("invalid frontend kind %q: valid kinds are web, mobile, vite-spa", kind)
	}

	// --output applies only to Next.js (kind=web / kind=""). Reject
	// up-front for mobile / vite-spa so the user gets a clear error
	// instead of silently-ignored flag.
	output = strings.ToLower(strings.TrimSpace(output))
	switch output {
	case "", "static", "standalone", "server":
	default:
		return fmt.Errorf("invalid --output %q: valid values are static (default), standalone, server", output)
	}
	if output != "" && kind != "" && kind != "web" {
		return fmt.Errorf("--output only applies to Next.js frontends (--kind web); got --kind %q", kind)
	}

	// --base-path follows the same Next.js-only rule, plus the strict
	// shape contract shared with forge.yaml validation (leading "/", no
	// trailing "/", [A-Za-z0-9._-] segments) — the value is spliced
	// verbatim into next.config.ts and generated TypeScript literals.
	basePath = strings.TrimSpace(basePath)
	if basePath != "" {
		if msg, ok := config.ValidateBasePath(basePath); !ok {
			return fmt.Errorf("invalid --base-path %q: %s", basePath, msg)
		}
		if kind != "" && kind != "web" {
			return fmt.Errorf("--base-path only applies to Next.js frontends (--kind web); got --kind %q", kind)
		}
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "frontend"); err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Check for name conflict
	for _, frontend := range cfg.Frontends {
		if frontend.Name == name {
			return fmt.Errorf("frontend %q already exists in the project", name)
		}
	}

	// Auto-assign port if not specified
	if port == 0 {
		port = 3000
		for _, frontend := range cfg.Frontends {
			if frontend.Port >= port {
				port = frontend.Port + 1
			}
		}
	}

	frontendType := "nextjs"
	frontendKind := kind
	frontendDescription := "frontend"
	switch kind {
	case "mobile":
		frontendType = "react-native"
		frontendDescription = "mobile frontend"
	case "vite-spa":
		frontendType = "vite-spa"
		frontendDescription = "Vite SPA frontend"
	case "":
		frontendKind = ""
	}

	fmt.Printf("Adding %s '%s' (port %d)...\n", frontendDescription, name, port)

	// Determine the API port from the first service
	apiPort := 8080
	if len(cfg.Services) > 0 {
		apiPort = cfg.Services[0].Port
	}

	// Generate frontend files. When the project has opted into the
	// pnpm-workspaces layout, both the per-frontend file emitter (so
	// package.json declares workspace deps + connect.ts imports from
	// the shared package) and the workspace scaffolder (so packages/api
	// + packages/hooks + pnpm-workspace.yaml exist if this is the first
	// frontend added since flipping the flag) need to know.
	workspaces := cfg.IsFrontendWorkspacesEnabled()
	if err := generator.WriteFrontendWorkspaceFiles(root, cfg.Name, workspaces); err != nil {
		return fmt.Errorf("write frontend workspace files: %w", err)
	}
	if err := generator.GenerateFrontendFilesWithOptions(root, cfg.ModulePath, cfg.Name, name, apiPort, kind, generator.FrontendGenOptions{
		Workspaces: workspaces,
		Output:     output,
		BasePath:   basePath,
	}); err != nil {
		return fmt.Errorf("generate frontend files: %w", err)
	}
	// When the frontend just added is React Native AND workspaces are
	// on, scaffold the @<scope>/ui-native primitives package alongside.
	// The forge.yaml hasn't been written back to disk yet at this
	// point so HasReactNativeFrontend(cfg) can't see the new entry —
	// we detect via the explicit `kind == "mobile"` we already
	// branched on a few lines up.
	if workspaces && kind == "mobile" {
		layout := generator.NewFrontendWorkspaceLayout(cfg.Name)
		if err := generator.WriteUINativePackageFiles(root, layout); err != nil {
			return fmt.Errorf("write ui-native package files: %w", err)
		}
	}

	// Update forge.yaml. Only persist `output:` when the user passed
	// the flag — keeping the field empty lets the per-frontend
	// scaffold default (currently "static") evolve without forcing
	// every existing forge.yaml to track it explicitly.
	feEntry := config.FrontendConfig{
		Name: name,
		Type: frontendType,
		Kind: frontendKind,
		Path: fmt.Sprintf("frontends/%s", name),
		Port: port,
	}
	if output != "" && (kind == "" || kind == "web") {
		feEntry.Output = output
	}
	// Persist base_path so `forge generate` keeps regenerating the
	// basepath_gen.ts helper with the right prefix.
	if basePath != "" && (kind == "" || kind == "web") {
		feEntry.BasePath = basePath
	}
	cfg.Frontends = append(cfg.Frontends, feEntry)
	// Flip features.frontend on so subsequent `forge generate` runs
	// pick up the frontend codegen pass. Projects scaffolded with
	// `forge new --kind service` (no --frontend) leave this field
	// explicitly false; without this flip the frontend dir + files
	// would be emitted but never regenerated. Use a stable address so
	// the *bool survives marshal round-trips.
	frontendOn := true
	cfg.Features.Frontend = &frontendOn

	// Bring stack.frontend.framework in sync with the frontend we just
	// added. Projects scaffolded without --frontend leave this field as
	// "none" — downstream tooling (lint config, CI, codegen branching)
	// reads the framework field directly and would misread the project
	// as having no frontend stack. Only overwrite when empty or "none"
	// so a user who set something exotic (e.g. "svelte") keeps it.
	if cfg.Stack.Frontend.Framework == "" || cfg.Stack.Frontend.Framework == "none" {
		cfg.Stack.Frontend.Framework = frontendType
	}

	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	// Install the new frontend's npm dependencies so the user can run
	// the dev server (or `forge generate` post-codegen for the hooks
	// import) without an extra manual step. Failures here are non-fatal:
	// if `npm` isn't on PATH, the scaffold is still on disk and we just
	// nudge the user to install dependencies themselves.
	frontendDir := filepath.Join(root, "frontends", name)
	if err := runFrontendNpmInstall(ctx, frontendDir); err != nil {
		fmt.Printf("\n⚠️  %v\n", err)
	}

	fmt.Printf("\n✅ Frontend '%s' added successfully!\n", name)

	return nil
}

// runFrontendNpmInstall runs `npm install` in the freshly scaffolded
// frontend directory so the user can immediately run the dev server.
// A missing `npm` binary is treated as a soft warning — the scaffold
// itself succeeded and the user can install dependencies later.
//
// FORGE_SKIP_NPM_INSTALL=1 short-circuits the install. This is the
// testing seam: unit tests that exercise the forge.yaml/scaffold logic
// of `forge add frontend` don't care about node_modules, and the npm
// install was ~13s apiece — three such tests dominated the entire
// internal/cli suite (85s → ~18s once skipped). CI/agents can export it
// for the same reason; the real install is still covered by the e2e
// frontend fixture, which needs node_modules to actually build.
func runFrontendNpmInstall(ctx context.Context, frontendDir string) error {
	if os.Getenv("FORGE_SKIP_NPM_INSTALL") != "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "npm", "install")
	cmd.Dir = frontendDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("\nRunning `npm install` in %s ...\n", frontendDir)
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("npm not found on PATH; run `npm install` in %s manually", frontendDir)
		}
		return fmt.Errorf("npm install failed in %s: %v (run it manually to see full output)", frontendDir, err)
	}
	return nil
}

// --- add webhook ---

func newAddWebhookCmd() *cobra.Command {
	var serviceName string

	cmd := &cobra.Command{
		Use:   "webhook <name>",
		Short: "Add a webhook endpoint to an existing service",
		Long: `Add a webhook ingestion endpoint to an existing Go service.

This scaffolds a webhook handler with signature verification and idempotency,
along with a test file. The handler is added to the service's handler directory.

Example:
  forge add webhook stripe --service payments
  forge add webhook github --service notifications`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddWebhook(args[0], serviceName)
		},
	}

	cmd.Flags().StringVar(&serviceName, "service", "", "Target service name (required)")
	_ = cmd.MarkFlagRequired("service")

	return cmd
}

func runAddWebhook(name, serviceName string) error {
	if err := validateProjectName(name); err != nil {
		return fmt.Errorf("invalid webhook name: %w", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "webhook"); err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Find the target service.
	svcIdx := -1
	for i, svc := range cfg.Services {
		if svc.Name == serviceName {
			svcIdx = i
			break
		}
	}
	if svcIdx == -1 {
		return fmt.Errorf("service %q not found in forge.yaml", serviceName)
	}
	// Webhooks require a serving binary; declaring one on a service this
	// binary does not register (no serviceRow in pkg/app/services.go)
	// would fail the next `forge generate`. Reject at add time with the
	// full story. Best-effort parse: a broken registry falls open here —
	// the generate-time check is the hard gate.
	if reg, regErr := loadServiceRegistry(root); regErr == nil &&
		isConnectServiceConfig(cfg.Services[svcIdx]) && !reg.registered(serviceName) {
		return fmt.Errorf("service %q is not registered in %s — webhooks require a serving binary; add `%s(app, cfg, logger, devMode, opts...),` to RegisteredServices there first, or add the webhook to the binary that serves it",
			serviceName, serviceRegistryRelPath, codegen.ServiceRowFuncName(serviceName))
	}

	// Check for duplicate webhook.
	for _, wh := range cfg.Services[svcIdx].Webhooks {
		if wh.Name == name {
			return fmt.Errorf("webhook %q already exists in service %q", name, serviceName)
		}
	}

	fmt.Printf("Adding webhook '%s' to service '%s'...\n", name, serviceName)

	// Generate webhook files.
	if err := generator.GenerateWebhookFiles(root, cfg.ModulePath, serviceName, name); err != nil {
		return fmt.Errorf("generate webhook files: %w", err)
	}

	// Update forge.yaml.
	cfg.Services[svcIdx].Webhooks = append(cfg.Services[svcIdx].Webhooks, config.WebhookConfig{
		Name: name,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Webhook '%s' added to service '%s'!\n", name, serviceName)

	return nil
}

// --- add binary ---

// newAddBinaryCmd is the cobra surface for `forge add binary <name>`.
//
// "Binary" is the third long-running shape forge generates, alongside
// service (Connect-RPC server) and worker (in-process goroutine under
// the canonical server). It exists for processes that need their own
// Deployment but don't fit the server / worker / operator templates —
// reverse proxies, sidecars, off-service NATS consumers, gateways.
//
// Pre-binary, every project that needed a second long-running process
// hand-wrote ~270 LOC of cobra + signal-handling + lifecycle
// boilerplate (cpnext's workspace_proxy went through this rewrite three
// times across rebuilds). The scaffold here lifts that boilerplate
// into the generator so the next equivalent is the user's business
// logic plus a thin glue layer.
func newAddBinaryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "binary <name>",
		Short: "Add a non-server long-running binary to the project",
		Long: `Add a non-server long-running binary to an existing Forge project.

A binary is a process with its own Deployment shape. Use this when:
  - You need a reverse proxy / gateway in front of pods.
  - You want an off-service NATS consumer that isn't an in-process worker.
  - You need a sidecar with its own deploy lifecycle.

For in-process background work, use 'forge add worker' instead — workers
share the canonical server's lifecycle and Deps.

This creates:
  cmd/<name>.go                       Cobra subcommand (registered against the shared root)
  internal/<name>/contract.go          Deps, Service, New(deps) (*Runner, error)
  internal/<name>/<name>.go            Runner.Run(ctx) lifecycle body
  internal/<name>/<name>_test.go       Lifecycle + validateDeps tests

And an entry under 'binaries:' in forge.yaml so deploy emits a
Deployment for the binary. See the binaries skill (` + "`forge skill load binaries`" + `)
for when to choose a binary vs worker vs service.

Example:
  forge add binary workspace-proxy`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddBinary(args[0])
		},
	}

	return cmd
}

func runAddBinary(name string) error {
	if err := validateIdentifier(name); err != nil {
		return fmt.Errorf("invalid binary name: %w", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "binary"); err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	// Conflict checks. Binaries share the cmd/ directory with the
	// canonical `cmd/server.go` and any per-service shared subcommands,
	// so we check both binaries: AND services:.
	for _, b := range cfg.Binaries {
		if b.Name == name {
			return fmt.Errorf("binary %q already exists in the project", name)
		}
	}
	for _, svc := range cfg.Services {
		if svc.Name == name {
			return fmt.Errorf("%q already exists in the project as a %s", name, svc.Type)
		}
	}
	// Reserved cobra subcommand names that would shadow the binary.
	switch naming.ServicePackage(name) {
	case "server", "version", "db":
		return fmt.Errorf("%q conflicts with a reserved cobra subcommand; pick a different name", name)
	}

	fmt.Printf("Adding binary '%s'...\n", name)

	// Generate the four scaffold files (cmd-binary.go, contract.go,
	// binary.go, binary_test.go).
	if err := generator.GenerateBinaryFiles(root, cfg.ModulePath, name); err != nil {
		return fmt.Errorf("generate binary files: %w", err)
	}

	// Update forge.yaml. Path uses the Go-package form so it matches
	// the directory the scaffolder creates ("workspace-proxy" ->
	// cmd/workspace_proxy.go).
	pkg := naming.ServicePackage(name)
	cfg.Binaries = append(cfg.Binaries, config.BinaryConfig{
		Name: name,
		Path: fmt.Sprintf("cmd/%s.go", pkg),
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Binary '%s' added successfully!\n", name)
	fmt.Printf("   - cmd/%s.go\n", pkg)
	fmt.Printf("   - internal/%s/contract.go\n", pkg)
	fmt.Printf("   - internal/%s/%s.go\n", pkg, pkg)
	fmt.Printf("   - internal/%s/%s_test.go\n", pkg, pkg)
	fmt.Printf("   - forge.yaml (binaries: entry)\n\n")
	fmt.Printf("Next steps:\n")
	fmt.Printf("  1. Edit internal/%s/%s.go to implement the runtime loop.\n", pkg, pkg)
	fmt.Printf("  2. Add a Deployment for the binary in deploy/kcl/<env>/main.k\n")
	fmt.Printf("     (the {{range .Binaries}} block under `applications` is wired\n")
	fmt.Printf("     for new projects; existing projects need to copy the entry\n")
	fmt.Printf("     pattern from a sibling Application).\n")
	return nil
}
