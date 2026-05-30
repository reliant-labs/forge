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
// browser-targeted frontends (nextjs + vite-spa). Called during `forge
// generate` so existing projects pick up any new core components added in
// newer forge versions.
func ensureFrontendComponents(cfg *config.ProjectConfig, projectDir string) {
	for _, fe := range cfg.Frontends {
		feType := strings.ToLower(strings.TrimSpace(fe.Type))
		if feType != "nextjs" && feType != "vite-spa" {
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
// CRUD-pattern RPCs across all browser-targeted frontends (nextjs + vite-spa).
// Only generates pages for CRUD-pattern RPCs whose entity name (e.g.
// "Daemon" from "ListDaemons") matches a real entity from the proto
// descriptor — without that filter, page templates produce broken output for
// services whose List/Get/Create RPCs don't follow the entity-name-as-field
// convention.
//
// Per-kind dispatch:
//   - nextjs:   pages/ templates → src/app/<slug>/[id]/{,edit/}page.tsx
//   - vite-spa: vite-spa-pages/ templates → src/pages/<slug>/{List,Detail,Create,Edit}.tsx
func generateFrontendPages(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string, entities []codegen.EntityDef) error {
	if len(services) == 0 {
		return nil
	}

	entitySet := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		entitySet[strings.ToLower(e.Name)] = struct{}{}
	}

	for _, fe := range cfg.Frontends {
		feType := strings.ToLower(strings.TrimSpace(fe.Type))
		if feType != "nextjs" && feType != "vite-spa" {
			continue
		}

		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}

		layout, err := pageLayoutForKind(feType)
		if err != nil {
			return err
		}

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
				if entity.HasList {
					if err := renderPageToFile(layout.listTmpl, entity, filepath.Join(projectDir, feDir, layout.listPath(entity.EntitySlug))); err != nil {
						return fmt.Errorf("render list page for %s: %w", entity.EntityName, err)
					}
					pageCount++
				}

				if entity.HasGet {
					if err := renderPageToFile(layout.detailTmpl, entity, filepath.Join(projectDir, feDir, layout.detailPath(entity.EntitySlug))); err != nil {
						return fmt.Errorf("render detail page for %s: %w", entity.EntityName, err)
					}
					pageCount++
				}

				if entity.HasCreate {
					if err := renderPageToFile(layout.createTmpl, entity, filepath.Join(projectDir, feDir, layout.createPath(entity.EntitySlug))); err != nil {
						return fmt.Errorf("render create page for %s: %w", entity.EntityName, err)
					}
					pageCount++
				}

				if entity.HasUpdate {
					if err := renderPageToFile(layout.editTmpl, entity, filepath.Join(projectDir, feDir, layout.editPath(entity.EntitySlug))); err != nil {
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

// pageLayout bundles parsed templates with the per-kind output-path policy
// used when emitting CRUD pages. Output paths are framework-specific
// (Next.js App Router uses [id]/page.tsx routes; tanstack-router code-based
// routing has no on-disk route convention so we write to src/pages/).
type pageLayout struct {
	listTmpl   *template.Template
	detailTmpl *template.Template
	createTmpl *template.Template
	editTmpl   *template.Template

	listPath   func(slug string) string
	detailPath func(slug string) string
	createPath func(slug string) string
	editPath   func(slug string) string
}

// pageLayoutForKind returns the parsed templates and path policy for the
// given frontend kind. The kind is the resolved `Type` field on the
// frontend config ("nextjs" or "vite-spa").
func pageLayoutForKind(feType string) (*pageLayout, error) {
	switch feType {
	case "nextjs":
		listTmpl, err := loadPageTemplate("pages", "list-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		detailTmpl, err := loadPageTemplate("pages", "detail-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		createTmpl, err := loadPageTemplate("pages", "create-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		editTmpl, err := loadPageTemplate("pages", "edit-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		appDir := filepath.Join("src", "app")
		return &pageLayout{
			listTmpl: listTmpl, detailTmpl: detailTmpl, createTmpl: createTmpl, editTmpl: editTmpl,
			listPath:   func(slug string) string { return filepath.Join(appDir, slug, "page.tsx") },
			detailPath: func(slug string) string { return filepath.Join(appDir, slug, "[id]", "page.tsx") },
			createPath: func(slug string) string { return filepath.Join(appDir, slug, "new", "page.tsx") },
			editPath:   func(slug string) string { return filepath.Join(appDir, slug, "[id]", "edit", "page.tsx") },
		}, nil
	case "vite-spa":
		listTmpl, err := loadPageTemplate("vite-spa-pages", "list-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		detailTmpl, err := loadPageTemplate("vite-spa-pages", "detail-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		createTmpl, err := loadPageTemplate("vite-spa-pages", "create-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		editTmpl, err := loadPageTemplate("vite-spa-pages", "edit-page.tsx.tmpl")
		if err != nil {
			return nil, err
		}
		pagesDir := filepath.Join("src", "pages")
		return &pageLayout{
			listTmpl: listTmpl, detailTmpl: detailTmpl, createTmpl: createTmpl, editTmpl: editTmpl,
			listPath:   func(slug string) string { return filepath.Join(pagesDir, slug, "List.tsx") },
			detailPath: func(slug string) string { return filepath.Join(pagesDir, slug, "Detail.tsx") },
			createPath: func(slug string) string { return filepath.Join(pagesDir, slug, "Create.tsx") },
			editPath:   func(slug string) string { return filepath.Join(pagesDir, slug, "Edit.tsx") },
		}, nil
	default:
		return nil, fmt.Errorf("unsupported frontend type for page generation: %q", feType)
	}
}

// loadPageTemplate reads and parses a page template from the embedded FS.
// `dir` is the per-kind template subdirectory under internal/templates/frontend/
// (e.g. "pages" for nextjs, "vite-spa-pages" for vite-spa).
func loadPageTemplate(dir, name string) (*template.Template, error) {
	content, err := templates.FrontendTemplates().Get(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("read page template %s/%s: %w", dir, name, err)
	}

	tmpl, err := template.New(name).Funcs(templates.FuncMap()).Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse page template %s/%s: %w", dir, name, err)
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