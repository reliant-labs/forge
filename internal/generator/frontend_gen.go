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
// kind="mobile" uses react-native templates; everything else uses nextjs.
func frontendTemplateDir(kind string) string {
	if kind == "mobile" {
		return "react-native"
	}
	return "nextjs"
}

// GenerateFrontendFiles generates the frontend directory and files.
// kind selects the template set: "" or "web" for Next.js, "mobile" for React Native.
// Both the "new" project flow and the "add frontend" flow delegate here so
// the output is always identical.
func GenerateFrontendFiles(root, modulePath, projectName, frontendName string, apiPort int, kind string) error {
	frontendDir := filepath.Join(root, "frontends", frontendName)
	if err := os.MkdirAll(frontendDir, 0755); err != nil {
		return fmt.Errorf("create frontend directory: %w", err)
	}

	tmplDir := frontendTemplateDir(kind)

	frontendFiles, err := templates.FrontendTemplates().List(tmplDir)
	if err != nil {
		return fmt.Errorf("list frontend templates: %w", err)
	}

	data := templates.FrontendTemplateData{
		FrontendName: frontendName,
		ProjectName:  projectName,
		ApiUrl:       fmt.Sprintf("http://localhost:%d", apiPort),
		ApiPort:      fmt.Sprintf("%d", apiPort),
		Module:       modulePath,
	}

	for _, file := range frontendFiles {
		content, err := templates.FrontendTemplates().Render(filepath.Join(tmplDir, file), data)
		if err != nil {
			return fmt.Errorf("render frontend template %s: %w", file, err)
		}

		destFile := file
		if strings.HasSuffix(destFile, ".tmpl") {
			destFile = strings.TrimSuffix(destFile, ".tmpl")
		}

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

	// Install core web UI components for Next.js frontends only. React Native
	// uses platform-specific primitives and should not receive web components.
	if tmplDir == "nextjs" {
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
