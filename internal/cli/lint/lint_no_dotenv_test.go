package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDotenvTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("K=v\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A project with no .env files passes.
func TestNoDotenvLint_CleanProjectPasses(t *testing.T) {
	root := t.TempDir()
	writeDotenvTestFile(t, filepath.Join(root, "deploy", "kcl", "dev", "main.k"))
	writeDotenvTestFile(t, filepath.Join(root, "secrets", "dev", "STRIPE_SECRET_KEY"))

	if err := runNoDotenvLint(root); err != nil {
		t.Fatalf("clean project should pass, got: %v", err)
	}
}

// Every .env* spelling is caught — including the ones a developer is most
// likely to copy in from an older project.
func TestNoDotenvLint_CatchesEveryDotenvSpelling(t *testing.T) {
	for _, name := range []string{
		".env",
		".env.dev",
		".env.dev.secrets",
		".env.local",
		".env.production",
		".env.example",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeDotenvTestFile(t, filepath.Join(root, name))

			err := runNoDotenvLint(root)
			if err == nil {
				t.Fatalf("%s should fail the no-dotenv lint", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error should name %s, got: %v", name, err)
			}
		})
	}
}

// The message has to say what to do instead, or it just blocks someone
// with no path forward.
func TestNoDotenvLint_MessageGivesTheFix(t *testing.T) {
	root := t.TempDir()
	writeDotenvTestFile(t, filepath.Join(root, ".env.dev.secrets"))

	err := runNoDotenvLint(root)
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "forge secret migrate") {
		t.Fatalf("a *.secrets file should point at `forge secret migrate`, got: %v", msg)
	}
	if !strings.Contains(msg, "forge secret set") {
		t.Fatalf("message should name the command that stores a value, got: %v", msg)
	}
}

// A .env inside node_modules is a dependency's file, not the project's.
func TestNoDotenvLint_SkipsVendoredTrees(t *testing.T) {
	root := t.TempDir()
	writeDotenvTestFile(t, filepath.Join(root, "node_modules", "pkg", ".env"))
	writeDotenvTestFile(t, filepath.Join(root, "vendor", "x", ".env.dev"))
	writeDotenvTestFile(t, filepath.Join(root, ".git", ".env"))

	if err := runNoDotenvLint(root); err != nil {
		t.Fatalf("vendored .env files should be ignored, got: %v", err)
	}
}

// Nested project files are still the project's own.
func TestNoDotenvLint_CatchesNestedProjectFiles(t *testing.T) {
	root := t.TempDir()
	writeDotenvTestFile(t, filepath.Join(root, "frontends", "web", ".env.local"))

	err := runNoDotenvLint(root)
	if err == nil {
		t.Fatal("a .env under a project subdirectory should fail")
	}
	if !strings.Contains(err.Error(), filepath.Join("frontends", "web", ".env.local")) {
		t.Fatalf("error should give the relative path, got: %v", err)
	}
}

// JSON mode reports one finding per file with a stable rule name.
func TestNoDotenvLint_JSONFindings(t *testing.T) {
	root := t.TempDir()
	writeDotenvTestFile(t, filepath.Join(root, ".env"))
	writeDotenvTestFile(t, filepath.Join(root, ".env.dev"))

	findings, err := collectNoDotenvJSON(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}
	for _, f := range findings {
		if f.Rule != "no-dotenv" {
			t.Errorf("rule = %q, want no-dotenv", f.Rule)
		}
		if f.Severity != "error" {
			t.Errorf("severity = %q, want error", f.Severity)
		}
	}
}
