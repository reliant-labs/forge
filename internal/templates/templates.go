package templates

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"

	"github.com/jinzhu/inflection"
	"github.com/reliant-labs/forge/internal/naming"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

//go:embed all:project all:deploy all:frontend all:ci all:test service/*.tmpl middleware/*.tmpl all:internal-package webhook/*.tmpl worker/*.tmpl operator/*.tmpl
var templateFS embed.FS

// FuncMap returns the shared template function map used across all templates.
func FuncMap() template.FuncMap {
	caser := cases.Title(language.English)
	return template.FuncMap{
		"lower":         strings.ToLower,
		"upper":         strings.ToUpper,
		"title":         caser.String,
		"snakeCase":     hyphenToUnderscore,
		"camelCase":     toCamelCase,
		"pascalCase":    toPascalCase,
		"kebabCase":     toKebabCase,
		"plural":        pluralize,
		"singular":      singularize,
		"formatComment": formatComment,
		"joinStrings":   strings.Join,
		"default":       getDefault,
		"add":           add,
		"last":          lastStringSlice,
		"tableFromFK":   tableFromFK,
		"columnFromFK":  columnFromFK,
	}
}

// stripBuildIgnore removes a leading //go:build ignore directive and the
// following blank line from embedded template .go files. These directives
// prevent the Go toolchain from compiling the templates as part of forge
// itself, but must not appear in scaffolded output.
func stripBuildIgnore(data []byte) []byte {
	const header = "//go:build ignore\n"
	if s := string(data); strings.HasPrefix(s, header) {
		s = strings.TrimPrefix(s, header)
		s = strings.TrimLeft(s, "\n")
		return []byte(s)
	}
	return data
}

// GetProjectTemplate returns the raw content of a project template file.
// The name should be relative to the project/ directory (e.g. "go.mod.tmpl").
// If the file starts with a //go:build ignore directive, it is stripped.
func GetProjectTemplate(name string) ([]byte, error) {
	data, err := templateFS.ReadFile(path.Join("project", filepath.ToSlash(name)))
	if err != nil {
		return nil, err
	}
	return stripBuildIgnore(data), nil
}

// renderTemplate is the shared implementation for all Render*Template functions.
// It reads a template file from the embedded FS at basePath/name, and if the file
// has a .tmpl suffix it parses and executes it with the shared FuncMap. Non-.tmpl
// files are returned as-is.
func renderTemplate(fsys embed.FS, basePath, name string, data interface{}) ([]byte, error) {
	content, err := fsys.ReadFile(path.Join(basePath, filepath.ToSlash(name)))
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", name, err)
	}

	if !strings.HasSuffix(name, ".tmpl") {
		return stripBuildIgnore(content), nil
	}

	content = stripBuildIgnore(content)

	tmpl, err := template.New(name).Funcs(FuncMap()).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template %s: %w", name, err)
	}

	return stripBuildIgnore(buf.Bytes()), nil
}

// RenderProjectTemplate renders a project template with the given data,
// applying the shared funcMap. Only files with .tmpl suffix are treated
// as Go templates; all others are returned as-is.
func RenderProjectTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "project", name, data)
}

// ListProjectTemplates returns all file paths under the given project template
// subdirectory (e.g. "skills"), relative to that subdirectory. Used for
// scaffolding trees of files like skills/ where the number and names of files
// shouldn't be hard-coded in the generator.
func ListProjectTemplates(subdir string) ([]string, error) {
	var files []string
	root := path.Join("project", filepath.ToSlash(subdir))
	entries, err := templateFS.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read project template dir %s: %w", subdir, err)
	}

	var walk func(dir string, entries []fs.DirEntry) error
	walk = func(dir string, entries []fs.DirEntry) error {
		for _, e := range entries {
			rel := path.Join(dir, e.Name())
			if e.IsDir() {
				sub, err := templateFS.ReadDir(path.Join(root, rel))
				if err != nil {
					return err
				}
				if err := walk(rel, sub); err != nil {
					return err
				}
			} else {
				files = append(files, rel)
			}
		}
		return nil
	}

	if err := walk("", entries); err != nil {
		return nil, err
	}
	return files, nil
}

// GetFrontendTemplate returns the raw content of a frontend template file.
// The name should be relative to the frontend/ directory (e.g. "nextjs/package.json.tmpl").
func GetFrontendTemplate(name string) ([]byte, error) {
	return templateFS.ReadFile(path.Join("frontend", filepath.ToSlash(name)))
}

