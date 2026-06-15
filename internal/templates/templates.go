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
	"unicode"

	"github.com/jinzhu/inflection"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/reliant-labs/forge/internal/naming"
)

//go:embed all:project all:deploy all:frontend all:ci all:test all:ingress service/*.tmpl middleware/*.tmpl all:internal-package webhook/*.tmpl worker/*.tmpl worker-cron/*.tmpl operator/*.tmpl crd/*.tmpl
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

// TemplateCategory provides Get, Render, and List operations for a specific
// template directory within the embedded filesystem.
type TemplateCategory struct {
	basePath string
}

// Get returns the raw bytes of a template file. Any //go:build ignore
// directives are stripped from the output.
func (c TemplateCategory) Get(name string) ([]byte, error) {
	data, err := templateFS.ReadFile(path.Join(c.basePath, filepath.ToSlash(name)))
	if err != nil {
		return nil, err
	}
	return stripBuildIgnore(data), nil
}

// Render executes a template with the given data and returns the result.
func (c TemplateCategory) Render(name string, data interface{}) ([]byte, error) {
	return RenderFromFS(templateFS, c.basePath, name, data)
}

// List returns all template names in the category (recursive).
func (c TemplateCategory) List(subdir string) ([]string, error) {
	return listTemplates(path.Join(c.basePath, subdir), true)
}

// ListFlat returns only direct children (non-recursive).
func (c TemplateCategory) ListFlat(subdir string) ([]string, error) {
	return listTemplates(path.Join(c.basePath, subdir), false)
}

// Category accessors return a TemplateCategory rooted at the named
// embedded template directory.
//
// These were package-level vars in forge; they were converted to functions
// to satisfy `contracts.allow_exported_vars: false` while keeping the
// call-site shape close to the original (`ProjectTemplates()` instead of
// `ProjectTemplates`). TemplateCategory is a value type with no internal
// state, so reconstructing on each call is free.

// ProjectTemplates returns the category for project-scaffold templates.
func ProjectTemplates() TemplateCategory { return TemplateCategory{basePath: "project"} }

// FrontendTemplates returns the category for frontend-scaffold templates.
func FrontendTemplates() TemplateCategory { return TemplateCategory{basePath: "frontend"} }

// DeployTemplates returns the category for deploy-scaffold templates.
func DeployTemplates() TemplateCategory { return TemplateCategory{basePath: "deploy"} }

// IngressTemplates returns the embedded Gateway API install assets
// (vendored Traefik install, GatewayClass) keyed under
// `internal/templates/ingress/<provider>/`. The Gateway API CRDs
// themselves are fetched from the upstream GitHub release at
// `forge dev cluster up` time and cached under ~/.cache/forge/ —
// see internal/cli/dev_cluster_ingress.go for the download path.
func IngressTemplates() TemplateCategory { return TemplateCategory{basePath: "ingress"} }

// TestTemplates returns the category for test-scaffold templates.
func TestTemplates() TemplateCategory { return TemplateCategory{basePath: "test"} }

// InternalPkgTemplates returns the category for shared internal-package templates.
func InternalPkgTemplates() TemplateCategory {
	return TemplateCategory{basePath: "internal-package"}
}

// ServiceTemplates returns the category for service-scaffold templates.
func ServiceTemplates() TemplateCategory { return TemplateCategory{basePath: "service"} }

// WebhookTemplates returns the category for webhook-scaffold templates.
func WebhookTemplates() TemplateCategory { return TemplateCategory{basePath: "webhook"} }

// MiddlewareTemplates returns the category for middleware-scaffold templates.
func MiddlewareTemplates() TemplateCategory { return TemplateCategory{basePath: "middleware"} }

// WorkerTemplates returns the category for worker-scaffold templates.
func WorkerTemplates() TemplateCategory { return TemplateCategory{basePath: "worker"} }

// WorkerCronTemplates returns the category for worker-cron-scaffold templates.
func WorkerCronTemplates() TemplateCategory { return TemplateCategory{basePath: "worker-cron"} }

// OperatorTemplates returns the category for operator-scaffold templates.
func OperatorTemplates() TemplateCategory { return TemplateCategory{basePath: "operator"} }

// CITemplates returns a TemplateCategory for a specific CI provider.
func CITemplates(provider string) TemplateCategory {
	return TemplateCategory{basePath: path.Join("ci", provider)}
}

