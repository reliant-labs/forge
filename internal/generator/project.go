package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
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
	Kind               string                // project kind: "service" (default), "cli", "library"
	Binary             string                // binary mode: "per-service" (default), "shared" — emit one Go binary with cobra subcommand per service when shared
	ServiceName        string                // initial service name (empty if none specified)
	AdditionalServices []string              // additional service names beyond ServiceName — only consumed by binary=shared scaffolds; per-service mode adds these post-scaffold
	ServicePort        int                   // initial service port (default: 8080)
	FrontendName       string                // optional initial Next.js frontend name
	FrontendPort       int                   // frontend port (default: 3000)
	GoVersionOverride  string                // if set, use this Go version instead of detecting
	Features           config.FeaturesConfig // feature flags for generation
	MemoryFormat       MemoryFormat          // AI memory file format (default: reliant)
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

// allServices returns the full list of service names this generator
// will emit at scaffold time (ServiceName first, then
// AdditionalServices). Used by binary=shared paths that need to enumerate
// every service before the post-scaffold AppendServiceToConfig loop runs.
func (g *ProjectGenerator) allServices() []string {
	if g.ServiceName == "" {
		return nil
	}
	out := make([]string, 0, 1+len(g.AdditionalServices))
	out = append(out, g.ServiceName)
	out = append(out, g.AdditionalServices...)
	return out
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
		dirs = append(dirs, "handlers", "handlers/mocks", "pkg/app", "pkg/middleware")
	}
	if g.hasCmd() {
		dirs = append(dirs, "cmd")
	}

	if g.Features.MigrationsEnabled() || g.Features.ORMEnabled() {
		dirs = append(dirs, "db", "db/migrations")
	}
	if g.Features.DeployEnabled() {
		dirs = append(dirs, "deploy/kcl")
	}
	if g.Features.CodegenEnabled() {
		dirs = append(dirs,
			"proto",
			"proto/api",
			"proto/services",
			"proto/db",
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
		svcPkg := ServicePackageName(g.ServiceName)
		dirs = append(dirs, fmt.Sprintf("handlers/%s", svcPkg))
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
		// proto/api and proto/db are reserved scaffold directories used by
		// 'forge generate': proto/api holds cross-service message definitions
		// and proto/db holds entity definitions consumed by protoc-gen-forge-orm.
		// Populate each with a README so the directory is tracked by git and
		// users understand what belongs there.
		protoDirReadmes := map[string]string{
			filepath.Join(g.Path, "proto", "api", "README.md"): "# proto/api\n\nShared API message definitions (e.g. common request/response types)\nreferenced by multiple services. Files placed here are compiled into\n`gen/api/` by `forge generate`.\n",
			filepath.Join(g.Path, "proto", "db", "README.md"):  "# proto/db\n\nEntity (database model) proto definitions. Files placed here are\nconsumed by `protoc-gen-forge-orm` to generate typed ORM bindings and\nmigrations. See `forge generate`.\n",
		}
		for p, body := range protoDirReadmes {
			if err := os.WriteFile(p, []byte(body), 0644); err != nil {
				return fmt.Errorf("failed to create %s: %w", p, err)
			}
		}
	}

	goVersion := g.resolveGoVersion()

	// Sanitize name for proto files (no hyphens allowed). Use underscores
	// rather than stripping so that "my-cool-app" becomes "my_cool_app"
	// (a valid proto package identifier) instead of "mycoolapp" — which
	// silently loses the word boundaries and breaks grep.
	protoName := strings.ReplaceAll(g.Name, "-", "_")

	// ServicePackage is the Go-package-safe form of ServiceName: hyphens
	// become underscores so the value is valid in `package` declarations
	// and proto package segments. Templates that emit Go/proto identifiers
	// must use ServicePackage; ServiceName is retained for display strings.
	servicePackage := ""
	if g.ServiceName != "" {
		servicePackage = ServicePackageName(g.ServiceName)
	}

	templateData := struct {
		Name                   string
		ProtoName              string
		Module                 string
		ServiceName            string
		ServicePackage         string
		ServicePort            int
		ProjectName            string
		FrontendName           string
		FrontendPort           int
		GoVersion              string
		GoVersionMinor         string
		DockerBuilderGoVersion string
		ConfigFields           map[string]bool
		// LocalForgePkgVendored indicates whether <projectDir>/.forge-pkg/
		// holds a vendored copy of forge/pkg (sibling-checkout dev mode).
		// At scaffold time this is always false; `forge generate` flips it
		// on if the project's go.mod has a host-absolute replace pointing
		// at forge/pkg. The Dockerfile template uses this to gate the
		// COPY .forge-pkg/ ./.forge-pkg/ line.
		LocalForgePkgVendored bool
	}{
		Name:                   g.Name,
		ProtoName:              protoName,
		Module:                 g.ModulePath,
		ServiceName:            g.ServiceName,
		ServicePackage:         servicePackage,
		ServicePort:            g.ServicePort,
		ProjectName:            g.Name,
		FrontendName:           g.FrontendName,
		FrontendPort:           g.FrontendPort,
		GoVersion:              goVersion,
		GoVersionMinor:         goVersionMinor(goVersion),
		DockerBuilderGoVersion: dockerBuilderGoVersion(goVersion),
		ConfigFields:           codegen.DefaultConfigFieldNames(),
		// false by default — only flipped by RegenerateInfraFiles after
		// dev-mode vendoring has run.
		LocalForgePkgVendored: false,
	}

	// Strip migration-related config fields when migrations are disabled.
	// The server template conditionally includes migration code based on
	// ConfigFields["AutoMigrate"], so removing the field here prevents
	// the template from emitting app.AutoMigrate() calls.
	if !g.Features.MigrationsEnabled() {
		delete(templateData.ConfigFields, "AutoMigrate")
		delete(templateData.ConfigFields, "DatabaseUrl")
		delete(templateData.ConfigFields, "MaxOpenConns")
		delete(templateData.ConfigFields, "MaxIdleConns")
		delete(templateData.ConfigFields, "ConnMaxIdleTime")
		delete(templateData.ConfigFields, "ConnMaxLifetime")
	}

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
			struct{ template, dest string }{"buf.yaml", "buf.yaml"},
			struct{ template, dest string }{"buf.gen.yaml", "buf.gen.yaml"},
			struct{ template, dest string }{"tools.go.tmpl", "tools/tools.go"},
		)
	} else if g.isCLI() {
		// CLI projects: keep buf.yaml/buf.gen.yaml and a minimal go.work
		// so that adding entity protos later "just works".
		files = append(files,
			struct{ template, dest string }{"go.work.tmpl", "go.work"},
			struct{ template, dest string }{"gen-go.mod.tmpl", "gen/go.mod"},
			struct{ template, dest string }{"buf.yaml", "buf.yaml"},
			struct{ template, dest string }{"buf.gen.yaml", "buf.gen.yaml"},
			struct{ template, dest string }{"tools.go.tmpl", "tools/tools.go"},
		)
	}

	switch {
	case g.isService():
		// In binary=shared mode the canonical cmd/main.go cobra root is
		// replaced with cmd-shared-main.go.tmpl, which adds doc text
		// referencing per-service subcommands. Functionally identical to
		// cmd-root for top-level routing — the per-service files (emitted
		// below) call rootCmd.AddCommand themselves in their init().
		if g.isBinaryShared() {
			files = append(files,
				struct{ template, dest string }{"cmd-shared-main.go.tmpl", "cmd/main.go"},
				struct{ template, dest string }{"cmd-version.go.tmpl", "cmd/version.go"},
			)
		} else {
			files = append(files,
				struct{ template, dest string }{"cmd-root.go.tmpl", "cmd/main.go"},
				struct{ template, dest string }{"cmd-version.go.tmpl", "cmd/version.go"},
			)
		}
	case g.isCLI():
		// CLI binaries get their own root.go + version.go under
		// cmd/<binary>/ so multi-binary projects extend cleanly later.
		bin := g.binaryName()
		files = append(files,
			struct{ template, dest string }{"cmd-cli-main.go.tmpl", filepath.Join("cmd", bin, "main.go")},
			struct{ template, dest string }{"cmd-cli-version.go.tmpl", filepath.Join("cmd", bin, "version.go")},
		)
	}

	// cmd/server.go, cmd/otel.go, and cmd/db.go import pkg/config and
	// pkg/app which are only generated by the codegen pipeline. They are
	// service-shaped, so CLI/library kinds never emit them.
	if g.isService() && g.Features.CodegenEnabled() {
		// otel.go is always emitted alongside server.go because
		// server.go calls setupOTel(). The observability feature flag
		// controls infra files (alloy, grafana, prometheus rules), not
		// the Go-level tracing wiring.
		files = append(files,
			struct{ template, dest string }{"cmd-server.go.tmpl", "cmd/server.go"},
			struct{ template, dest string }{"otel.go", "cmd/otel.go"},
		)
		// cmd/db.go (migrate CLI) depends on both pkg/config and
		// golang-migrate; skip when migrations are also disabled.
		if g.Features.MigrationsEnabled() {
			files = append(files, struct{ template, dest string }{"cmd-db.go.tmpl", "cmd/db.go"})
		}
	}

	if g.isService() && g.Features.DeployEnabled() {
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

	// In binary=shared mode, emit one cobra subcommand file per service —
	// `cmd/<svc>.go` — so callers can run `./<bin> <svc>` directly. Each
	// per-service file is a thin wrapper that invokes the same `runServer`
	// pipeline (in cmd/server.go) with a single-name service filter, which
	// in shared mode triggers the lazy-construction codepath in
	// app.BootstrapOnly. The canonical `./<bin> server [<svc>...]` form
	// still works.
	if g.isBinaryShared() && g.Features.CodegenEnabled() {
		if err := g.generateSharedSubcommands(); err != nil {
			return fmt.Errorf("failed to generate shared-binary subcommands: %w", err)
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
		if err := g.generateBootstrap(); err != nil {
			return fmt.Errorf("failed to generate pkg/app/bootstrap.go: %w", err)
		}
		// Generate setup.go (user-owned, never overwritten) so bootstrap.go compiles
		// even with zero services.
		// Initial scaffold: no database driver wired and no ORM — the pipeline
		// runs `forge generate` immediately after and rewrites this file with
		// the correct flags once proto/db and forge.yaml are present.
		if err := codegen.GenerateSetup(g.ModulePath, "", false, g.Path); err != nil {
			return fmt.Errorf("failed to generate pkg/app/setup.go: %w", err)
		}
		if err := g.generateBootstrapTesting(); err != nil {
			return fmt.Errorf("failed to generate pkg/app/testing.go: %w", err)
		}
		// Emit pkg/app/CONVENTIONS.md once at scaffold so per-service
		// service.go files can point at a single canonical explainer for
		// the wire_gen / Setup / validateDeps story (post-2026-05-07
		// wire-gen migration), instead of each shipping a 12-line block
		// comment that drifts.
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

	if g.isService() && g.Features.DeployEnabled() {
		// Generate KCL deploy files
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
	if err := g.recordFrozenChecksums(templateData); err != nil {
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
	if g.Features.Deploy == nil {
		g.Features.Deploy = off()
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
	// Library projects also disable CI by default (no binaries to build,
	// no images to push). Users can re-enable manually if they want
	// lint+test workflows.
	if g.isLibrary() && g.Features.CI == nil {
		g.Features.CI = off()
	}
}

func (g *ProjectGenerator) copyForgeV1Proto() error {
	v1Dir := filepath.Join(g.Path, "proto", "forge", "v1")
	return assets.WriteForgeV1Proto(v1Dir, g.ModulePath)
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
	svcPkg := ServicePackageName(svcName)
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

// generateSharedSubcommands writes one cobra subcommand file per service
// to cmd/<svc>.go for binary=shared projects. Each emitted file is a
// thin wrapper that delegates to runServer (in cmd/server.go) with a
// pre-filled single-name service filter; this triggers the lazy
// dep-graph construction codepath in app.BootstrapOnly so unselected
// services contribute zero work to the running process.
//
// File naming follows the existing flat cmd/ layout (no per-binary
// subdirectory) so the Dockerfile / Taskfile / VSCode launch configs
// continue to point at `./cmd` without modification. See the migration
// skill for projects that prefer the cmd/<project>/<svc>.go layout.
func (g *ProjectGenerator) generateSharedSubcommands() error {
	for _, svcName := range g.allServices() {
		pkg := ServicePackageName(svcName)
		data := struct {
			ServiceName    string
			ServicePackage string
		}{
			ServiceName:    svcName,
			ServicePackage: pkg,
		}
		dest := filepath.Join(g.Path, "cmd", pkg+".go")
		if err := assets.WriteTemplateWithData("cmd-shared-service.go.tmpl", dest, data); err != nil {
			return fmt.Errorf("write cmd/%s.go: %w", pkg, err)
		}
	}
	return nil
}

func (g *ProjectGenerator) generateFrontendFiles() error {
	if g.FrontendName == "" {
		return nil
	}
	return GenerateFrontendFiles(g.Path, g.ModulePath, g.Name, g.FrontendName, g.ServicePort, "")
}

// generateE2ETests generates the E2E test harness for the initial service.
func (g *ProjectGenerator) generateE2ETests() error {
	methods := MethodsFromProtoStub(g.ServiceName)
	return GenerateE2ETests(g.Path, g.ServiceName, g.ModulePath, g.Name, methods)
}