package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// ProjectGenerator generates new project structure
type ProjectGenerator struct {
	Name              string
	Path              string
	ModulePath        string
	ServiceName       string // initial service name (default: "api")
	ServicePort       int    // initial service port (default: 8080)
	FrontendName      string // optional initial Next.js frontend name
	FrontendPort      int    // frontend port (default: 3000)
	GoVersionOverride string // if set, use this Go version instead of detecting
}

const (
	defaultGoVersion = "1.25.0"
	// maxKnownGoMinor is the latest stable Go minor version known to this
	// build of forge. Detected versions newer than this are capped.
	maxKnownGoMinor = 25
)

// detectGoVersion returns a validated Go version from the host (e.g. "1.24.0").
// Falls back to defaultGoVersion if detection fails.
func detectGoVersion() string {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return defaultGoVersion
	}
	v := strings.TrimSpace(string(out))
	// "go1.25.0" -> "1.25.0"
	v = strings.TrimPrefix(v, "go")
	if v == "" {
		return defaultGoVersion
	}
	return validateGoVersion(v)
}

// validateGoVersion checks a raw Go version string and returns a safe version
// for use in go.mod/go.work files. It handles:
//   - Pre-release versions (beta, rc) → falls back to previous stable minor
//   - Versions newer than maxKnownGoMinor → capped to maxKnownGoMinor
//   - Completely unparseable strings → defaultGoVersion
func validateGoVersion(v string) string {
	// Strip pre-release suffixes (e.g. "1.25beta1", "1.25rc1", "1.25.0-rc.1")
	prerelease := false
	for _, tag := range []string{"beta", "rc", "alpha"} {
		if idx := strings.Index(strings.ToLower(v), tag); idx != -1 {
			v = v[:idx]
			prerelease = true
			break
		}
	}

	// Also handle devel/tip versions (e.g. "devel go1.25-...")
	if strings.HasPrefix(v, "devel") || v == "" {
		fmt.Fprintf(os.Stderr, "⚠️  Detected Go development build. Using go %s instead.\n", goVersionMinor(defaultGoVersion))
		return defaultGoVersion
	}

	// Strip any trailing dot left after suffix removal (e.g. "1.25." -> "1.25")
	v = strings.TrimRight(v, ".")

	major, minor, _, ok := parseGoVersion(v)
	if !ok {
		fmt.Fprintf(os.Stderr, "⚠️  Could not parse Go version %q. Using go %s instead.\n", v, goVersionMinor(defaultGoVersion))
		return defaultGoVersion
	}

	if major != 1 {
		fmt.Fprintf(os.Stderr, "⚠️  Unexpected Go major version %d. Using go %s instead.\n", major, goVersionMinor(defaultGoVersion))
		return defaultGoVersion
	}

	if prerelease {
		// For pre-release, use the previous stable minor
		safeMinor := minor - 1
		if safeMinor < 21 {
			safeMinor = 21 // don't go below Go 1.21
		}
		result := fmt.Sprintf("%d.%d.0", major, safeMinor)
		fmt.Fprintf(os.Stderr, "⚠️  Detected Go pre-release version. Using go %s instead.\n", goVersionMinor(result))
		return result
	}

	if minor > maxKnownGoMinor {
		result := fmt.Sprintf("%d.%d.0", major, maxKnownGoMinor)
		fmt.Fprintf(os.Stderr, "⚠️  Detected Go version %s which is newer than the latest known stable (1.%d). Using go %s instead.\n", v, maxKnownGoMinor, goVersionMinor(result))
		return result
	}

	return v
}

// parseGoVersion extracts major, minor, and patch from a version string.
// Accepts "1.24", "1.24.3". Returns ok=false if the format is invalid.
func parseGoVersion(v string) (major, minor, patch int, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, 0, false
	}

	n, err := fmt.Sscanf(parts[0], "%d", &major)
	if err != nil || n != 1 {
		return 0, 0, 0, false
	}

	n, err = fmt.Sscanf(parts[1], "%d", &minor)
	if err != nil || n != 1 {
		return 0, 0, 0, false
	}

	if len(parts) == 3 {
		n, err = fmt.Sscanf(parts[2], "%d", &patch)
		if err != nil || n != 1 {
			return 0, 0, 0, false
		}
	}

	return major, minor, patch, true
}

