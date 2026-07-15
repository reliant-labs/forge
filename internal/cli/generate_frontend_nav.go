package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
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
//   - components/nav.tsx (scaffold-once "yours"): user-owned
//     presentation that imports ALL_ROUTES from nav_gen and renders the
//     curated subset.
//   - app/dashboard_gen.tsx (Tier-1, banner): EntityTiles + QuickActions
//     React components, regenerated every run.
//   - app/page.tsx (scaffold-once "yours"): user-owned dashboard root
//     that composes the generated tile/action components.
//
// The split lets users hand-edit nav.tsx + page.tsx (icon palette, route
// pruning, custom widgets) without forge overwriting them on the next
// `forge generate` — the Tier-1 guard in checksums.go prevents accidental
// stomps on the gen files too. New entities flowing into the nav need
// zero user action: nav_gen.tsx picks them up automatically and the
// user's nav.tsx maps over ALL_ROUTES so they appear in the sidebar.
//
// The scaffold files (nav.tsx / page.tsx) are write-if-absent: once on
// disk forge NEVER overwrites them (no flag). To refresh one, delete it
// and regenerate.
func generateFrontendNav(cfg *config.ProjectConfig, services []codegen.ServiceDef, projectDir string, entities []codegen.EntityDef, cs *checksums.FileChecksums) error {
	// CRITICAL: nav/dashboard routes derive from the SAME entity set that
	// gates CRUD page emission (generateFrontendPages skips RPC-name-derived
	// entities with no proto entity definition behind them). Before this
	// filter the two generators disagreed and a pristine scaffold's nav
	// advertised routes that 404'd.
	pages := buildNavPages(services, entities)

	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") {
			continue
		}

		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}

		data := templates.FrontendTemplateData{
			FrontendName:   fe.Name,
			ProjectName:    cfg.Name,
			Pages:          pages,
			NavHookImports: buildNavHookImports(pages),
			BasePath:       strings.TrimSpace(fe.BasePath),
			ApiUrl:         devAPIURL(cfg, projectDir),
		}

		if err := os.MkdirAll(filepath.Join(projectDir, feDir, "src", "components"), 0o755); err != nil {
			return fmt.Errorf("create components dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(projectDir, feDir, "src", "app"), 0o755); err != nil {
			return fmt.Errorf("create app dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(projectDir, feDir, "src", "lib"), 0o755); err != nil {
			return fmt.Errorf("create lib dir: %w", err)
		}

		// ── Tier-1: src/lib/basepath_gen.ts (always regenerated) ──
		// BASE_PATH + joinBasePath() sourced from forge.yaml's
		// frontends[].base_path. Regenerated every run so editing
		// base_path in forge.yaml propagates without re-scaffolding;
		// next.config.ts (Tier-2, scaffold-once) reads the same value
		// via the NEXT_PUBLIC_BASE_PATH env var or its baked default.
		bpGenContent, err := templates.FrontendTemplates().Render(
			filepath.Join("nextjs", "src", "lib", "basepath_gen.ts.tmpl"), data)
		if err != nil {
			return fmt.Errorf("render basepath_gen.ts for %s: %w", fe.Name, err)
		}
		bpGenRel := filepath.Join(feDir, "src", "lib", "basepath_gen.ts")
		if _, err := checksums.WriteGeneratedFileTier1(projectDir, bpGenRel, bpGenContent, cs, true); err != nil {
			return fmt.Errorf("write basepath_gen.ts: %w", err)
		}

		// ── Tier-1: src/lib/apiurl_gen.ts (always regenerated) ──
		// DEV_API_URL baked from forge.yaml's first service port, refreshed
		// on every `forge generate`. connect.ts uses it as the non-mock dev
		// fallback when NEXT_PUBLIC_API_URL is unset — and fails LOUD when
		// both are empty, instead of silently pointing at a port nobody is
		// listening on (downstream projects hand-patched a stale
		// localhost:8080 default twice before this existed).
		auGenContent, err := templates.FrontendTemplates().Render(
			filepath.Join("nextjs", "src", "lib", "apiurl_gen.ts.tmpl"), data)
		if err != nil {
			return fmt.Errorf("render apiurl_gen.ts for %s: %w", fe.Name, err)
		}
		auGenRel := filepath.Join(feDir, "src", "lib", "apiurl_gen.ts")
		if _, err := checksums.WriteGeneratedFileTier1(projectDir, auGenRel, auGenContent, cs, true); err != nil {
			return fmt.Errorf("write apiurl_gen.ts: %w", err)
		}

		// next.config.ts is Tier-2 (user-owned, scaffold-once), so forge
		// can't rewrite it when base_path is added to forge.yaml later.
		// A config that never reads NEXT_PUBLIC_BASE_PATH will serve the
		// app at "/" while basepath_gen.ts prefixes hand-built URLs with
		// the declared base_path — exactly the silent split-brain this
		// feature exists to kill. Warn loudly (non-fatal: the user may
		// have wired basePath by other means).
		if data.BasePath != "" {
			warnIfNextConfigIgnoresBasePath(projectDir, feDir, fe.Name, data.BasePath)
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

		// ── Scaffold ("yours"): nav.tsx (user-owned, scaffold-once) ──
		// Only emit when the file is missing — once the user has it on
		// disk, forge never overwrites it (no flag), because the user may
		// have hand-curated it. The Tier-1 guard separately catches stomps
		// on nav_gen.tsx.
		navRel := filepath.Join(feDir, "src", "components", "nav.tsx")
		if err := emitScaffoldOnceIfMissing(projectDir, navRel, "nextjs/src/components/nav.tsx.tmpl", data); err != nil {
			return err
		}

		// ── Scaffold ("yours"): page.tsx (user-owned, scaffold-once) ──
		pageRel := filepath.Join(feDir, "src", "app", "page.tsx")
		if err := emitScaffoldOnceIfMissing(projectDir, pageRel, "nextjs/src/app/page.tsx.tmpl", data); err != nil {
			return err
		}

		if len(pages) > 0 {
			fmt.Printf("  ✅ Updated nav_gen.tsx + dashboard_gen.tsx with %d page(s) for frontend %s\n", len(pages), fe.Name)
		}
	}

	return nil
}

// warnIfNextConfigIgnoresBasePath prints an advisory when forge.yaml
// declares frontends[].base_path but the frontend's (user-owned)
// next.config.ts never references the canonical env var / basePath key.
// Scaffolds produced after basePath support landed always contain
// `NEXT_PUBLIC_BASE_PATH`; older hand-rolled configs need the block
// added by hand (see the frontend skill's "Serving under a path prefix"
// section). Missing next.config.ts is skipped silently — the scaffold
// step owns that complaint.
func warnIfNextConfigIgnoresBasePath(projectDir, feDir, feName, basePath string) {
	body, err := os.ReadFile(filepath.Join(projectDir, feDir, "next.config.ts"))
	if err != nil {
		return
	}
	s := string(body)
	if strings.Contains(s, "NEXT_PUBLIC_BASE_PATH") || strings.Contains(s, "basePath") {
		return
	}
	fmt.Printf("  ⚠️  frontend %s: forge.yaml declares base_path %q but next.config.ts never reads NEXT_PUBLIC_BASE_PATH or sets basePath.\n"+
		"      Routes/assets will be served at \"/\" while generated helpers prefix URLs with %q. Add to next.config.ts:\n"+
		"        const basePath = process.env.NEXT_PUBLIC_BASE_PATH ?? %q;\n"+
		"        ...(basePath ? { basePath, assetPrefix: basePath } : {}),\n",
		feName, basePath, basePath, basePath)
}

// emitScaffoldOnceIfMissing writes a scaffold ("yours:" banner) template
// only when the destination file does not yet exist on disk. Scaffold
// files are user-owned from birth: forge writes them once at scaffold
// time and NEVER overwrites an existing file — no flag, no exception.
// To refresh one, delete it and regenerate.
func emitScaffoldOnceIfMissing(projectDir, relPath, tmplPath string, data templates.FrontendTemplateData) error {
	full := filepath.Join(projectDir, relPath)
	if _, statErr := os.Stat(full); statErr == nil {
		return nil // exists — forge never overwrites a scaffold
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat %s: %w", relPath, statErr)
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
	fmt.Printf("  ✅ Scaffolded %s (yours to edit)\n", relPath)
	return nil
}

// buildNavPages derives navigation page entries from CRUD service methods.
// A nav entry is created only for entities that (a) have a List RPC — the
// nav links the list page — AND (b) exist in the entity set, the SAME
// predicate generateFrontendPages applies before emitting page files.
// Anything looser advertises 404s on a pristine scaffold.
//
// The two halves are matched on the kebab SLUG — the actual route
// identity — not on the raw EntityDef.Name string. The entity set
// (BuildSchemaEntities) carries the singular as EntityDef.Name; the route
// side (ExtractCRUDEntities) re-derives plural + slug. Keying the gate on
// PascalToKebab(Pluralize(Name)) ties it to the SAME transform that
// produces e.EntitySlug, so the match tracks the route the page generator
// actually emits rather than an incidental name string that an entity-
// projection change can reshape underneath it. The applied-schema entity
// join (BuildSchemaEntities) replaced the old proto-annotation entity
// names with singular CRUD-RPC names; matching on the derived slug keeps
// this gate stable across that projection churn — if the slug a route is
// emitted under matches an entity in the set, the route is kept, full
// stop. A regression here empties ALL_ROUTES silently (no error), dropping
// every dashboard tile, so the match is pinned by
// TestBuildNavPages_ControlPlaneEntitySet.
func buildNavPages(services []codegen.ServiceDef, entities []codegen.EntityDef) []templates.NavPageData {
	entitySet := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		entitySet[codegen.PascalToKebab(naming.Pluralize(e.Name))] = struct{}{}
	}

	seen := make(map[string]bool)
	var pages []templates.NavPageData

	for _, svc := range services {
		crudEntities := codegen.ExtractCRUDEntities(svc)
		for _, e := range crudEntities {
			if !e.HasList {
				continue
			}
			if _, ok := entitySet[e.EntitySlug]; !ok {
				continue
			}
			if seen[e.EntitySlug] {
				continue
			}
			seen[e.EntitySlug] = true

			pages = append(pages, templates.NavPageData{
				Label:          e.EntityNamePlural,
				LabelLower:     strings.ToLower(e.EntityNamePlural),
				LabelSingular:  e.EntityName,
				Slug:           e.EntitySlug,
				HasCreate:      e.HasCreate,
				ListHook:       "use" + e.ListRPC,
				HooksModule:    e.HooksImportPath,
				ItemsField:     e.ItemsField,
				ComponentIdent: e.EntityNamePlural,
			})
		}
	}

	return pages
}

// buildNavHookImports merges the dashboard tiles' list-hook imports by
// module so the template emits one import statement per generated hooks
// file (two entities on one service share a module).
func buildNavHookImports(pages []templates.NavPageData) []templates.NavHookImport {
	byModule := map[string][]string{}
	var order []string
	for _, p := range pages {
		if p.HooksModule == "" || p.ListHook == "" {
			continue
		}
		if _, ok := byModule[p.HooksModule]; !ok {
			order = append(order, p.HooksModule)
		}
		byModule[p.HooksModule] = append(byModule[p.HooksModule], p.ListHook)
	}
	sort.Strings(order)
	out := make([]templates.NavHookImport, 0, len(order))
	for _, m := range order {
		syms := byModule[m]
		sort.Strings(syms)
		out = append(out, templates.NavHookImport{Module: m, Symbols: syms})
	}
	return out
}

// defaultDevAPIPort is the canonical dev-mode HTTP port the runtime server
// binds (see config-dev.yaml's `port: 8080`) and the same fallback
// `forge api` uses. Service ports moved out of forge.yaml into KCL (a
// deploy fact), so neither the configured nor the descriptor-discovered
// server components carry a port — the dev fallback URL is derived from
// this default instead.
const defaultDevAPIPort = 8080

// devAPIURL derives the dev-mode API base URL that connect.ts targets when
// NEXT_PUBLIC_API_URL is unset. Empty only when the project has no backend
// at all (CLI/library kind with a stray frontend) — connect.ts then refuses
// to guess and fails loud in non-mock dev.
func devAPIURL(cfg *config.ProjectConfig, projectDir string) string {
	port := resolveDevAPIPort(cfg, projectDir)
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

// resolveDevAPIPort resolves the dev-mode API port the same way the server /
// `forge api` do: from the SAME proto-descriptor discovery
// (codegen.IntrospectComponents), NOT from a forge.yaml services block —
// which no longer exists (services are descriptor-discovered; ports live in
// KCL). Precedence:
//
//  1. An explicit port on a configured or discovered server component (rare —
//     forge.yaml no longer carries ports, but honor one if ever set).
//  2. The canonical dev port (defaultDevAPIPort) when a backend exists: the
//     proto descriptor exposes a Connect service, OR the project is
//     service-kind (its dev server binds :8080 and serves /healthz even
//     before the first service is added). This is what `forge run` actually
//     listens on, so the baked fallback can't drift from the backend.
//  3. Otherwise 0 → devAPIURL returns "" and connect.ts fails loud.
func resolveDevAPIPort(cfg *config.ProjectConfig, projectDir string) int {
	for _, s := range cfg.Servers() {
		if p := s.PrimaryPort(); p != 0 {
			return p
		}
	}
	discovered := codegen.IntrospectComponents(projectDir)
	for _, s := range discovered {
		if p := s.PrimaryPort(); p != 0 {
			return p
		}
	}
	if len(discovered) > 0 || cfg.IsServiceKind() {
		return defaultDevAPIPort
	}
	return 0
}
