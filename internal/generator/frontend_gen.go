package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/pkg/components"
	"github.com/reliant-labs/forge/pkg/seedplan"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/templates"
)

// refreshedByNavGenerator reports whether a frontend scaffold file is one of
// the two the nav generator keeps current until the user edits it
// (internal/cli/generate_frontend_nav.go, emitScaffoldUntilTouched).
//
// These two are special because they must EXIST before they can be correct:
// layout.tsx imports { Nav } and page.tsx imports the dashboard, so both are
// written here — before any entity exists — and refreshed later as entities
// arrive. That refresh is gated on the birth hash recorded at this write.
// Every other file in the frontend tree is either plain scaffold-once or
// regenerated from its own input, and needs no hash.
func refreshedByNavGenerator(destFile string) bool {
	switch filepath.ToSlash(destFile) {
	case "src/components/nav.tsx", "src/app/dashboard.tsx":
		return true
	default:
		return false
	}
}

// frontendTemplateDir returns the per-kind template subdirectory for the
// given kind. kind="mobile" uses react-native templates; kind="vite-spa"
// uses vite-spa templates; everything else (including "" and "web") uses
// nextjs.
//
// This names the PLATFORM-SPECIFIC directory only. The full tree a
// frontend is scaffolded from also includes the shared roots — see
// templates.FrontendTemplateRoots / templates.ListFrontendTree.
func frontendTemplateDir(kind string) string {
	switch kind {
	case "mobile":
		return "react-native"
	case "vite-spa":
		return "vite-spa"
	default:
		return "nextjs"
	}
}

// frontendKindHasMockSurface reports whether a frontend --kind gets the
// generated mock/scenario surface (see EmitFrontendMockSurface). Browser
// frontends do; React Native does not — its connect.ts never references
// "@/lib/mock-transport", and Metro would bundle the fixtures into the app.
func frontendKindHasMockSurface(kind string) bool {
	return frontendTemplateDir(kind) != "react-native"
}

// FrontendTypeHasMockSurface is frontendKindHasMockSurface keyed by the
// forge.yaml `frontends[].type` (nextjs / vite-spa / react-native) rather
// than by the `--kind` flag the scaffold takes. Used by the `forge generate`
// frontend-mocks step, which reads the persisted config. An unrecognised
// type gets nothing — the surface is rendered from templates that only the
// two browser kinds import.
func FrontendTypeHasMockSurface(feType string) bool {
	return strings.EqualFold(feType, "nextjs") || strings.EqualFold(feType, "vite-spa")
}

// FrontendGenOptions carries optional project-level settings forwarded
// to the per-frontend file emitter. Kept distinct from the existing
// positional GenerateFrontendFiles params so that adding new optional
// settings (here: Workspaces) doesn't churn every call site.
type FrontendGenOptions struct {
	// Workspaces opts the frontend into the pnpm-workspace layout —
	// its package.json declares "workspace:*" deps on @<scope>/api /
	// @<scope>/hooks, and templates render imports of those packages
	// instead of relative @/gen / @/hooks paths.
	Workspaces bool
	// Output selects the Next.js build/runtime shape rendered into
	// `next.config.ts`. Valid values: "standalone" (default), "static",
	// "server". See config.FrontendConfig.Output for the per-mode
	// semantics. Empty string defaults to "standalone" — the only mode
	// that both pairs with the shipped Dockerfile and supports the
	// dynamic `[id]` CRUD routes forge generates (static export fails
	// `next build` on any dynamic segment without generateStaticParams).
	//
	// Ignored for kind=mobile (react-native) and kind=vite-spa; those
	// trees have their own production shapes.
	Output string
	// BasePath is the URL prefix the frontend is mounted under (e.g.
	// "/admin"), mirroring config.FrontendConfig.BasePath. Rendered
	// into `next.config.ts` (basePath + assetPrefix defaults) and
	// `src/lib/basepath_gen.ts` (BASE_PATH / joinBasePath fallback).
	// Empty = served from the host root. Like Output, only the nextjs
	// template tree reads it.
	BasePath string
	// TypedConfig describes the config message bound to THIS frontend by
	// (forge.v1.frontend_config), when the project declares one. Zero value
	// means the project has not opted in, and every template renders its
	// previous env-var form unchanged.
	TypedConfig FrontendTypedConfig
}

