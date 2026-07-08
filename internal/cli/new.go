package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/naming"
)

func newNewCmd() *cobra.Command {
	var (
		projectPath     string
		modulePath      string
		kindFlag        string
		serviceNames    []string
		frontendNames   []string
		goVersion       string
		inPlace         bool
		force           bool
		license         string
		licenseAuthor   string
		disableFeatures []string
		harness         string
		skipTools       bool
		bufPlugins      string
		binaryMode      string
		// frontendWorkspaces opts the project into the pnpm-workspaces
		// layout — packages/api + packages/hooks shared across all
		// frontends, frontends import via "@<scope>/api". Default off;
		// must be opted in explicitly. See SKILL.md/frontend-workspaces.
		frontendWorkspaces bool
	)

	cmd := &cobra.Command{
		Use:   "new [project-name] --mod [module-path]",
		Short: "Create a new Forge project (service / CLI / library)",
		Long: `Create a new project with the Forge framework structure.

By default no service is scaffolded: the binary is a deployment unit
that mounts services — it is not a domain entity. Add your first
service after scaffolding with 'forge add service <entity>' (name it
after a domain entity like item/order/user, not the binary), or opt
into an initial service at creation time with --service <entity>.

Pick a project kind with --kind:

  --kind service  (default) Connect-RPC service: handlers, middleware, deploy
                            manifests, observability wiring, frontend support.
  --kind cli                Cobra-based CLI binary: cmd/<name>/main.go +
                            cmd/<name>/version.go, no server scaffolding,
                            no proto/services, no deploy/.
  --kind library            Pure Go module: pkg/<name>/ skeleton, no cmd/,
                            no CI workflows by default.

Use --disable to turn off features at creation time:
  forge new my-project --mod ... --disable ci,deploy
  forge new my-project --mod ... --disable orm --disable migrations

Valid feature names: orm, codegen, migrations, ci, build, deploy,
contracts, docs, frontend, observability, hot_reload, packs.

Example:
  forge new my-project --mod github.com/example/my-project
  forge new my-project --mod github.com/example/my-project --service gateway
  forge new my-project --mod github.com/example/my-project --frontend web
  forge new mycli      --mod github.com/example/mycli --kind cli
  forge new mylib      --mod github.com/example/mylib --kind library
  forge new --in-place --mod github.com/example/my-project`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var projectName string
			if len(args) > 0 {
				projectName = args[0]
			}
			return runNew(cmd.Context(), projectName, projectPath, modulePath, kindFlag, serviceNames, frontendNames, goVersion, inPlace, force, license, licenseAuthor, disableFeatures, harness, skipTools, bufPlugins, binaryMode, frontendWorkspaces)
		},
	}

	cmd.Flags().StringVarP(&projectPath, "path", "p", ".", "Path where to create the project")
	cmd.Flags().StringVar(&modulePath, "mod", "", "Go module path (required, e.g., github.com/example/my-project)")
	cmd.Flags().StringVar(&kindFlag, "kind", "service", "Project kind: service (default), cli, library")
	cmd.Flags().StringSliceVar(&serviceNames, "service", nil, "Name(s) of initial Go services (repeatable or comma-separated). Name services after domain entities (item, order), not the binary. Omit to scaffold zero services and add them later via 'forge add service <entity>'")
	cmd.Flags().StringSliceVar(&frontendNames, "frontend", nil, "Name(s) of Next.js frontends (can be repeated or comma-separated)")
	cmd.Flags().StringVar(&goVersion, "go-version", "", "Go version to use in go.mod (e.g., 1.24); defaults to detected version")
	cmd.Flags().BoolVar(&inPlace, "in-place", false, "Create project in current directory instead of a new subdirectory")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing project configuration")
	cmd.Flags().StringVar(&license, "license", "MIT", "License to include (MIT, Apache-2.0, BSD-3-Clause, none)")
	cmd.Flags().StringVar(&licenseAuthor, "license-author", "", "Author/copyright holder for the LICENSE file (defaults to git config user.name)")
	cmd.Flags().StringSliceVar(&disableFeatures, "disable", nil, "Features to disable (comma-separated): orm, codegen, migrations, ci, build, deploy, contracts, docs, frontend, observability, hot_reload, packs")
	cmd.Flags().StringVar(&harness, "harness", "reliant", "AI harness conventions to scaffold for: reliant (default; reliant CLI auto-discovers skills via forge.yaml), claude (writes CLAUDE.md + .claude/skills/), cursor (.cursorrules), copilot (.github/copilot-instructions.md), codex (AGENTS.md)")
	cmd.Flags().BoolVar(&skipTools, "skip-tools", false, "Skip auto-installing protoc-gen-go / protoc-gen-connect-go (run 'forge tools install' later)")
	cmd.Flags().StringVar(&bufPlugins, "buf-plugins", "local", "Default proto plugin source: 'local' (resolved from PATH; no BSR auth needed) or 'remote' (BSR-hosted, requires login under load)")
	cmd.Flags().StringVar(&binaryMode, "binary", "per-service", "Binary packaging: 'per-service' (default — canonical cmd/server.go cobra root, one Application per service) or 'shared' (one Go binary, cobra subcommand per service, KCL MultiServiceApplication for deploy)")
	cmd.Flags().BoolVar(&frontendWorkspaces, "frontend-workspaces", false, "Opt into pnpm-workspaces layout: emit packages/api + packages/hooks + packages/ui-web shared across all frontends. Off by default; recommended once you have 2+ frontends (web + mobile).")
	_ = cmd.MarkFlagRequired("mod")

	return cmd
}

