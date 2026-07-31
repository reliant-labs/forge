package lint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContractFixture lays down a minimal self-contained module whose
// internal/widgets package violates the contract rule: impl carries an
// exported method (Rogue) not declared on the contract Service
// interface.
func writeContractFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.24\n",
		"internal/widgets/contract.go": `package widgets

type Service interface {
	Do() error
}

type Deps struct{}

func New(Deps) Service { return &impl{} }
`,
		"internal/widgets/impl.go": `package widgets

type impl struct{}

func (i *impl) Do() error { return nil }

// Rogue is an exported method outside the contract interface — the
// single-seam violation the contract analyzer exists to catch.
func (i *impl) Rogue() {}
`,
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The in-process driver must find real violations — proof the analysis
// actually runs inside forge (no separately-installed contractlint
// binary involved, so no version skew is possible).
func TestContractAnalysisInProcess_FindsViolations(t *testing.T) {
	dir := writeContractFixture(t)
	t.Chdir(dir)

	diags, err := runContractAnalysisInProcess(context.Background(), []string{"./..."}, nil)
	if err != nil {
		t.Fatalf("in-process contract analysis: %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "Rogue") {
			found = true
			if d.Pos.Line == 0 || !strings.HasSuffix(d.Pos.Filename, "impl.go") {
				t.Errorf("diagnostic position not resolved: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("expected a violation naming Rogue, got %v", diags)
	}
}

// forge.yaml's contracts.exclude list must still be honored in-process
// (the subprocess passed it via -exclude; in-process it reaches
// contract.SetExcludes directly).
func TestContractAnalysisInProcess_HonorsExcludes(t *testing.T) {
	dir := writeContractFixture(t)
	t.Chdir(dir)

	diags, err := runContractAnalysisInProcess(context.Background(), []string{"./..."}, []string{"internal/widgets"})
	if err != nil {
		t.Fatalf("in-process contract analysis: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("excluded package must produce no findings, got %v", diags)
	}
}