// FrontendTypedConfig tells the frontend templates which typed config
// fields exist for the frontend being rendered.
//
// It carries per-FIELD presence rather than a bare "has config" flag
// because the generated TypeScript module's type has exactly the keys the
// proto declared. A template that reads cfg.OIDC_SCOPES when the proto
// never declared oidc_scopes produces a frontend that does not type-check —
// so each read is gated on the field that backs it.
type FrontendTypedConfig struct {
	// Bound reports that a config message names this frontend.
	Bound bool

	HasRedirectURI bool
	HasScopes      bool
	HasResource    bool
	HasMockAPI     bool
}

// FrontendTypedConfigFrom builds the per-field presence set from the leaf
// fields of the frontend's config message, keyed on each field's env_var
// (the one spelling shared by the proto, the KCL projection, the runtime
// document and the generated module).
func FrontendTypedConfigFrom(envVars []string) FrontendTypedConfig {
	out := FrontendTypedConfig{Bound: true}
	for _, v := range envVars {
		switch v {
		case "OIDC_REDIRECT_URI":
			out.HasRedirectURI = true
		case "OIDC_SCOPES":
			out.HasScopes = true
		case "OIDC_RESOURCE":
			out.HasResource = true
		case "MOCK_API":
			out.HasMockAPI = true
		}
	}
	return out
}

// GenerateFrontendFiles generates the frontend directory and files.
// kind selects the template set: "" or "web" for Next.js, "mobile" for React Native.
// Both the "new" project flow and the "scaffold frontend" flow delegate here so
// the output is always identical.
//
// This thin shim preserves the original signature for backward
// compatibility with the Service contract; new call sites should prefer
// GenerateFrontendFilesWithOptions when they have access to the
// project-level Frontend config.
func GenerateFrontendFiles(root, modulePath, projectName, frontendName string, apiPort int, kind string) error {
	return GenerateFrontendFilesWithOptions(root, modulePath, projectName, frontendName, apiPort, kind, FrontendGenOptions{})
}

