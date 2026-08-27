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
	ServicePort        int      // scaffold artifact port (devcontainer/vscode); the dev backend's real bind port is allocated at runtime — see resolveEphemeralHostPorts
	FrontendName       string   // optional initial Next.js frontend name
	FrontendPort       int      // frontend port; 0 = ephemeral (forge run/up allocates a free port at launch and reports it)
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

// binaryName returns the PRIMARY binary name — the leaf of the cmd/<bin>/
// tree, the Dockerfile `go build ... ./cmd/<bin>` target, and the built
// binary. It is the project name VERBATIM (hyphens preserved): a project
// named "peptide-platform" scaffolds cmd/peptide-platform/. `go build
// ./cmd/peptide-platform` is a valid target (a directory path, not a Go
// package identifier — the package clause inside is `package main`
// regardless), and the primary dir mirroring the project/module name is the
// established convention every existing project + generated Dockerfile
// already uses. Secondary binaries (`forge scaffold binary`) DO sanitize, because
// they are named after a Go component, not the project. The deploy-side
// build declaration must therefore also target the raw primary path — see
// codegen.WorkloadStanza, which writes it into deploy/kcl/workloads.k.
func (g *ProjectGenerator) binaryName() string {
	return g.Name
}

// NewProjectGenerator creates a new project generator
func NewProjectGenerator(name, path, modulePath string) *ProjectGenerator {
	return &ProjectGenerator{
		Name:       name,
		Path:       path,
		ModulePath: modulePath,
		// ServicePort feeds a few scaffold artifacts (devcontainer forwards,
		// the vscode launch PORT), but the backend's ACTUAL dev bind port is
		// NOT this value: forge strips per-service ports from forge.yaml
		// (components ship `ports: []`) and the dev backend binds the
		// architectural default (config_schema.k `port: int = 8080` /
		// defaultDevAPIPort). De-colliding the dev backend is therefore a
		// RUNTIME concern — `forge run` / `forge env up` allocate a
		// free port per project (see resolveEphemeralHostPorts in run.go) —
		// not a scaffold constant.
		ServicePort: 8080,
		// Frontend: 0 = ephemeral. The scaffolded forge.yaml omits the
		// frontend port (FrontendConfig.Port is omitempty); `forge run` /
		// `forge env up` allocate a free OS port at launch and print it in
		// the summary. Teaches the "discover the dev port from forge's output,
		// don't hardcode 3000" pattern and removes the frontend port-collision
		// that made two dev stacks fight on one host.
		FrontendPort: 0,
	}
}