// validateNewArgs runs the pure validation/normalization logic for runNew —
// the part that doesn't touch the filesystem or run subprocesses. Returns the
// normalized kind, the normalized buf-plugins choice, the normalized binary
// mode, or an error.
//
// Extracted so tests can exercise the validation surface without invoking the
// full scaffold (which calls `go mod tidy`, `buf generate`, etc., and is slow
// or hangs in CI environments without network access).
func validateNewArgs(kindFlag, bufPlugins, binaryMode string, serviceNames, frontendNames []string) (kind, plugins, binary string, err error) {
	// Validate --kind. Empty string is treated as "service" for back-compat
	// (callers that don't pass the flag at all).
	kind = strings.ToLower(strings.TrimSpace(kindFlag))
	if kind == "" {
		kind = config.ProjectKindService
	}
	switch kind {
	case config.ProjectKindService, config.ProjectKindCLI, config.ProjectKindLibrary:
		// ok
	default:
		return "", "", "", cliutil.UserErr("forge new",
			fmt.Sprintf("invalid --kind %q: valid values are service, cli, library", kindFlag),
			"",
			"pass --kind=service for a Connect-RPC server, --kind=cli for a Cobra binary, or --kind=library for a pure Go module")
	}

	// Validate --buf-plugins. Default 'local' (no BSR auth required); the
	// 'remote' opt-in is preserved for users who genuinely want BSR-hosted
	// plugins (no install required, latest version always — but rate-limited
	// for anonymous users).
	plugins = strings.ToLower(strings.TrimSpace(bufPlugins))
	if plugins == "" {
		plugins = "local"
	}
	switch plugins {
	case "local", "remote":
		// ok
	default:
		return "", "", "", cliutil.UserErr("forge new",
			fmt.Sprintf("invalid --buf-plugins %q: valid values are local, remote", bufPlugins),
			"",
			"pass --buf-plugins=local (default; uses protoc-gen-go on PATH) or --buf-plugins=remote (BSR-hosted, no install required)")
	}

	// Validate --binary. Empty string is treated as "per-service" for
	// back-compat (callers/tests that don't pass the flag at all). Only
	// meaningful for service projects.
	binary = strings.ToLower(strings.TrimSpace(binaryMode))
	if binary == "" {
		binary = config.ProjectBinaryPerService
	}
	switch binary {
	case config.ProjectBinaryPerService, config.ProjectBinaryShared:
		// ok
	default:
		return "", "", "", cliutil.UserErr("forge new",
			fmt.Sprintf("invalid --binary %q: valid values are per-service, shared", binaryMode),
			"",
			"pass --binary=per-service (default; one cmd/server.go per service) or --binary=shared (one binary, cobra subcommand per service)")
	}

	// Reject incompatible flag combinations early so the user gets a
	// clean error before any directory is created.
	if kind != config.ProjectKindService {
		if len(serviceNames) > 0 {
			return "", "", "", cliutil.UserErr("forge new",
				fmt.Sprintf("--service is only meaningful with --kind service (got --kind %s)", kind),
				"",
				"drop --service, or change to --kind service")
		}
		if len(frontendNames) > 0 {
			return "", "", "", cliutil.UserErr("forge new",
				fmt.Sprintf("--frontend is only meaningful with --kind service (got --kind %s)", kind),
				"",
				"drop --frontend, or change to --kind service")
		}
		if binary == config.ProjectBinaryShared {
			return "", "", "", cliutil.UserErr("forge new",
				fmt.Sprintf("--binary shared is only meaningful with --kind service (got --kind %s)", kind),
				"",
				"drop --binary=shared, or change to --kind service")
		}
	}
	return kind, plugins, binary, nil
}

