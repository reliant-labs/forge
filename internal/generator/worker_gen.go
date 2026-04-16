package generator

import (
	"fmt"
	"os"
	"path/filepath"
)

// GenerateWorkerFiles generates all files for a single worker:
//   - workers/<name>/worker.go       (from worker/worker.go.tmpl)
//   - workers/<name>/worker_test.go   (from worker/worker_test.go.tmpl)
//
// Both the "new project" and "add worker" flows delegate here so the
// generated output is always identical.
func GenerateWorkerFiles(root, modulePath, workerName string) error {
	workerDir := filepath.Join(root, "workers", workerName)

	if err := os.MkdirAll(workerDir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", workerDir, err)
	}

	data := struct {
		Name   string
		Module string
	}{
		Name:   workerName,
		Module: modulePath,
	}

	// -- worker.go (via worker/worker.go.tmpl) --
	workerContent, err := renderWorkerTemplate("worker/worker.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render worker.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), workerContent, 0644); err != nil {
		return err
	}

	// -- worker_test.go (via worker/worker_test.go.tmpl) --
	testContent, err := renderWorkerTemplate("worker/worker_test.go.tmpl", data)
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
