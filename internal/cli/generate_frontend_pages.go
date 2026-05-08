package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// ensureFrontendComponents installs missing core UI components for all
// Next.js frontends. Called during `forge generate` so existing projects
// pick up any new core components added in newer forge versions.
func ensureFrontendComponents(cfg *config.ProjectConfig, projectDir string) {
	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") {
			continue
		}
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		frontendDir := filepath.Join(projectDir, feDir)
		if err := generator.EnsureCoreComponents(frontendDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: component install for %s failed: %v\n", fe.Name, err)
		}
	}
}

// generateFrontendPages generates CRUD page files for each entity that has
// CRUD-pattern RPCs across all Next.js frontends. Only generates pages for
// CRUD-pattern RPCs whose entity name (e.g. "Daemon" from "ListDaemons")
// matches a real entity from the proto descriptor — without that filter,
// page templates produce broken output for services whose List/Get/Create
// RPCs don't follow the entity-name-as-field convention.
func generateFrontendPages(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string, entities []codegen.EntityDef) error {
	if len(services) == 0 {
		return nil
	}

	entitySet := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		entitySet[strings.ToLower(e.Name)] = struct{}{}
	}

	// Load page templates
	listTmpl, err := loadPageTemplate("list-page.tsx.tmpl")
	if err != nil {
		return err
	}
	detailTmpl, err := loadPageTemplate("detail-page.tsx.tmpl")
	if err != nil {
		return err
	}
	createTmpl, err := loadPageTemplate("create-page.tsx.tmpl")
	if err != nil {
		return err
	}
	editTmpl, err := loadPageTemplate("edit-page.tsx.tmpl")
	if err != nil {
		return err
	}

	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") {
			continue
		}

		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		appDir := filepath.Join(projectDir, feDir, "src", "app")

		var pageCount int

		for _, svc := range services {
			pages := codegen.ExtractCRUDEntities(svc)

			for _, entity := range pages {
				// Skip RPC-name-derived entities that don't have a real
				// entity definition behind them — the page templates would
				// emit broken field references.
				if _, ok := entitySet[strings.ToLower(entity.EntityName)]; !ok {
					continue
				}
				// List page: src/app/<slug>/page.tsx
				if entity.HasList {
					if err := renderPageToFile(listTmpl, entity, filepath.Join(appDir, entity.EntitySlug, "page.tsx")); err != nil {
						return fmt.Errorf("render list page for %s: %w", entity.EntityName, err)
					}
					pageCount++
				}

				// Detail page: src/app/<slug>/[id]/page.tsx
				if entity.HasGet {
					if err := renderPageToFile(detailTmpl, entity, filepath.Join(appDir, entity.EntitySlug, "[id]", "page.tsx")); err != nil {
						return fmt.Errorf("render detail page for %s: %w", entity.EntityName, err)
					}
					pageCount++
				}

				// Create page: src/app/<slug>/new/page.tsx
				if entity.HasCreate {
					if err := renderPageToFile(createTmpl, entity, filepath.Join(appDir, entity.EntitySlug, "new", "page.tsx")); err != nil {
						return fmt.Errorf("render create page for %s: %w", entity.EntityName, err)
					}
					pageCount++
				}

				// Edit page: src/app/<slug>/[id]/edit/page.tsx
				if entity.HasUpdate {
					if err := renderPageToFile(editTmpl, entity, filepath.Join(appDir, entity.EntitySlug, "[id]", "edit", "page.tsx")); err != nil {
						return fmt.Errorf("render edit page for %s: %w", entity.EntityName, err)
					}
					pageCount++
				}
			}
		}

		if pageCount > 0 {
			fmt.Printf("  ✅ Generated %d CRUD page(s) for frontend %s\n", pageCount, fe.Name)
		}
	}

	return nil
}

// loadPageTemplate reads and parses a page template from the embedded FS.
func loadPageTemplate(name string) (*template.Template, error) {
	content, err := templates.FrontendTemplates().Get(filepath.Join("pages", name))
	if err != nil {
		return nil, fmt.Errorf("read page template %s: %w", name, err)
	}

	tmpl, err := template.New(name).Funcs(templates.FuncMap()).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse page template %s: %w", name, err)
	}

	return tmpl, nil
}

// renderPageToFile renders a template to a file, creating directories as needed.
func renderPageToFile(tmpl *template.Template, data codegen.PageTemplateData, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", outPath, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}

	return os.WriteFile(outPath, buf.Bytes(), 0o644)
}