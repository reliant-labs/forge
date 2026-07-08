package docs

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/codegen"
)

// ArchitectureGenerator produces architecture overview documentation with Mermaid diagrams.
type ArchitectureGenerator struct{}

func (g *ArchitectureGenerator) Name() string { return "architecture" }

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
		// descriptor + owned worker/operator files + cmd/ binaries), not
		// the removed components.json manifest — see
		// codegen.IntrospectComponents. The synthesized ComponentConfigs
		// carry the Name/Kind/Path the template's EffectiveKind/IsServer/
		// PrimaryPort calls need (ports are a deploy fact, so absent).
		"Components":   codegen.IntrospectComponents(ctx.ProjectDir),
		"Frontends":    cfg.Frontends,
		"Packages":     cfg.Packages,
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
