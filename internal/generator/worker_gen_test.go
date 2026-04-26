package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateWorkerFilesDefault(t *testing.T) {
	root := t.TempDir()

	if err := GenerateWorkerFiles(root, "example.com/myapp", "processor", "", ""); err != nil {
		t.Fatalf("GenerateWorkerFiles() error = %v", err)
	}

	workerDir := filepath.Join(root, "workers", "processor")

	// Both files must exist
	for _, f := range []string{"worker.go", "worker_test.go"} {
		if _, err := os.Stat(filepath.Join(workerDir, f)); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}

	// worker.go should use the default (non-cron) template
	content := readFile(t, filepath.Join(workerDir, "worker.go"))
	if !strings.Contains(content, "package processor") {
		t.Errorf("worker.go should have package processor, got:\n%s", content)
	}
	if !strings.Contains(content, "func (w *Worker) Start(ctx context.Context) error") {
		t.Error("worker.go should contain Start method")
	}
	if !strings.Contains(content, "func (w *Worker) Stop(ctx context.Context) error") {
		t.Error("worker.go should contain Stop method")
	}
	// Default worker should NOT have cron imports
	if strings.Contains(content, "robfig/cron") {
		t.Error("default worker should not import cron package")
	}
	if strings.Contains(content, "Schedule") {
		t.Error("default worker should not contain Schedule constant")
	}
}

func TestGenerateWorkerFilesCron(t *testing.T) {
	root := t.TempDir()

	if err := GenerateWorkerFiles(root, "example.com/myapp", "cleanup", "cron", "*/5 * * * *"); err != nil {
		t.Fatalf("GenerateWorkerFiles(cron) error = %v", err)
	}

	workerDir := filepath.Join(root, "workers", "cleanup")

	// Both files must exist
	for _, f := range []string{"worker.go", "worker_test.go"} {
		if _, err := os.Stat(filepath.Join(workerDir, f)); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}

	// worker.go should use the cron template
	content := readFile(t, filepath.Join(workerDir, "worker.go"))
	if !strings.Contains(content, "package cleanup") {
		t.Errorf("worker.go should have package cleanup, got:\n%s", content)
	}
	if !strings.Contains(content, `Schedule = "*/5 * * * *"`) {
		t.Error("cron worker.go should contain Schedule constant with the cron expression")
	}
	if !strings.Contains(content, "robfig/cron") {
		t.Error("cron worker should import robfig/cron")
	}
	if !strings.Contains(content, "func (w *Worker) Run()") {
		t.Error("cron worker should have Run method")
	}

	// worker_test.go should contain cron-specific test
	testContent := readFile(t, filepath.Join(workerDir, "worker_test.go"))
	if !strings.Contains(testContent, "TestCronWorkerStartStop") {
		t.Error("cron worker_test.go should contain TestCronWorkerStartStop")
	}
}