//nolint:revive,cyclop // TODO: collapse into a runNewOptions struct; the cyclomatic complexity comes from cobra flag fan-out (resume/force/in-place/per-feature toggles) and refactoring requires a shared options type — cobra flag wiring is the only call site.
func runNew(ctx context.Context, projectName, projectPath, modulePath, kindFlag string, serviceNames []string, frontendNames []string, goVersion string, inPlace bool, force bool, license, licenseAuthor string, disableFeatures []string, harness string, skipTools bool, bufPlugins, binaryMode string, frontendWorkspaces bool) error {
	kindNormalized, bufPluginsNormalized, binaryNormalized, err := validateNewArgs(kindFlag, bufPlugins, binaryMode, serviceNames, frontendNames)
	if err != nil {
		return err
	}

	targetPath, projectName, err := resolveNewTargetPath(projectName, projectPath, inPlace, force)
	if err != nil {
		return err
	}

	// Validate service names
	for _, svcName := range serviceNames {
		if err := validateServiceName(svcName); err != nil {
			return fmt.Errorf("invalid service name %q: %w", svcName, err)
		}
	}

	// Validate frontend names
	for _, feName := range frontendNames {
		if err := validateFrontendName(feName); err != nil {
			return fmt.Errorf("invalid frontend name %q: %w", feName, err)
		}
	}

	fmt.Printf("Creating new project '%s' at %s\n", projectName, targetPath)
	if len(serviceNames) > 0 {
		if len(serviceNames) == 1 {
			fmt.Printf("  Service: %s\n", serviceNames[0])
		} else {
			fmt.Printf("  Services: %s\n", strings.Join(serviceNames, ", "))
		}
	}
	if len(frontendNames) > 0 {
		fmt.Printf("  Frontend: %s\n", strings.Join(frontendNames, ", "))
	}

	// Clean up on failure. To guard against TOCTOU where another process
	// might have created files at targetPath in the meantime, we drop a
	// marker file immediately after creating the directory and only run
	// cleanup when that marker is still present.
	// In --in-place mode, we never RemoveAll the target directory since it
	// is an existing directory the user owns. We only remove the marker.
	var success bool
	markerPath := filepath.Join(targetPath, ".forge", ".scaffold-in-progress")
	defer func() {
		if success {
			return
		}
		if _, err := os.Stat(markerPath); err != nil {
			return
		}
		if inPlace {
			// In --in-place mode, only remove the marker — don't nuke the user's directory.
			if rmErr := os.Remove(markerPath); rmErr != nil && !os.IsNotExist(rmErr) {
				fmt.Fprintf(os.Stderr, "warning: failed to remove scaffold marker: %v\n", rmErr)
			}
			return
		}
		if rmErr := os.RemoveAll(targetPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to clean up %s: %v\n", targetPath, rmErr)
		}
	}()

	// Create the target directory and drop the in-progress marker before
	// invoking the generator. The generator is expected to populate the
	// directory; creating it up-front is safe because the generator uses
	// MkdirAll internally.
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}
	if err := os.WriteFile(markerPath, []byte("forge scaffold in progress\n"), 0o644); err != nil {
		return fmt.Errorf("failed to write scaffold marker: %w", err)
	}

	gen, err := configureProjectGenerator(newGeneratorConfig{
		projectName:        projectName,
		targetPath:         targetPath,
		modulePath:         modulePath,
		kind:               kindNormalized,
		binary:             binaryNormalized,
		serviceNames:       serviceNames,
		frontendNames:      frontendNames,
		goVersion:          goVersion,
		frontendWorkspaces: frontendWorkspaces,
		harness:            harness,
		disableFeatures:    disableFeatures,
	})
	if err != nil {
		return err
	}

	// Generate project structure
	if err := gen.Generate(); err != nil {
		return fmt.Errorf("failed to generate project: %w", err)
	}

	// Write LICENSE file if requested
	if err := writeLicenseFile(targetPath, license, licenseAuthor); err != nil {
		return fmt.Errorf("failed to write LICENSE: %w", err)
	}

	emitHarnessSkills(gen, targetPath)

	// Generate additional services beyond the first (if any).
	//
	// Both binary modes call GenerateServiceFiles to scaffold the
	// per-service handler/proto skeleton. The forge.yaml services list,
	// however, is populated differently: in binary=per-service we append
	// each service post-scaffold via AppendServiceToConfig (the
	// historical, additive path). In binary=shared we wrote ALL services
	// into forge.yaml during writeProjectConfig (so the bootstrap.go
	// generator could see the full set up-front), and skipping the
	// append step here prevents duplicates.
	if err := generateAdditionalServices(targetPath, modulePath, projectName, serviceNames, binaryNormalized, gen.ServicePort); err != nil {
		return err
	}

	// Generate additional frontends beyond the first
	if err := generateAdditionalFrontends(targetPath, modulePath, projectName, frontendNames, gen.ServicePort, gen.FrontendPort, frontendWorkspaces); err != nil {
		return err
	}

	// Apply the --buf-plugins=remote opt-in BEFORE bootstrapGeneratedCode
	// runs, since the bootstrap invokes `buf generate` which reads
	// buf.gen.yaml. The default ('local') is already what the template
	// emits, so only act on 'remote'.
	if bufPluginsNormalized == "remote" {
		applyRemoteBufPlugins(targetPath, frontendNames)
	}

	finalizeNewProject(ctx, newFinalizeInput{
		targetPath:    targetPath,
		kind:          kindNormalized,
		binary:        binaryNormalized,
		bufPlugins:    bufPluginsNormalized,
		skipTools:     skipTools,
		frontendNames: frontendNames,
		markerPath:    markerPath,
	})

	success = true
	fmt.Printf("\n✅ Project '%s' created successfully!\n", projectName)
	printNewNextSteps(projectName, inPlace, kindNormalized, serviceNames)

	return nil
}

