package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/components"
	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateFrontendFiles generates the frontend directory and files for a
// Next.js frontend. Both the "new" project flow and the "add frontend" flow
// delegate here so the output is always identical.
func GenerateFrontendFiles(root, modulePath, projectName, frontendName string, apiPort int) error {
	frontendDir := filepath.Join(root, "frontends", frontendName)
	if err := os.MkdirAll(frontendDir, 0755); err != nil {
		return fmt.Errorf("create frontend directory: %w", err)
	}

	frontendFiles, err := templates.FrontendTemplates.List("nextjs")
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
		content, err := templates.FrontendTemplates.Render(filepath.Join("nextjs", file), data)
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

	// Install core UI components from the component library.
	if err := installCoreComponents(frontendDir); err != nil {
		return fmt.Errorf("install core components: %w", err)
	}

	return nil
}

// coreComponents lists the components automatically installed during scaffold.
var coreComponents = []string{
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