package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/naming"
)

// GenerateOperatorFiles generates all files for a single operator:
//   - operators/<name>/types.go           (from operator/types.go.tmpl)
//   - operators/<name>/controller.go      (from operator/controller.go.tmpl)
//   - operators/<name>/controller_test.go (from operator/controller_test.go.tmpl)
//
// Both the "new project" and "add operator" flows delegate here so the
// generated output is always identical.
func GenerateOperatorFiles(root, modulePath, name, group, version string) error {
	operatorDir := filepath.Join(root, "operators", name)

	if err := os.MkdirAll(operatorDir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", operatorDir, err)
	}

	data := struct {
		Name     string
		TypeName string
		Group    string
		Version  string
		Module   string
	}{
		Name:     name,
		TypeName: naming.ToPascalCase(name),
		Group:    group,
		Version:  version,
		Module:   modulePath,
	}

	// -- types.go (via operator/types.go.tmpl) --
	typesContent, err := renderOperatorTemplate("operator/types.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render types.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(operatorDir, "types.go"), typesContent, 0644); err != nil {
		return err
	}

	// -- controller.go (via operator/controller.go.tmpl) --
	controllerContent, err := renderOperatorTemplate("operator/controller.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render controller.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(operatorDir, "controller.go"), controllerContent, 0644); err != nil {
		return err
	}

	// -- controller_test.go (via operator/controller_test.go.tmpl) --
	testContent, err := renderOperatorTemplate("operator/controller_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render controller_test.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(operatorDir, "controller_test.go"), testContent, 0644); err != nil {
		return err
	}

	return nil
}

// renderOperatorTemplate renders an operator template from the embedded FS.
func renderOperatorTemplate(name string, data interface{}) ([]byte, error) {
	engine, err := getTemplateEngine()
	if err != nil {
		return nil, err
	}
	result, err := engine.RenderTemplate(name, data)
	if err != nil {
		return nil, err
	}
	return []byte(result), nil
}