// resolveNewTargetPath resolves the scaffold target directory and the effective
// project name from the CLI args, validating the name and refusing to scaffold
// over an existing project. In --in-place mode the project name defaults to the
// target directory's base name and an existing forge.yaml is only tolerated
// with --force. In fresh-directory mode a name is required and the target must
// not already exist. Returns (targetPath, resolvedName).
func resolveNewTargetPath(projectName, projectPath string, inPlace, force bool) (string, string, error) {
	if inPlace {
		// In-place mode: scaffold into the current (or --path) directory directly
		targetPath, err := filepath.Abs(projectPath)
		if err != nil {
			return "", "", fmt.Errorf("failed to resolve path: %w", err)
		}
		// Derive project name from directory if not provided
		if projectName == "" {
			projectName = filepath.Base(targetPath)
		}
		// Validate project name (hyphens allowed for directory/module paths)
		if err := validateProjectName(projectName); err != nil {
			return "", "", fmt.Errorf("invalid project name %q: %w", projectName, err)
		}
		// Check that we're not scaffolding over an existing project
		if _, err := os.Stat(filepath.Join(targetPath, defaultProjectConfigFile)); err == nil {
			if !force {
				return "", "", cliutil.UserErr("forge new --in-place",
					fmt.Sprintf("%s already exists in %s; this directory already contains a Forge project", defaultProjectConfigFile, targetPath),
					"",
					"pass --force to overwrite, or scaffold into a fresh directory")
			}
			fmt.Printf("  --force: overwriting existing %s\n", defaultProjectConfigFile)
		}
		return targetPath, projectName, nil
	}

	if projectName == "" {
		return "", "", cliutil.UserErr("forge new",
			"project name is required",
			"",
			"pass a project name as the first positional arg, or use --in-place to scaffold in the current directory")
	}

	targetPath := filepath.Join(projectPath, projectName)

	// Validate project name (hyphens allowed for directory/module paths)
	if err := validateProjectName(projectName); err != nil {
		return "", "", cliutil.WrapUserErr("forge new",
			fmt.Sprintf("invalid project name %q", projectName),
			"",
			"use a name starting with a letter, containing only letters/digits/_/-",
			err)
	}

	// Check if directory already exists
	if _, err := os.Stat(targetPath); err == nil {
		return "", "", cliutil.UserErr("forge new",
			fmt.Sprintf("directory %s already exists", targetPath),
			"",
			"pick a different project name, or use --in-place --force to overwrite")
	} else if !os.IsNotExist(err) {
		return "", "", cliutil.WrapUserErr("forge new",
			fmt.Sprintf("failed to stat %s", targetPath),
			"",
			"check filesystem permissions on the parent directory",
			err)
	}
	return targetPath, projectName, nil
}

// newGeneratorConfig carries the inputs configureProjectGenerator projects onto
// a generator.ProjectGenerator.
type newGeneratorConfig struct {
	projectName        string
	targetPath         string
	modulePath         string
	kind               string
	binary             string
	serviceNames       []string
	frontendNames      []string
	goVersion          string
	frontendWorkspaces bool
	harness            string
	disableFeatures    []string
}

// configureProjectGenerator builds and configures the project generator from
// the CLI flags: the first service/frontend seed the primary names (additional
// services are passed through so binary=shared can emit one cobra subcommand
// per service at scaffold time), the harness is parsed, and kind-aware feature
// defaults are applied BEFORE --disable so an explicit --disable always wins.
func configureProjectGenerator(c newGeneratorConfig) (*generator.ProjectGenerator, error) {
	gen := generator.NewProjectGenerator(c.projectName, c.targetPath, c.modulePath)
	gen.Kind = c.kind
	gen.Binary = c.binary
	gen.AdditionalServices = nil
	if len(c.serviceNames) > 0 {
		gen.ServiceName = c.serviceNames[0]
		// Pass the rest so binary=shared can emit one cobra subcommand per
		// service at scaffold time. Per-service mode ignores this and
		// continues to add additional services post-scaffold via
		// GenerateServiceFiles + AppendServiceToConfig.
		if len(c.serviceNames) > 1 {
			gen.AdditionalServices = append([]string(nil), c.serviceNames[1:]...)
		}
	}
	gen.GoVersionOverride = c.goVersion
	if len(c.frontendNames) > 0 {
		gen.FrontendName = c.frontendNames[0]
	}
	gen.FrontendWorkspaces = c.frontendWorkspaces

	h, err := generator.ParseHarness(c.harness)
	if err != nil {
		return nil, err
	}
	gen.Harness = h

	// Kind-aware feature defaults BEFORE --disable. Service is the default and
	// leaves every feature enabled; CLI and library kinds turn off the
	// server-shaped features so the scaffolded forge.yaml accurately describes
	// the project shape. See ApplyKindFeatureDefaults for the per-kind matrix.
	gen.ApplyKindFeatureDefaults(c.kind)

	if err := applyDisableFlags(gen, c.disableFeatures); err != nil {
		return nil, err
	}
	return gen, nil
}

