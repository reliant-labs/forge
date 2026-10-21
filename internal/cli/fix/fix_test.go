package fix

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

// Every release codemod is a `forge fix` subcommand, run over the project
// root; --dry-run reports and writes nothing.
func TestFix_CodemodSubcommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte("name: app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "internal", "app", "lifecycle.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const src = "package app\n\nimport \"github.com/reliant-labs/forge/pkg/lifecyclekit\"\n\nvar _ lifecyclekit.Options\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd := newCmd(&factory.Factory{Out: &out})
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("forge fix %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		return out.String()
	}

	if out := run("operatorkit", "--dry-run"); !strings.Contains(out, "would rewrite: internal/app/lifecycle.go") {
		t.Errorf("dry run output:\n%s", out)
	}
	if got, _ := os.ReadFile(path); string(got) != src {
		t.Fatalf("--dry-run wrote the file:\n%s", got)
	}
	run("operatorkit")
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "operatorkit.Options") {
		t.Fatalf("not rewritten:\n%s", got)
	}
	if out := run("operatorkit"); !strings.Contains(out, "nothing to rewrite") {
		t.Errorf("second run output:\n%s", out)
	}
}
