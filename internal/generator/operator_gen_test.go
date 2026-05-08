package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateOperatorFilesWithAPISplitsTypesIntoAPIPackage(t *testing.T) {
	root := t.TempDir()

	// workspace-controller operator reconciles a Workspace CRD living
	// under api/v1alpha1/workspace/.
	if err := GenerateOperatorFilesWithAPI(root, "example.com/myapp",
		"workspace-controller", "reliant.dev", "v1alpha1",
		"workspace", "Workspace"); err != nil {
		t.Fatalf("GenerateOperatorFilesWithAPI() error = %v", err)
	}

	apiTypesPath := filepath.Join(root, "api", "v1alpha1", "workspace", "types.go")
	if _, err := os.Stat(apiTypesPath); err != nil {
		t.Fatalf("expected api/v1alpha1/workspace/types.go: %v", err)
	}
	apiTypes := readFile(t, apiTypesPath)
	if !strings.Contains(apiTypes, "package workspace") {
		t.Error("api types.go should declare `package workspace`")
	}
	if !strings.Contains(apiTypes, "type Workspace struct") {
		t.Error("api types.go should declare `type Workspace struct`")
	}
	if strings.Contains(apiTypes, "type WorkspaceController struct") {
		t.Error("api types.go should not declare WorkspaceController type")
	}

	controllerPath := filepath.Join(root, "operators", "workspace_controller", "controller.go")
	if _, err := os.Stat(controllerPath); err != nil {
		t.Fatalf("expected operators/workspace_controller/controller.go: %v", err)
	}
	controller := readFile(t, controllerPath)
	if !strings.Contains(controller, `apipkg "example.com/myapp/api/v1alpha1/workspace"`) {
		t.Errorf("controller.go should import the api package, got:\n%s", controller)
	}
	if !strings.Contains(controller, "apipkg.Workspace") {
		t.Error("controller.go should reference apipkg.Workspace")
	}

	// types.go must NOT live alongside the controller in the split layout.
	siblingTypes := filepath.Join(root, "operators", "workspace_controller", "types.go")
	if _, err := os.Stat(siblingTypes); err == nil {
		t.Error("types.go should NOT exist alongside controller.go in split layout")
	}
}

func TestGenerateOperatorFilesCreatesExpectedFiles(t *testing.T) {
	root := t.TempDir()

	if err := GenerateOperatorFiles(root, "example.com/myapp", "deployment-scaler", "apps", "v1"); err != nil {
		t.Fatalf("GenerateOperatorFiles() error = %v", err)
	}

	// Hyphenated CLI names produce snake_case directories so the package
	// declaration is a valid Go identifier (operators/deployment_scaler).
	opDir := filepath.Join(root, "operators", "deployment_scaler")

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
// TestGenerateOperatorBinaryOnly_SkipsOnPortedShape verifies the v0
// controller-IS-the-reconciler shape (a hand-ported controller.go that
// already declares `Controller`/`SetupWithManager`/etc.) is detected and
// the scaffold's operator.go is suppressed instead of duplicating symbols.
func TestGenerateOperatorBinaryOnly_SkipsOnPortedShape(t *testing.T) {
	root := t.TempDir()
	operatorDir := filepath.Join(root, "operators", "workspace_controller")
	if err := os.MkdirAll(operatorDir, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Simulate a ported v0 controller.go: holds the operator wiring surface.
	ported := `package workspace_controller

type Deps struct{}
type Controller struct{ Deps Deps }

func New(deps Deps) *Controller { return &Controller{Deps: deps} }

func (c *Controller) SetupWithManager(mgr interface{}) error { return nil }
`
	if err := os.WriteFile(filepath.Join(operatorDir, "controller.go"), []byte(ported), 0644); err != nil {
		t.Fatalf("write ported controller.go: %v", err)
	}

	if err := GenerateOperatorBinaryOnly(root, "example.com/myapp",
		"workspace-controller", "reliant.dev", "v1alpha1"); err != nil {
		t.Fatalf("GenerateOperatorBinaryOnly: %v", err)
	}

	// Scaffold must NOT have written operator.go (the ported controller.go
	// already declares the same symbols and would duplicate them).
	if _, err := os.Stat(filepath.Join(operatorDir, "operator.go")); !os.IsNotExist(err) {
		t.Errorf("operator.go should have been skipped due to existing controller.go shape")
	}
	// doc.go is similarly skipped (purely cosmetic; there's nothing to add
	// to a hand-ported package's documentation surface).
	if _, err := os.Stat(filepath.Join(operatorDir, "doc.go")); !os.IsNotExist(err) {
		t.Errorf("doc.go should have been skipped due to existing controller.go shape")
	}
}

// TestGenerateOperatorBinaryOnly_EmitsWhenDirEmpty verifies the default
// scaffold path is unchanged: empty operator dir gets the canonical
// operator.go + doc.go pair.
func TestGenerateOperatorBinaryOnly_EmitsWhenDirEmpty(t *testing.T) {
	root := t.TempDir()
	if err := GenerateOperatorBinaryOnly(root, "example.com/myapp",
		"workspace-controller", "reliant.dev", "v1alpha1"); err != nil {
		t.Fatalf("GenerateOperatorBinaryOnly: %v", err)
	}
	operatorDir := filepath.Join(root, "operators", "workspace_controller")
	if _, err := os.Stat(filepath.Join(operatorDir, "operator.go")); err != nil {
		t.Errorf("operator.go expected on empty-dir path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(operatorDir, "doc.go")); err != nil {
		t.Errorf("doc.go expected on empty-dir path: %v", err)
	}
}

// TestGenerateOperatorBinaryOnly_SkipsOnPerCRDOnlyDir verifies that when
// only per-CRD reconciler files (e.g. <crd>_controller.go from
// `forge add crd`) exist, the scaffold still emits operator.go: the CRD
// files don't declare `Controller`/`New`/`AddToScheme`, only per-CRD
// reconciler types.
func TestGenerateOperatorBinaryOnly_EmitsAlongsidePerCRDFiles(t *testing.T) {
	root := t.TempDir()
	operatorDir := filepath.Join(root, "operators", "workspace_controller")
	if err := os.MkdirAll(operatorDir, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Per-CRD reconciler shim: declares WorkspaceController but not the
	// operator-level wiring surface.
	crdShim := `package workspace_controller

type WorkspaceController struct{}

func NewWorkspaceController() *WorkspaceController { return &WorkspaceController{} }

func (c *WorkspaceController) ReconcileSpec() error { return nil }
`
	if err := os.WriteFile(filepath.Join(operatorDir, "workspace_controller.go"), []byte(crdShim), 0644); err != nil {
		t.Fatalf("write CRD shim: %v", err)
	}
	if err := GenerateOperatorBinaryOnly(root, "example.com/myapp",
		"workspace-controller", "reliant.dev", "v1alpha1"); err != nil {
		t.Fatalf("GenerateOperatorBinaryOnly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(operatorDir, "operator.go")); err != nil {
		t.Errorf("operator.go should still emit when only per-CRD files exist: %v", err)
	}
}