// Generate creates the project structure.
func (g *ProjectGenerator) Generate() error { //nolint:gocognit,funlen // the scaffold pipeline: one gated step per emitted artifact family. The gates are flat and independent; the order is the contract.
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
	// `features.deploy: false` turns them (and `forge env deploy`) off.
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
		dirs = append(dirs,
			fmt.Sprintf("internal/handlers/%s", svcPkg),
			fmt.Sprintf("proto/services/%s/v1", svcPkg),
		)
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
	// family, the forge/pkg version pin, the migrations-off ConfigFields
	// pruning) live in forScaffold so the scaffold and upgrade lanes share
	// one named render type. See project_template_data.go.
	templateData := g.forScaffold()

	if g.Features.CodegenEnabled() {
		if err := g.copyForgeV1Proto(); err != nil {
			return err
		}
		// No example RPC surface is shipped: a fresh `--service X` gets an
		// empty service proto stub (written by GenerateServiceFiles, with the
		// (forge.v1.service) option block but zero RPCs) plus the wired
		// handler dir. Author your own messages/RPCs, then `forge generate`.
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
		// dir-nested by category (devspace idiom). cmd/<bin>/main.go is the
		// composition root: it NAMES every group constructor and hands them to
		// cmd.Execute. binary=shared only changes the DEPLOY story (the per-env
		// main.k gives each component its own `/app/<project> <name>` command);
		// the command tree itself is identical in both modes, so both use the
		// same main (cmd-main.go.tmpl covers the shared story).
		bin := g.binaryName()
		cmdDir := filepath.Join("cmd", bin)
		treeDir := filepath.Join(cmdDir, "cmd")
		// NOTE: cmd/<bin>/main.go (the composition root) is NOT rendered here.
		// It names every group constructor explicitly, so it is inventory-
		// dependent and written by the codegen pipeline's cmd-group step
		// (codegen.GenerateCmdGroups), exactly like the per-component group
		// files — not from the project-level scaffold data, which has no
		// service list yet.
		// cmd/<bin>/cmd/commands.go — the user-owned cobra extension point
		// newRootCmd consumes (userCommands(deps)). Scaffolded once here;
		// the generate pipeline re-ensures it for older projects but never
		// overwrites an existing copy.
		files = append(files,
			struct{ template, dest string }{"cmd-tree-root.go.tmpl", filepath.Join(treeDir, "root.go")},
			struct{ template, dest string }{"cmd-tree-version.go.tmpl", filepath.Join(treeDir, "version.go")},
			struct{ template, dest string }{"cmd-tree-commands.go.tmpl", filepath.Join(treeDir, "commands.go")},
		)
	case g.isCLI():
		// CLI binaries get their own root.go + version.go under
		// cmd/<binary>/ so multi-binary projects extend cleanly later.
		bin := g.binaryName()
		files = append(files,
			struct{ template, dest string }{"cmd-cli-main.go.tmpl", filepath.Join("cmd", bin, "main.go")},
			struct{ template, dest string }{"cmd-cli-version.go.tmpl", filepath.Join("cmd", bin, "version.go")},
		)
	}

	// cmd/<bin>/cmd/{server,db}.go import pkg/config and pkg/app
	// which are only generated by the codegen pipeline. They are
	// service-shaped, so CLI/library kinds never emit them.
	//
	// cmd/<bin>/cmd/serve.go is NOT written here, for exactly the reason
	// cmd/<bin>/main.go is not (see the bootstrap note in cli/new.go): it
	// is scaffold-once and user-owned, so the FIRST writer wins forever.
	// This lane only has the DEFAULT config field set; the codegen pipeline
	// has the project's real one, parsed from proto/config. Birthing it
	// here would permanently freeze a serve.go whose ConfigFields-gated
	// blocks disagree with the config the project actually has, and
	// nothing would ever correct it. So the codegen lane births it
	// (GenerateCmdServerWithFields), and a failed bootstrap is recoverable
	// by re-running rather than baked in.
	//
	// OTel is owned by serverkit now (it calls observe.Setup internally from
	// the projected serverkit.Config OTLPEndpoint + ServiceName); there is
	// no generated cmd/otel.go shim.
	if g.isService() && g.Features.CodegenEnabled() {
		bin := g.binaryName()
		treeDir := filepath.Join("cmd", bin, "cmd")
		files = append(files,
			struct{ template, dest string }{"cmd-tree-server.go.tmpl", filepath.Join(treeDir, "server.go")},
		)
		// cmd/<bin>/cmd/db.go (migrate CLI) depends on both pkg/config and
		// golang-migrate; skip when migrations are also disabled.
		if g.Features.MigrationsEnabled() {
			files = append(files, struct{ template, dest string }{"cmd-tree-db.go.tmpl", filepath.Join(treeDir, "db.go")})
		}
		// cmd/<bin>/cmd/auth.go (`auth idp-provision`) exists to converge
		// the dev IdP application — an IdP exists to complete a browser
		// sign-in, so a project with no frontend never gets this command
		// at all, the same gate docker-compose.yml's `idp` service uses.
		if g.FrontendName != "" {
			files = append(files, struct{ template, dest string }{"cmd-tree-auth.go.tmpl", filepath.Join(treeDir, "auth.go")})
		}
	}

	// Service-kind scaffolds always get Dockerfile / .dockerignore — see
	// the note on the `deploy/kcl` dirs block above. The runtime gate
	// lives on the `forge env deploy` command itself.
	if g.isService() {
		files = append(files,
			struct{ template, dest string }{".dockerignore", ".dockerignore"},
			struct{ template, dest string }{"Dockerfile.tmpl", "Dockerfile"},
		)
	}
	if g.isService() && g.Features.HotReloadEnabled() {
		files = append(files,
			struct{ template, dest string }{"air.toml.tmpl", ".air.toml"},
			struct{ template, dest string }{"air-debug.toml.tmpl", ".air-debug.toml"},
		)
	}

	for _, file := range files {
		destPath := filepath.Join(g.Path, file.dest)
		if err := assets.WriteTemplateWithData(file.template, destPath, templateData); err != nil {
			return fmt.Errorf("failed to create %s: %w", file.dest, err)
		}
	}

	// cmd/<bin>/main.go — the composition root — is written EXACTLY ONCE, and
	// only where the service list is already known. It NAMES every group
	// constructor (services.New<Svc>Cmd), so a write from here — before proto
	// is compiled — could only ever produce a bare cmd.Execute(), and being
	// write-if-absent OWNED code it would then be the FINAL content: every
	// generated per-service subcommand would stay unreferenced forever.
	//
	// So for a codegen project the write belongs to the pass that has the
	// inventory: GenerateCmdGroups, run by the generate pipeline's cmd-group
	// step, which `forge project new` invokes immediately via
	// bootstrapGeneratedCode (and which any later `forge generate` re-runs, so
	// a project whose bootstrap failed still gets a correct root).
	//
	// features.codegen=false has no such pass and no services to name — the
	// bare root IS the finished answer there, so it is written here.
	// Requires go.mod, written in the file loop above.
	if g.isService() && !g.Features.CodegenEnabled() {
		if err := codegen.GenerateCmdMainRoot(g.Path, g.binaryName(), nil); err != nil {
			return fmt.Errorf("failed to scaffold cmd/<bin>/main.go: %w", err)
		}
	}

	// cmd/<bin>/cmd/commands.go — the user-owned cobra extension point the
	// generated root.go consumes (userCommands()). Scaffolded once here so the
	// root command compiles.
	//
	// The command-group subpackages (cmd/<bin>/cmd/{services,workers,operators})
	// are NOT anchored here. They are written by the same GenerateCmdGroups call
	// that writes the composition root, so main.go and the packages it imports
	// can never appear one without the other. Nothing else imports them, so
	// there is no window in which the tree references an empty group dir.
	if g.isService() && g.Features.CodegenEnabled() {
		if err := codegen.GenerateCmdCommands(g.Path, g.binaryName()); err != nil {
			return fmt.Errorf("failed to scaffold cmd/<bin>/cmd/commands.go: %w", err)
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
	// recorded checksum — which made `forge project upgrade` flag them as
	// user-modified on a fresh scaffold. The call is now at the end of
	// Generate so every managed file exists when its checksum is taken.

	if g.isService() && g.Features.CodegenEnabled() {
		// pkg/app is now a thin substrate: the LIVE runtime DI composition
		// lives in internal/app (OpenInfra → NewComponents, emitted by the
		// post-scaffold `forge generate`). At scaffold time we only emit the
		// per-component test harness (each service's own helpers_gen_test.go)
		// + the CONVENTIONS explainer; migrate.go is emitted below when
		// migrations are enabled.
		if err := g.generateBootstrapTesting(); err != nil {
			return fmt.Errorf("failed to generate service test helpers: %w", err)
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
		// gate lives on `forge env deploy` itself (features.deploy, derived
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
	// file has been written. `forge project upgrade` uses these checksums to
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
// `forge project new` after parsing the --kind flag. It delegates to the
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

func (g *ProjectGenerator) generateServiceFiles() error {
	return GenerateServiceFiles(g.Path, g.ModulePath, g.ServiceName, g.Name)
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
	// `forge project new` doesn't currently support scaffolding an RN frontend
	// as the initial one (the FrontendName path always uses Next.js —
	// kind="" → frontendTemplateDir returns "nextjs"). So WriteUINativePackageFiles
	// isn't reachable here in practice; users add the RN frontend via
	// `forge scaffold frontend --kind mobile` which already wires it up.
	// If the initial-RN-frontend path ever lands, gate the call here
	// the same way add.go does.

	// Declare this frontend's public runtime config BEFORE rendering its
	// files. The annotation in that proto is what activates the typed
	// config module, the KCL schema and the per-env config.js; without it
	// the whole system emits nothing and the templates below fall back to
	// build-time process.env reads. Only meaningful when codegen is on —
	// a project with codegen disabled never runs the generators that
	// would consume the annotation.
	typedConfig := FrontendTypedConfig{}
	if g.Features.CodegenEnabled() {
		if err := WriteFrontendConfigProto(g.Path, g.ModulePath, g.FrontendName, g.ServicePort); err != nil {
			return fmt.Errorf("write frontend config proto: %w", err)
		}
		// Derived from the template just written rather than from the
		// descriptor: `forge generate` has not run yet at scaffold time,
		// so there is nothing to parse. This is what lets the frontend be
		// scaffolded already reading the typed module.
		typedConfig = ScaffoldedFrontendTypedConfig()
	}

	return GenerateFrontendFilesWithOptions(g.Path, g.ModulePath, g.Name, g.FrontendName, g.ServicePort, "", FrontendGenOptions{
		Workspaces:  g.FrontendWorkspaces,
		TypedConfig: typedConfig,
	})
}

// generateE2ETests generates the E2E test harness for the initial service.
func (g *ProjectGenerator) generateE2ETests() error {
	methods := MethodsFromProtoStub(g.ServiceName)
	return GenerateE2ETests(g.Path, g.ServiceName, g.ModulePath, g.Name, methods)
}
