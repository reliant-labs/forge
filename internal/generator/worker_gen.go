package generator

import (
	"fmt"
	"os"
	"path/filepath"
)

// GenerateWorkerFiles generates all files for a single worker:
//   - workers/<package>/worker.go       (from worker/worker.go.tmpl or worker-cron/worker.go.tmpl)
//   - workers/<package>/worker_test.go   (from worker/worker_test.go.tmpl or worker-cron/worker_test.go.tmpl)
//
// The CLI/display name (which may contain hyphens) is translated to a
// Go-package-safe form for the directory and `package` declaration so
// hyphenated worker names like "email-sender" produce a buildable
// workers/email_sender/ package.
//
// When kind is "cron", the cron-specific templates are used and the schedule
// is embedded as a constant in the generated code.
//
// Both the "new project" and "add worker" flows delegate here so the
// generated output is always identical.
func GenerateWorkerFiles(root, modulePath, workerName, kind, schedule string) error {
	workerPackage := ServicePackageName(workerName)
	workerDir := filepath.Join(root, "workers", workerPackage)

	if err := os.MkdirAll(workerDir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", workerDir, err)
	}

	// Select template prefix based on kind.
	tmplPrefix := "worker"
	if kind == "cron" {
		tmplPrefix = "worker-cron"
	}

	data := struct {
		Name     string // display form, may contain hyphens
		Package  string // Go-package-safe form
		Module   string
		Schedule string
	}{
		Name:     workerName,
		Package:  workerPackage,
		Module:   modulePath,
		Schedule: schedule,
	}

	// -- worker.go --
	workerContent, err := renderWorkerTemplate(tmplPrefix+"/worker.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render worker.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), workerContent, 0644); err != nil {
		return err
	}

	// -- worker_test.go --
	testContent, err := renderWorkerTemplate(tmplPrefix+"/worker_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render worker_test.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "worker_test.go"), testContent, 0644); err != nil {
		return err
	}

	return nil
}

// renderWorkerTemplate renders a worker template from the embedded FS.
func renderWorkerTemplate(name string, data interface{}) ([]byte, error) {
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
