package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateFrontendNav re-renders the sidebar navigation and dashboard page
// for each Next.js frontend using entity data derived from CRUD service
// methods. Called during `forge generate` after services are parsed.
//
// Layout: each Next.js frontend gets two pairs of files —
//
//   - components/nav_gen.tsx (Tier-1, banner): pure-data RouteSpec[]
//     export, regenerated every run from forge.yaml's services.
//   - components/nav.tsx (Tier-2, scaffold-once): user-owned
//     presentation that imports ALL_ROUTES from nav_gen and renders the
//     curated subset.
//   - app/dashboard_gen.tsx (Tier-1, banner): EntityTiles + QuickActions
//     React components, regenerated every run.
//   - app/page.tsx (Tier-2, scaffold-once): user-owned dashboard root
//     that composes the generated tile/action components.
//
// The split lets users hand-edit nav.tsx + page.tsx (icon palette, route
// pruning, custom widgets) without forge overwriting them on the next
// `forge generate` — the Tier-1 guard in checksums.go prevents accidental
// stomps on the gen files too. New entities flowing into the nav need
// zero user action: nav_gen.tsx picks them up automatically and the
// user's nav.tsx maps over ALL_ROUTES so they appear in the sidebar.
//
// `force` is plumbed through (forge generate --force) for users who
// explicitly want to clobber the Tier-2 files and re-scaffold from the
// templates.
func generateFrontendNav(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string, cs *checksums.FileChecksums, force bool) error {
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

		if err := os.MkdirAll(filepath.Join(projectDir, feDir, "src", "components"), 0o755); err != nil {
			return fmt.Errorf("create components dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(projectDir, feDir, "src", "app"), 0o755); err != nil {
			return fmt.Errorf("create app dir: %w", err)
		}

		// ── Tier-1: nav_gen.tsx (always regenerated) ──
		navGenContent, err := templates.FrontendTemplates().Render(
			filepath.Join("nextjs", "src", "components", "nav_gen.tsx.tmpl"), data)
		if err != nil {
			return fmt.Errorf("render nav_gen.tsx for %s: %w", fe.Name, err)
		}
		navGenRel := filepath.Join(feDir, "src", "components", "nav_gen.tsx")
		if _, err := checksums.WriteGeneratedFileTier1(projectDir, navGenRel, navGenContent, cs, true); err != nil {
			return fmt.Errorf("write nav_gen.tsx: %w", err)
		}

		// ── Tier-1: dashboard_gen.tsx (always regenerated) ──
		dashGenContent, err := templates.FrontendTemplates().Render(
			filepath.Join("nextjs", "src", "app", "dashboard_gen.tsx.tmpl"), data)
		if err != nil {
			return fmt.Errorf("render dashboard_gen.tsx for %s: %w", fe.Name, err)
		}
		dashGenRel := filepath.Join(feDir, "src", "app", "dashboard_gen.tsx")
		if _, err := checksums.WriteGeneratedFileTier1(projectDir, dashGenRel, dashGenContent, cs, true); err != nil {
			return fmt.Errorf("write dashboard_gen.tsx: %w", err)
		}

		// ── Tier-2: nav.tsx (user-owned, scaffold-once) ──
		// Only emit when the file is missing — once the user has it on
		// disk, we never overwrite (even on --force) unless explicitly
		// asked, because the user may have hand-curated it. The Tier-1
		// guard separately catches stomps on nav_gen.tsx.
		navRel := filepath.Join(feDir, "src", "components", "nav.tsx")
		if err := emitTier2OnceIfMissing(projectDir, navRel, "nextjs/src/components/nav.tsx.tmpl", data, force); err != nil {
			return err
		}

		// ── Tier-2: page.tsx (user-owned, scaffold-once) ──
		pageRel := filepath.Join(feDir, "src", "app", "page.tsx")
		if err := emitTier2OnceIfMissing(projectDir, pageRel, "nextjs/src/app/page.tsx.tmpl", data, force); err != nil {
			return err
		}

		if len(pages) > 0 {
			fmt.Printf("  ✅ Updated nav_gen.tsx + dashboard_gen.tsx with %d page(s) for frontend %s\n", len(pages), fe.Name)
		}
	}

	return nil
}

// emitTier2OnceIfMissing writes a Tier-2 ("forge:scaffold one-shot")
// template only when the destination file does not yet exist on disk.
// Tier-2 files are user-owned: forge writes them once at scaffold time
// and never overwrites (the user may have hand-curated them). `force`
// re-emits and clobbers existing content — the explicit opt-out for
// "throw my changes away and re-scaffold from the template".
func emitTier2OnceIfMissing(projectDir, relPath, tmplPath string, data templates.FrontendTemplateData, force bool) error {
	full := filepath.Join(projectDir, relPath)
	_, statErr := os.Stat(full)
	exists := statErr == nil
	if exists && !force {
		return nil
	}
	content, err := templates.FrontendTemplates().Render(tmplPath, data)
	if err != nil {
		return fmt.Errorf("render %s: %w", tmplPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", relPath, err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	if !exists {
		fmt.Printf("  ✅ Scaffolded Tier-2 file: %s (yours to edit)\n", relPath)
	} else {
		fmt.Printf("  ⚠️  --force: re-scaffolded Tier-2 file: %s (your edits discarded)\n", relPath)
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
