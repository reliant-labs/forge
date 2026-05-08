package docs

import "fmt"

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
		"Format":       ctx.Format,
		"ProjectName":  cfg.Name,
		"Services":     cfg.Services,
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
