package docs

import "fmt"

// ContractGenerator produces documentation for internal package contracts.
type ContractGenerator struct{}

func (g *ContractGenerator) Name() string { return "contracts" }

func (g *ContractGenerator) Generate(ctx *Context) ([]GeneratedDoc, error) {
	if len(ctx.Contracts) == 0 {
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
		"Contracts":   ctx.Contracts,
	}

	content, err := renderDocTemplate("contract.md.tmpl", data, customDir)
	if err != nil {
		return nil, fmt.Errorf("render contract docs: %w", err)
	}

	return []GeneratedDoc{{
		Path:    "contracts.md",
		Content: content,
	}}, nil
}
