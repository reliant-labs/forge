package shellbuildtokens

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func build(name string) string {
	return "import forge\n\n_w = forge.Workload {\n    name = \"" + name + "\"\n    build = forge.ShellBuild {\n        cmd = r\"\"\"docker build -t ${TAG} .\"\"\"\n    }\n}\n"
}

func TestScanKCLTree_MissingDirIsNotAnError(t *testing.T) {
	got, err := ScanKCLTree(filepath.Join(t.TempDir(), "no-deploy-kcl"))
	if err != nil {
		t.Fatalf("a project with no deploy/kcl must not error: %v", err)
	}
	if got != nil {
		t.Errorf("findings = %+v, want none", got)
	}
}

func TestScanKCLTree_EmptyDirHasNoFindings(t *testing.T) {
	got, err := ScanKCLTree(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("got (%+v, %v), want no findings and no error", got, err)
	}
}

func TestScanKCLTree_ReportsRelativeSortedFindings(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"prod/main.k":   build("zeta"),
		"dev/main.k":    build("alpha"),
		"README.md":     "${TAG} in a non-KCL file is ignored",
		"dev/notes.txt": "cmd = r\"\"\"${TAG}\"\"\"",
	})
	got, err := ScanKCLTree(root)
	if err != nil {
		t.Fatalf("ScanKCLTree: %v", err)
	}
	want := []FileFinding{
		{Finding: Finding{Workload: "alpha", Token: "TAG", Replacement: got[0].Replacement}, File: filepath.Join("dev", "main.k"), Line: 5},
		{Finding: Finding{Workload: "zeta", Token: "TAG", Replacement: got[1].Replacement}, File: filepath.Join("prod", "main.k"), Line: 5},
	}
	if len(got) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %+v\nwant     %+v", got, want)
	}
	if got[0].Replacement == "" {
		t.Error("a finding must carry the KCL replacement expression")
	}
}

func TestScanKCLTree_OrdersWithinAFileByLineThenToken(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.k": `import forge

_a = forge.Workload {
    name = "a"
    build = forge.ShellBuild {
        cmd = r"""echo ${TAG} ${IMAGE}"""
    }
}

_b = forge.Workload {
    name = "b"
    build = forge.ShellBuild {
        cmd = r"""echo ${REGISTRY}"""
    }
}
`,
	})
	got, err := ScanKCLTree(root)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range got {
		order = append(order, f.Workload+":"+f.Token)
	}
	want := []string{"a:IMAGE", "a:TAG", "b:REGISTRY"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v (by line, then token)", order, want)
	}
	if got[0].Line != 5 || got[2].Line != 12 {
		t.Errorf("lines = %d, %d; want 5 and 12", got[0].Line, got[2].Line)
	}
}

func TestScanKCLTree_SkipsDirectoriesHoldingNoProjectKCL(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"main.k": build("mine")}
	for _, skipped := range []string{".git", "node_modules", ".forge", "vendor", ".forge-pkg"} {
		files[skipped+"/dep/main.k"] = build("vendored-" + skipped)
	}
	writeTree(t, root, files)

	got, err := ScanKCLTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Workload != "mine" {
		t.Fatalf("findings = %+v, want only the project's own workload", got)
	}
}

func TestScanKCLTree_DirectoryNamedLikeAKclFileIsNotRead(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "weird.k"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{"weird.k/inner.k": build("inner")})
	got, err := ScanKCLTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != filepath.Join("weird.k", "inner.k") {
		t.Fatalf("findings = %+v, want exactly the inner file", got)
	}
}

func TestScanKCLTree_UnreadableFileIsReturnedAsError(t *testing.T) {
	if os.Geteuid() == 0 || filepath.Separator == '\\' {
		t.Skip("permission bits are not enforced for root or on windows")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{"main.k": build("x")})
	p := filepath.Join(root, "main.k")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })

	got, err := ScanKCLTree(root)
	if err == nil {
		t.Fatalf("want a read error, got findings %+v", got)
	}
	if got != nil {
		t.Errorf("findings must be dropped on error, got %+v", got)
	}
	if !os.IsPermission(err) {
		t.Errorf("error = %v, want a permission error", err)
	}
}