// RenderFrontendTemplate renders a frontend template with the given data,
// applying the shared funcMap. Only files with .tmpl suffix are treated
// as Go templates; all others are returned as-is.
func RenderFrontendTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "frontend", name, data)
}

// ListFrontendTemplates returns all file paths under the given frontend template directory
// (e.g. "nextjs"), relative to that directory. This is useful for iterating
// over all files that need to be written when scaffolding a frontend.
func ListFrontendTemplates(frontendType string) ([]string, error) {
	var files []string
	root := path.Join("frontend", filepath.ToSlash(frontendType))
	entries, err := templateFS.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read frontend template dir %s: %w", frontendType, err)
	}

	var walk func(dir string, entries []fs.DirEntry) error
	walk = func(dir string, entries []fs.DirEntry) error {
		for _, e := range entries {
			rel := path.Join(dir, e.Name())
			if e.IsDir() {
				sub, err := templateFS.ReadDir(path.Join(root, rel))
				if err != nil {
					return err
				}
				if err := walk(rel, sub); err != nil {
					return err
				}
			} else {
				files = append(files, rel)
			}
		}
		return nil
	}

	if err := walk("", entries); err != nil {
		return nil, err
	}
	return files, nil
}

// GetDeployTemplate returns the raw content of a deploy template file.
// The name should be relative to the deploy/ directory (e.g. "kcl/schema.k").
func GetDeployTemplate(name string) ([]byte, error) {
	return templateFS.ReadFile(filepath.Join("deploy", name))
}

// RenderDeployTemplate renders a deploy template with the given data.
// Only files with .tmpl suffix are treated as Go templates; others returned as-is.
func RenderDeployTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "deploy", name, data)
}

// GetCITemplate returns the raw content of a CI template file.
// The provider is the CI platform (e.g. "github") and name is the file
// relative to that provider directory (e.g. "ci.yml.tmpl").
func GetCITemplate(provider, name string) ([]byte, error) {
	return templateFS.ReadFile(filepath.Join("ci", provider, name))
}

// RenderCITemplate renders a CI template with the given data.
// Only files with .tmpl suffix are treated as Go templates; others returned as-is.
func RenderCITemplate(provider, name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, filepath.Join("ci", provider), name, data)
}

// ListCITemplates returns all file paths under the given CI provider directory,
// relative to that directory.
func ListCITemplates(provider string) ([]string, error) {
	var files []string
	root := filepath.Join("ci", provider)
	entries, err := templateFS.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read CI template dir %s: %w", provider, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	return files, nil
}

// CIWorkflowData holds data for the spec-driven CI workflow template.
type CIWorkflowData struct {
	ProjectName  string
	GoVersion    string // e.g. "1.26"
	HasFrontends bool
	Frontends    []FrontendCIConfig // from project config
	HasServices  bool

	// Lint
	LintGolangci bool
	LintBuf      bool
	LintFrontend bool

	// Test
	TestRace     bool
	TestCoverage bool

	// Vuln scan
	VulnGo     bool // govulncheck
	VulnDocker bool // trivy
	VulnNPM    bool // npm audit

	// E2E
	E2EEnabled bool
	E2ERuntime string // "docker-compose" or "k3d"

	// Permissions
	PermContents string // default "read"

	// Extra jobs
	ExtraJobs []CIExtraJob

	// Deploy-related
	HasKCL bool // validate KCL manifests

	// Environments (for KCL validation)
	Environments []string

	// Legacy fields used by other CI templates (build-images, deploy, dependabot)
	Module       string
	Registry     string // "ghcr", "gar", "ecr"
	GithubOrg    string
	FrontendName string // first frontend name for dependabot
}

// FrontendCIConfig is a minimal frontend descriptor for CI templates.
type FrontendCIConfig struct {
	Name string
	Path string
}

// CIExtraJob defines an additional user-specified CI job.
type CIExtraJob struct {
	Name    string
	Needs   []string
	RunsOn  string
	Steps   []CIExtraStep
}

// CIExtraStep is a single step inside an extra CI job.
type CIExtraStep struct {
	Name string
	Run  string
	Uses string
	With map[string]string
}

