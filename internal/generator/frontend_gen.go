package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/components"
	"github.com/reliant-labs/forge/internal/templates"
)

// frontendTemplateDir returns the template subdirectory for the given kind.
// kind="mobile" uses react-native templates; kind="vite-spa" uses vite-spa
// templates; everything else (including "" and "web") uses nextjs.
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
}

// GenerateFrontendFiles generates the frontend directory and files.
// kind selects the template set: "" or "web" for Next.js, "mobile" for React Native.
// Both the "new" project flow and the "add frontend" flow delegate here so
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

	frontendFiles, err := templates.FrontendTemplates().List(tmplDir)
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
	// shipped Dockerfile (.next/standalone/server.js). We canonicalise
	// here rather than in every template so callers can pass "" for
	// "use the scaffold default" without having to know what it is.
	output := strings.ToLower(strings.TrimSpace(opts.Output))
	if output == "" {
		output = "standalone"
	}
	data := templates.FrontendTemplateData{
		FrontendName: frontendName,
		ProjectName:  projectName,
		ApiUrl:       fmt.Sprintf("http://localhost:%d", apiPort),
		ApiPort:      fmt.Sprintf("%d", apiPort),
		Module:       modulePath,
		Workspaces:   opts.Workspaces,
		Output:       output,
		BasePath:     opts.BasePath,
	}
	if opts.Workspaces {
		data.ApiPackage = layout.ApiPackage
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
		content, err := templates.FrontendTemplates().Render(filepath.Join(tmplDir, file), data)
		if err != nil {
			return fmt.Errorf("render frontend template %s: %w", file, err)
		}

		destFile := strings.TrimSuffix(file, ".tmpl")

		destPath := filepath.Join(frontendDir, destFile)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write frontend file %s: %w", destFile, err)
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

	return nil
}

// coreComponents lists the components automatically installed during scaffold.
//
// The list is deliberately split: the "primitives" group is the layered
// base library that frontend packs (`data-table`, `auth-ui`, …) MUST
// import from instead of inlining their own button/input/etc. markup.
// The trailing "domain" group is higher-level building blocks the
// scaffold ships unconditionally because every forge frontend tends to
// reach for them.
var coreComponents = []string{
	// Primitives — base building blocks for every frontend pack.
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
