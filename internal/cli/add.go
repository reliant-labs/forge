package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
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
// or operator name. Hyphens are allowed since they are valid in module paths
// and directory names; templates use snakeCase/pascalCase helpers to convert
// when a Go identifier is needed (e.g. "admin-server" -> package "admin_server"
// and field "AdminServer"). The leading-character and reserved-word rules
// match validateProjectName so all top-level scaffold names share one shape.
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
		Short: "Add a service, worker, operator, or frontend to an existing project",
		Long: `Add a new service, worker, operator, or frontend to an existing Forge project.

Subcommands:
  forge add service <name>                        Add a new Go service
  forge add worker <name>                         Add a new background worker
  forge add operator <name> [--group G] [--version V]  Add a Kubernetes operator
  forge add frontend <name>                       Add a new Next.js frontend
  forge add webhook <name> --service S            Add a webhook endpoint to a service
  forge add package <name>                        Add a new internal package (alias for package new)`,
	}

	cmd.AddCommand(newAddServiceCmd())
	cmd.AddCommand(newAddWorkerCmd())
	cmd.AddCommand(newAddOperatorCmd())
	cmd.AddCommand(newAddCRDCmd())
	cmd.AddCommand(newAddFrontendCmd())
	cmd.AddCommand(newAddWebhookCmd())
	cmd.AddCommand(newAddPackageCmd())

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
		return "", fmt.Errorf("forge.yaml not found in current directory; run this from the project root")
	}
	return cwd, nil
}

// requireServiceKind reads forge.yaml at root and returns an error if the
// project's kind is not "service". `forge add service/operator/worker/webhook`
// only makes sense for server-shaped projects — CLI and library kinds have
// no Connect-RPC server to attach handlers to.
func requireServiceKind(root, action string) error {
	cfg, err := generator.ReadProjectConfig(filepath.Join(root, "forge.yaml"))
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}
	if !cfg.IsServiceKind() {
		return fmt.Errorf("'forge add %s' is only available for service projects (this project's kind: %s). Re-run `forge new` with --kind service to scaffold a server, or use `forge add package` for internal Go packages",
			action, cfg.EffectiveKind())
	}
	return nil
}

// --- add service ---

func newAddServiceCmd() *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "service <name>",
		Short: "Add a new Go service to the project",
		Long: `Add a new Go service to an existing Forge project.

This creates the service directory structure, proto file, Dockerfile,
hot-reload config, and updates the project configuration.

Example:
  forge add service users
  forge add service orders --port 8082`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddService(args[0], port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Service port (default: auto-increment from 8080)")

	return cmd
}

