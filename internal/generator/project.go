package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// cliName returns the command name users should type to invoke Forge.
// When the binary is "forge" (standalone), it returns "forge".
// When embedded in another binary (e.g. "reliant"), it returns "reliant forge".
func cliName() string {
	base := filepath.Base(os.Args[0])
	if base == "forge" {
		return "forge"
	}
	return base + " forge"
}

// ProjectGenerator generates new project structure
type ProjectGenerator struct {
	Name               string
	Path               string
	ModulePath         string
	Kind               string   // project kind: "service" (default), "cli", "library"
	Binary             string   // binary mode: "per-service" (default), "shared" — emit one Go binary with cobra subcommand per service when shared
	ServiceName        string   // initial service name (empty if none specified)
	AdditionalServices []string // additional service names beyond ServiceName — only consumed by binary=shared scaffolds; per-service mode adds these post-scaffold
	ServicePort        int      // initial service port (default: 8080)
	FrontendName       string   // optional initial Next.js frontend name
	FrontendPort       int      // frontend port (default: 3000)
	// FrontendWorkspaces opts the project into the pnpm-workspaces
	// layout: emit a root pnpm-workspace.yaml + packages/api +
	// packages/hooks, frontends consume @<scope>/api / @<scope>/hooks
	// via "workspace:*" deps. Off by default — single-frontend
	// projects keep the historic per-frontend layout unchanged.
	FrontendWorkspaces bool
	GoVersionOverride  string                // if set, use this Go version instead of detecting
	Features           config.FeaturesConfig // feature flags for generation
	Harness            Harness               // AI harness (default: reliant) — controls memory file path and skill emission
	// BuildVersionVar mirrors forge.yaml build.version_var: an additional
	// `-ldflags -X` target the Dockerfile stamps with the build version.
	// Empty at scaffold time (a fresh forge.yaml has no build: block), so
	// the Dockerfile's `{{if .VersionVar}}` renders nothing — preserving
	// the historical main.version-only stamping. `forge generate` /
	// upgrade re-render with the live forge.yaml value.
	BuildVersionVar string
}

// effectiveBinary returns the binary mode, defaulting to "per-service".
func (g *ProjectGenerator) effectiveBinary() string {
	return config.EffectiveProjectBinary(g.Binary)
}

// isBinaryShared reports whether this project uses the shared-binary
// codegen (one Go binary, cobra subcommand per service, KCL
// MultiServiceApplication for deploy). Only meaningful for services.
func (g *ProjectGenerator) isBinaryShared() bool {
	return g.isService() && g.effectiveBinary() == config.ProjectBinaryShared
}

// effectiveKind returns the project kind, defaulting to service so a
// zero-value ProjectGenerator preserves pre-existing behavior.
func (g *ProjectGenerator) effectiveKind() string {
	return config.EffectiveProjectKind(g.Kind)
}

// isService reports whether this generator emits a Connect-RPC server scaffold.
func (g *ProjectGenerator) isService() bool {
	return g.effectiveKind() == config.ProjectKindService
}

// isCLI reports whether this generator emits a Cobra-based CLI binary scaffold.
func (g *ProjectGenerator) isCLI() bool {
	return g.effectiveKind() == config.ProjectKindCLI
}

// isLibrary reports whether this generator emits a pure-Go library skeleton.
func (g *ProjectGenerator) isLibrary() bool {
	return g.effectiveKind() == config.ProjectKindLibrary
}

// hasCmd reports whether the project should produce any cmd/ directory.
// Libraries don't; services and CLIs do.
func (g *ProjectGenerator) hasCmd() bool { return !g.isLibrary() }

// binaryName returns the main binary name. For CLI projects this is
// the project name; for service projects it's still the project name
// (cmd/main.go currently lives at the project root).
func (g *ProjectGenerator) binaryName() string {
	if g.ServiceName != "" && g.isService() {
		// services keep the historical layout (single cmd/ regardless of name)
		return g.Name
	}
	return g.Name
}

// NewProjectGenerator creates a new project generator
func NewProjectGenerator(name, path, modulePath string) *ProjectGenerator {
	return &ProjectGenerator{
		Name:         name,
		Path:         path,
		ModulePath:   modulePath,
		ServicePort:  8080,
		FrontendPort: 3000,
	}
}

