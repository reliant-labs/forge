package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// A fresh scaffold makes gen/ its own module (gen/go.mod + `use gen`). An
// in-place adoption of a module that already owns gen/ (GenInRootModule)
// must keep it in the root module: no gen/go.mod, and a go.work that uses
// only the root. reliant is the case — control-plane imports its
// gen/reliant/v1 through reliant's ROOT module, so a gen/go.mod would break
// that build and leave the root's own `go mod tidy` unable to resolve gen/.
func TestProjectGenerator_GenModuleLayout(t *testing.T) {
	for _, tc := range []struct {
		name            string
		genInRootModule bool
		wantGenGoMod    bool
		wantUses        []string
	}{
		{name: "fresh scaffold: gen is its own module", wantGenGoMod: true, wantUses: []string{".", "gen"}},
		{name: "root module owns gen", genInRootModule: true, wantGenGoMod: false, wantUses: []string{"."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "proj")
			g := NewProjectGenerator("proj", root, "example.com/proj")
			g.GenInRootModule = tc.genInRootModule
			if err := g.Generate(); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			_, err := os.Stat(filepath.Join(root, "gen", "go.mod"))
			if gotGenGoMod := err == nil; gotGenGoMod != tc.wantGenGoMod {
				t.Errorf("gen/go.mod exists = %v, want %v", gotGenGoMod, tc.wantGenGoMod)
			}

			workPath := filepath.Join(root, "go.work")
			data, err := os.ReadFile(workPath)
			if err != nil {
				t.Fatal(err)
			}
			wf, err := modfile.ParseWork(workPath, data, nil)
			if err != nil {
				t.Fatalf("go.work does not parse: %v\n%s", err, data)
			}
			var uses []string
			for _, u := range wf.Use {
				uses = append(uses, u.Path)
			}
			if strings.Join(uses, " ") != strings.Join(tc.wantUses, " ") {
				t.Errorf("go.work uses %q, want %q:\n%s", uses, tc.wantUses, data)
			}
		})
	}
}