// GenerateFrontendFilesWithOptions is GenerateFrontendFiles with an
// extra FrontendGenOptions param for project-level toggles (today only
// Workspaces). The default-zero opts struct produces output
// byte-identical to GenerateFrontendFiles so existing callers and
// snapshot tests are unaffected.
func GenerateFrontendFilesWithOptions(root, modulePath, projectName, frontendName string, apiPort int, kind string, opts FrontendGenOptions) error {
	frontendDir := filepath.Join(root, "frontends", frontendName)
	if err := os.MkdirAll(frontendDir, 0755); err != nil {
		return fmt.Errorf("create frontend directory: %w", err)
	}

	tmplDir := frontendTemplateDir(kind)

	// The tree is composed from frontend/shared[-web]/ plus the per-kind
	// directory (see templates.FrontendTemplateRoots): generic mechanism
	// modules live in exactly one place and render into every kind that
	// claims them.
	frontendFiles, err := templates.ListFrontendTree(tmplDir)
	if err != nil {
		return fmt.Errorf("list frontend templates: %w", err)
	}

	layout := NewFrontendWorkspaceLayout(projectName)
	// Default the Next.js output shape to "standalone" when unset. The
	// generated CRUD detail/edit pages are dynamic client routes
	// (`/<slug>/[id]`), and `output: "export"` (the "static" mode) fails
	// `next build` on any dynamic segment without generateStaticParams —
	// so a static default would break `npm run build` on every project
	// the moment it has one entity. Standalone also pairs with the
	// shipped Dockerfile (.next-prod/standalone/server.js). We canonicalise
	// here rather than in every template so callers can pass "" for
	// "use the scaffold default" without having to know what it is.
	output := strings.ToLower(strings.TrimSpace(opts.Output))
	if output == "" {
		output = "standalone"
	}
	data := templates.FrontendTemplateData{
		FrontendName: frontendName,
		ProjectName:  projectName,
		Platform:     tmplDir,
		APIURL:       fmt.Sprintf("http://localhost:%d", apiPort),
		Module:       modulePath,
		Workspaces:   opts.Workspaces,
		Output:       output,
		BasePath:     opts.BasePath,

		// Typed frontend config, when the project declares a config message
		// bound to THIS frontend. Gated per-field because a template cannot
		// read a key the generated module's type does not have.
		HasTypedConfig: opts.TypedConfig.Bound,
		HasRedirectURI: opts.TypedConfig.HasRedirectURI,
		HasScopes:      opts.TypedConfig.HasScopes,
		HasResource:    opts.TypedConfig.HasResource,
		HasMockAPI:     opts.TypedConfig.HasMockAPI,
	}
	if opts.Workspaces {
		data.APIPackage = layout.APIPackage
		data.HooksPackage = layout.HooksPackage
		data.UIWebPackage = layout.UIWebPackage
		// UINativePackage only surfaces in mobile (RN) templates —
		// the nextjs and vite-spa templates don't reference it (the
		// `{{.UINativePackage}}` tag never appears under those template
		// trees). Populate unconditionally for workspaces=on so the
		// RN package.json can refer to it; Next.js renders ignore it.
		data.UINativePackage = layout.UINativePackage
	}

	for _, file := range frontendFiles {
		// nav.tsx and dashboard.tsx ARE emitted here, empty of routes, even
		// though the entity set that seeds them does not exist yet.
		//
		// Withholding them until the first entity was the obvious move and is
		// wrong: layout.tsx imports { Nav } and page.tsx imports the
		// dashboard, so `forge scaffold frontend` — which has no generate
		// pass behind it — would hand back a tree that does not typecheck.
		// TestFrontendScaffoldImportsResolve pins that invariant.
		//
		// The nav generator keeps them current instead, refreshing while the
		// user has not touched them (emitScaffoldUntilTouched), so the routes
		// arrive with the first entity and ownership transfers on the first
		// edit. See internal/cli/generate_frontend_nav.go.

		content, err := templates.FrontendTemplates().Render(file.Path, data)
		if err != nil {
			return fmt.Errorf("render frontend template %s: %w", file.Path, err)
		}

		destFile := strings.TrimSuffix(file.Rel, ".tmpl")

		destPath := filepath.Join(frontendDir, destFile)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write frontend file %s: %w", destFile, err)
		}
		// Record the birth hash for the files the nav generator keeps
		// current. emitScaffoldUntilTouched refreshes them only while
		// ScaffoldIsPristine, and that answers false for a path with no
		// ledger entry — so without this write the two files are frozen as
		// "user-owned" from birth, and the routes seeded by the first
		// entity never arrive. Recorded here rather than in the nav
		// generator because this loop is what writes them first, and the
		// hash has to match the bytes that actually landed on disk.
		if rel, relErr := filepath.Rel(root, destPath); relErr == nil {
			if refreshedByNavGenerator(destFile) {
				checksums.RecordScaffoldWithHash(root, rel, content)
			}
		}
	}

	// Emit a nested go.mod so that `go test ./...` from the project root
	// skips this subtree. Frontends contain no first-party Go code, but
	// npm dependencies (e.g. flatted) occasionally ship .go files under
	// node_modules, which Go's package discovery would otherwise pick up.
	// A nested module is the idiomatic Go boundary marker.
	//
	// The `go` directive is read from the project's top-level go.mod so the
	// nested module stays in lockstep with the project's declared Go version
	// (no literal `go 1.25` to drift). Falls back to the generator's default
	// when the project go.mod is missing or unparseable (e.g. during the
	// first-ever scaffold before the project go.mod is written).
	goVersion := goVersionFromGoMod(root)
	if goVersion == "" {
		goVersion = defaultGoVersion
	}
	goModPath := filepath.Join(frontendDir, "go.mod")
	goModContent := fmt.Sprintf("// Nested module boundary so `go test ./...` from the project root\n"+
		"// skips node_modules and other frontend assets. This frontend has no\n"+
		"// first-party Go code; the module exists solely as a boundary marker.\n"+
		"module %s/frontends/%s\n\ngo %s\n", modulePath, frontendName, goVersion)
	if err := os.WriteFile(goModPath, []byte(goModContent), 0644); err != nil {
		return fmt.Errorf("write frontend go.mod: %w", err)
	}

	// Install core web UI components for browser-targeted frontends (Next.js
	// and Vite SPA). React Native uses platform-specific primitives and
	// should not receive web components.
	//
	// In workspaces mode the components live ONCE under packages/ui-web/
	// (emitted separately by WriteUIWebPackageFiles); frontends import them
	// via the `@<scope>/ui-web` workspace dep + a tsconfig path mapping
	// that redirects `@/components/*` → `packages/ui-web/src/components/*`.
	// Skipping the per-frontend copy here is what makes the multi-frontend
	// case stop diverging.
	if (tmplDir == "nextjs" || tmplDir == "vite-spa") && !opts.Workspaces {
		if err := installCoreComponents(frontendDir); err != nil {
			return fmt.Errorf("install core components: %w", err)
		}
	}

	// Emit the mock/scenario surface the scaffolded src/lib/connect.ts
	// references — statically under Vite ESM, and as a literal require()
	// (which webpack resolves at build time) under Next.js. Without it the
	// tree the scaffold hands back does not typecheck and does not build,
	// and `forge scaffold frontend` has no generate pass behind it to
	// fill the gap. Entity fixtures arrive on the next `forge generate`;
	// the no-entity render is complete and self-contained on its own — see
	// EmitFrontendMockSurface.
	//
	// The ownership state is loaded here rather than threaded in: this is
	// the scaffold path, where the frontend directory did not exist a
	// moment ago, so there is nothing disowned under it — but the writer
	// still records what it stamps, and reading the project's real state
	// keeps a re-scaffold over a disowned path honest.
	if frontendKindHasMockSurface(kind) {
		cs, err := LoadChecksums(root)
		if err != nil {
			return fmt.Errorf("load ownership state: %w", err)
		}
		feRel := filepath.Join("frontends", frontendName)
		if _, err := EmitFrontendMockSurface(root, feRel, nil, nil, nil, cs); err != nil {
			return fmt.Errorf("emit frontend mock surface: %w", err)
		}
		// The freshness guard ships with the frontend from birth, so its
		// file set does not change shape the first time a migration
		// lands. At scaffold time the project's schema is whatever is
		// already on disk — usually nothing, which renders the
		// no-migrations branch — and the next `forge generate` records
		// the real fingerprint.
		fpFiles, fpConfig := codegen.SeedFingerprint(root, seedplan.DefaultConfig())
		if err := EmitFixtureFreshnessSurface(root, feRel, fpFiles, fpConfig, cs); err != nil {
			return fmt.Errorf("emit fixture freshness surface: %w", err)
		}
		if err := SaveChecksums(root, cs); err != nil {
			return fmt.Errorf("save ownership state: %w", err)
		}
	}

	// Resolve the @reliant-labs/web-runtime specifier for THIS machine before
	// anything installs. The scaffold flow runs `npm install` straight after
	// generating the files, so a dev build has to have swapped the published
	// range for its `file:` bridge by now or that first install would go
	// looking for the package in the registry.
	EnsureWebRuntimeDependency(root, filepath.Join("frontends", frontendName), frontendName)

	return nil
}