// DeployEnv represents a single deploy environment (e.g. staging, prod).
type DeployEnv struct {
	Name       string // "staging", "preprod", "prod"
	Auto       bool   // auto-deploy after image build
	Protection bool   // GitHub environment protection
	URL        string // environment URL
}

// DeployWorkflowData holds data for the deploy workflow template.
type DeployWorkflowData struct {
	ProjectName      string
	Environments     []DeployEnv // ordered: staging, preprod, prod
	Registry         string      // "ghcr", "gar", "ecr"
	HasFrontends     bool
	FrontendDeploy   string // "firebase", "vercel", "none"
	MigrationTest    bool   // test migrations before deploy
	Concurrency      bool   // per-env concurrency groups
	CancelInProgress bool
}

// BuildImagesWorkflowData holds data for the build-images workflow template.
type BuildImagesWorkflowData struct {
	ProjectName  string
	Registry     string // "ghcr", "gar"
	HasFrontends bool
	VulnDocker   bool // trivy scanning
}

// E2EWorkflowData holds data for the standalone E2E test workflow template.
type E2EWorkflowData struct {
	ProjectName  string
	GoVersion    string
	Runtime      string // "docker-compose" (default) or "k3d"
	HasFrontends bool
}

// GetTestTemplate returns the raw content of a test template file.
// The name should be relative to the test/ directory (e.g. "e2e/main_test.go.tmpl").
func GetTestTemplate(name string) ([]byte, error) {
	return templateFS.ReadFile(filepath.Join("test", name))
}

// RenderTestTemplate renders a test template with the given data,
// applying the shared funcMap. Only files with .tmpl suffix are treated
// as Go templates; all others are returned as-is.
func RenderTestTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "test", name, data)
}

// FrontendTemplateData holds data for frontend template rendering.
type FrontendTemplateData struct {
	FrontendName string
	ProjectName  string
	ApiUrl       string
	ApiPort      string
	Module       string
}

// GetInternalPackageTemplate returns the raw content of an internal-package template file.
// The name should be relative to the internal-package/ directory (e.g. "contract.go.tmpl").
func GetInternalPackageTemplate(name string) ([]byte, error) {
	return templateFS.ReadFile(filepath.Join("internal-package", name))
}

// RenderInternalPackageTemplate renders an internal-package template with the given data,
// applying the shared funcMap. Only files with .tmpl suffix are treated
// as Go templates; all others are returned as-is.
func RenderInternalPackageTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "internal-package", name, data)
}

// RenderInternalPackageKindTemplate renders a template from a kind-specific
// subdirectory under internal-package/ (e.g. internal-package/client/client.go.tmpl).
func RenderInternalPackageKindTemplate(kind, name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, path.Join("internal-package", kind), name, data)
}

// ListInternalPackageKindTemplates returns all template file names under the
// given kind subdirectory of internal-package/ (e.g. "client").
func ListInternalPackageKindTemplates(kind string) ([]string, error) {
	root := path.Join("internal-package", kind)
	entries, err := templateFS.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read internal-package kind dir %s: %w", kind, err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	return files, nil
}



// WebhookTemplateData holds data for webhook template rendering.
type WebhookTemplateData struct {
	Name        string // webhook name (e.g. "stripe", "github")
	ServiceName string // target service name
	Module      string // Go module path
}

// WebhookRoutesTemplateData holds data for the webhook_routes_gen.go template.
type WebhookRoutesTemplateData struct {
	Package  string                    // Go package name (e.g. "billing")
	Webhooks []WebhookRouteEntryData   // all webhooks for this service
}

// WebhookRouteEntryData holds per-webhook data for route generation.
type WebhookRouteEntryData struct {
	Name       string // kebab-case name for the URL path (e.g. "stripe")
	PascalName string // PascalCase name for the handler method (e.g. "Stripe")
}

// RenderWebhookTemplate renders a webhook template with the given data.
// name should be e.g. "webhook/webhooks.go.tmpl".
func RenderWebhookTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "", name, data)
}

// TemplateEngine handles code generation from service/middleware templates.
// NOTE: TemplateEngine pre-parses templates for reuse via a singleton (see generator/project.go),
// while the standalone Render*Template functions parse on each call. Both share FuncMap().
// Consider consolidating if this becomes a maintenance burden.
type TemplateEngine struct {
	templates map[string]*template.Template
	funcMap   template.FuncMap
}

