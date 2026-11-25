package cigate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// modulesFixture is a root module ex.com/m replacing ex.com/sdk => ./sdk/go:
// root package app imports ex.com/sdk/client; in the nested module,
// client has tests and util (imported by client) too.
func modulesFixture(t *testing.T) (string, func(context.Context, string, []string) (io.Reader, error)) {
	t.Helper()
	root := t.TempDir()
	for f, body := range map[string]string{
		"go.mod":        "module ex.com/m\n\ngo 1.22\n\nrequire ex.com/sdk v0.0.0\n\nreplace ex.com/sdk => ./sdk/go\n",
		"sdk/go/go.mod": "module ex.com/sdk\n\ngo 1.22\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sdk := filepath.Join(root, "sdk", "go")
	render := func(pkgs []listedPackage) io.Reader {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		for _, p := range pkgs {
			_ = enc.Encode(p)
		}
		return &b
	}
	list := func(_ context.Context, dir string, _ []string) (io.Reader, error) {
		if dir == sdk {
			return render([]listedPackage{
				{ImportPath: "ex.com/sdk/client", Name: "client", Dir: filepath.Join(sdk, "client"), Deps: []string{"ex.com/sdk/util"}},
				{ImportPath: "ex.com/sdk/util", Name: "util", Dir: filepath.Join(sdk, "util")},
				{ImportPath: "ex.com/sdk/client.test", Name: "main", Dir: filepath.Join(sdk, "client"), Deps: []string{"ex.com/sdk/client [ex.com/sdk/client.test]", "ex.com/sdk/util"}},
				{ImportPath: "ex.com/sdk/util.test", Name: "main", Dir: filepath.Join(sdk, "util"), Deps: []string{"ex.com/sdk/util [ex.com/sdk/util.test]"}},
			}), nil
		}
		return render([]listedPackage{
			{ImportPath: "ex.com/m/app", Name: "app", Dir: filepath.Join(root, "app"), Deps: []string{"ex.com/sdk/client", "ex.com/sdk/util"}},
			{ImportPath: "ex.com/m/other", Name: "other", Dir: filepath.Join(root, "other")},
			{ImportPath: "ex.com/m/app.test", Name: "main", Dir: filepath.Join(root, "app"), Deps: []string{"ex.com/m/app [ex.com/m/app.test]", "ex.com/sdk/client", "ex.com/sdk/util"}},
			{ImportPath: "ex.com/m/other.test", Name: "main", Dir: filepath.Join(root, "other"), Deps: []string{"ex.com/m/other [ex.com/m/other.test]"}},
		}), nil
	}
	return root, list
}

func TestComputeAffected_ReplacedModules(t *testing.T) {
	root, list := modulesFixture(t)
	run := func(mode string, changed ...string) Affected {
		return ComputeAffected(context.Background(), AffectedInput{Root: root, Modules: mode,
			Git: fakeGit{branch: "f", changed: changed}.run, GoList: list})
	}
	cases := []struct {
		name, mode string
		changed    []string
		full       bool
		pkgs       []string
		mod        *ModuleTests
	}{
		{name: "default is unchanged: full suite", changed: []string{"sdk/go/util/u.go"}, full: true},
		{name: "source maps into the root graph and the module", mode: "auto", changed: []string{"sdk/go/util/u.go"},
			pkgs: []string{"ex.com/m/app"}, mod: &ModuleTests{Dir: "sdk/go", Path: "ex.com/sdk", Packages: []string{"ex.com/sdk/client", "ex.com/sdk/util"}}},
		{name: "a module test file runs only in the module", mode: "auto", changed: []string{"sdk/go/client/c_test.go"},
			mod: &ModuleTests{Dir: "sdk/go", Path: "ex.com/sdk", Packages: []string{"ex.com/sdk/client"}}},
		{name: "module go.sum: module full, root narrowed", mode: "auto", changed: []string{"sdk/go/go.sum"},
			mod: &ModuleTests{Dir: "sdk/go", Path: "ex.com/sdk", Full: true, Reason: "sdk/go/go.sum changed"}},
		{name: "root go.sum too: root full", mode: "auto", changed: []string{"go.sum", "sdk/go/go.sum"}, full: true,
			mod: &ModuleTests{Dir: "sdk/go", Path: "ex.com/sdk", Full: true, Reason: "sdk/go/go.sum changed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := run(tc.mode, tc.changed...)
			if a.Full != tc.full {
				t.Fatalf("Full = %v (%s), want %v", a.Full, a.Reason, tc.full)
			}
			if !tc.full && !equal(a.Packages, tc.pkgs) {
				t.Errorf("Packages = %v, want %v", a.Packages, tc.pkgs)
			}
			switch {
			case tc.mod == nil && len(a.Modules) != 0:
				t.Errorf("Modules = %+v, want none", a.Modules)
			case tc.mod != nil && (len(a.Modules) != 1 || a.Modules[0].Dir != tc.mod.Dir || a.Modules[0].Path != tc.mod.Path ||
				a.Modules[0].Full != tc.mod.Full || a.Modules[0].Reason != tc.mod.Reason || !equal(a.Modules[0].Packages, tc.mod.Packages)):
				t.Errorf("Modules = %+v, want [%+v]", a.Modules, *tc.mod)
			}
		})
	}
}

func TestWithModuleTests(t *testing.T) {
	checks := []Check{{Name: UnitTests}, {Name: "lint"}}
	if got := WithModuleTests(checks, Affected{}); len(got) != 2 {
		t.Fatalf("no modules: %d rows, want 2", len(got))
	}
	got := WithModuleTests(checks, Affected{Modules: []ModuleTests{
		{Dir: "sdk/go", Path: "ex.com/sdk", Packages: []string{"ex.com/sdk/client"}},
		{Dir: "tools/x", Path: "ex.com/x", Full: true, Reason: "go.mod changed"},
	}})
	if len(got) != 4 || got[1].Name != "test-mod-sdk-go" || got[2].Name != "test-mod-tools-x" {
		t.Fatalf("rows = %+v", got)
	}
	if got[1].Dir != "sdk/go" || !equal(got[1].Argv, []string{"go", "test", "ex.com/sdk/client"}) {
		t.Errorf("affected row = %+v", got[1])
	}
	if !equal(got[2].Argv, []string{"go", "test", "./..."}) {
		t.Errorf("full row = %+v", got[2])
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