// emitHarnessSkills writes forge skills to disk for harnesses that have a
// native skills concept (e.g. claude → .claude/skills/). Reliant/copilot/codex
// skip this (no native skills mechanism, or auto-discovery via forge.yaml).
// Skill-write failures are warned, not fatal.
func emitHarnessSkills(gen *generator.ProjectGenerator, targetPath string) {
	dir := gen.Harness.SkillsDir()
	if dir == "" {
		return
	}
	style, ok := skillStyleForHarness(gen.Harness)
	if !ok {
		return
	}
	skillsDir := filepath.Join(targetPath, dir)
	// SkillAudienceAll: a new forge project always has forge.yaml, so the
	// harness gets both general methodology and framework skills with full
	// bodies (no @forge-only stripping).
	n, err := WriteSkills(skillsDir, style, SkillAudienceAll)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write forge skills to %s: %v\n", skillsDir, err)
	} else {
		fmt.Printf("📚 Wrote %d forge skills to %s\n", n, dir)
	}
}

// generateAdditionalServices scaffolds every service beyond the first. Both
// binary modes call GenerateServiceFiles for the handler/proto skeleton; the
// forge.yaml services list is populated differently: binary=per-service appends
// each service post-scaffold (the historical additive path), while binary=shared
// already wrote ALL services during writeProjectConfig, so the append is skipped
// to avoid duplicates. Ports are assigned as basePort+i+1.
func generateAdditionalServices(targetPath, modulePath, projectName string, serviceNames []string, binary string, basePort int) error {
	if len(serviceNames) <= 1 {
		return nil
	}
	for i, svcName := range serviceNames[1:] {
		port := basePort + i + 1
		fmt.Printf("\n🔧 Adding additional service '%s' (port %d)...\n", svcName, port)
		if err := generator.GenerateServiceFiles(targetPath, modulePath, svcName, projectName, port); err != nil {
			return fmt.Errorf("failed to generate service %s: %w", svcName, err)
		}
		if binary != config.ProjectBinaryShared {
			if err := generator.AppendServiceToConfig(targetPath, svcName, port); err != nil {
				return fmt.Errorf("failed to update config for service %s: %w", svcName, err)
			}
		}
	}
	return nil
}

// generateAdditionalFrontends scaffolds every frontend beyond the first,
// appending each to forge.yaml. Ports are assigned as baseFrontendPort+i+1.
func generateAdditionalFrontends(targetPath, modulePath, projectName string, frontendNames []string, servicePort, baseFrontendPort int, frontendWorkspaces bool) error {
	for i, feName := range frontendNames[min(1, len(frontendNames)):] {
		fePort := baseFrontendPort + i + 1
		fmt.Printf("\n🔧 Adding additional frontend '%s' (port %d)...\n", feName, fePort)
		if err := generator.GenerateFrontendFilesWithOptions(targetPath, modulePath, projectName, feName, servicePort, "", generator.FrontendGenOptions{
			Workspaces: frontendWorkspaces,
		}); err != nil {
			return fmt.Errorf("failed to generate frontend %s: %w", feName, err)
		}
		if err := generator.AppendFrontendToConfig(targetPath, feName, fePort); err != nil {
			return fmt.Errorf("failed to update config for frontend %s: %w", feName, err)
		}
	}
	return nil
}

// applyRemoteBufPlugins switches the scaffolded Go and per-frontend buf.gen.yaml
// files from local: plugins to BSR-hosted remote: plugins, the coherent
// --buf-plugins=remote opt-in. Failures are warned, not fatal.
func applyRemoteBufPlugins(targetPath string, frontendNames []string) {
	if err := rewriteBufGenYamlToRemote(targetPath); err != nil {
		fmt.Fprintf(os.Stderr, "\n⚠️  Failed to switch buf.gen.yaml to remote plugins: %v\n", err)
	} else {
		fmt.Println("\n🔧 Switched buf.gen.yaml to BSR-hosted (remote:) plugins per --buf-plugins=remote")
		fmt.Println("    Note: anonymous users may hit BSR rate limits; run 'buf registry login' if needed.")
	}
	// Also rewrite each frontend's buf.gen.yaml to use remote: bufbuild/es
	// rather than the local protoc-gen-es plugin. Mirrors the Go-side switch.
	for _, feName := range frontendNames {
		feBufGen := filepath.Join(targetPath, "frontends", feName, "buf.gen.yaml")
		if err := rewriteFrontendBufGenYamlToRemote(feBufGen, feName); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to switch frontends/%s/buf.gen.yaml to remote plugin: %v\n", feName, err)
		}
	}
}

