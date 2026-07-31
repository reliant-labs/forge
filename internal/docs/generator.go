// Package docs provides automated documentation generation from project metadata.
// It parses proto definitions, Go AST contracts, and project config to produce
// markdown or Hugo-compatible documentation.
package docs

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/internal/templates"
)

//go:embed templates/*.tmpl
var docsTemplateFS embed.FS

// Generator produces documentation files from project metadata.
type Generator interface {
	// Name returns the generator's identifier (e.g. "api", "architecture").
	Name() string

	// Generate produces documentation from the given context.
	Generate(ctx *Context) ([]GeneratedDoc, error)
}

// GeneratedDoc represents a single generated documentation file.
type GeneratedDoc struct {
	Path    string // relative path within output dir (e.g. "api/index.md")
	Content []byte
}

// Context holds all pre-parsed project data that generators use.
type Context struct {
	ProjectConfig  *config.ProjectConfig
	Services       []codegen.ServiceDef
	ConfigMessages []codegen.ConfigMessage
	Contracts      []*ContractInfo
	Format         string // "markdown" or "hugo"
	ProjectDir     string
	ModulePath     string
}

// ContractInfo holds a parsed contract with its package path.
type ContractInfo struct {
	PackageName string
	Contract    *contract.File
}

// Registry holds named generators that can be selectively enabled.
type Registry struct {
	generators map[string]Generator
	order      []string // preserves registration order
}

// NewRegistry creates an empty generator registry.
func NewRegistry() *Registry {
	return &Registry{
		generators: make(map[string]Generator),
	}
}

// Register adds a generator to the registry.
func (r *Registry) Register(g Generator) {
	r.generators[g.Name()] = g
	r.order = append(r.order, g.Name())
}

// Get returns a generator by name, or nil if not found.
func (r *Registry) Get(name string) Generator {
	return r.generators[name]
}

// Names returns all registered generator names in registration order.
func (r *Registry) Names() []string {
	return r.order
}

// DefaultRegistry returns a registry with all built-in generators.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(&APIGenerator{})
	r.Register(&ArchitectureGenerator{})
	r.Register(&ConfigGenerator{})
	r.Register(&ContractGenerator{})
	return r
}

// Run executes documentation generation for the given project.
func Run(projectDir string, cfg *config.ProjectConfig, overrides *Overrides) error {
	docsCfg := cfg.Docs
	if overrides != nil {
		overrides.Apply(&docsCfg)
	}

	// Disabled is a refusal, not a no-op. `forge docs generate` has exactly
	// one caller — a user who just typed it — so returning nil here printed
	// an informational line under a "✅ complete!" trailer and exited 0
	// having written nothing. Name the switch instead.
	if !docsCfg.IsEnabled() {
		return fmt.Errorf("documentation generation is disabled (docs.enabled: false in forge.yaml) — set it to true to generate docs")
	}

	ctx, err := buildContext(projectDir, cfg, docsCfg)
	if err != nil {
		return fmt.Errorf("build docs context: %w", err)
	}

	registry := DefaultRegistry()
	enabledGenerators, err := resolveGenerators(registry, docsCfg.Generators)
	if err != nil {
		return err
	}

	var allDocs []GeneratedDoc
	for _, g := range enabledGenerators {
		fmt.Printf("  📝 Generating %s docs...\n", g.Name())
		docs, err := g.Generate(ctx)
		if err != nil {
			return fmt.Errorf("generator %s: %w", g.Name(), err)
		}
		allDocs = append(allDocs, docs...)
	}

	outputDir := filepath.Join(projectDir, docsCfg.EffectiveOutputDir())
	if err := writeDocs(allDocs, outputDir); err != nil {
		return fmt.Errorf("write docs: %w", err)
	}

	fmt.Printf("  ✅ Generated %d doc file(s) in %s/\n", len(allDocs), docsCfg.EffectiveOutputDir())
	return nil
}

// Overrides allows CLI flags to override config values.
type Overrides struct {
	OutputDir  string
	Format     string
	Generators []string
}

// Apply merges overrides into the docs config.
func (o *Overrides) Apply(cfg *config.DocsConfig) {
	if o.OutputDir != "" {
		cfg.OutputDir = o.OutputDir
	}
	if o.Format != "" {
		cfg.Format = o.Format
	}
	if len(o.Generators) > 0 {
		cfg.Generators = o.Generators
	}
}