// coreComponents lists the components automatically installed during scaffold.
//
// The list is deliberately split: the "primitives" group is the layered
// base library that the owned frontend components shipped in the base
// scaffold (`src/components/nav.tsx`, `src/components/session_nav.tsx`, …)
// import
// from instead of inlining their own button/input/etc. markup. The
// trailing "domain" group is higher-level building blocks the scaffold
// ships unconditionally because every forge frontend tends to reach for
// them.
var coreComponents = []string{
	// Primitives — base building blocks for every generated frontend.
	// "link" first: every navigating component (page_header,
	// row_actions_menu) imports "./link". The library copy is a plain
	// anchor; installCoreComponents/EnsureCoreComponents overwrite it
	// with a framework-aware version (see linkComponentForDir).
	"link",
	"button",
	"input",
	"label",
	"form",
	"card",
	"avatar",
	"tabs",
	"table",
	"select",
	"chip",
	"row_actions_menu",
	"progress_bar",
	"status_dot",

	// Domain components — higher-level shapes the scaffold ships by default.
	"sidebar_layout",
	"page_header",
	"badge",
	"modal",
	"skeleton_loader",
	"pagination",
	"search_input",
	"alert_banner",
	"toast_notification",
	"key_value_list",
	"login_form",
}

// installCoreComponents writes core UI components from the component library
// into the frontend's src/components/ui/ directory.
func installCoreComponents(frontendDir string) error {
	lib := components.NewLibrary()
	componentsDir := filepath.Join(frontendDir, "src", "components", "ui")
	if err := os.MkdirAll(componentsDir, 0755); err != nil {
		return fmt.Errorf("create components dir: %w", err)
	}

	for _, name := range coreComponents {
		content := componentContentForDir(frontendDir, name)
		if content == "" {
			c, err := lib.Get(name)
			if err != nil {
				return fmt.Errorf("get component %s: %w", name, err)
			}
			content = c
		}
		dest := filepath.Join(componentsDir, name+".tsx")
		if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
			return fmt.Errorf("write component %s: %w", name, err)
		}
	}
	return nil
}