// newFinalizeInput carries the inputs finalizeNewProject needs for the
// post-generation phase.
type newFinalizeInput struct {
	targetPath    string
	kind          string
	binary        string
	bufPlugins    string
	skipTools     bool
	frontendNames []string
	markerPath    string
}

// finalizeNewProject runs the post-generation phase: install proto codegen
// plugins (local-plugin service kind only), install frontend deps BEFORE
// bootstrap (the scaffolded buf.gen.yaml's local: es plugin needs node_modules),
// bootstrap proto/Connect codegen (service kind only), re-record frozen-file
// checksums (bootstrap's goimports -w reformats pkg/* and would otherwise show
// as user-modified), init git, run go mod tidy, and remove the in-progress
// marker. Every step is best-effort — failures are warned, never fatal.
func finalizeNewProject(ctx context.Context, in newFinalizeInput) {
	// Auto-install required proto plugins for the default local-plugin
	// workflow. Skipped for --skip-tools, remote plugins (no local binaries),
	// and non-service kinds.
	if !in.skipTools && in.bufPlugins == "local" && in.kind == config.ProjectKindService {
		fmt.Println("\n🔧 Ensuring proto codegen plugins are installed (use --skip-tools to skip)...")
		if err := runToolsInstall(ctx, "latest", false); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Plugin install incomplete: %v\n", err)
			fmt.Fprintf(os.Stderr, "    Run '%s tools install' manually before '%s generate'.\n", Name(), Name())
		}
	}

	// Frontend dependencies must be installed BEFORE bootstrapGeneratedCode
	// runs, because the scaffolded buf.gen.yaml uses a `local:` plugin path
	// pointing at frontends/<name>/node_modules/.bin/protoc-gen-es.
	if len(in.frontendNames) > 0 {
		fmt.Println("🔧 Installing frontend dependencies (this generates package-lock.json)...")
		if err := runNpmInstall(ctx, in.targetPath, in.frontendNames); err != nil {
			fmt.Printf("Warning: npm install failed: %v\n", err)
			fmt.Println("    @bufbuild/protoc-gen-es will be missing — run 'npm install' in each frontends/<name>/ before 'forge generate'.")
			fmt.Println("    CI also requires package-lock.json to exist.")
		}
	}

	// Service projects bootstrap proto/Connect codegen immediately so the
	// scaffold compiles. CLI/library kinds have no proto/services.
	if in.kind == config.ProjectKindService {
		fmt.Println("\n🔧 Bootstrapping generated proto code...")
		if err := bootstrapGeneratedCode(in.targetPath); err != nil {
			fmt.Fprintf(os.Stderr, "\n⚠️  Project scaffolded but initial code generation failed: %v\n", err)
			fmt.Fprintf(os.Stderr, "    Run '%s generate && %s build' to retry.\n", Name(), Name())
		}
	}

	// Re-record frozen-file checksums after bootstrap (goimports -w reformats
	// pkg/* godoc list markers, which would otherwise show as user-modified).
	postBootstrapBinary := config.ProjectBinaryPerService
	if in.binary != "" {
		postBootstrapBinary = in.binary
	}
	if err := generator.RecordFrozenChecksums(in.targetPath, postBootstrapBinary, in.kind); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to re-record frozen checksums: %v\n", err)
	}

	fmt.Println("\n🔧 Initializing git repository...")
	if err := initGitRepository(ctx, in.targetPath); err != nil {
		fmt.Printf("Warning: failed to initialize git repository: %v\n", err)
	}

	fmt.Println("🔧 Running go mod tidy...")
	if err := runGoModTidy(ctx, in.targetPath); err != nil {
		fmt.Printf("Warning: go mod tidy failed: %v\n", err)
		fmt.Println("You can run 'go mod tidy' manually later")
	}

	// Scaffold finished — remove the in-progress marker so a later failure
	// (if any were ever added) wouldn't delete a completed project.
	if err := os.Remove(in.markerPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: failed to remove scaffold marker: %v\n", err)
	}
}

