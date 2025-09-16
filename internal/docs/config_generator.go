package docs

import "fmt"

// ConfigGenerator produces configuration reference documentation from config proto annotations.
type ConfigGenerator struct{}

func (g *ConfigGenerator) Name() string { return "config" }

func (g *ConfigGenerator) Generate(ctx *Context) ([]GeneratedDoc, error) {
	if len(ctx.ConfigMessages) == 0 {
		return nil, nil
	}

	customDir := ""
	projectName := "Project"
	if ctx.ProjectConfig != nil {
		customDir = ctx.ProjectConfig.Docs.CustomTemplatesDir
		projectName = ctx.ProjectConfig.Name
	}

	data := map[string]any{
		"Format":      ctx.Format,
		"ProjectName": projectName,
		"Messages":    ctx.ConfigMessages,
	}

	content, err := renderDocTemplate("config_reference.md.tmpl", data, customDir)
	if err != nil {
		return nil, fmt.Errorf("render config reference: %w", err)
	}

	return []GeneratedDoc{{
		Path:    "config.md",
		Content: content,
	}}, nil
}