// EnsureCoreComponents installs any missing core components into an existing
// frontend directory. Safe to call repeatedly — only writes files that don't
// already exist.
func EnsureCoreComponents(frontendDir string) error {
	lib := components.NewLibrary()
	componentsDir := filepath.Join(frontendDir, "src", "components", "ui")
	if err := os.MkdirAll(componentsDir, 0755); err != nil {
		return fmt.Errorf("create components dir: %w", err)
	}

	for _, name := range coreComponents {
		dest := filepath.Join(componentsDir, name+".tsx")
		if _, err := os.Stat(dest); err == nil {
			continue // already exists
		}
		content := componentContentForDir(frontendDir, name)
		if content == "" {
			c, err := lib.Get(name)
			if err != nil {
				return fmt.Errorf("get component %s: %w", name, err)
			}
			content = c
		}
		if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
			return fmt.Errorf("write component %s: %w", name, err)
		}
	}
	return nil
}

// componentContentForDir returns framework-specific override content for a
// component, or "" to use the library copy verbatim. Today only the "link"
// primitive is framework-specific: internal navigation must go through the
// host framework's router (next/link handles basePath + client transitions;
// tanstack-router's history does the SPA equivalent). The library's plain-
// anchor fallback would force a full page load AND 404 under a Next.js
// basePath deployment.
func componentContentForDir(frontendDir, name string) string {
	if name != "link" {
		return ""
	}
	switch detectFrontendKind(frontendDir) {
	case "nextjs":
		return nextLinkComponent
	case "vite-spa":
		return viteLinkComponent
	default:
		return ""
	}
}

