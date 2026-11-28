package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/testkit"
)

// snapshotDryRunTree maps every entry under root (directories included, so an
// empty directory a dry run created is caught too) to its content.
func snapshotDryRunTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			out[filepath.ToSlash(rel)+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runProjectNewCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		cmd := newNewCmd()
		cmd.SetArgs(args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		runErr = cmd.ExecuteContext(t.Context())
	})
	return out, runErr
}

// A service project adopted in place: --dry-run lists the files it would
// write and the user's files it would keep, says what happens to their
// .gitignore and which steps follow, and changes nothing in the directory.
func TestProjectNewDryRunInPlaceServiceWritesNothing(t *testing.T) {
	testkit.Slow(t, "renders a whole service scaffold and execs git")
	root := t.TempDir()
	for name, body := range map[string]string{
		"README.md":  "# shop\n",
		".gitignore": "node_modules/\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotDryRunTree(t, root)

	out, err := runProjectNewCmd(t, "--in-place", "--path", root, "--name", "shop", "--service", "orders", "--harness", "none", "--dry-run")
	if err != nil {
		t.Fatalf("project new --in-place --dry-run: %v\n%s", err, out)
	}
	after := snapshotDryRunTree(t, root)
	if len(after) != len(before) {
		t.Fatalf("--dry-run changed the directory: %d entries before, %d after", len(before), len(after))
	}
	for rel, body := range before {
		if after[rel] != body {
			t.Errorf("--dry-run changed %s", rel)
		}
	}
	for _, want := range []string{
		"Nothing was written",
		`service project "shop"`,
		"services: orders",
		"\nWould write (",
		"\n  forge.yaml\n",
		"\n  proto/services/orders/v1/orders.proto\n",
		"Would keep existing (1):\n  README.md\n",
		".gitignore: would merge forge's entries with yours",
		"Then a real run would:",
		"generate to bootstrap generated code",
		"git init and attempt an initial commit",
		"the same command without --dry-run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Would replace") {
		t.Errorf("without --force nothing is replaced:\n%s", out)
	}
}

// --force turns every kept file into a replaced one, and still writes nothing.
func TestProjectNewDryRunForceListsReplacements(t *testing.T) {
	testkit.Slow(t, "renders a whole service scaffold and execs git")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runProjectNewCmd(t, "--in-place", "--path", root, "--name", "shop", "--harness", "none", "--force", "--dry-run")
	if err != nil {
		t.Fatalf("project new --in-place --force --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Would replace (--force) (1):\n  README.md\n") {
		t.Errorf("README.md not listed as replaced:\n%s", out)
	}
	if got := readFile(t, filepath.Join(root, "README.md")); got != "# shop\n" {
		t.Errorf("--dry-run --force replaced README.md: %q", got)
	}
}

// Every Go kind previews, a fresh project included: its directory is never
// created.
func TestProjectNewDryRunFreshKinds(t *testing.T) {
	testkit.Slow(t, "renders project scaffolds and execs git")
	for _, tc := range []struct {
		kind, file string
		extra      []string
	}{
		{"service", "proto/services/orders/v1/orders.proto", []string{"--service", "orders"}},
		{"cli", "cmd/tool/main.go", nil},
		{"library", "pkg/tool/doc.go", nil},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			parent := t.TempDir()
			out, err := runProjectNewCmd(t, append([]string{"tool", "--kind", tc.kind, "--path", parent, "--harness", "none", "--dry-run"}, tc.extra...)...)
			if err != nil {
				t.Fatalf("--kind %s --dry-run: %v\n%s", tc.kind, err, out)
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 0 {
				t.Fatalf("--dry-run wrote %d entries into %s", len(entries), parent)
			}
			if got, want := strings.Contains(out, "run go mod tidy"), tc.kind == "service"; got != want {
				t.Errorf("go mod tidy step present = %t, want %t:\n%s", got, want, out)
			}
			for _, want := range []string{
				"Nothing was written",
				tc.kind + ` project "tool" into a new directory`,
				"\n  forge.yaml\n",
				"\n  " + tc.file + "\n",
				"\n  .gitignore\n",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("dry-run output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}
