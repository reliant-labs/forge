package cigate

import (
	"context"
	"strings"
	"testing"
)

// readerGit is fakeGit plus `git ls-files --cached`: the tracked files.
type readerGit struct {
	fakeGit
	tracked []string
}

func (r readerGit) run(ctx context.Context, dir string, args ...string) (string, error) {
	if len(args) >= 2 && args[0] == "ls-files" && args[1] == "--cached" {
		return strings.Join(r.tracked, "\n"), nil
	}
	return r.fakeGit.run(ctx, dir, args...)
}

// readerSrc is package c's test: it locates the repo root and reads
// deploy/kcl/ under it, and builds a fixture path under t.TempDir().
const readerSrc = `package c

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func repoRootDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..")
}

func TestReads(t *testing.T) {
	_, _ = os.ReadFile(filepath.Join(repoRootDir(), "deploy", "kcl"))
	_, _ = os.ReadFile(filepath.Join(t.TempDir(), "docs", "guide"))
	_ = "config/settings.yaml"
}
`

func readerRoot(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	write(t, root, "c/c_test.go", readerSrc)
	tracked := []string{"c/c_test.go", "deploy/kcl/main.k", "deploy/kcl/lib/x.k", "docs/guide/index.md", "config/settings.yaml", "scripts/run.sh"}
	for _, f := range tracked[1:] {
		write(t, root, f, "x")
	}
	return root, tracked
}

func TestAffectedReaders(t *testing.T) {
	cases := []struct {
		name     string
		readers  bool
		changed  string
		wantFull bool
		wantPkgs string
		wantSel  string
	}{
		{name: "file under a joined root dir", readers: true, changed: "deploy/kcl/lib/x.k", wantPkgs: "ex.com/m/c", wantSel: "ex.com/m/c (names deploy/kcl)"},
		{name: "literal naming the file", readers: true, changed: "config/settings.yaml", wantPkgs: "ex.com/m/c", wantSel: "config/settings.yaml changed → ex.com/m/c (its"},
		{name: "join onto TempDir names nothing", readers: true, changed: "docs/guide/index.md", wantFull: true},
		{name: "no reader still goes full", readers: true, changed: "scripts/run.sh", wantFull: true},
		{name: "off by default", readers: false, changed: "deploy/kcl/main.k", wantFull: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, tracked := readerRoot(t)
			g := readerGit{fakeGit: fakeGit{branch: "f", changed: []string{tc.changed}}, tracked: tracked}
			a := ComputeAffected(context.Background(), AffectedInput{Root: root, Git: g.run, GoList: listed(root), Readers: tc.readers})
			if a.Full != tc.wantFull {
				t.Fatalf("full = %v (%s), want %v", a.Full, a.Reason, tc.wantFull)
			}
			if tc.wantFull {
				if !strings.Contains(a.Reason, tc.changed) {
					t.Errorf("reason %q does not name %s", a.Reason, tc.changed)
				}
				return
			}
			if got := strings.Join(a.Packages, ","); got != tc.wantPkgs {
				t.Errorf("packages = %q, want %q", got, tc.wantPkgs)
			}
			if len(a.Selections) != 1 || !strings.Contains(a.Selections[0], tc.wantSel) {
				t.Errorf("selections = %q, want one containing %q", a.Selections, tc.wantSel)
			}
		})
	}
}

func TestReaderScan_PackageNotLocatingRootIsNotScanned(t *testing.T) {
	root := t.TempDir()
	write(t, root, "c/c_test.go", "package c\n\nvar _ = \"config/settings.yaml\"\n")
	write(t, root, "config/settings.yaml", "x")
	g := readerGit{fakeGit: fakeGit{branch: "f", changed: []string{"config/settings.yaml"}}, tracked: []string{"c/c_test.go", "config/settings.yaml"}}
	a := ComputeAffected(context.Background(), AffectedInput{Root: root, Git: g.run, GoList: listed(root), Readers: true})
	if !a.Full {
		t.Fatalf("a package that never locates the root reads only its own dir; got packages %v", a.Packages)
	}
}