func runAddService(name string, port int) error {
	if err := validateServiceName(name); err != nil {
		return fmt.Errorf("invalid service name: %w", err)
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
		return fmt.Errorf("read project config: %w", err)
	}

	// Check for name conflict
	for _, svc := range cfg.Services {
		if svc.Name == name {
			return fmt.Errorf("service %q already exists in the project", name)
		}
	}

	// Auto-assign port if not specified
	if port == 0 {
		port = 8080
		for _, svc := range cfg.Services {
			if svc.Port >= port {
				port = svc.Port + 1
			}
		}
	}

	fmt.Printf("Adding service '%s' (port %d)...\n", name, port)

	// Generate service files (service.go, handlers.go, proto)
	if err := generator.GenerateServiceFiles(root, cfg.ModulePath, name, cfg.Name, port); err != nil {
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
	// the Go-package form so it matches the directory the scaffolder
	// actually creates ("admin-server" -> handlers/admin_server).
	cfg.Services = append(cfg.Services, config.ServiceConfig{
		Name: name,
		Type: "go_service",
		Path: fmt.Sprintf("handlers/%s", generator.ServicePackageName(name)),
		Port: port,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	// Run the full generation pipeline: buf generate, service stubs, mocks,
	// bootstrap.go, testing.go, go mod tidy, etc.
	fmt.Println("\n🔧 Running generation pipeline...")
	generateMu.Lock()
	err = runGeneratePipeline(root, false, false)
	generateMu.Unlock()
	if err != nil {
		if restoreErr := os.WriteFile(configPath, originalConfigBytes, 0o644); restoreErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to restore original project config after pipeline failure: %v\n", restoreErr)
		}
		return fmt.Errorf("generation pipeline failed for service %q (project config restored): %w", name, err)
	}

	// Generate E2E test harness
	fmt.Println("Generating E2E test harness...")
	e2eMethods := generator.MethodsFromProtoStub(name)
	if err := generator.GenerateE2ETests(root, name, cfg.ModulePath, cfg.Name, e2eMethods); err != nil {
		fmt.Printf("  warning: failed to generate E2E tests: %v\n", err)
		// Non-fatal: the service was created successfully
	}

	fmt.Printf("\n✅ Service '%s' added successfully!\n", name)

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

	cmd := &cobra.Command{
		Use:   "worker <name>",
		Short: "Add a new background worker to the project",
		Long: `Add a new background worker to an existing Forge project.

A worker is a long-running process that doesn't serve HTTP but participates
in the single-binary lifecycle. It has Start(ctx)/Stop(ctx) methods, health
reporting, and the same Deps injection as services.

Example:
  forge add worker email_sender
  forge add worker order_processor
  forge add worker cleanup --kind cron --schedule "*/5 * * * *"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddWorker(args[0], kind, schedule)
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", "worker kind (use cron for scheduled workers)")
	cmd.Flags().StringVar(&schedule, "schedule", "", "cron schedule for --kind cron workers")

	return cmd
}

func runAddWorker(name, kind, schedule string) error {
	if err := validateIdentifier(name); err != nil {
		return fmt.Errorf("invalid worker name: %w", err)
	}

	kind = strings.ToLower(strings.TrimSpace(kind))
	schedule = strings.TrimSpace(schedule)
	switch kind {
	case "", "cron":
	default:
		return fmt.Errorf("invalid worker kind %q: valid kinds are cron", kind)
	}
	if kind == "cron" {
		if schedule == "" {
			return fmt.Errorf("--schedule is required when --kind cron")
		}
		if _, err := cron.ParseStandard(schedule); err != nil {
			return fmt.Errorf("invalid cron schedule %q: %w", schedule, err)
		}
	} else if schedule != "" {
		return fmt.Errorf("--schedule requires --kind cron")
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
		Path:     fmt.Sprintf("workers/%s", generator.ServicePackageName(name)),
		Schedule: schedule,
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
		// Non-fatal: the worker files were created successfully
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
		Path:    fmt.Sprintf("operators/%s", generator.ServicePackageName(name)),
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

	cmd := &cobra.Command{
		Use:   "frontend <name>",
		Short: "Add a new frontend to the project",
		Long: `Add a new frontend to an existing forge project.

By default this creates a Next.js web frontend with Connect RPC client setup.
Use --kind mobile to scaffold a React Native app using Expo.

Example:
  forge add frontend web
  forge add frontend dashboard --port 3001
  forge add frontend mobile --kind mobile`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddFrontend(args[0], port, kind)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Frontend dev server port (default: auto-increment from 3000)")
	cmd.Flags().StringVar(&kind, "kind", "", "frontend kind (web or mobile)")

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

func runAddFrontend(name string, port int, kind string) error {
	if err := validateFrontendName(name); err != nil {
		return fmt.Errorf("invalid frontend name: %w", err)
	}

	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "", "web", "mobile":
	default:
		return fmt.Errorf("invalid frontend kind %q: valid kinds are web, mobile", kind)
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
	if kind == "mobile" {
		frontendType = "react-native"
		frontendDescription = "mobile frontend"
	} else if kind == "" {
		frontendKind = ""
	}

	fmt.Printf("Adding %s '%s' (port %d)...\n", frontendDescription, name, port)

	// Determine the API port from the first service
	apiPort := 8080
	if len(cfg.Services) > 0 {
		apiPort = cfg.Services[0].Port
	}

	// Generate frontend files
	if err := generator.GenerateFrontendFiles(root, cfg.ModulePath, cfg.Name, name, apiPort, kind); err != nil {
		return fmt.Errorf("generate frontend files: %w", err)
	}

	// Update forge.yaml
	cfg.Frontends = append(cfg.Frontends, config.FrontendConfig{
		Name: name,
		Type: frontendType,
		Kind: frontendKind,
		Path: fmt.Sprintf("frontends/%s", name),
		Port: port,
	})
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Frontend '%s' added successfully!\n", name)

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