// goVersionMinor returns the major.minor portion (e.g. "1.25.0" -> "1.25").
func goVersionMinor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// resolveGoVersion returns the Go version to use, preferring the override if set.
func (g *ProjectGenerator) resolveGoVersion() string {
	if g.GoVersionOverride != "" {
		v := g.GoVersionOverride
		// Normalize: accept "1.24" as "1.24.0"
		parts := strings.SplitN(v, ".", 3)
		if len(parts) == 2 {
			v = v + ".0"
		}
		_, _, _, ok := parseGoVersion(v)
		if !ok {
			fmt.Fprintf(os.Stderr, "⚠️  Invalid --go-version %q. Using detected version instead.\n", g.GoVersionOverride)
			return detectGoVersion()
		}
		return v
	}
	return detectGoVersion()
}

// NewProjectGenerator creates a new project generator
func NewProjectGenerator(name, path, modulePath string) *ProjectGenerator {
	return &ProjectGenerator{
		Name:        name,
		Path:        path,
		ModulePath:  modulePath,
		ServiceName: "api",
		ServicePort: 8080,
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
		"internal",
	}

	// Add service directory
	dirs = append(dirs, fmt.Sprintf("handlers/%s", g.ServiceName))
	dirs = append(dirs, fmt.Sprintf("proto/services/%s/v1", g.ServiceName))

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

	goVersion := g.resolveGoVersion()

	// Sanitize name for proto files (no hyphens allowed). Use underscores
	// rather than stripping so that "my-cool-app" becomes "my_cool_app"
	// (a valid proto package identifier) instead of "mycoolapp" — which
	// silently loses the word boundaries and breaks grep.
	protoName := strings.ReplaceAll(g.Name, "-", "_")

	templateData := struct {
		Name           string
		ProtoName      string
		Module         string
		ServiceName    string
		ServicePort    int
		ProjectName    string
		FrontendName   string
		FrontendPort   int
		GoVersion      string
		GoVersionMinor string
	}{
		Name:           g.Name,
		ProtoName:      protoName,
		Module:         g.ModulePath,
		ServiceName:    g.ServiceName,
		ServicePort:    g.ServicePort,
		ProjectName:    g.Name,
		FrontendName:   g.FrontendName,
		FrontendPort:   g.FrontendPort,
		GoVersion:      goVersion,
		GoVersionMinor: goVersionMinor(goVersion),
	}

	if err := g.copyforgeProtos(); err != nil {
		return err
	}
	if err := g.createExampleProto(templateData); err != nil {
		return err
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
		{"Dockerfile.tmpl", "Dockerfile"},
		{"README.md.tmpl", "README.md"},
		{"go.mod.tmpl", "go.mod"},
		{"go.work.tmpl", "go.work"},
		{"gen-go.mod.tmpl", "gen/go.mod"},
		{"buf.yaml", "buf.yaml"},
		{"buf.gen.yaml", "buf.gen.yaml"},
		{"cmd-root.go.tmpl", "cmd/main.go"},
		{"cmd-server.go.tmpl", "cmd/server.go"},
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
	if err := g.generateBootstrap(); err != nil {
		return fmt.Errorf("failed to generate pkg/app/bootstrap.go: %w", err)
	}
	if err := g.generateBootstrapTesting(); err != nil {
		return fmt.Errorf("failed to generate pkg/app/testing.go: %w", err)
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

	if err := g.generateGolangciLint(); err != nil {
		return fmt.Errorf("failed to generate .golangci.yml: %w", err)
	}
	if err := g.generateServiceFiles(); err != nil {
		return fmt.Errorf("failed to generate service files: %w", err)
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
	if err := g.generateE2ETests(); err != nil {
		return fmt.Errorf("failed to generate E2E tests: %w", err)
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



func (g *ProjectGenerator) writeProjectConfig() error {
	cfg := config.ProjectConfig{
		Name:       g.Name,
		ModulePath: g.ModulePath,
		Version:    "0.1.0",
		HotReload:  true,
		Envs: []config.EnvironmentConfig{
			{Name: "dev", Type: "local"},
			{Name: "staging", Type: "cloud"},
			{Name: "prod", Type: "cloud"},
		},
		Database: config.DatabaseConfig{
			Driver:        "postgres",
			MigrationsDir: "db/migrations",
			SQLCEnabled:   false,
		},
		CI: config.CIConfig{
			Provider: "github",
			Lint:     true,
			Test:     true,
			Build:    true,
			Deploy:   true,
			VulnScan: true,
		},
		Docker: config.DockerConfig{
			Registry: "ghcr.io",
		},
		K8s: config.K8sConfig{
			Provider: "k3d",
			KCLDir:   "deploy/kcl",
		},
		Lint: config.LintConfig{
			ProtoMethod: true,
			Contract:    true,
		},
		Auth: config.AuthConfig{
			Provider: "none",
		},
	}

	cfg.Services = []config.ServiceConfig{
		{
			Name: g.ServiceName,
			Type: "go_service",
			Path: fmt.Sprintf("handlers/%s", g.ServiceName),
			Port: g.ServicePort,
		},
	}

	if g.FrontendName != "" {
		cfg.Frontends = []config.FrontendConfig{
			{
				Name: g.FrontendName,
				Type: "nextjs",
				Path: fmt.Sprintf("frontends/%s", g.FrontendName),
				Port: g.FrontendPort,
			},
		}
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}

	destPath := filepath.Join(g.Path, "forge.project.yaml")
	return os.WriteFile(destPath, data, 0644)
}

func (g *ProjectGenerator) generateKCLDeploy() error {
	deployDir := filepath.Join(g.Path, "deploy", "kcl")

	// Generate kcl.mod at project root so KCL imports like deploy.kcl.schema resolve.
	kclModData := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}
	kclModContent, err := templates.RenderDeployTemplate("kcl/kcl.mod.tmpl", kclModData)
	if err != nil {
		return fmt.Errorf("render kcl.mod template: %w", err)
	}
	if err := os.WriteFile(filepath.Join(g.Path, "kcl.mod"), kclModContent, 0644); err != nil {
		return fmt.Errorf("write kcl.mod: %w", err)
	}

	// Static files (no templating needed)
	staticFiles := []struct {
		templateName string
		dest         string
	}{
		{"kcl/schema.k", "schema.k"},
		{"kcl/render.k", "render.k"},
		{"kcl/base.k", "base.k"},
	}

	for _, f := range staticFiles {
		content, err := templates.GetDeployTemplate(f.templateName)
		if err != nil {
			return fmt.Errorf("read deploy template %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(deployDir, f.dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	// Templated per-env files
	envTemplates := []struct {
		templateName string
		dest         string
	}{
		{"kcl/dev/main.k.tmpl", "dev/main.k"},
		{"kcl/staging/main.k.tmpl", "staging/main.k"},
		{"kcl/prod/main.k.tmpl", "prod/main.k"},
	}

	templateData := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}

	for _, f := range envTemplates {
		content, err := templates.RenderDeployTemplate(f.templateName, templateData)
		if err != nil {
			return fmt.Errorf("render deploy template %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(deployDir, f.dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	return nil
}

// generateDevConfig writes the k3d cluster configuration for local development.
func (g *ProjectGenerator) generateDevConfig() error {
	data := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}

	content, err := templates.RenderDeployTemplate("k3d.yaml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render k3d.yaml: %w", err)
	}

	destPath := filepath.Join(g.Path, "deploy", "k3d.yaml")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateGolangciLint() error {
	content, err := templates.GetProjectTemplate("golangci.yml")
	if err != nil {
		return fmt.Errorf("read golangci.yml template: %w", err)
	}
	destPath := filepath.Join(g.Path, ".golangci.yml")
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateDockerCompose() error {
	data := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}
	content, err := templates.RenderProjectTemplate("docker-compose.yml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render docker-compose.yml: %w", err)
	}
	destPath := filepath.Join(g.Path, "docker-compose.yml")
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateServiceFiles() error {
	return GenerateServiceFiles(g.Path, g.ModulePath, g.ServiceName, g.Name, g.ServicePort)
}

// generateBootstrap writes pkg/app/bootstrap.go with explicit service construction.
func (g *ProjectGenerator) generateBootstrap() error {
	type bootstrapService struct {
		Name      string
		Package   string
		FieldName string
	}

	type bootstrapPackage struct {
		Name      string
		Package   string
		FieldName string
	}

	pkg := g.ServiceName
	fieldName := naming.ToExportedFieldName(pkg)

	data := struct {
		Module   string
		Services []bootstrapService
		Packages []bootstrapPackage
	}{
		Module: g.ModulePath,
		Services: []bootstrapService{
			{
				Name:      pkg,
				Package:   pkg,
				FieldName: fieldName,
			},
		},
		Packages: nil, // No packages at initial project creation
	}

	content, err := templates.RenderProjectTemplate("bootstrap.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap.go.tmpl: %w", err)
	}

	destPath := filepath.Join(g.Path, "pkg", "app", "bootstrap.go")
	return os.WriteFile(destPath, content, 0644)
}

// generateBootstrapTesting writes pkg/app/testing.go with test helper functions.
func (g *ProjectGenerator) generateBootstrapTesting() error {
	pkg := g.ServiceName
	fieldName := naming.ToExportedFieldName(pkg)

	protoServiceName := naming.ToPascalCase(pkg) + "Service"

	type bootstrapTestService struct {
		Name             string
		Package          string
		FieldName        string
		ProtoServiceName string
	}

	type bootstrapPackage struct {
		Name      string
		Package   string
		FieldName string
	}

	data := struct {
		Module   string
		Services []bootstrapTestService
		Packages []bootstrapPackage
	}{
		Module: g.ModulePath,
		Services: []bootstrapTestService{
			{
				Name:             pkg,
				Package:          pkg,
				FieldName:        fieldName,
				ProtoServiceName: protoServiceName,
			},
		},
		Packages: nil, // No packages at initial project creation
	}

	content, err := templates.RenderProjectTemplate("bootstrap_testing.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap_testing.go.tmpl: %w", err)
	}

	destPath := filepath.Join(g.Path, "pkg", "app", "testing.go")
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateFrontendFiles() error {
	if g.FrontendName == "" {
		return nil
	}
	return GenerateFrontendFiles(g.Path, g.ModulePath, g.Name, g.FrontendName, g.ServicePort)
}

// writeProjectMetadata writes everything under .reliant/, the top-level
// reliant.md stub, and the project-level .mcp.json files.
//
// File ownership model:
//
//   - forge-owned (always overwritten on regeneration):
//     .reliant/project.json, .reliant/README.md, .reliant/reliant-forge.md,
//     .reliant/skills/**.
//
//   - User-owned (written only if absent, never touched if present):
//     reliant.md, .mcp.json, .mcp.json.example.
//
// This split eliminates the merge-logic footguns of the earlier design: the
// forge-owned files are safe to regenerate freely because the user has no
// reason to edit them; the user-owned files are safe to leave alone because
// they point at the forge-owned content via links.
func (g *ProjectGenerator) writeProjectMetadata() error {
	reliantDir := filepath.Join(g.Path, ".reliant")
	if err := os.MkdirAll(reliantDir, 0o755); err != nil {
		return fmt.Errorf("failed to create .reliant directory: %w", err)
	}

	if err := g.writeProjectJSON(reliantDir); err != nil {
		return err
	}

	if err := assets.WriteTemplateWithData(".reliant-README.md", filepath.Join(reliantDir, "README.md"), nil); err != nil {
		return fmt.Errorf("failed to write .reliant/README.md: %w", err)
	}

	templateData := struct{ Name string }{Name: g.Name}

	// forge-owned conventions file. Always regenerated.
	forgeMemoryPath := filepath.Join(reliantDir, "reliant-forge.md")
	if err := assets.WriteTemplateWithData("reliant-forge.md.tmpl", forgeMemoryPath, templateData); err != nil {
		return fmt.Errorf("failed to write .reliant/reliant-forge.md: %w", err)
	}

	// Skills tree. Always regenerated.
	if err := g.writeSkills(reliantDir); err != nil {
		return fmt.Errorf("failed to write skills: %w", err)
	}

	// User-owned top-level memory file — write only if absent.
	if err := writeIfAbsent(filepath.Join(g.Path, "reliant.md"), "reliant.md.tmpl", templateData); err != nil {
		return fmt.Errorf("failed to write reliant.md: %w", err)
	}

	// User-owned MCP config — write only if absent.
	if err := writeIfAbsent(filepath.Join(g.Path, ".mcp.json"), "mcp.json.tmpl", templateData); err != nil {
		return fmt.Errorf("failed to write .mcp.json: %w", err)
	}

	// Documentation file for opt-in MCP servers — write only if absent so a
	// user who deleted it intentionally isn't pestered.
	if err := writeIfAbsent(filepath.Join(g.Path, ".mcp.json.example"), "mcp.json.example.tmpl", templateData); err != nil {
		return fmt.Errorf("failed to write .mcp.json.example: %w", err)
	}

	return nil
}

// writeProjectJSON writes the immutable project metadata JSON under .reliant/.
func (g *ProjectGenerator) writeProjectJSON(reliantDir string) error {
	metadata := map[string]interface{}{
		"name":        g.Name,
		"module_path": g.ModulePath,
		"created_at":  time.Now().Format(time.RFC3339),
		"version":     "1.0.0",
		"generator":   "forge",
	}

	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(reliantDir, "project.json"), data, 0o644); err != nil {
		return fmt.Errorf("failed to write .reliant/project.json: %w", err)
	}
	return nil
}

// writeSkills copies every file under project/skills/ in the embedded
// templates into <reliantDir>/skills/, preserving directory structure.
// Files are copied verbatim (not rendered as templates) so their prose may
// contain literal examples like {{.Name}} without conflict.
func (g *ProjectGenerator) writeSkills(reliantDir string) error {
	skillFiles, err := templates.ListProjectTemplates("skills")
	if err != nil {
		return fmt.Errorf("failed to list skill templates: %w", err)
	}

	for _, rel := range skillFiles {
		templateName := path.Join("skills", filepath.ToSlash(rel))
		content, err := templates.GetProjectTemplate(templateName)
		if err != nil {
			return fmt.Errorf("failed to read skill template %s: %w", templateName, err)
		}

		destPath := filepath.Join(reliantDir, "skills", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("failed to create skill dir %s: %w", filepath.Dir(destPath), err)
		}
		if err := os.WriteFile(destPath, content, 0o644); err != nil {
			return fmt.Errorf("failed to write skill file %s: %w", destPath, err)
		}
	}

	return nil
}

// writeIfAbsent renders the given template to destPath only if destPath does
// not already exist. This is used for user-owned files (reliant.md, .mcp.json,
// .mcp.json.example) to avoid clobbering local edits on regeneration.
func writeIfAbsent(destPath, templateName string, data interface{}) error {
	if _, err := os.Stat(destPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat %s: %w", destPath, err)
	}
	return assets.WriteTemplateWithData(templateName, destPath, data)
}

func (g *ProjectGenerator) generateCIFiles() error {
	provider := "github"

	data := templates.CITemplateData{
		ProjectName:  g.Name,
		Module:       g.ModulePath,
		GoVersion:    goVersionMinor(g.resolveGoVersion()),
		Registry:     "ghcr.io",
		GithubOrg:    g.Name,
		FrontendName: g.FrontendName,
		Lint:         true,
		Test:         true,
		Build:        true,
		Deploy:       true,
		VulnScan:     true,
	}

	// Templated files
	templatedFiles := []struct {
		templateName string
		dest         string
	}{
		{"ci.yml.tmpl", ".github/workflows/ci.yml"},
		{"build-images.yml.tmpl", ".github/workflows/build-images.yml"},
		{"deploy.yml.tmpl", ".github/workflows/deploy.yml"},
		{"dependabot.yml.tmpl", ".github/dependabot.yml"},
	}

	for _, f := range templatedFiles {
		content, err := templates.RenderCITemplate(provider, f.templateName, data)
		if err != nil {
			return fmt.Errorf("render CI template %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(g.Path, f.dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	// Static files
	staticFiles := []struct {
		templateName string
		dest         string
	}{
		{"pull_request_template.md", ".github/pull_request_template.md"},
	}

	for _, f := range staticFiles {
		content, err := templates.GetCITemplate(provider, f.templateName)
		if err != nil {
			return fmt.Errorf("read CI template %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(g.Path, f.dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

	// CODEOWNERS (templated)
	content, err := templates.RenderCITemplate(provider, "CODEOWNERS.tmpl", data)
	if err != nil {
		return fmt.Errorf("render CODEOWNERS: %w", err)
	}
	destPath := filepath.Join(g.Path, ".github", "CODEOWNERS")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(destPath, content, 0644); err != nil {
		return fmt.Errorf("write CODEOWNERS: %w", err)
	}

	return nil
}

// generatePkgMiddleware writes Connect-compatible interceptors into pkg/middleware/.
func (g *ProjectGenerator) generatePkgMiddleware() error {
	middlewareFiles := []struct {
		templateName string
		destName     string
	}{
		{"middleware-recovery.go", "recovery.go"},
		{"middleware-logging.go", "logging.go"},
		{"middleware-authz.go", "authz.go"},
		{"middleware-claims.go", "claims.go"},
		{"middleware-audit.go", "audit.go"},
		{"middleware-http.go", "http.go"},
		{"middleware-cors.go", "cors.go"},
	}

	for _, f := range middlewareFiles {
		content, err := templates.GetProjectTemplate(f.templateName)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(g.Path, "pkg", "middleware", f.destName)
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.destName, err)
		}
	}
	return nil
}

// generateE2ETests generates the E2E test harness for the initial service.
func (g *ProjectGenerator) generateE2ETests() error {
	methods := MethodsFromProtoStub(g.ServiceName)
	return GenerateE2ETests(g.Path, g.ServiceName, g.ModulePath, g.Name, methods)
}

var (
	templateEngineOnce sync.Once
	templateEngineInst *templates.TemplateEngine
	templateEngineErr  error
)

func getTemplateEngine() (*templates.TemplateEngine, error) {
	templateEngineOnce.Do(func() {
		templateEngineInst, templateEngineErr = templates.NewTemplateEngine()
	})
	return templateEngineInst, templateEngineErr
}

// renderServiceTemplate renders a service template from the embedded FS.
func renderServiceTemplate(name string, data interface{}) ([]byte, error) {
	engine, err := getTemplateEngine()
	if err != nil {
		return nil, err
	}
	result, err := engine.RenderTemplate(name, data)
	if err != nil {
		return nil, err
	}
	return []byte(result), nil
}

// ReadProjectConfig reads a forge.project.yaml from the given path.
func ReadProjectConfig(path string) (*config.ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project config: %w", err)
	}
	var cfg config.ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse project config: %w", err)
	}
	return &cfg, nil
}

// WriteProjectConfig writes a config.ProjectConfig to the given path.
func WriteProjectConfigFile(cfg *config.ProjectConfig, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// AppendServiceToConfig reads the project config at the given project root,
// appends a new service entry, and writes it back. It uses yaml.Node
// round-tripping so that unknown keys, comments, and field ordering added
// by the user are preserved.
func AppendServiceToConfig(projectRoot, serviceName string, port int) error {
	configPath := filepath.Join(projectRoot, "forge.project.yaml")
	entry := config.ServiceConfig{
		Name: serviceName,
		Type: "go_service",
		Path: fmt.Sprintf("handlers/%s", serviceName),
		Port: port,
	}
	return appendToProjectConfigSequence(configPath, "services", entry)
}

// AppendFrontendToConfig reads the project config at the given project root,
// appends a new frontend entry, and writes it back. It uses yaml.Node
// round-tripping so that unknown keys, comments, and field ordering added
// by the user are preserved.
func AppendFrontendToConfig(projectRoot, frontendName string, port int) error {
	configPath := filepath.Join(projectRoot, "forge.project.yaml")
	entry := config.FrontendConfig{
		Name: frontendName,
		Type: "nextjs",
		Path: fmt.Sprintf("frontends/%s", frontendName),
		Port: port,
	}
	return appendToProjectConfigSequence(configPath, "frontends", entry)
}

// appendToProjectConfigSequence appends entry to the YAML sequence at the
// top-level key on the project config at configPath, preserving any keys,
// comments, and ordering the user added that are not part of the Go struct.
// If the key does not exist, it is created.
func appendToProjectConfigSequence(configPath, key string, entry interface{}) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse project config: %w", err)
	}

	// The document node wraps a single mapping node.
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("project config %s: expected a YAML document", configPath)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("project config %s: expected top-level mapping", configPath)
	}

	// Build the node for the new entry via round-tripping through yaml.Node.
	entryBytes, err := yaml.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal new %s entry: %w", key, err)
	}
	var entryDoc yaml.Node
	if err := yaml.Unmarshal(entryBytes, &entryDoc); err != nil {
		return fmt.Errorf("parse new %s entry: %w", key, err)
	}
	if entryDoc.Kind != yaml.DocumentNode || len(entryDoc.Content) == 0 {
		return fmt.Errorf("unexpected YAML shape for new %s entry", key)
	}
	entryNode := entryDoc.Content[0]

	// Find the sequence node for `key` in the top-level mapping. Mapping
	// nodes store keys and values as alternating children.
	var seq *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		k := root.Content[i]
		v := root.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			seq = v
			break
		}
	}

	if seq == nil {
		// Key does not exist — create an empty sequence and append it.
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			seq,
		)
	} else if seq.Kind != yaml.SequenceNode {
		// The key is present but set to null/empty — replace with a sequence.
		seq.Kind = yaml.SequenceNode
		seq.Tag = "!!seq"
		seq.Value = ""
	}

	seq.Content = append(seq.Content, entryNode)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	return os.WriteFile(configPath, out, 0644)
}