// Generate creates the project structure.
func (g *ProjectGenerator) Generate() error {
	// CLI/library kinds force-disable a number of features so the rest of
	// the generator (which uses Features.*Enabled() to gate server-shaped
	// emission) does not have to learn about Kind. This keeps the gate in
	// one place and means existing feature checks still work unchanged.
	g.applyKindFeatureDefaults()

	// Create project directory
	if err := os.MkdirAll(g.Path, 0755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}

	// Create directory structure
	dirs := []string{
		"gen",
		// internal/ is intentionally not pre-created. `forge package new <name>`
		// materializes internal/<name>/ on demand; shipping an empty directory
		// would just leave a dangling .gitkeep or an untracked empty dir.
	}
	if g.isService() {
		dirs = append(dirs, "internal/handlers", "internal/handlers/mocks", "pkg/app", "pkg/middleware")
		// The command tree lives under cmd/<bin>/cmd, dir-nested by category
		// (devspace idiom). Pre-create the group subpackage dirs so main.go's
		// blank imports resolve even before any service/worker/operator exists.
		bin := g.binaryName()
		dirs = append(dirs,
			filepath.Join("cmd", bin, "cmd"),
			filepath.Join("cmd", bin, "cmd", "services"),
			filepath.Join("cmd", bin, "cmd", "workers"),
			filepath.Join("cmd", bin, "cmd", "operators"),
		)
	}
	if g.hasCmd() {
		dirs = append(dirs, "cmd")
	}

	if g.Features.MigrationsEnabled() || g.Features.ORMEnabled() {
		dirs = append(dirs, "db", "db/migrations")
	}
	// Service-kind scaffolds always get a deploy/kcl directory so the
	// user has a complete starting point. The deploy feature itself
	// derives from kind (deploy ⇔ service), so the generate pipeline's
	// deploy steps run against this tree by default; an explicit
	// `features.deploy: false` turns them (and `forge deploy`) off.
	if g.isService() {
		dirs = append(dirs, "deploy/kcl")
	}
	if g.Features.CodegenEnabled() {
		dirs = append(dirs,
			"proto",
			"proto/api",
			"proto/services",
			"proto/config/v1",
			"proto/forge",
			"proto/forge/v1",
		)
	} else if g.isCLI() {
		// CLI projects keep proto/forge + proto/config so users can use
		// forge annotations on data types and config protos if they want.
		dirs = append(dirs,
			"proto",
			"proto/config/v1",
			"proto/forge",
			"proto/forge/v1",
		)
	}

	// Add service directory if a service is specified. Use the Go-package
	// form so directories match `package <name>` declarations in generated
	// Go code (hyphens in CLI names become underscores on disk).
	if g.ServiceName != "" {
		svcPkg := naming.ServicePackage(g.ServiceName)
		dirs = append(dirs, fmt.Sprintf("internal/handlers/%s", svcPkg))
		dirs = append(dirs, fmt.Sprintf("proto/services/%s/v1", svcPkg))
	}

	// Add frontend directory if specified
	if g.FrontendName != "" {
		dirs = append(dirs, fmt.Sprintf("frontends/%s", g.FrontendName))
	}

	for _, dir := range dirs {
		path := filepath.Join(g.Path, dir)
		if err := os.MkdirAll(path, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	if g.Features.MigrationsEnabled() || g.Features.ORMEnabled() {
		// Create .gitkeep in db/migrations so the directory is tracked by git
		if err := os.WriteFile(filepath.Join(g.Path, "db", "migrations", ".gitkeep"), []byte{}, 0644); err != nil {
			return fmt.Errorf("failed to create db/migrations/.gitkeep: %w", err)
		}

		// Scaffold a README.md in db/ so the migrations workflow is self-documenting.
		dbReadme := "# db\n\nSQL migrations managed by [golang-migrate](https://github.com/golang-migrate/migrate).\n\n" +
			"## Layout\n\n" +
			"Place numbered migration pairs in `db/migrations/`:\n\n" +
			"```\ndb/migrations/\n  0001_init.up.sql\n  0001_init.down.sql\n  0002_add_users.up.sql\n  0002_add_users.down.sql\n```\n\n" +
			"## CLI\n\n" +
			"The generated binary exposes `db migrate` subcommands:\n\n" +
			"```\ngo run ./cmd db migrate up      # apply all pending migrations\ngo run ./cmd db migrate down    # revert the most recently applied migration\ngo run ./cmd db migrate status  # print current version / dirty flag\n```\n\n" +
			"All subcommands read `DATABASE_URL` (or `--database-url`) from the standard project config.\n"
		if err := os.WriteFile(filepath.Join(g.Path, "db", "README.md"), []byte(dbReadme), 0644); err != nil {
			return fmt.Errorf("failed to create db/README.md: %w", err)
		}
	}

	if g.Features.CodegenEnabled() {
		// proto/api is a reserved scaffold directory used by 'forge
		// generate' for cross-service message definitions. Populate it
		// with a README so the directory is tracked by git and users
		// understand what belongs there. (Entity protos are retired:
		// entities are projections of db/migrations.)
		protoDirReadmes := map[string]string{
			filepath.Join(g.Path, "proto", "api", "README.md"): "# proto/api\n\nShared API message definitions (e.g. common request/response types)\nreferenced by multiple services. Files placed here are compiled into\n`gen/api/` by `forge generate`.\n",
		}
		for p, body := range protoDirReadmes {
			if err := os.WriteFile(p, []byte(body), 0644); err != nil {
				return fmt.Errorf("failed to create %s: %w", p, err)
			}
		}
	}

	// All per-field derivations (protoName, servicePackage, the goVersion
	// family, the forge/pkg dep + its LocalForgePkgVendored gate, the
	// migrations-off ConfigFields pruning) live in ForScaffold so the
	// scaffold and upgrade lanes share one named render type. See
	// project_template_data.go.
	templateData := g.ForScaffold()

	if g.Features.CodegenEnabled() {
		if err := g.copyForgeV1Proto(); err != nil {
			return err
		}
		if g.ServiceName != "" {
			if err := g.createExampleProto(templateData); err != nil {
				return err
			}
		}
		if err := g.createConfigProto(templateData); err != nil {
			return err
		}
	} else if g.isCLI() {
		// CLI projects still copy the forge.v1 annotation proto so users
		// who add data-type protos later get the same annotation surface.
		if err := g.copyForgeV1Proto(); err != nil {
			return err
		}
	}

	// go.mod and Taskfile.yml templates differ by kind — service projects
	// pull in the full Connect/OTel/migrate stack and have many task verbs;
	// CLI projects only need cobra and a small task set; libraries are even
	// leaner. `go mod tidy` adds anything else the project actually imports.
	goModTmpl := "go.mod.tmpl"
	taskfileTmpl := "Taskfile.yml.tmpl"
	launchTmpl := "vscode-launch.json.tmpl"
	readmeTmpl := "README.md.tmpl"
	switch {
	case g.isCLI():
		goModTmpl = "go.mod.cli.tmpl"
		taskfileTmpl = "Taskfile.cli.yml.tmpl"
		launchTmpl = "vscode-launch.cli.json.tmpl"
		readmeTmpl = "README.cli.md.tmpl"
	case g.isLibrary():
		goModTmpl = "go.mod.library.tmpl"
		taskfileTmpl = "Taskfile.library.yml.tmpl"
		launchTmpl = "vscode-launch.library.json.tmpl"
		readmeTmpl = "README.library.md.tmpl"
	}

	files := []struct {
		template string
		dest     string
	}{
		{taskfileTmpl, "Taskfile.yml"},
		{".gitignore", ".gitignore"},
		{readmeTmpl, "README.md"},
		{"CONTRIBUTING.md.tmpl", "CONTRIBUTING.md"},
		{"CHANGELOG.md.tmpl", "CHANGELOG.md"},
		{goModTmpl, "go.mod"},
		{launchTmpl, ".vscode/launch.json"},
	}

	// go.work + gen/go.mod only matter when there is a gen/ subtree
	// (i.e. the codegen pipeline produces stubs). Library/CLI projects
	// without codegen don't need a workspace.
	//
	// tools/tools.go pins protoc-gen-go and protoc-gen-connect-go in
	// go.mod via blank imports under //go:build tools, so contributors
	// can `go install` them without remembering the module paths. The
	// runtime `local:` plugins in buf.gen.yaml resolve them via $PATH.
	if g.Features.CodegenEnabled() {
		files = append(files,
			struct{ template, dest string }{"go.work.tmpl", "go.work"},
			struct{ template, dest string }{"gen-go.mod.tmpl", "gen/go.mod"},
			struct{ template, dest string }{"buf.yaml.tmpl", "buf.yaml"},
			struct{ template, dest string }{"buf.gen.yaml", "buf.gen.yaml"},
			struct{ template, dest string }{"tools.go.tmpl", "tools/tools.go"},
		)
	} else if g.isCLI() {
		// CLI projects: keep buf.yaml/buf.gen.yaml and a minimal go.work
		// so that adding entity protos later "just works".
		files = append(files,
			struct{ template, dest string }{"go.work.tmpl", "go.work"},
			struct{ template, dest string }{"gen-go.mod.tmpl", "gen/go.mod"},
			struct{ template, dest string }{"buf.yaml.tmpl", "buf.yaml"},
			struct{ template, dest string }{"buf.gen.yaml", "buf.gen.yaml"},
			struct{ template, dest string }{"tools.go.tmpl", "tools/tools.go"},
		)
	}

	switch {
	case g.isService():
		// The command tree lives under cmd/<bin>/cmd as a real cobra package,
		// dir-nested by category (devspace idiom). cmd/<bin>/main.go is a thin
		// cmd.Execute() that blank-imports the group subpackages so their
		// commands self-register. binary=shared only changes the DEPLOY story
		// (one image serving many services via cobra subcommands); the command
		// tree itself is identical in both modes, so both use the same thin
		// main (the cmd-main.go.tmpl doc text already covers the shared story).
		bin := g.binaryName()
		cmdDir := filepath.Join("cmd", bin)
		treeDir := filepath.Join(cmdDir, "cmd")
		files = append(files,
			struct{ template, dest string }{"cmd-main.go.tmpl", filepath.Join(cmdDir, "main.go")},
			struct{ template, dest string }{"cmd-tree-root.go.tmpl", filepath.Join(treeDir, "root.go")},
			struct{ template, dest string }{"cmd-tree-version.go.tmpl", filepath.Join(treeDir, "version.go")},
		)
		// cmd/<bin>/cmd/commands.go — the user-owned cobra extension point
		// newRootCmd consumes (userCommands(deps)). Scaffolded once here;
		// the generate pipeline re-ensures it for older projects but never
		// overwrites an existing copy.
		files = append(files, struct{ template, dest string }{"cmd-tree-commands.go.tmpl", filepath.Join(treeDir, "commands.go")})
	case g.isCLI():
		// CLI binaries get their own root.go + version.go under
		// cmd/<binary>/ so multi-binary projects extend cleanly later.
		bin := g.binaryName()
		files = append(files,
			struct{ template, dest string }{"cmd-cli-main.go.tmpl", filepath.Join("cmd", bin, "main.go")},
			struct{ template, dest string }{"cmd-cli-version.go.tmpl", filepath.Join("cmd", bin, "version.go")},
		)
	}

	// cmd/<bin>/cmd/{serve,server,db}.go import pkg/config and pkg/app
	// which are only generated by the codegen pipeline. They are
	// service-shaped, so CLI/library kinds never emit them.
	//
	// OTel is owned by serverkit now (it calls observe.Setup internally from
	// the projected serverkit.Config OTLPEndpoint + ServiceName); there is
	// no generated cmd/otel.go shim.
	if g.isService() && g.Features.CodegenEnabled() {
		bin := g.binaryName()
		treeDir := filepath.Join("cmd", bin, "cmd")
		files = append(files,
			struct{ template, dest string }{"cmd-tree-serve.go.tmpl", filepath.Join(treeDir, "serve.go")},
			struct{ template, dest string }{"cmd-tree-server.go.tmpl", filepath.Join(treeDir, "server.go")},
		)
		// cmd/<bin>/cmd/db.go (migrate CLI) depends on both pkg/config and
		// golang-migrate; skip when migrations are also disabled.
		if g.Features.MigrationsEnabled() {
			files = append(files, struct{ template, dest string }{"cmd-tree-db.go.tmpl", filepath.Join(treeDir, "db.go")})
		}
	}

	// Service-kind scaffolds always get Dockerfile / .dockerignore — see
	// the note on the `deploy/kcl` dirs block above. The runtime gate
	// lives on the `forge deploy` command itself.
	if g.isService() {
		files = append(files, struct{ template, dest string }{".dockerignore", ".dockerignore"})
		files = append(files, struct{ template, dest string }{"Dockerfile.tmpl", "Dockerfile"})
	}
	if g.isService() && g.Features.HotReloadEnabled() {
		files = append(files, struct{ template, dest string }{"air.toml.tmpl", ".air.toml"})
		files = append(files, struct{ template, dest string }{"air-debug.toml.tmpl", ".air-debug.toml"})
	}

	for _, file := range files {
		destPath := filepath.Join(g.Path, file.dest)
		if err := assets.WriteTemplateWithData(file.template, destPath, templateData); err != nil {
			return fmt.Errorf("failed to create %s: %w", file.dest, err)
		}
	}

	// cmd/commands.go — the user-owned cobra extension point the
	// generated cmd/main.go consumes (userCommands()). Scaffolded once so
	// the initial build compiles. The REAL per-service subcommands
	// (cmd/services_gen.go — one first-class cobra command per service,
	// each delegating to runServer with its own name pre-selected over the
	// data-only internal/app Inventory) are emitted by the generate
	// pipeline's internal/app composition step, which the service-kind
	// scaffold runs immediately via bootstrapGeneratedCode.
	if g.isService() && g.Features.CodegenEnabled() {
		if err := codegen.GenerateCmdCommands(g.Path, g.binaryName()); err != nil {
			return fmt.Errorf("failed to scaffold cmd/<bin>/cmd/commands.go: %w", err)
		}
		// Emit the command-group anchors (services/workers/operators
		// register_gen.go) with ZERO items so the group subpackages exist
		// from the first scaffold — cmd/<bin>/main.go blank-imports them, so
		// an empty (Go-file-less) group dir would make `go mod tidy` 404 the
		// local import. The post-scaffold composition step re-emits these
		// alongside the per-item files once proto is compiled.
		if err := codegen.GenerateCmdGroups(codegen.CmdServiceGroupInput{Bin: g.binaryName()}, g.Path, nil); err != nil {
			return fmt.Errorf("failed to scaffold cmd/<bin>/cmd group anchors: %w", err)
		}
	}

	if g.isService() {
		if err := g.generatePkgMiddleware(); err != nil {
			return fmt.Errorf("failed to generate pkg/middleware: %w", err)
		}
	}

	// recordFrozenChecksums was historically called here, but several
	// Tier-2 frozen files (e.g. .golangci.yml via generateGolangciLint)
	// are written later in Generate(). Recording at this earlier point
	// silently skipped them via os.IsNotExist, leaving them with no
	// recorded checksum — which made `forge upgrade` flag them as
	// user-modified on a fresh scaffold. The call is now at the end of
	// Generate so every managed file exists when its checksum is taken.

	if g.isService() && g.Features.CodegenEnabled() {
		// pkg/app is now a thin substrate: the LIVE runtime DI composition
		// lives in internal/app (OpenInfra → NewComponents, emitted by the
		// post-scaffold `forge generate`). At scaffold time we only emit the
		// per-component test harness (testing.go) + the CONVENTIONS explainer;
		// migrate.go is emitted below when migrations are enabled.
		if err := g.generateBootstrapTesting(); err != nil {
			return fmt.Errorf("failed to generate pkg/app/testing.go: %w", err)
		}
		// Emit pkg/app/CONVENTIONS.md once at scaffold so per-service
		// service.go files can point at a single canonical explainer for
		// the OpenInfra → NewComponents composition + validateDeps story
		// instead of each shipping a block comment that drifts.
		if err := g.generatePkgAppConventions(); err != nil {
			return fmt.Errorf("failed to generate pkg/app/CONVENTIONS.md: %w", err)
		}
	}
	if g.isService() && g.Features.MigrationsEnabled() {
		// Generate migrate.go stub (no migrations embedded at project creation)
		if err := codegen.GenerateMigrate(g.Path, g.ModulePath, false, nil); err != nil {
			return fmt.Errorf("failed to generate pkg/app/migrate.go: %w", err)
		}
	}

	// Write forge.yaml
	if err := g.writeProjectConfig(); err != nil {
		return fmt.Errorf("failed to write project config: %w", err)
	}

	if g.isService() {
		// Generate KCL deploy files. Always emitted for service-kind so
		// the scaffold ships a complete project shape — the runtime
		// gate lives on `forge deploy` itself (features.deploy, derived
		// on for service kind).
		if err := g.generateKCLDeploy(); err != nil {
			return fmt.Errorf("failed to generate KCL deploy files: %w", err)
		}

		// Generate dev config (k3d.yaml)
		if err := g.generateDevConfig(); err != nil {
			return fmt.Errorf("failed to generate dev config: %w", err)
		}

		// Generate docker-compose.yml
		if err := g.generateDockerCompose(); err != nil {
			return fmt.Errorf("failed to generate docker-compose.yml: %w", err)
		}
	}

	if g.isService() && g.Features.ObservabilityEnabled() {
		if err := g.generateAlloyConfig(); err != nil {
			return fmt.Errorf("failed to generate alloy config: %w", err)
		}
	}

	// Generate .env.example with common environment variables (services only)
	if g.isService() {
		if err := g.generateEnvExample(); err != nil {
			return fmt.Errorf("failed to generate .env.example: %w", err)
		}
	}

	if err := g.generateGolangciLint(); err != nil {
		return fmt.Errorf("failed to generate .golangci.yml: %w", err)
	}
	if g.isService() && g.Features.CodegenEnabled() && g.ServiceName != "" {
		if err := g.generateServiceFiles(); err != nil {
			return fmt.Errorf("failed to generate service files: %w", err)
		}
	}

	// Generate frontend files if specified (services only — frontends are
	// not meaningful for CLI/library projects).
	if g.isService() && g.Features.FrontendEnabled() && g.FrontendName != "" {
		if err := g.generateFrontendFiles(); err != nil {
			return fmt.Errorf("failed to generate frontend files: %w", err)
		}
	}

	// Generate CI/CD workflow files. CLI projects get a kind-aware CI
	// (no docker/deploy/proto-breaking jobs); libraries inherit the same.
	if g.Features.CIEnabled() {
		if err := g.generateCIFiles(); err != nil {
			return fmt.Errorf("failed to generate CI files: %w", err)
		}
	}

	// Generate E2E test harness (server-shaped — services only)
	if g.isService() && g.Features.CodegenEnabled() && g.ServiceName != "" {
		if err := g.generateE2ETests(); err != nil {
			return fmt.Errorf("failed to generate E2E tests: %w", err)
		}
	}

	// Scaffold examples/ placeholder so the convention is discoverable.
	// Libraries don't need a separate examples/ tree — Go convention puts
	// runnable examples in `*_test.go` Example funcs alongside the code.
	if !g.isLibrary() {
		if err := g.generateExamplesReadme(); err != nil {
			return fmt.Errorf("failed to generate examples/README.md: %w", err)
		}
	}

	// Library projects need at least one Go file so `go build ./...`
	// doesn't bail out with "matched no packages". A doc.go in the
	// project's pkg/ skeleton is the convention. The package name is
	// derived from the project name (hyphens → underscores) so it's a
	// valid Go identifier. Tests that exercise the library should be
	// added by the user.
	if g.isLibrary() {
		if err := g.generateLibrarySkeleton(); err != nil {
			return fmt.Errorf("failed to generate library skeleton: %w", err)
		}
	}

	// CLI projects ship a minimal pkg/config so that internal/<pkg>/
	// templates (which import {{.Module}}/pkg/config) compile even
	// before the user adds a real config proto. Service projects get
	// pkg/config from the codegen pipeline; CLI projects don't run
	// the pipeline, so we materialize the stub up front.
	if g.isCLI() {
		if err := g.generateCLIConfigStub(); err != nil {
			return fmt.Errorf("failed to generate pkg/config stub: %w", err)
		}
	}

	// Developer experience + ops scaffolding: .vscode/, .devcontainer/,
	// scripts/bootstrap.sh, SECURITY.md, .pre-commit-config.yaml,
	// example migration + seeds, docs/adr/, benchmarks/. Kept behind a
	// single helper so the entry point in Generate stays readable.
	if err := g.generateDXFiles(); err != nil {
		return fmt.Errorf("failed to generate DX scaffolding: %w", err)
	}

	// Write project metadata to .reliant directory (both modes)
	if err := g.writeProjectMetadata(); err != nil {
		return fmt.Errorf("failed to write project metadata: %w", err)
	}

	// Record checksums for frozen (Tier-2) files now that every managed
	// file has been written. `forge upgrade` uses these checksums to
	// distinguish stale codegen from user edits.
	if err := g.recordFrozenChecksums(); err != nil {
		return fmt.Errorf("failed to record frozen file checksums: %w", err)
	}

	return nil
}

// generateCLIConfigStub writes a minimal pkg/config/config.go for CLI
// projects. The internal-package templates import {{.Module}}/pkg/config
// — for service projects that's filled in by the codegen pipeline, but
// CLI projects don't run codegen so we ship a Config{} stub up front.
// Users grow it by editing this file (or adding a proto/config/v1
// entry and running `forge generate`).
func (g *ProjectGenerator) generateCLIConfigStub() error {
	pkgDir := filepath.Join(g.Path, "pkg", "config")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return fmt.Errorf("create pkg/config: %w", err)
	}
	body := `// Package config holds runtime configuration for the CLI.
//
// This file is a stub. Add fields here and load them from environment
// variables, command-line flags, or a config file as your CLI grows.
// If you add proto/config/v1/*.proto and run ` + "`forge generate`" + `,
// this file will be regenerated automatically.
package config

// Config is the runtime configuration value passed to internal packages.
// Extend this struct (or replace it with a proto-driven generator) as
// your CLI grows.
type Config struct{}
`
	return os.WriteFile(filepath.Join(pkgDir, "config.go"), []byte(body), 0o644)
}

// generateLibrarySkeleton writes a doc.go under pkg/<libname>/ so that
// `go build ./...` succeeds on a freshly scaffolded library project.
// The directory layout matches the standard Go library convention: the
// importable code lives in pkg/<libname>/, and contributors can grow it
// from there. We intentionally don't ship an exported function — the
// project's first feature should drive that.
func (g *ProjectGenerator) generateLibrarySkeleton() error {
	pkgName := strings.ReplaceAll(g.Name, "-", "_")
	pkgDir := filepath.Join(g.Path, "pkg", pkgName)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return fmt.Errorf("create pkg/%s: %w", pkgName, err)
	}
	doc := fmt.Sprintf(`// Package %s is the public API surface for the %s library.
//
// Add exported types and functions here. Tests for any code in this
// package live alongside it (xxx_test.go).
package %s
`, pkgName, g.Name, pkgName)
	return os.WriteFile(filepath.Join(pkgDir, "doc.go"), []byte(doc), 0o644)
}