// buildContext parses all project metadata into a Context.
func buildContext(projectDir string, cfg *config.ProjectConfig, docsCfg config.DocsConfig) (*Context, error) {
	ctx := &Context{
		ProjectConfig: cfg,
		Format:        docsCfg.EffectiveFormat(),
		ProjectDir:    projectDir,
	}

	// Parse module path
	modulePath, err := codegen.GetModulePath(projectDir)
	if err == nil {
		ctx.ModulePath = modulePath
	}

	// Parse proto services
	servicesDir := filepath.Join(projectDir, "proto", "services")
	if dirExists(servicesDir) {
		services, err := codegen.ParseServicesFromProtos(servicesDir, projectDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  Could not parse proto services: %v\n", err)
		} else {
			ctx.Services = services
		}
	}

	// Parse config protos
	configDir := filepath.Join(projectDir, "proto", "config")
	if dirExists(configDir) {
		messages, err := codegen.ParseConfigProtosFromDir(configDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  Could not parse config protos: %v\n", err)
		} else {
			ctx.ConfigMessages = messages
		}
	}

	// Parse contract.go files
	internalDir := filepath.Join(projectDir, "internal")
	if dirExists(internalDir) {
		entries, err := os.ReadDir(internalDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				contractPath := filepath.Join(internalDir, entry.Name(), "contract.go")
				if _, err := os.Stat(contractPath); os.IsNotExist(err) {
					continue
				}
				cf, err := contract.ParseContract(contractPath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  ⚠️  Could not parse contract %s: %v\n", entry.Name(), err)
					continue
				}
				ctx.Contracts = append(ctx.Contracts, &ContractInfo{
					PackageName: entry.Name(),
					Contract:    cf,
				})
			}
		}
	}

	return ctx, nil
}

// resolveGenerators returns the generators to run, filtering by the enabled list.
// If enabledNames is empty, all registered generators are returned.
// resolveGenerators maps the configured/`--generators` names onto registered
// generators. An unknown name is an ERROR, not a silent drop: dropping it
// selected zero generators, wrote zero files, and still printed "Generated 0
// doc file(s)" followed by "Documentation generation complete!" — success for
// a run that produced nothing, and no way for the caller to tell a typo from a
// project that genuinely has nothing to document.
func resolveGenerators(registry *Registry, enabledNames []string) ([]Generator, error) {
	if len(enabledNames) == 0 {
		var all []Generator
		for _, name := range registry.Names() {
			all = append(all, registry.Get(name))
		}
		return all, nil
	}

	var (
		result  []Generator
		unknown []string
	)
	for _, name := range enabledNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		g := registry.Get(name)
		if g == nil {
			unknown = append(unknown, name)
			continue
		}
		result = append(result, g)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown doc generator(s) %s; available: %s",
			strings.Join(unknown, ", "), strings.Join(registry.Names(), ", "))
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no doc generators selected; available: %s", strings.Join(registry.Names(), ", "))
	}
	return result, nil
}

// writeDocs writes generated doc files to the output directory.
func writeDocs(docs []GeneratedDoc, outputDir string) error {
	for _, doc := range docs {
		path := filepath.Join(outputDir, doc.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Errorf("create dir for %s: %w", doc.Path, err)
		}
		if err := os.WriteFile(path, doc.Content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", doc.Path, err)
		}
	}
	return nil
}

// renderDocTemplate renders an embedded doc template, with optional user override.
func renderDocTemplate(name string, data any, customDir string) ([]byte, error) {
	var tmplContent []byte

	// A user override wins when it is present. A missing or unreadable
	// override is not an error — customDir is an optional directory that
	// need only hold the templates the user chose to override, so any
	// read failure falls through to the embedded copy below.
	if customDir != "" {
		if override, readErr := os.ReadFile(filepath.Join(customDir, name)); readErr == nil {
			tmplContent = override
		}
	}

	if tmplContent == nil {
		embedded, err := docsTemplateFS.ReadFile(filepath.Join("templates", name))
		if err != nil {
			return nil, fmt.Errorf("read doc template %s: %w", name, err)
		}
		tmplContent = embedded
	}

	funcMap := templates.FuncMap()
	funcMap["streamingType"] = streamingType
	funcMap["indent"] = indent
	funcMap["hasPrefix"] = strings.HasPrefix

	tmpl, err := template.New(name).Funcs(funcMap).Parse(string(tmplContent))
	if err != nil {
		return nil, fmt.Errorf("parse doc template %s: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute doc template %s: %w", name, err)
	}

	return buf.Bytes(), nil
}

// streamingType returns a human-readable streaming description.
func streamingType(clientStreaming, serverStreaming bool) string {
	switch {
	case clientStreaming && serverStreaming:
		return "Bidirectional Streaming"
	case clientStreaming:
		return "Client Streaming"
	case serverStreaming:
		return "Server Streaming"
	default:
		return "Unary"
	}
}

// indent prepends each line with the given number of spaces.
func indent(spaces int, s string) string {
	pad := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = pad + line
		}
	}
	return strings.Join(lines, "\n")
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