// NewTemplateEngine creates a new template engine with all service and
// middleware templates pre-loaded.
func NewTemplateEngine() (*TemplateEngine, error) {
	engine := &TemplateEngine{
		templates: make(map[string]*template.Template),
		funcMap:   FuncMap(),
	}

	if err := engine.loadTemplates(); err != nil {
		return nil, err
	}

	return engine, nil
}

// loadTemplates loads all embedded service and middleware templates.
func (e *TemplateEngine) loadTemplates() error {
	templateFiles := []string{
		"service/service.go.tmpl",
		"service/handlers.go.tmpl",
		"service/authorizer.go.tmpl",
		"service/unit_test.go.tmpl",
		"service/integration_test.go.tmpl",
		"middleware/auth.go.tmpl",
		"worker/worker.go.tmpl",
		"worker/worker_test.go.tmpl",
		"operator/types.go.tmpl",
		"operator/controller.go.tmpl",
		"operator/controller_test.go.tmpl",
	}

	for _, file := range templateFiles {
		content, err := templateFS.ReadFile(file)
		if err != nil {
			// Template doesn't exist yet, skip
			continue
		}

		tmpl, err := template.New(file).Funcs(e.funcMap).Parse(string(content))
		if err != nil {
			return fmt.Errorf("failed to parse template %s: %w", file, err)
		}

		e.templates[file] = tmpl
	}

	return nil
}

// RenderTemplate renders a template with the given data.
func (e *TemplateEngine) RenderTemplate(name string, data interface{}) (string, error) {
	tmpl, ok := e.templates[name]
	if !ok {
		return "", fmt.Errorf("template %s not found", name)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", name, err)
	}

	return buf.String(), nil
}

// RenderMiddlewareTemplate renders a middleware template from the embedded middleware/ directory.
// name should be e.g. "middleware/auth_gen.go.tmpl".
func RenderMiddlewareTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "", name, data)
}

// RenderServiceTemplate renders a service template from the embedded service/ directory.
// name should be e.g. "service/service.go.tmpl" or "service/handlers.go.tmpl".
func RenderServiceTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "", name, data)
}

// RenderWorkerTemplate renders a worker template from the embedded worker/ directory.
// name should be e.g. "worker/worker.go.tmpl" or "worker/worker_test.go.tmpl".
func RenderWorkerTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "", name, data)
}

// RenderOperatorTemplate renders an operator template from the embedded operator/ directory.
// name should be e.g. "operator/types.go.tmpl" or "operator/controller.go.tmpl".
func RenderOperatorTemplate(name string, data interface{}) ([]byte, error) {
	return renderTemplate(templateFS, "", name, data)
}

// Case conversion functions

// hyphenToUnderscore replaces hyphens with underscores.
// Used in templates for proto package names where hyphens aren't valid.
func hyphenToUnderscore(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

func toCamelCase(s string) string {
	caser := cases.Title(language.English)
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		parts[i] = caser.String(parts[i])
	}
	return strings.Join(parts, "")
}

func toPascalCase(s string) string {
	return naming.ToPascalCase(s)
}

func toKebabCase(s string) string {
	return strings.ReplaceAll(s, "_", "-")
}

func pluralize(s string) string {
	if len(s) == 0 {
		return s
	}
	return inflection.Plural(s)
}

func singularize(s string) string {
	if len(s) == 0 {
		return s
	}
	return inflection.Singular(s)
}

func formatComment(s string) string {
	if s == "" {
		return ""
	}
	return "// " + s
}

func getDefault(defaultValue interface{}, actualValue interface{}) interface{} {
	if actualValue == nil {
		return defaultValue
	}
	v := reflect.ValueOf(actualValue)
	if v.IsZero() {
		return defaultValue
	}
	return actualValue
}

func add(a, b int) int {
	return a + b
}

func lastStringSlice(i int, slice interface{}) bool {
	switch s := slice.(type) {
	case []string:
		return i == len(s)-1
	default:
		return false
	}
}

func tableFromFK(fk string) string {
	parts := strings.Split(fk, ".")
	if len(parts) > 0 {
		return parts[0]
	}
	return fk
}

func columnFromFK(fk string) string {
	parts := strings.Split(fk, ".")
	if len(parts) > 1 {
		return parts[1]
	}
	return "id"
}