// ApplyKindFeatureDefaults is the public entry point invoked by
// `forge new` after parsing the --kind flag. It delegates to the
// private applyKindFeatureDefaults helper so the scaffold-time
// matrix lives in one place. Exposed publicly so other callers
// (tests, sibling tools) can derive the same defaults from a kind
// string without duplicating the per-feature decisions.
func (g *ProjectGenerator) ApplyKindFeatureDefaults(kind string) {
	// Sync `Kind` so the private helper's isService/isLibrary
	// predicates resolve correctly. Callers that already set
	// Kind on the struct (the normal path from new.go) pass the
	// same value; resetting is idempotent.
	g.Kind = kind
	g.applyKindFeatureDefaults()
}

// applyKindFeatureDefaults force-disables features that don't make sense
// for CLI/library kinds (so generators that gate on Features still do the
// right thing without learning about Kind). Explicit user overrides via
// `--disable` already set fields to false; this helper additionally
// disables service-shaped features for non-service kinds.
//
// Note: these are scaffold-time defaults. The generated forge.yaml records
// the resulting Features state so subsequent forge runs see the same flags.
func (g *ProjectGenerator) applyKindFeatureDefaults() {
	if g.isService() {
		return
	}
	off := func() *bool { b := false; return &b }
	// CLI and library: no protobuf/RPC codegen, no service migrations,
	// no deploy artefacts, no observability infra, no frontend, no
	// hot-reload (no long-running server to reload).
	if g.Features.Codegen == nil {
		g.Features.Codegen = off()
	}
	if g.Features.ORM == nil {
		g.Features.ORM = off()
	}
	if g.Features.Migrations == nil {
		g.Features.Migrations = off()
	}
	if g.Features.Observability == nil {
		g.Features.Observability = off()
	}
	if g.Features.Frontend == nil {
		g.Features.Frontend = off()
	}
	if g.Features.HotReload == nil {
		g.Features.HotReload = off()
	}
	// Packs target server-shaped projects. CLI and library kinds get
	// no useful work out of packs: they install auth middleware, audit
	// interceptors, payment integrations (all service-shape). Disabling
	// at scaffold time matches the per-kind --kind matrix in the prompt.
	if g.Features.Packs == nil {
		g.Features.Packs = off()
	}
	// Deploy derives from kind (deploy ⇔ service) at load time, but
	// generators consult g.Features before any forge.yaml exists, so
	// record the explicit false here like the other service-shaped
	// features. NormalizeForWrite drops it again (matches derivation).
	if g.Features.Deploy == nil {
		g.Features.Deploy = off()
	}
	// Ingress is experimental (default-off for every kind), so no
	// per-kind override is needed — see ExperimentalConfig.
	// Library: every server-shaped feature is off. CI/Build are
	// off because there's no binary to lint/test/build — the user
	// can re-enable manually if they want a lint+test workflow
	// against the package, but the historic forge convention is
	// to leave the .github/workflows/ tree absent on a library
	// scaffold (TestProjectGeneratorKindLibraryScaffold asserts
	// this). Docs stays on — godoc-style API reference is the
	// headline output of a library project.
	if g.isLibrary() {
		if g.Features.CI == nil {
			g.Features.CI = off()
		}
		if g.Features.Build == nil {
			g.Features.Build = off()
		}
	}
}

