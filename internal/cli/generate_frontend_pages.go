package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// ensureFrontendComponents installs missing core UI components for all
// browser-targeted frontends (nextjs + vite-spa). Called during `forge
// generate` so existing projects pick up any new core components added in
// newer forge versions.
//
// In workspaces mode there is no per-frontend src/components/ui/ to
// populate — the shared component library lives at packages/ui-web/.
// We ensure it once and skip the per-frontend loop; the tsconfig path
// mapping (and Vite alias) emitted by the frontend templates routes
// `@/components/*` imports there.
//
// Returns the first scaffold error encountered. The pipeline caller
// (stepFrontendComponents) routes the result through ctx.warnOrFail so
// failures are warn-by-default and fatal under --strict.
func ensureFrontendComponents(cfg *config.ProjectConfig, projectDir string) error {
	if cfg.IsFrontendWorkspacesEnabled() {
		if err := generator.WriteUIWebPackageFiles(projectDir, cfg.Name, true); err != nil {
			return fmt.Errorf("ui-web package scaffold: %w", err)
		}
		return nil
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
		frontendDir := filepath.Join(projectDir, feDir)
		if err := generator.EnsureCoreComponents(frontendDir); err != nil {
			return fmt.Errorf("component install for %s: %w", fe.Name, err)
		}
	}
	return nil
}

// generateFrontendPages generates CRUD page files for each entity that has
// CRUD-pattern RPCs across all browser-targeted frontends (nextjs + vite-spa).
// Only generates pages for CRUD-pattern RPCs whose entity name (e.g.
// "Daemon" from "ListDaemons") matches a real entity from the proto
// descriptor — without that filter, page templates produce broken output for
// services whose List/Get/Create RPCs don't follow the entity-name-as-field
// convention.
//
// Tier-2 (scaffold-once) lifecycle: every page template carries a
//
//	`// yours: scaffolded once, never touched again — forge will not overwrite this file`
//
// banner promising the user that hand-edits will survive subsequent
// `forge generate` runs. Honor that promise by skipping the write when
// the target file already exists on disk, mirroring the
// `emitTier2OnceIfMissing` pattern that `generateFrontendNav` already
// uses for nav.tsx / page.tsx. Re-scaffolding is gated on the
// `--reset-tier2` hook (checksums.Tier2OverwriteFn) — NOT on --force,
// which is scoped to the Tier-1 files the stomp guard flagged
// (journey fr-a04f8c0609).
//
// Per-kind dispatch:
//   - nextjs:   pages/ templates → src/app/<slug>/[id]/{,edit/}page.tsx
//   - vite-spa: vite-spa-pages/ templates → src/pages/<slug>/{List,Detail,Create,Edit}.tsx
func generateFrontendPages(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string, entities []codegen.EntityDef, cs *checksums.FileChecksums) error {
	if len(services) == 0 {
		return nil
	}

	entityByName := make(map[string]codegen.EntityDef, len(entities))
	for _, e := range entities {
		entityByName[strings.ToLower(e.Name)] = e
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

		var pageCount, skipCount int

		for _, svc := range services {
			pages := codegen.ExtractCRUDEntities(svc)

			for _, entity := range pages {
				// Skip RPC-name-derived entities that don't have a real
				// entity definition behind them — the page templates would
				// emit broken field references.
				entityDef, ok := entityByName[strings.ToLower(entity.EntityName)]
				if !ok {
					continue
				}
				// Typed columns / search fields / detail rows: the
				// templates render explicit field declarations from the
				// proto entity instead of Object.keys reflection.
				codegen.AttachEntityMeta(&entity, entityDef)
				kinds := []struct {
					emit bool
					tmpl *template.Template
					rel  string
					kind string
				}{
					{entity.HasList, layout.listTmpl, layout.listPath(entity.EntitySlug), "list"},
					{entity.HasGet, layout.detailTmpl, layout.detailPath(entity.EntitySlug), "detail"},
					{entity.HasCreate, layout.createTmpl, layout.createPath(entity.EntitySlug), "create"},
					{entity.HasUpdate, layout.editTmpl, layout.editPath(entity.EntitySlug), "edit"},
				}
				for _, k := range kinds {
					if !k.emit {
						continue
					}
					relPath := filepath.Join(feDir, k.rel)
					wrote, err := renderPageToFileTier2(k.tmpl, entity, projectDir, relPath, cs)
					if err != nil {
						return fmt.Errorf("render %s page for %s: %w", k.kind, entity.EntityName, err)
					}
					if wrote {
						pageCount++
					} else {
						skipCount++
					}
				}
			}
		}

		if pageCount > 0 {
			fmt.Printf("  ✅ Generated %d CRUD page(s) for frontend %s\n", pageCount, fe.Name)
		}
		if skipCount > 0 {
			fmt.Printf("  ⏭️  Preserved %d existing CRUD page(s) for frontend %s (pass --reset-tier2 to re-scaffold)\n", skipCount, fe.Name)
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

// renderPageToFileTier2 renders a page template to disk under
// scaffold-once ("yours:" banner) semantics: the file is written once at
// scaffold time and never overwritten on subsequent `forge generate`
// runs, matching the leading banner comment every page template
// carries. Re-scaffolding an existing page requires the `--reset-tier2`
// hook (checksums.Tier2OverwriteFn): when the hook is installed the
// write proceeds to WriteGeneratedFileTier2, which prompts per
// hand-edited file. `--force` deliberately has no effect here — it is
// scoped to the Tier-1 files the stomp guard flagged (journey
// fr-a04f8c0609 lost user pages to a --force recovery from an
// unrelated Tier-1 trip).
//
// Returns (wrote, err) — wrote=false when the destination already
// existed and was preserved, so the caller can distinguish freshly-
// scaffolded pages from preserved ones in the summary log.
//
// The checksums hand-off (`checksums.WriteGeneratedFileTier2`) tags the
// emit as Tier-2 (no certification marker), which the stomp-guard
// reader uses to *skip* the file in CheckTier1Drift — Tier-2 files are
// expected to drift from forge's recorded render, that's the whole
// point.
func renderPageToFileTier2(tmpl *template.Template, data codegen.PageTemplateData, projectDir, relPath string, cs *checksums.FileChecksums) (bool, error) {
	fullPath := filepath.Join(projectDir, relPath)

	// Scaffold-once: if the user already has this file on disk, leave
	// it alone unless the --reset-tier2 hook is installed (the hook
	// itself still adjudicates hand-edited files per file inside
	// WriteGeneratedFileTier2).
	if checksums.Tier2OverwriteFn == nil {
		if _, err := os.Stat(fullPath); err == nil {
			return false, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("stat %s: %w", relPath, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return false, fmt.Errorf("create directory for %s: %w", relPath, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return false, err
	}

	wrote, err := checksums.WriteGeneratedFileTier2(projectDir, relPath, buf.Bytes(), cs, false)
	if err != nil {
		return false, err
	}
	return wrote, nil
}
