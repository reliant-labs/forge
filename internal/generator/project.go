package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/codegen"
)

// forgeCmdRE matches "forge" used as a CLI command — i.e. followed by a space
// and a lowercase subcommand token. This avoids replacing skill paths like
// "forge/run", filenames like "forge.project.yaml", or directory paths.
var forgeCmdRE = regexp.MustCompile(`\bforge( )`)

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
	Name              string
	Path              string
	ModulePath        string
	ServiceName       string // initial service name (empty if none specified)
	ServicePort       int    // initial service port (default: 8080)
	FrontendName      string // optional initial Next.js frontend name
	FrontendPort      int    // frontend port (default: 3000)
	GoVersionOverride string // if set, use this Go version instead of detecting
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

	// Create project directory
	if err := os.MkdirAll(g.Path, 0755); err != nil {
		return fmt.Errorf("failed to create project directory: %w", err)
	}

	// Create directory structure
	dirs := []string{
		"db",
		"db/migrations",
		"deploy/kcl",
		"proto",
		"proto/api",
		"proto/services",
		"proto/db",
		"proto/config/v1",
		"proto/forge",
		"proto/forge/options/v1",
		"handlers",
		"handlers/mocks",
		"gen",
		"cmd",
		"pkg/app",
		"pkg/middleware",
		// internal/ is intentionally not pre-created. `forge package new <name>`
		// materializes internal/<name>/ on demand; shipping an empty directory
		// would just leave a dangling .gitkeep or an untracked empty dir.
	}

	// Add service directory if a service is specified
	if g.ServiceName != "" {
		dirs = append(dirs, fmt.Sprintf("handlers/%s", g.ServiceName))
		dirs = append(dirs, fmt.Sprintf("proto/services/%s/v1", g.ServiceName))
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

	goVersion := g.resolveGoVersion()

	// Sanitize name for proto files (no hyphens allowed). Use underscores
	// rather than stripping so that "my-cool-app" becomes "my_cool_app"
	// (a valid proto package identifier) instead of "mycoolapp" — which
	// silently loses the word boundaries and breaks grep.
	protoName := strings.ReplaceAll(g.Name, "-", "_")

	templateData := struct {
		Name                   string
		ProtoName              string
		Module                 string
		ServiceName            string
		ServicePort            int
		ProjectName            string
		FrontendName           string
		FrontendPort           int
		GoVersion              string
		GoVersionMinor         string
		DockerBuilderGoVersion string
	}{
		Name:                   g.Name,
		ProtoName:              protoName,
		Module:                 g.ModulePath,
		ServiceName:            g.ServiceName,
		ServicePort:            g.ServicePort,
		ProjectName:            g.Name,
		FrontendName:           g.FrontendName,
		FrontendPort:           g.FrontendPort,
		GoVersion:              goVersion,
		GoVersionMinor:         goVersionMinor(goVersion),
		DockerBuilderGoVersion: dockerBuilderGoVersion(goVersion),
	}

	if err := g.copyforgeProtos(); err != nil {
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

	files := []struct {
		template string
		dest     string
	}{
		{"Taskfile.yml.tmpl", "Taskfile.yml"},
		{".gitignore", ".gitignore"},
		{".dockerignore", ".dockerignore"},
		{"Dockerfile.tmpl", "Dockerfile"},
		{"README.md.tmpl", "README.md"},
		{"CONTRIBUTING.md.tmpl", "CONTRIBUTING.md"},
		{"CHANGELOG.md.tmpl", "CHANGELOG.md"},
		{"go.mod.tmpl", "go.mod"},
		{"go.work.tmpl", "go.work"},
		{"gen-go.mod.tmpl", "gen/go.mod"},
		{"buf.yaml", "buf.yaml"},
		{"buf.gen.yaml", "buf.gen.yaml"},
		{"cmd-root.go.tmpl", "cmd/main.go"},
		{"cmd-server.go.tmpl", "cmd/server.go"},
		{"cmd-db.go.tmpl", "cmd/db.go"},
		{"otel.go", "cmd/otel.go"},
		{"cmd-version.go.tmpl", "cmd/version.go"},
		{"air.toml.tmpl", ".air.toml"},
		{"air-debug.toml.tmpl", ".air-debug.toml"},
		{"vscode-launch.json.tmpl", ".vscode/launch.json"},
	}

	for _, file := range files {
		destPath := filepath.Join(g.Path, file.dest)
		if err := assets.WriteTemplateWithData(file.template, destPath, templateData); err != nil {
			return fmt.Errorf("failed to create %s: %w", file.dest, err)
		}
	}

	if err := g.generatePkgMiddleware(); err != nil {
		return fmt.Errorf("failed to generate pkg/middleware: %w", err)
	}

	// Record checksums for frozen files so `forge upgrade` can detect drift.
	if err := g.recordFrozenChecksums(templateData); err != nil {
		return fmt.Errorf("failed to record frozen file checksums: %w", err)
	}

	if err := g.generateBootstrap(); err != nil {
		return fmt.Errorf("failed to generate pkg/app/bootstrap.go: %w", err)
	}
	// Generate setup.go (user-owned, never overwritten) so bootstrap.go compiles
	// even with zero services.
	// Initial scaffold: no database driver wired and no ORM — the pipeline
	// runs `forge generate` immediately after and rewrites this file with
	// the correct flags once proto/db and forge.project.yaml are present.
	if err := codegen.GenerateSetup(g.ModulePath, "", false, g.Path); err != nil {
		return fmt.Errorf("failed to generate pkg/app/setup.go: %w", err)
	}
	if err := g.generateBootstrapTesting(); err != nil {
		return fmt.Errorf("failed to generate pkg/app/testing.go: %w", err)
	}
	// Generate migrate.go stub (no migrations embedded at project creation)
	if err := codegen.GenerateMigrate(g.Path, g.ModulePath, false); err != nil {
		return fmt.Errorf("failed to generate pkg/app/migrate.go: %w", err)
	}

	// Write forge.project.yaml
	if err := g.writeProjectConfig(); err != nil {
		return fmt.Errorf("failed to write project config: %w", err)
	}

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

	// Generate .env.example with common environment variables
	if err := g.generateEnvExample(); err != nil {
		return fmt.Errorf("failed to generate .env.example: %w", err)
	}

	if err := g.generateGolangciLint(); err != nil {
		return fmt.Errorf("failed to generate .golangci.yml: %w", err)
	}
	if g.ServiceName != "" {
		if err := g.generateServiceFiles(); err != nil {
			return fmt.Errorf("failed to generate service files: %w", err)
		}
	}

	// Generate frontend files if specified (both modes)
	if g.FrontendName != "" {
		if err := g.generateFrontendFiles(); err != nil {
			return fmt.Errorf("failed to generate frontend files: %w", err)
		}
	}

	// Generate CI/CD workflow files (both modes)
	if err := g.generateCIFiles(); err != nil {
		return fmt.Errorf("failed to generate CI files: %w", err)
	}

	// Generate E2E test harness (both modes)
	if g.ServiceName != "" {
		if err := g.generateE2ETests(); err != nil {
			return fmt.Errorf("failed to generate E2E tests: %w", err)
		}
	}

	// Scaffold examples/ placeholder so the convention is discoverable.
	if err := g.generateExamplesReadme(); err != nil {
		return fmt.Errorf("failed to generate examples/README.md: %w", err)
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

	return nil
}

// copyforgeProtos copies the versioned options protos used by newly generated projects.
func (g *ProjectGenerator) copyforgeProtos() error {
	v1Dir := filepath.Join(g.Path, "proto", "forge", "options", "v1")
	protos, err := assets.GetForgeOptionsV1Protos()
	if err != nil {
		return fmt.Errorf("failed to load v1 options protos: %w", err)
	}

	oldGoPackage := `option go_package = "github.com/reliant-labs/forge/gen/forge/options/v1;optionsv1";`
	newGoPackage := fmt.Sprintf(`option go_package = "%s/gen/forge/options/v1;optionsv1";`, g.ModulePath)

	for name, content := range protos {
		adjustedContent := strings.Replace(string(content), oldGoPackage, newGoPackage, 1)
		destPath := filepath.Join(v1Dir, name)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("failed to create options proto directory: %w", err)
		}
		if err := os.WriteFile(destPath, []byte(adjustedContent), 0o644); err != nil {
			return fmt.Errorf("failed to write v1 options proto %s: %w", name, err)
		}
	}

	return nil
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
	destPath := filepath.Join(g.Path, "proto", "services", svcName, "v1", fmt.Sprintf("%s.proto", svcName))
	return assets.WriteExampleProto(svcName, destPath, data)
}

func (g *ProjectGenerator) generateServiceFiles() error {
	return GenerateServiceFiles(g.Path, g.ModulePath, g.ServiceName, g.Name, g.ServicePort)
}

func (g *ProjectGenerator) generateFrontendFiles() error {
	if g.FrontendName == "" {
		return nil
	}
	return GenerateFrontendFiles(g.Path, g.ModulePath, g.Name, g.FrontendName, g.ServicePort)
}

// generateE2ETests generates the E2E test harness for the initial service.
func (g *ProjectGenerator) generateE2ETests() error {
	methods := MethodsFromProtoStub(g.ServiceName)
	return GenerateE2ETests(g.Path, g.ServiceName, g.ModulePath, g.Name, methods)
}
