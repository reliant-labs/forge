package docs

import (
	"fmt"
	"strings"
)

// APIGenerator produces API reference documentation from proto service definitions.
type APIGenerator struct{}

func (g *APIGenerator) Name() string { return "api" }

func (g *APIGenerator) Generate(ctx *Context) ([]GeneratedDoc, error) {
	if len(ctx.Services) == 0 {
		return nil, nil
	}

	customDir := ""
	if ctx.ProjectConfig != nil {
		customDir = ctx.ProjectConfig.Docs.CustomTemplatesDir
	}

	projectName := "Project"
	if ctx.ProjectConfig != nil {
		projectName = ctx.ProjectConfig.Name
	}

	// Generate index page
	indexData := map[string]any{
		"Format":      ctx.Format,
		"ProjectName": projectName,
		"Services":    ctx.Services,
	}

	indexContent, err := renderDocTemplate("api_index.md.tmpl", indexData, customDir)
	if err != nil {
		return nil, fmt.Errorf("render API index: %w", err)
	}

	docs := []GeneratedDoc{{
		Path:    "api/index.md",
		Content: indexContent,
	}}

	// Generate per-service pages
	for _, svc := range ctx.Services {
		svcData := map[string]any{
			"Format":  ctx.Format,
			"Service": svc,
		}

		content, err := renderDocTemplate("api_service.md.tmpl", svcData, customDir)
		if err != nil {
			return nil, fmt.Errorf("render API doc for %s: %w", svc.Name, err)
		}

		docs = append(docs, GeneratedDoc{
			Path:    fmt.Sprintf("api/%s.md", strings.ToLower(svc.Name)),
			Content: content,
		})
	}

	return docs, nil
}