// printNewNextSteps prints the post-scaffold guidance block. The
// zero-service default is deliberate: a binary is a deployment unit that
// mounts services — it is NOT a domain entity, so `forge new` never
// invents a `<project>Service` with CRUD RPCs nobody asked for. On a
// bare service-kind scaffold the documented first step is
// `forge add service <entity>` with a real domain entity name.
func printNewNextSteps(projectName string, inPlace bool, kind string, serviceNames []string) {
	n := Name()
	fmt.Println("\nNext steps:")
	if !inPlace {
		fmt.Printf("  cd %s\n", projectName)
	}
	switch {
	case kind == config.ProjectKindCLI:
		fmt.Println("  go build ./...        # the cobra skeleton compiles out of the box")
		fmt.Println("  see README.md for the CLI workflow")
	case kind == config.ProjectKindLibrary:
		fmt.Println("  go build ./...        # the pkg/ skeleton compiles out of the box")
		fmt.Println("  add exported types under pkg/ and tests alongside them")
	case len(serviceNames) == 0:
		fmt.Printf("  %s add service <entity>   # first step — name it after a DOMAIN ENTITY (e.g. item, order, user), not the binary\n", n)
		fmt.Printf("  %s run                    # boots the stack; /healthz serves even before any service exists\n", n)
	default:
		fmt.Printf("  edit proto/services/%s/v1/ to define your API (the scaffold ships an example Item entity)\n", naming.ServicePackage(serviceNames[0]))
		fmt.Printf("  %s generate               # regenerate after proto edits\n", n)
		fmt.Printf("  %s run                    # boots the stack\n", n)
	}
}

// rewriteBufGenYamlToRemote switches the scaffolded buf.gen.yaml from
// `local:` plugins (the default) to BSR-hosted `remote:` plugins. Used
// by `forge new --buf-plugins=remote` for users who explicitly want the
// no-install-required experience and accept BSR rate-limits / auth.
//
// Idempotent: a buf.gen.yaml that already declares the remote plugins is
// rewritten to itself.
func rewriteBufGenYamlToRemote(projectDir string) error {
	path := filepath.Join(projectDir, "buf.gen.yaml")
	if _, err := os.Stat(path); err != nil {
		// No buf.gen.yaml in this project (e.g. library kind). Nothing to do.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat buf.gen.yaml: %w", err)
	}
	remote := `version: v2
# Switched to BSR-hosted plugins via 'forge new --buf-plugins=remote'.
# No local protoc-gen-go install required, but anonymous users may hit
# BSR rate limits during heavy generate cycles — 'buf registry login'
# raises the cap. To switch back, replace 'remote: <bsr-path>' with
# 'local: <binary-name>'.
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt:
      - paths=source_relative
  - remote: buf.build/connectrpc/go
    out: gen
    opt:
      - paths=source_relative
`
	return os.WriteFile(path, []byte(remote), 0o644)
}

// rewriteFrontendBufGenYamlToRemote switches a frontend's buf.gen.yaml from
// the default local: TS plugin to the BSR-hosted remote: bufbuild/es. Mirrors
// rewriteBufGenYamlToRemote — used by `forge new --buf-plugins=remote` so
// users who explicitly want the no-install BSR experience get it on both
// the Go and TS sides. Idempotent and a no-op when the file is missing.
func rewriteFrontendBufGenYamlToRemote(path, feName string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	remote := fmt.Sprintf(`version: v2
# Switched to BSR-hosted plugin via 'forge new --buf-plugins=remote'.
# No npm install of @bufbuild/protoc-gen-es required, but anonymous users
# may hit BSR rate limits — 'buf registry login' raises the cap. To switch
# back, replace 'remote: buf.build/bufbuild/es' with
#   'local: ./frontends/%s/node_modules/.bin/protoc-gen-es'
plugins:
  - remote: buf.build/bufbuild/es
    out: frontends/%s/src/gen
    include_imports: true
    opt:
      - target=ts
      - import_extension=.js
`, feName, feName)
	return os.WriteFile(path, []byte(remote), 0o644)
}

// initGitRepository initializes a git repository and makes initial commit
func initGitRepository(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "git", "init")
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init failed: %s", string(output))
	}

	// Activate forge's committed git hooks (.githooks/pre-push runs
	// `forge lint` + `forge audit`; pre-commit stays fast). core.hooksPath
	// is set RELATIVE so it resolves against each worktree's own checkout —
	// one setting in the shared .git/config makes the hooks fire in every
	// linked worktree. Set here, before the initial commit, so the repo
	// ships activated; fresh clones self-heal via ensureGitHooksActivated.
	cmd = exec.CommandContext(ctx, "git", "config", "--local", "core.hooksPath", ".githooks")
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git config core.hooksPath failed: %s", string(output))
	}

	cmd = exec.CommandContext(ctx, "git", "add", ".")
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add failed: %s", string(output))
	}

	// --no-verify: the initial commit is forge-authored, known-good
	// scaffolding — skip the pre-commit framework chain (gofmt/gitleaks/…)
	// on it. The heavy gate (forge lint + audit) is on pre-push, which this
	// commit doesn't trigger anyway; the hooks govern the user's work from
	// here on.
	cmd = exec.CommandContext(ctx, "git", "commit", "--no-verify", "-m", "Initial commit from forge")
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit failed: %s", string(output))
	}

	return nil
}