func (g *ProjectGenerator) copyForgeV1Proto() error {
	v1Dir := filepath.Join(g.Path, "proto", "forge", "v1")
	return assets.WriteForgeV1Proto(v1Dir)
}

func (g *ProjectGenerator) createConfigProto(data interface{}) error {
	destPath := filepath.Join(g.Path, "proto", "config", "v1", "config.proto")
	return assets.WriteTemplateWithData("config.proto.tmpl", destPath, data)
}

func (g *ProjectGenerator) createExampleProto(data interface{}) error {
	svcName := g.ServiceName
	if svcName == "" {
		svcName = g.Name
	}
	// Proto package segments require [a-z][a-z0-9_]*, so use the Go-package form.
	svcPkg := naming.ServicePackage(svcName)
	destPath := filepath.Join(g.Path, "proto", "services", svcPkg, "v1", fmt.Sprintf("%s.proto", svcPkg))
	return assets.WriteExampleProto(svcName, destPath, data)
}

func (g *ProjectGenerator) generateServiceFiles() error {
	return GenerateServiceFiles(g.Path, g.ModulePath, g.ServiceName, g.Name, g.ServicePort)
}

// generatePkgAppConventions writes pkg/app/CONVENTIONS.md, the canonical
// explainer for the wire_gen / Setup wiring shape + the no-per-RPC-nil-check rule. Per-
// service service.go files trim their inline comment to a one-line pointer
// at this file. The template carries no data so we read it raw.
func (g *ProjectGenerator) generatePkgAppConventions() error {
	content, err := templates.ProjectTemplates().Get("pkg-app-CONVENTIONS.md.tmpl")
	if err != nil {
		return fmt.Errorf("read pkg-app-CONVENTIONS.md.tmpl: %w", err)
	}
	appDir := filepath.Join(g.Path, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return fmt.Errorf("create pkg/app dir: %w", err)
	}
	return os.WriteFile(filepath.Join(appDir, "CONVENTIONS.md"), content, 0o644)
}

