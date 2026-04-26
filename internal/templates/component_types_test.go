package templates

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// --- Worker templates ---

func TestWorkerTemplatesRenderDefault(t *testing.T) {
	data := struct {
		Name     string
		Module   string
		Schedule string
	}{
		Name:   "processor",
		Module: "example.com/myapp",
	}

	content, err := WorkerTemplates.Render("worker.go.tmpl", data)
	if err != nil {
		t.Fatalf("render worker.go.tmpl: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "package processor") {
		t.Error("worker.go should have package processor")
	}
	if !strings.Contains(s, "func (w *Worker) Start(ctx context.Context) error") {
		t.Error("worker.go should contain Start method")
	}
	if !strings.Contains(s, "func (w *Worker) Stop(ctx context.Context) error") {
		t.Error("worker.go should contain Stop method")
	}
	if strings.Contains(s, "robfig/cron") {
		t.Error("default worker should not import cron")
	}
	if strings.Contains(s, "//go:build ignore") {
		t.Error("rendered output should not retain //go:build ignore")
	}
}

func TestWorkerTemplatesRenderTest(t *testing.T) {
	data := struct {
		Name     string
		Module   string
		Schedule string
	}{
		Name:   "processor",
		Module: "example.com/myapp",
	}

	content, err := WorkerTemplates.Render("worker_test.go.tmpl", data)
	if err != nil {
		t.Fatalf("render worker_test.go.tmpl: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "package processor") {
		t.Error("worker_test.go should have package processor")
	}
	if !strings.Contains(s, "Test") {
		t.Error("worker_test.go should contain test functions")
	}
}

// --- Worker-cron templates ---

func TestWorkerCronTemplatesRender(t *testing.T) {
	data := struct {
		Name     string
		Module   string
		Schedule string
	}{
		Name:     "cleanup",
		Module:   "example.com/myapp",
		Schedule: "0 * * * *",
	}

	content, err := WorkerCronTemplates.Render("worker.go.tmpl", data)
	if err != nil {
		t.Fatalf("render worker-cron/worker.go.tmpl: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "package cleanup") {
		t.Error("cron worker.go should have package cleanup")
	}
	if !strings.Contains(s, `Schedule = "0 * * * *"`) {
		t.Error("cron worker should embed the schedule constant")
	}
	if !strings.Contains(s, "robfig/cron") {
		t.Error("cron worker should import robfig/cron")
	}
	if !strings.Contains(s, "func (w *Worker) Run()") {
		t.Error("cron worker should have Run method")
	}
	if strings.Contains(s, "//go:build ignore") {
		t.Error("rendered output should not retain //go:build ignore")
	}
}

func TestWorkerCronTemplatesRenderTest(t *testing.T) {
	data := struct {
		Name     string
		Module   string
		Schedule string
	}{
		Name:     "cleanup",
		Module:   "example.com/myapp",
		Schedule: "0 * * * *",
	}

	content, err := WorkerCronTemplates.Render("worker_test.go.tmpl", data)
	if err != nil {
		t.Fatalf("render worker-cron/worker_test.go.tmpl: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "TestCronWorkerStartStop") {
		t.Error("cron worker_test.go should contain TestCronWorkerStartStop")
	}
	if !strings.Contains(s, "TestCronWorkerName") {
		t.Error("cron worker_test.go should contain TestCronWorkerName")
	}
}

// --- Operator templates ---

func TestOperatorTemplatesRender(t *testing.T) {
	data := struct {
		Name     string
		TypeName string
		Group    string
		Version  string
		Module   string
	}{
		Name:     "scaler",
		TypeName: "Scaler",
		Group:    "apps",
		Version:  "v1",
		Module:   "example.com/myapp",
	}

	// types.go
	typesContent, err := OperatorTemplates.Render("types.go.tmpl", data)
	if err != nil {
		t.Fatalf("render operator/types.go.tmpl: %v", err)
	}
	ts := string(typesContent)
	if !strings.Contains(ts, "type Scaler struct") {
		t.Error("types.go should define Scaler struct")
	}
	if !strings.Contains(ts, "type ScalerSpec struct") {
		t.Error("types.go should define ScalerSpec")
	}
	if !strings.Contains(ts, "type ScalerStatus struct") {
		t.Error("types.go should define ScalerStatus")
	}
	if !strings.Contains(ts, `Group: "apps"`) {
		t.Error("types.go should embed the group")
	}
	if !strings.Contains(ts, `Version: "v1"`) {
		t.Error("types.go should embed the version")
	}
	if strings.Contains(ts, "//go:build ignore") {
		t.Error("rendered output should not retain //go:build ignore")
	}

	// controller.go
	ctrlContent, err := OperatorTemplates.Render("controller.go.tmpl", data)
	if err != nil {
		t.Fatalf("render operator/controller.go.tmpl: %v", err)
	}
	cs := string(ctrlContent)
	if !strings.Contains(cs, "func (c *Controller) Reconcile(") {
		t.Error("controller.go should have Reconcile method")
	}
	if !strings.Contains(cs, "func (c *Controller) SetupWithManager(") {
		t.Error("controller.go should have SetupWithManager method")
	}
	if !strings.Contains(cs, "var obj Scaler") {
		t.Error("controller.go should reference the Scaler type")
	}

	// controller_test.go
	testContent, err := OperatorTemplates.Render("controller_test.go.tmpl", data)
	if err != nil {
		t.Fatalf("render operator/controller_test.go.tmpl: %v", err)
	}
	tts := string(testContent)
	if !strings.Contains(tts, "TestReconcile") {
		t.Error("controller_test.go should contain TestReconcile")
	}
	if !strings.Contains(tts, "TestReconcileNotFound") {
		t.Error("controller_test.go should contain TestReconcileNotFound")
	}
}

func TestBootstrapTemplate_WithAllComponentTypes(t *testing.T) {
	data := struct {
		Module   string
		Services []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Packages []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Workers []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Operators []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		HasDatabase  bool
		OrmEnabled   bool
		HasFallible  bool
		ConfigFields map[string]bool
	}{
		Module:       "example.com/fullproject",
		ConfigFields: map[string]bool{},
		Services: []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}{
			{Name: "api", Package: "api", FieldName: "API"},
		},
		Workers: []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}{
			{Name: "indexer", Package: "indexer", FieldName: "Indexer"},
		},
		Operators: []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}{
			{Name: "scaler", Package: "scaler", FieldName: "Scaler"},
		},
		HasDatabase: true,
	}

	content, err := ProjectTemplates.Render("bootstrap.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap.go.tmpl with all types: %v", err)
	}

	rendered := string(content)

	// All component type structs should be present
	for _, expected := range []string{
		"func Bootstrap(",
		"func BootstrapOnly(",
		"func (a *App) Shutdown(",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("bootstrap with all types missing: %s", expected)
		}
	}

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered bootstrap.go with all types does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}
}