// runNpmInstall runs `npm install` in each frontend directory so that a
// package-lock.json exists before first commit. CI relies on `npm ci` which
// requires the lockfile.
func runNpmInstall(ctx context.Context, root string, frontends []string) error {
	if _, err := exec.LookPath("npm"); err != nil {
		return fmt.Errorf("npm not found on PATH: %w", err)
	}
	for _, name := range frontends {
		feDir := filepath.Join(root, "frontends", name)
		if _, err := os.Stat(filepath.Join(feDir, "package.json")); err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, "npm", "install", "--no-audit", "--no-fund")
		cmd.Dir = feDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("npm install (%s) failed: %w", name, err)
		}
	}
	return nil
}

// runGoModTidy runs go mod tidy in the project root and gen/ directories when safe.
func runGoModTidy(ctx context.Context, path string) error {

	shouldTidyRoot, err := shouldRunRootGoModTidy(path)
	if err != nil {
		return err
	}

	if shouldTidyRoot {
		cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
		cmd.Dir = path
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go mod tidy (root) failed: %w", err)
		}
	} else {
		fmt.Printf("ℹ️  Skipping root go mod tidy until generated proto code exists. Run '%s generate' first.\n", Name())
	}

	genDir := filepath.Join(path, "gen")
	if _, err := os.Stat(filepath.Join(genDir, "go.mod")); err == nil {
		cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
		cmd.Dir = genDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go mod tidy (gen) failed: %w", err)
		}
	}

	return nil
}

func bootstrapGeneratedCode(path string) error {
	generateMu.Lock()
	defer generateMu.Unlock()

	return runGeneratePipeline(path, false, false)
}

func shouldRunRootGoModTidy(path string) (bool, error) {
	moduleName, err := readModuleName(path)
	if err != nil {
		return false, err
	}

	serviceRoot := filepath.Join(path, "internal", "handlers")
	if _, err := os.Stat(serviceRoot); os.IsNotExist(err) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect handlers directory: %w", err)
	}

	missingGeneratedImports := false
	err = filepath.Walk(serviceRoot, func(currentPath string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || filepath.Ext(currentPath) != ".go" {
			return nil
		}

		contents, err := os.ReadFile(currentPath)
		if err != nil {
			return err
		}

		for _, line := range strings.Split(string(contents), "\n") {
			importPath, ok := extractQuotedImportPath(line)
			if !ok || !strings.HasPrefix(importPath, moduleName+"/gen/") {
				continue
			}

			relativeImportPath := strings.TrimPrefix(importPath, moduleName+"/")
			generatedPath := filepath.Join(path, filepath.FromSlash(relativeImportPath))
			if _, err := os.Stat(generatedPath); os.IsNotExist(err) {
				missingGeneratedImports = true
				return filepath.SkipAll
			} else if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return false, fmt.Errorf("inspect generated imports before go mod tidy: %w", err)
	}

	return !missingGeneratedImports, nil
}

func extractQuotedImportPath(line string) (string, bool) {
	firstQuote := strings.Index(line, "\"")
	if firstQuote == -1 {
		return "", false
	}

	remaining := line[firstQuote+1:]
	secondQuote := strings.Index(remaining, "\"")
	if secondQuote == -1 {
		return "", false
	}

	return remaining[:secondQuote], true
}

func readModuleName(path string) (string, error) {
	contents, err := os.ReadFile(filepath.Join(path, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}

	for _, line := range strings.Split(string(contents), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "module ")), nil
		}
	}

	return "", fmt.Errorf("module directive not found in go.mod")
}

func applyDisableFlags(gen *generator.ProjectGenerator, disable []string) error {
	f := func(b bool) *bool { return &b }(false)
	for _, name := range disable {
		switch strings.TrimSpace(strings.ToLower(name)) {
		case "orm":
			gen.Features.ORM = f
		case "codegen":
			gen.Features.Codegen = f
		case "migrations":
			gen.Features.Migrations = f
		case "ci":
			gen.Features.CI = f
		case "build":
			gen.Features.Build = f
		case "contracts":
			gen.Features.Contracts = f
		case "docs":
			gen.Features.Docs = f
		case "frontend":
			gen.Features.Frontend = f
		case "observability":
			gen.Features.Observability = f
		case "hot_reload", "hot-reload", "hotreload":
			gen.Features.HotReload = f
		case "packs":
			gen.Features.Packs = f
		case "deploy":
			gen.Features.Deploy = f
		case "ingress", "external_builds", "operators", "strict_wiring":
			return cliutil.UserErr("forge new --disable",
				fmt.Sprintf("feature %q is experimental (opt-in only); cannot be --disable'd because it's already off by default", name),
				"",
				"experimental features default off; opt in per project via `features.experimental.<name>: true` in forge.yaml")
		default:
			return cliutil.UserErr("forge new --disable",
				fmt.Sprintf("unknown feature %q; valid features: orm, codegen, migrations, ci, build, deploy, contracts, docs, frontend, observability, hot_reload, packs", name),
				"",
				"pick a feature from the list above (comma-separated, repeatable); names are case-insensitive")
		}
	}
	return nil
}
