package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateFrontendNav re-renders the sidebar navigation and dashboard page
// for each Next.js frontend using entity data derived from CRUD service
// methods. Called during `forge generate` after services are parsed.
func generateFrontendNav(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string) error {
	pages := buildNavPages(services)

	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") {
			continue
		}

		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}

		data := templates.FrontendTemplateData{
			FrontendName: fe.Name,
			ProjectName:  cfg.Name,
			Pages:        pages,
		}

		// Re-render nav component
		navContent, err := templates.FrontendTemplates.Render(
			filepath.Join("nextjs", "src", "components", "nav.tsx.tmpl"), data)
		if err != nil {
			return fmt.Errorf("render nav for %s: %w", fe.Name, err)
		}
		navDir := filepath.Join(projectDir, feDir, "src", "components")
		if err := os.MkdirAll(navDir, 0o755); err != nil {
			return fmt.Errorf("create components dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(navDir, "nav.tsx"), navContent, 0o644); err != nil {
			return fmt.Errorf("write nav.tsx: %w", err)
		}

		// Re-render dashboard page
		pageContent, err := templates.FrontendTemplates.Render(
			filepath.Join("nextjs", "src", "app", "page.tsx.tmpl"), data)
		if err != nil {
			return fmt.Errorf("render page for %s: %w", fe.Name, err)
		}
		pagePath := filepath.Join(projectDir, feDir, "src", "app", "page.tsx")
		if err := os.WriteFile(pagePath, pageContent, 0o644); err != nil {
			return fmt.Errorf("write page.tsx: %w", err)
		}

		if len(pages) > 0 {
			fmt.Printf("  ✅ Updated nav with %d page(s) for frontend %s\n", len(pages), fe.Name)
		}
	}

	return nil
}

// buildNavPages derives navigation page entries from CRUD service methods.
// For each entity that has at least one CRUD RPC, a nav entry is created.
func buildNavPages(services []codegen.ServiceDef) []templates.NavPageData {
	seen := make(map[string]bool)
	var pages []templates.NavPageData

	for _, svc := range services {
		entities := codegen.ExtractCRUDEntities(svc)
		for _, e := range entities {
			if !e.HasList {
				continue
			}
			if seen[e.EntitySlug] {
				continue
			}
			seen[e.EntitySlug] = true

			pages = append(pages, templates.NavPageData{
				Label:      e.EntityNamePlural,
				LabelLower: strings.ToLower(e.EntityNamePlural),
				Slug:       e.EntitySlug,
			})
		}
	}

	return pages
}