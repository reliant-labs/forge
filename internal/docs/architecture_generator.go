package docs

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/codegen"
)

// ArchitectureGenerator produces architecture overview documentation with Mermaid diagrams.
type ArchitectureGenerator struct{}

// Name returns the generator's registry key, 'architecture'.
func (g *ArchitectureGenerator) Name() string { return "architecture" }

// Generate produces the architecture overview page.
func (g *ArchitectureGenerator) Generate(ctx *Context) ([]GeneratedDoc, error) {
	if ctx.ProjectConfig == nil {
		return nil, nil
	}

	cfg := ctx.ProjectConfig
	customDir := cfg.Docs.CustomTemplatesDir

	// Build frontend name for Mermaid references
	frontendName := ""
	if len(cfg.Frontends) > 0 {
		frontendName = cfg.Frontends[0].Name
	}

	data := map[string]any{
		"Format":      ctx.Format,
		"ProjectName": cfg.Name,
		// Component inventory is enumerated from the REAL sources (proto
		// descriptor + owned worker/operator files + cmd/ binaries) — see
		// codegen.IntrospectComponents. The synthesized ComponentConfigs
		// carry the Name/Kind/Path the template's EffectiveKind/IsServer
		// calls need. There is no port: a port is a deploy fact declared in
		// KCL, so a component never carries one.
		"Components": codegen.IntrospectComponents(ctx.ProjectDir),
		"Frontends":  cfg.Frontends,
		// Internal packages are DISCOVERED from internal/<pkg>/contract.go —
		// the same walk that wires them into internal/app/compose.go, so the
		// architecture doc lists the packages the binary actually builds.
		"Packages":     internalPackages(ctx.ProjectDir),
		"Database":     cfg.Database,
		"FrontendName": frontendName,
	}

	content, err := renderDocTemplate("architecture.md.tmpl", data, customDir)
	if err != nil {
		return nil, fmt.Errorf("render architecture doc: %w", err)
	}

	return []GeneratedDoc{{
		Path:    "architecture.md",
		Content: content,
	}}, nil
}

// internalPackages resolves the project's contract packages for the
// architecture template. Discovery failure yields no rows — the doc drops
// the "Internal Packages" section rather than failing the whole docs run
// over a tree the generate pipeline reports on in far more detail.
func internalPackages(projectDir string) []codegen.InternalPackage {
	pkgs, err := codegen.DiscoverInternalPackages(projectDir)
	if err != nil {
		return nil
	}
	return pkgs
}