// InternalPkgKindTemplates returns a TemplateCategory for a specific
// internal-package kind subdirectory.
func InternalPkgKindTemplates(kind string) TemplateCategory {
	return TemplateCategory{basePath: path.Join("internal-package", kind)}
}

// ListInternalPackageKindTemplates lists template files for a specific
// internal-package kind subdirectory (e.g. "client", "eventbus").
func ListInternalPackageKindTemplates(kind string) ([]string, error) {
	return InternalPkgKindTemplates(kind).ListFlat("")
}

// RenderInternalPackageKindTemplate renders a template from a kind subdirectory
// of the internal-package templates.
func RenderInternalPackageKindTemplate(kind, name string, data interface{}) ([]byte, error) {
	return InternalPkgKindTemplates(kind).Render(name, data)
}

// RenderInternalPackageTemplate renders a base internal-package template.
func RenderInternalPackageTemplate(name string, data interface{}) ([]byte, error) {
	return InternalPkgTemplates().Render(name, data)
}

// RenderFromFS renders a template from an arbitrary fs.FS. It reads the file at
// basePath/name, and if the name has a .tmpl suffix it parses and executes it
// with the shared FuncMap. Non-.tmpl files are returned as-is. Any //go:build
// ignore directives are stripped from the output.
//
// This is the canonical template-rendering function used by both the built-in
// template helpers and the pack system.
func RenderFromFS(fsys fs.FS, basePath, name string, data interface{}) ([]byte, error) {
	content, err := fs.ReadFile(fsys, path.Join(basePath, filepath.ToSlash(name)))
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

// listTemplates walks the embedded template FS and returns template names under root.
// If recursive is true, it walks subdirectories. Otherwise, only lists direct children.
func listTemplates(root string, recursive bool) ([]string, error) {
	entries, err := templateFS.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read template dir %s: %w", root, err)
	}

	var files []string
	var walk func(dir string, entries []fs.DirEntry) error
	walk = func(dir string, entries []fs.DirEntry) error {
		for _, e := range entries {
			rel := path.Join(dir, e.Name())
			if e.IsDir() {
				if !recursive {
					continue
				}
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

// CIWorkflowData holds data for the spec-driven CI workflow template.
type CIWorkflowData struct {
	ProjectName  string
	GoVersion    string // e.g. "1.26"
	HasFrontends bool
	Frontends    []FrontendCIConfig // from project config
	HasServices  bool

	// Lint
	LintGolangci        bool
	LintBuf             bool
	LintBufBreaking     bool
	LintFrontend        bool
	LintFrontendStyles  bool
	LintMigrationSafety bool

	// Test
	TestRace     bool
	TestCoverage bool

	// Vuln scan
	VulnGo     bool // govulncheck
	VulnDocker bool // trivy
	VulnNPM    bool // npm audit

	// License compliance
	LicenseCheck bool // go-licenses

	// E2E
	E2EEnabled bool
	E2ERuntime string // "docker-compose" or "k3d"

	// Permissions
	PermContents string // default "read"

	// Extra jobs
	ExtraJobs []CIExtraJob

	// Deploy-related
	HasKCL    bool // validate KCL manifests
	HasDocker bool // emit docker-build job (project has a Dockerfile)
	// VerifyGenerated controls whether the verify-generated job is emitted.
	// Service projects always emit it (forge regenerates handlers/middleware
	// and we want CI to flag drift). CLI/library projects have very little
	// generated output worth verifying, so the job is suppressed.
	VerifyGenerated bool

	// Environments (for KCL validation)
	Environments []string

	// Legacy fields used by other CI templates (build-images, deploy, dependabot)
	Module       string
	Registry     string // "ghcr", "gar", "ecr"
	GithubOrg    string
	FrontendName string // first frontend name for dependabot

	// GitHubOwner is the inferred GitHub owner for default CODEOWNERS entries.
	// Empty when we couldn't confidently infer one (e.g. non-github module
	// path), in which case the CODEOWNERS template emits no file contents and
	// the generator drops the file. Populated by the generator from the
	// project's module path (e.g. "github.com/example/demo" -> "example").
	GitHubOwner string

	// ForgeVersion is the version of the forge CLI that produced the scaffold.
	// Used to pin `go install` in the verify-generated CI job so the
	// regeneration step is reproducible across runs. Empty or "dev" falls
	// back to ForgeGitCommit (when known), then to the pinned default.
	ForgeVersion string

	// ForgeGitCommit is the git commit SHA the forge binary was built from.
	// Used as a fallback for `go install` pinning when ForgeVersion is "dev"
	// (local builds). A full SHA is a valid `go install ...@<ref>` target,
	// so dev-built scaffolds remain reproducible.
	ForgeGitCommit string
}

// FrontendCIConfig is a minimal frontend descriptor for CI templates.
type FrontendCIConfig struct {
	Name string
	Path string
}

// CIExtraJob defines an additional user-specified CI job.
type CIExtraJob struct {
	Name   string
	Needs  []string
	RunsOn string
	Steps  []CIExtraStep
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
	// FrontendPath points the setup-node `node-version-file` input at a
	// package.json whose `engines.node` is honored by setup-node@v4. Empty
	// when there are no frontends — the template then falls back to a
	// fixed node-version.
	FrontendPath string
}

// NavPageData describes a single page entry for the frontend navigation.
type NavPageData struct {
	Label      string // display name, e.g. "Tasks"
	LabelLower string // lowercase for descriptions, e.g. "tasks"
	Slug       string // URL path segment, e.g. "tasks"
	// LabelSingular is the singular display name ("Task") — used by
	// "Create X" buttons. Derived from the proto entity name, not a
	// strip-the-trailing-s guess ("Categories" → "Category", not
	// "Categorie").
	LabelSingular string
	// HasCreate reports whether the page generator emitted a create page
	// (<slug>/new) for this entity. The dashboard's QuickActions grid
	// gates its "Create X" buttons on this — advertising a create route
	// the page generator never wrote is a guaranteed 404.
	HasCreate bool
	// ListHook is the generated React Query list hook name
	// ("useListTasks"); the dashboard tile uses it to render a real
	// entity count instead of a placeholder.
	ListHook string
	// HooksModule is the import path of the generated hooks file, e.g.
	// "@/hooks/task-service-hooks".
	HooksModule string
	// ItemsField is the camelCase repeated field on the list response
	// holding the entities ("tasks").
	ItemsField string
	// ComponentIdent is a valid TS identifier for per-entity generated
	// components ("Tasks" → TasksTile).
	ComponentIdent string
}

// NavHookImport is one merged import statement the dashboard template
// emits for list hooks: all hook symbols whose generated hooks file is
// the same module. Pre-grouped in Go so two entities served by one
// service don't produce duplicate import statements (import/no-duplicates).
type NavHookImport struct {
	Module  string   // "@/hooks/task-service-hooks"
	Symbols []string // ["useListTasks", "useListProjects"] — sorted
}

// FrontendTemplateData holds data for frontend template rendering.
type FrontendTemplateData struct {
	FrontendName string
	ProjectName  string
	// ApiUrl / ApiPort / ApiPackage are exposed to text/template files
	// under internal/templates/frontend/. Renaming to APIUrl / APIPort /
	// APIPackage would force a coordinated rename of dozens of .tmpl
	// files (vite, react-native, nextjs scaffolds) and break any
	// downstream project that references {{.ApiUrl}} in a customized
	// template. Keep the mixed-case field names; the revive var-naming
	// rule is suppressed on each line.
	ApiUrl  string //nolint:revive // template field name; see comment above
	ApiPort string //nolint:revive // template field name; see comment above
	Module  string
	Pages   []NavPageData
	// NavHookImports is the per-module aggregation of the list hooks the
	// dashboard tiles consume. Derived from Pages by the nav generator.
	NavHookImports []NavHookImport
	// Workspaces reports whether the project opted into the pnpm-
	// workspaces layout. When true, frontend templates emit imports
	// of ApiPackage / HooksPackage instead of relative @/gen and
	// @/hooks paths, and the per-frontend package.json declares
	// "workspace:*" deps on those packages.
	//
	// When false (the default), templates produce the historic
	// single-frontend output — byte-identical to projects scaffolded
	// before workspaces landed. Snapshot tests rely on this.
	Workspaces bool
	// ApiPackage is the npm package name for the shared Connect TS
	// clients workspace, e.g. "@myapp/api". Empty when Workspaces is
	// false.
	ApiPackage string //nolint:revive // template field name; matches ApiUrl above
	// HooksPackage is the npm package name for the shared React Query
	// hooks workspace, e.g. "@myapp/hooks". Empty when Workspaces is
	// false.
	HooksPackage string
	// UIWebPackage is the npm package name for the shared web UI
	// component workspace, e.g. "@myapp/ui-web". Empty when Workspaces
	// is false. Frontend package.json templates declare a
	// "workspace:*" dep on this, and tsconfig templates redirect
	// `@/components/*` paths into the package via path mapping.
	UIWebPackage string
	// UINativePackage is the npm package name for the shared React
	// Native primitives workspace, e.g. "@myapp/ui-native". Empty
	// when Workspaces is false OR the project has no RN frontend.
	// React Native templates reference this in their package.json
	// workspace deps and example screens.
	UINativePackage string
	// Output selects the Next.js build/runtime shape. Mirrors
	// config.FrontendConfig.Output. Only the nextjs templates read it
	// (`next.config.ts.tmpl`); other template trees ignore it.
	//
	// Valid values rendered by templates:
	//   - "standalone" (default): emit `output: "standalone"`
	//     unconditionally — pairs with the shipped Dockerfile and
	//     supports the generated dynamic `[id]` CRUD routes.
	//   - "static":     emit `output: "export"` gated on
	//     NODE_ENV=production. Dev server stays unchanged. Opt-in
	//     only: static export fails `next build` on dynamic route
	//     segments without generateStaticParams, which the generated
	//     CRUD detail/edit pages cannot provide.
	//   - "server":     omit `output` entirely (full Next.js dev+prod).
	//
	// Empty string is treated as "standalone" by the template — callers
	// that don't thread this field get the scaffold default.
	Output string
	// BasePath mirrors config.FrontendConfig.BasePath — the URL prefix
	// the frontend is mounted under (e.g. "/admin"), or "" for root.
	// Read by the nextjs templates only:
	//
	//   - next.config.ts.tmpl renders it as the build-time default for
	//     `basePath` + `assetPrefix` (overridable via the single
	//     canonical env var NEXT_PUBLIC_BASE_PATH).
	//   - src/lib/basepath_gen.ts.tmpl bakes it as the fallback for
	//     BASE_PATH / joinBasePath().
	//
	// Already validated by config.LoadStrict (leading "/", no trailing
	// "/", [A-Za-z0-9._-] segments) — templates splice it verbatim into
	// TypeScript string literals.
	BasePath string
}

// WebhookTemplateData holds data for webhook template rendering.
type WebhookTemplateData struct {
	Name           string // webhook name (e.g. "stripe", "github")
	ServiceName    string // target service display name (may contain hyphens)
	ServicePackage string // Go-package-safe form of ServiceName (snake_case)
	Module         string // Go module path
}

// WebhookRoutesTemplateData holds data for the webhook_routes_gen.go template.
type WebhookRoutesTemplateData struct {
	Package  string                  // Go package name (e.g. "billing")
	Webhooks []WebhookRouteEntryData // all webhooks for this service
}

// WebhookRouteEntryData holds per-webhook data for route generation.
type WebhookRouteEntryData struct {
	Name       string // kebab-case name for the URL path (e.g. "stripe")
	PascalName string // PascalCase name for the handler method (e.g. "Stripe")
}

// TemplateEngine handles code generation from service/middleware templates.
// NOTE: TemplateEngine pre-parses templates for reuse via a singleton (see generator/project.go),
// while the TemplateCategory.Render method parses on each call. Both share FuncMap().
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
		"worker/worker.go.tmpl",
		"worker/worker_test.go.tmpl",
		"worker-cron/worker.go.tmpl",
		"worker-cron/worker_test.go.tmpl",
		"operator/types.go.tmpl",
		"operator/controller.go.tmpl",
		"operator/controller_test.go.tmpl",
		"crd/groupversion.go.tmpl",
		"crd/types.go.tmpl",
		"crd/controller.go.tmpl",
		"crd/controller_test.go.tmpl",
		"crd/crd.k.tmpl",
		"function/function.go.tmpl",
		"function/function_test.go.tmpl",
	}

	for _, file := range templateFiles {
		content, err := templateFS.ReadFile(file)
		if err != nil {
			// Template doesn't exist yet, skip
			continue
		}
		content = stripBuildIgnore(content)

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

	return string(stripBuildIgnore(buf.Bytes())), nil
}

// Case conversion functions

// hyphenToUnderscore replaces hyphens with underscores.
// Used in templates for proto package names where hyphens aren't valid.
func hyphenToUnderscore(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

func toCamelCase(s string) string {
	if s == "" {
		return s
	}
	// Handle PascalCase input (no underscores): just lowercase the first letter.
	if !strings.Contains(s, "_") {
		runes := []rune(s)
		runes[0] = unicode.ToLower(runes[0])
		return string(runes)
	}
	// Handle snake_case input: capitalize each part except the first.
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
	return naming.Pluralize(s)
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