func (g *ProjectGenerator) generateFrontendFiles() error {
	if g.FrontendName == "" {
		return nil
	}
	// Emit the workspace-layout scaffolding (pnpm-workspace.yaml +
	// packages/api + packages/hooks) once, before any per-frontend
	// files are written. WriteFrontendWorkspaceFiles is idempotent and
	// no-op'd when FrontendWorkspaces is false, so it's safe to call
	// unconditionally.
	if err := WriteFrontendWorkspaceFiles(g.Path, g.Name, g.FrontendWorkspaces); err != nil {
		return fmt.Errorf("write frontend workspace files: %w", err)
	}
	// `forge new` doesn't currently support scaffolding an RN frontend
	// as the initial one (the FrontendName path always uses Next.js —
	// kind="" → frontendTemplateDir returns "nextjs"). So WriteUINativePackageFiles
	// isn't reachable here in practice; users add the RN frontend via
	// `forge add frontend --kind mobile` which already wires it up.
	// If the initial-RN-frontend path ever lands, gate the call here
	// the same way add.go does.
	return GenerateFrontendFilesWithOptions(g.Path, g.ModulePath, g.Name, g.FrontendName, g.ServicePort, "", FrontendGenOptions{
		Workspaces: g.FrontendWorkspaces,
	})
}

// generateE2ETests generates the E2E test harness for the initial service.
func (g *ProjectGenerator) generateE2ETests() error {
	methods := MethodsFromProtoStub(g.ServiceName)
	return GenerateE2ETests(g.Path, g.ServiceName, g.ModulePath, g.Name, methods)
}
