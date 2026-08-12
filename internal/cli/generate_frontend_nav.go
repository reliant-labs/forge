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
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// generateFrontendNav re-renders the sidebar navigation and dashboard page
// for each Next.js frontend using entity data derived from CRUD service
// methods. Called during `forge generate` after services are parsed.
//
// Every file it writes is scaffold-once ("yours"): components/nav.tsx,
// app/dashboard.tsx and app/page.tsx are written when absent and never
// touched again.
//
// None of them is Tier-1, and the reason is the rule Tier-1 exists for: a
// regenerated file must have a direct input forge re-derives it FROM — a
// proto to its stubs, the applied schema to an ORM struct, contract.go to
// its mocks. A nav and a dashboard are layouts. The entity set seeds them,
// but which routes belong in a sidebar and which numbers belong on a
// dashboard are product decisions with nothing to re-derive from, and
// regenerating them would mean forge holding an opinion about someone
// else's product and enforcing it with a hash guard.
//
// The cost is explicit and small: adding an entity scaffolds its pages but
// does not touch the nav, so its link is a one-line hand edit — the same
// edit a route with no entity behind it already required. The benefit is
// that these files can be rewritten, which for starter code is the point.
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
			APIURL:         devAPIURL(cfg, projectDir),
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

		// ── Tier-1: src/lib/otel_gen.ts (always regenerated) ──
		// The browser OpenTelemetry CONFIGURATION — the NEXT_PUBLIC_* env
		// reads Next.js inlines at build time, handed to the SDK wiring in
		// @reliant-labs/web-runtime/otel. It has carried the canonical
		// "Code generated by forge. DO NOT EDIT. / regenerated every run"
		// banner since it was written, and the banner linter's isKnownTier1
		// list names it — but nothing ever regenerated it, so the claim was
		// false in both directions: a hand-edit survived (and the drift lint
		// could not see it, there being no recorded hash), and DELETING it
		// left providers.tsx importing a module that no longer existed, so
		// `tsc --noEmit` failed and no amount of `forge generate` brought it
		// back. Emitting it here makes the banner true.
		otelContent, err := templates.FrontendTemplates().Render(
			filepath.Join("nextjs", "src", "lib", "otel_gen.ts.tmpl"), data)
		if err != nil {
			return fmt.Errorf("render otel_gen.ts for %s: %w", fe.Name, err)
		}
		otelRel := filepath.Join(feDir, "src", "lib", "otel_gen.ts")
		if _, err := checksums.WriteGeneratedFileTier1(projectDir, otelRel, otelContent, cs, true); err != nil {
			return fmt.Errorf("write otel_gen.ts: %w", err)
		}
		// Retire the pre-`_gen` copy from a project generated before the
		// rename. providers.tsx is scaffold-once, so an older project's copy
		// still names "@/lib/otel" — see rewriteRenamedFrontendImports, which
		// repoints it. Leaving the old module in place instead would let that
		// stale import keep resolving, to a file nothing regenerates.
		codegen.RetireRenamedGenerated(projectDir, filepath.Join(feDir, "src", "lib", "otel.ts"), cs)

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

		// ── Scaffold ("yours"): nav.tsx + dashboard.tsx ──
		//
		// These are LAYOUTS, not projections. Tier-1 is for a file with a
		// direct input that forge re-derives — a proto to its stubs, the
		// applied schema to an ORM struct, contract.go to its mocks. The set
		// of entities is such an input; how those entities are ARRANGED on a
		// page is a design decision with no input to re-derive from, and
		// regenerating it would mean forge holding an opinion about someone
		// else's product.
		//
		// They were Tier-1 and the contradiction showed: the dashboard's own
		// header called it "exemplar code", and exemplar code that trips a
		// hash guard when edited teaches nothing.
		// This is the ONLY writer of these two: the frontend scaffold defers
		// them here (generator.deferredToNavGenerator) so they are seeded from
		// the entity set rather than from the empty one that exists while the
		// frontend tree is being written.
		//
		// They cannot simply be withheld until the first entity: layout.tsx
		// imports { Nav } and page.tsx imports the dashboard, so a project
		// with no entities yet would not compile. So they are written
		// immediately AND refreshed while untouched — see
		// emitScaffoldUntilTouched. A greenfield project therefore picks up
		// its first entity's route, and the moment the user edits either file
		// it is theirs and forge stops.
		navRel := filepath.Join(feDir, "src", "components", "nav.tsx")
		if err := emitScaffoldUntilTouched(projectDir, navRel, "nextjs/src/components/nav.tsx.tmpl", data, cs); err != nil {
			return err
		}
		dashRel := filepath.Join(feDir, "src", "app", "dashboard.tsx")
		if err := emitScaffoldUntilTouched(projectDir, dashRel, "nextjs/src/app/dashboard.tsx.tmpl", data, cs); err != nil {
			return err
		}

		// The cost of not regenerating the nav: an entity added later has
		// pages but no link to them. Silence there would be the worst of both
		// worlds — forge stopped maintaining the file but never said so, and
		// the route is only missing, never broken, so nothing else surfaces
		// it. Name the routes and stop; editing is the user's call.
		reportUnlinkedRoutes(projectDir, navRel, pages, fe.Name)

		// ── package.json: the @reliant-labs/web-runtime specifier ──
		// Re-resolved every run so a project that moved on disk, or one
		// picked up by a differently-built forge, gets a specifier that
		// still points somewhere real.
		generator.EnsureWebRuntimeDependency(projectDir, feDir, fe.Name)

		// ── Scaffold ("yours"): page.tsx (user-owned, scaffold-once) ──
		pageRel := filepath.Join(feDir, "src", "app", "page.tsx")
		if err := emitScaffoldOnceIfMissing(projectDir, pageRel, "nextjs/src/app/page.tsx.tmpl", data); err != nil {
			return err
		}

		if len(pages) > 0 {
			fmt.Printf("  ✅ Scaffolded nav.tsx + dashboard.tsx with %d page(s) for frontend %s (yours to edit)\n", len(pages), fe.Name)
		}
	}

	// ── Vite SPA frontends ──
	// The Next-only nav/dashboard/basepath files above don't apply here, so
	// this is a separate, minimal loop.
	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "vite-spa") {
			continue
		}
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		data := templates.FrontendTemplateData{
			FrontendName: fe.Name,
			ProjectName:  cfg.Name,
			APIURL:       devAPIURL(cfg, projectDir),
		}
		// ── Tier-1: src/lib/apiurl_gen.ts (always regenerated) ──
		// The dev-mode API-URL FLOOR connect.ts falls back to when
		// VITE_API_URL is unset. Vite frontends used to rely on a
		// scaffold-once .env.local for this; that file is gone, so the floor
		// is now a regenerated gen file exactly like Next.js's — it can't
		// drift from the port the backend binds.
		if err := emitAPIURLGen(projectDir, feDir, "vite-spa", data, cs); err != nil {
			return fmt.Errorf("emit apiurl_gen.ts for %s: %w", fe.Name, err)
		}
		// use-query-resource.ts pairs with <Resource>. It ships in the Vite
		// static scaffold tree (new projects), so ensure EXISTING projects
		// pick it up too — emit-if-missing keeps any user edits.
		if err := ensureViteQueryResourceHook(projectDir, feDir); err != nil {
			return fmt.Errorf("ensure use-query-resource for %s: %w", fe.Name, err)
		}
		// ── package.json: the @reliant-labs/web-runtime specifier ──
		generator.EnsureWebRuntimeDependency(projectDir, feDir, fe.Name)
	}

	// ── React Native frontends: emit the apiurl_gen dev floor ──
	// RN scaffolds carry no nav/dashboard/runtime and, like Vite, used to
	// lean on a scaffold-once .env.local for EXPO_PUBLIC_API_URL. With that
	// file gone, the regenerated apiurl_gen.ts is the dev floor connect.ts
	// falls back to (per-env EXPO_PUBLIC_API_URL still comes from KCL).
	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "react-native") && !strings.EqualFold(fe.Type, "rn") {
			continue
		}
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		data := templates.FrontendTemplateData{
			FrontendName: fe.Name,
			ProjectName:  cfg.Name,
			APIURL:       devAPIURL(cfg, projectDir),
		}
		if err := emitAPIURLGen(projectDir, feDir, "react-native", data, cs); err != nil {
			return fmt.Errorf("emit apiurl_gen.ts for %s: %w", fe.Name, err)
		}
		// ── package.json: the @reliant-labs/web-runtime specifier ──
		generator.EnsureWebRuntimeDependency(projectDir, feDir, fe.Name)
	}

	return nil
}

