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
	// `next.config.ts`. Valid values: "static" (default), "standalone",
	// "server". See config.FrontendConfig.Output for the per-mode
	// semantics. Empty string defaults to "static" — the new
	// scaffold default since the static-default switchover.
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
	// Default the Next.js output shape to "static" when unset. Templates
	// (`next.config.ts.tmpl`) branch on this value; an empty string
	// would emit a malformed file. We canonicalise here rather than in
	// every template so callers can pass "" for "use the scaffold
	// default" without having to know what that default is.
	output := strings.ToLower(strings.TrimSpace(opts.Output))
	if output == "" {
		output = "static"
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
		content, err := lib.Get(name)
		if err != nil {
			return fmt.Errorf("get component %s: %w", name, err)
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
		content, err := lib.Get(name)
		if err != nil {
			return fmt.Errorf("get component %s: %w", name, err)
		}
		if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
			return fmt.Errorf("write component %s: %w", name, err)
		}
	}
	return nil
}