// detectFrontendKind sniffs the frontend framework from config files the
// scaffold always lays down before components are installed. Returns
// "nextjs", "vite-spa", or "" when neither marker exists.
func detectFrontendKind(frontendDir string) string {
	for _, marker := range []string{"next.config.ts", "next.config.js", "next.config.mjs"} {
		if _, err := os.Stat(filepath.Join(frontendDir, marker)); err == nil {
			return "nextjs"
		}
	}
	if _, err := os.Stat(filepath.Join(frontendDir, "vite.config.ts")); err == nil {
		return "vite-spa"
	}
	return ""
}

// nextLinkComponent routes internal hrefs through next/link (client-side
// navigation + automatic basePath prefixing) and keeps plain anchors for
// external URLs. Generated pages and library components (page_header,
// row_actions_menu) import this instead of rendering raw <a href> — raw
// anchors break client routing and 404 under `--base-path` deployments.
const nextLinkComponent = `import NextLink from "next/link";
import React from "react";

/**
 * Link — the navigation primitive other library components route through
 * (PageHeader actions/breadcrumbs, RowActionsMenu href items, ...).
 *
 * Internal hrefs render next/link: client-side transitions, prefetching,
 * and automatic basePath prefixing. External URLs (http(s)://, mailto:,
 * tel:) render a plain <a> — next/link must never handle those.
 */

const EXTERNAL_HREF = /^(?:[a-z][a-z0-9+.-]*:)?\/\//i;

/** True for absolute/external URLs that must bypass client routing. */
export function isExternalHref(href: string): boolean {
  return (
    EXTERNAL_HREF.test(href) ||
    href.startsWith("mailto:") ||
    href.startsWith("tel:")
  );
}

export type LinkProps = React.AnchorHTMLAttributes<HTMLAnchorElement> & {
  href: string;
};

export default function Link({ href, children, ...rest }: LinkProps) {
  if (isExternalHref(href)) {
    return (
      <a href={href} {...rest}>
        {children}
      </a>
    );
  }
  return (
    <NextLink href={href} {...rest}>
      {children}
    </NextLink>
  );
}
`

// viteLinkComponent is the tanstack-router flavor: internal hrefs push
// through the router's history (SPA navigation, no full reload) while
// modified-click / new-tab semantics and external URLs keep native anchor
// behavior.
const viteLinkComponent = `import { useRouter } from "@tanstack/react-router";
import React from "react";

/**
 * Link — the navigation primitive other library components route through
 * (PageHeader actions/breadcrumbs, RowActionsMenu href items, ...).
 *
 * Internal hrefs navigate via tanstack-router's history (client-side, no
 * full reload). External URLs (http(s)://, mailto:, tel:) and modified
 * clicks (cmd/ctrl/shift, middle-click, target="_blank") keep native
 * anchor behavior.
 */

const EXTERNAL_HREF = /^(?:[a-z][a-z0-9+.-]*:)?\/\//i;

/** True for absolute/external URLs that must bypass client routing. */
export function isExternalHref(href: string): boolean {
  return (
    EXTERNAL_HREF.test(href) ||
    href.startsWith("mailto:") ||
    href.startsWith("tel:")
  );
}

export type LinkProps = React.AnchorHTMLAttributes<HTMLAnchorElement> & {
  href: string;
};

export default function Link({ href, children, onClick, target, ...rest }: LinkProps) {
  const router = useRouter();

  if (isExternalHref(href) || target === "_blank") {
    return (
      <a href={href} target={target} {...rest}>
        {children}
      </a>
    );
  }

  return (
    <a
      href={href}
      onClick={(e) => {
        onClick?.(e);
        if (
          e.defaultPrevented ||
          e.metaKey ||
          e.ctrlKey ||
          e.shiftKey ||
          e.altKey ||
          e.button !== 0
        ) {
          return;
        }
        e.preventDefault();
        router.history.push(href);
      }}
      {...rest}
    >
      {children}
    </a>
  );
}
`
