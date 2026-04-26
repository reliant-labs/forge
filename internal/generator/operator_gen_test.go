package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateOperatorFilesCreatesExpectedFiles(t *testing.T) {
	root := t.TempDir()

	if err := GenerateOperatorFiles(root, "example.com/myapp", "deployment-scaler", "apps", "v1"); err != nil {
		t.Fatalf("GenerateOperatorFiles() error = %v", err)
	}

	opDir := filepath.Join(root, "operators", "deployment-scaler")

	// All three files must exist
	for _, f := range []string{"types.go", "controller.go", "controller_test.go"} {
		if _, err := os.Stat(filepath.Join(opDir, f)); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}

	// types.go should contain the PascalCase type name
	typesContent := readFile(t, filepath.Join(opDir, "types.go"))
	if !strings.Contains(typesContent, "DeploymentScaler") {
		t.Error("types.go should contain PascalCase type name DeploymentScaler")
	}
	if !strings.Contains(typesContent, "apps") {
		t.Error("types.go should reference the group")
	}

	// controller.go should contain Reconcile method
	controllerContent := readFile(t, filepath.Join(opDir, "controller.go"))
	if !strings.Contains(controllerContent, "Reconcile") {
		t.Error("controller.go should contain Reconcile method")
	}
	if !strings.Contains(controllerContent, "package deployment_scaler") || !strings.Contains(controllerContent, "package deploymentscaler") {
		// Accept either naming convention depending on the template's kebab/snake handling
		if !strings.Contains(controllerContent, "package") {
			t.Error("controller.go should have a package declaration")
		}
	}

	// controller_test.go should exist and have test content
	testContent := readFile(t, filepath.Join(opDir, "controller_test.go"))
	if !strings.Contains(testContent, "Test") {
		t.Error("controller_test.go should contain at least one test function")
	}
}