// emitAPIURLGen renders <tmplSubdir>/src/lib/apiurl_gen.ts.tmpl into the
// frontend's src/lib/apiurl_gen.ts as a Tier-1 (always-regenerated,
// checksum-guarded) file. Shared by the Vite and React Native loops so
// their dev-URL floor tracks the backend port on every `forge generate`,
// exactly the way the Next.js loop already emits its own apiurl_gen.
func emitAPIURLGen(projectDir, feDir, tmplSubdir string, data templates.FrontendTemplateData, cs *checksums.FileChecksums) error {
	if err := os.MkdirAll(filepath.Join(projectDir, feDir, "src", "lib"), 0o755); err != nil {
		return fmt.Errorf("create lib dir: %w", err)
	}
	content, err := templates.FrontendTemplates().Render(
		filepath.Join(tmplSubdir, "src", "lib", "apiurl_gen.ts.tmpl"), data)
	if err != nil {
		return fmt.Errorf("render apiurl_gen.ts: %w", err)
	}
	rel := filepath.Join(feDir, "src", "lib", "apiurl_gen.ts")
	if _, err := checksums.WriteGeneratedFileTier1(projectDir, rel, content, cs, true); err != nil {
		return fmt.Errorf("write apiurl_gen.ts: %w", err)
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
// time and NEVER writes that path again — no flag, no exception. That
// covers deletion as well as editing; to re-scaffold on purpose, drop the
// path's entry from .forge/scaffolded.json.
func emitScaffoldOnceIfMissing(projectDir, relPath, tmplPath string, data templates.FrontendTemplateData) error {
	full := filepath.Join(projectDir, relPath)
	if !checksums.ScaffoldOnceDecision(projectDir, relPath) {
		return nil // present, or deliberately deleted — either way, not ours
	}
	if _, statErr := os.Stat(full); statErr != nil && !os.IsNotExist(statErr) {
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
	checksums.RecordScaffold(projectDir, relPath)
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
				Label:           e.EntityNamePlural,
				LabelLower:      strings.ToLower(e.EntityNamePlural),
				LabelSingular:   e.EntityName,
				Slug:            e.EntitySlug,
				HasCreate:       e.HasCreate,
				ListHook:        "use" + e.ListRPC,
				HooksModule:     e.HooksImportPath,
				ItemsField:      e.ItemsField,
				HasTotalCount:   e.HasTotalCount,
				TotalCountField: e.TotalCountField,
				HasPageSize:     e.HasPageSize,
				ComponentIdent:  e.EntityNamePlural,
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
// `forge api` do: a backend exists when the proto descriptor exposes a
// Connect service (codegen.IntrospectComponents) OR the project is
// service-kind — its dev server binds the mux and serves /healthz even before
// the first service is added — and every backend listens on
// config.DefaultServePort. No backend → 0, devAPIURL returns "" and connect.ts
// fails loud rather than guessing a port.
//
// This is a GENERATE-time value baked into apiurl_gen.ts, so it cannot be the
// ephemeral port `forge run` / `forge env up` allocate at launch: those inject
// the live URL through NEXT_PUBLIC_API_URL / VITE_API_URL / EXPO_PUBLIC_API_URL,
// which always wins over this baked fallback (see src/lib/connect.ts).
func resolveDevAPIPort(cfg *config.ProjectConfig, projectDir string) int {
	if len(codegen.IntrospectComponents(projectDir)) > 0 || cfg.IsServiceKind() {
		return config.DefaultServePort
	}
	return 0
}

// reportUnlinkedRoutes names entity pages that exist but that the user-owned
// nav does not link to.
//
// nav.tsx is scaffold-once, so an entity added after the first scaffold gets
// pages that nothing navigates to. That is the deliberate cost of the file
// being the user's; what would NOT be acceptable is discovering it by noticing
// a missing sidebar link. A missing route breaks no build and fails no test,
// so this notice — plus its duplicate in the post-generation warnings block
// (unlinkedRouteWarnings, collected by validateGeneratedProject) — is the
// only thing that surfaces it. The inline ℹ️ line here is easy to miss: it is
// one line among the ~150 a full `forge generate` prints, sandwiched between
// walls of ✅. The warnings block is read every time because it is also where
// hard-to-miss findings like the pkg/config-import check land, so the same
// finding is promoted there rather than relocated — losing the inline line
// would mean a project with only ONE frontend loses its immediate, in-context
// notice for no benefit.
//
// It reads the nav as TEXT rather than parsing it, because by this point the
// file is arbitrary user code: it may map, filter, or build its routes from a
// CMS. A substring hit means the path is mentioned somewhere, which is enough
// to conclude the user knows about it — and being wrong in that direction
// costs nothing, while a false alarm on every generate would train people to
// ignore the line.
func reportUnlinkedRoutes(projectDir, navRel string, pages []templates.NavPageData, feName string) {
	missing := missingNavRoutes(projectDir, navRel, pages)
	if len(missing) == 0 {
		return
	}
	fmt.Printf("  ℹ️  %s: %d page(s) with no link in %s — %s\n",
		feName, len(missing), navRel, strings.Join(missing, ", "))
	fmt.Printf("      nav.tsx is yours, so forge does not edit it. Add them to ALL_ROUTES if you want them in the sidebar.\n")
}

// missingNavRoutes is the detection half of reportUnlinkedRoutes, split out
// so validateGeneratedProject's post-generation warnings pass (see
// unlinkedRouteWarnings below) can reuse the exact same pristine-suppression
// and substring-match rules instead of re-deriving them and risking drift
// between the inline notice and the warnings-block one.
func missingNavRoutes(projectDir, navRel string, pages []templates.NavPageData) []string {
	if len(pages) == 0 {
		return nil
	}
	// While the nav is still pristine forge is keeping it current, so a route
	// missing from it is about to be added rather than dropped — warning then
	// would fire on every greenfield run and be wrong every time.
	if checksums.ScaffoldIsPristine(projectDir, navRel) {
		return nil
	}
	src, err := os.ReadFile(filepath.Join(projectDir, navRel))
	if err != nil {
		return nil // deliberately deleted, or unreadable
	}
	nav := string(src)

	var missing []string
	for _, p := range pages {
		if !strings.Contains(nav, `"/`+p.Slug+`"`) {
			missing = append(missing, "/"+p.Slug)
		}
	}
	return missing
}

// unlinkedRouteWarnings recomputes reportUnlinkedRoutes' finding as entries
// for the post-generation warnings block. It cannot reuse a value stashed by
// stepFrontendNav — pipeline steps communicate forward through ctx fields,
// not backward through return values — so it re-derives the same
// (services, entities) → pages projection stepFrontendNav used and re-reads
// each Next.js frontend's nav.tsx. Cheap (a handful of file reads) and safe
// to recompute: it is a property of the file's contents at validate time, not
// of the earlier step's run.
func unlinkedRouteWarnings(cfg *config.ProjectConfig, projectDir string, services []codegen.ServiceDef, entities []codegen.EntityDef) []string {
	if cfg == nil {
		return nil
	}
	pages := buildNavPages(services, entities)
	if len(pages) == 0 {
		return nil
	}
	var warnings []string
	for _, fe := range cfg.Frontends {
		if !strings.EqualFold(fe.Type, "nextjs") {
			continue
		}
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		navRel := filepath.Join(feDir, "src", "components", "nav.tsx")
		missing := missingNavRoutes(projectDir, navRel, pages)
		if len(missing) == 0 {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"frontend %s: %d page(s) with no link in %s — %s. "+
				"nav.tsx is yours, so forge does not edit it; add them to ALL_ROUTES if you want them in the sidebar.",
			fe.Name, len(missing), navRel, strings.Join(missing, ", ")))
	}
	return warnings
}

// emitScaffoldUntilTouched writes a scaffold and KEEPS it current until the
// user edits it, at which point it becomes theirs permanently.
//
// Ordinary scaffolds are write-once (emitScaffoldOnceIfMissing): forge writes
// them if absent and never looks again. That is wrong for nav.tsx and
// dashboard.tsx for one reason — they must exist before they can be correct.
// layout.tsx imports { Nav } and page.tsx imports the dashboard, so they have
// to be on disk from the first render, which happens before any entity
// exists; write-once would therefore freeze the empty version and every
// project's first entity would arrive unlinked.
//
// Refreshing while pristine resolves that without weakening ownership. A
// greenfield project accumulates its routes right up until the user takes
// over, and taking over is a single edit — not a flag, not a disown, not a
// file to delete. Pristine is decided by the birth hash rather than by
// timestamps, so a file rewritten with identical content, or touched by a
// formatter that changed nothing, is still forge's; one changed byte is not.
func emitScaffoldUntilTouched(projectDir, relPath, tmplPath string, data templates.FrontendTemplateData, cs *checksums.FileChecksums) error {
	if cs != nil && cs.IsDisowned(relPath) {
		return nil
	}
	full := filepath.Join(projectDir, relPath)
	_, statErr := os.Stat(full)
	switch {
	case statErr == nil:
		// Present: refresh only while it is still exactly what forge wrote.
		if !checksums.ScaffoldIsPristine(projectDir, relPath) {
			return nil
		}
	case os.IsNotExist(statErr):
		// Absent: a deliberate deletion stays deleted, same as any scaffold.
		if checksums.ScaffoldRecorded(projectDir, relPath) {
			return nil
		}
	default:
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
	checksums.RecordScaffoldWithHash(projectDir, relPath, content)
	return nil
}
