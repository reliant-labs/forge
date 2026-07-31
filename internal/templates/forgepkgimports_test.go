package templates

import (
	"os"
	"path/filepath"
	"testing"
)

// TestForgePkgImportsAreRealPackages pins ForgePkgImports against the
// repo's own pkg/ tree: every name it reports must exist as a directory
// containing Go source. That makes the set self-maintaining — a regex
// drift or a renamed subpackage fails here rather than silently shrinking
// the set some other check depends on.
//
// The empty case is a hard failure on purpose. A consumer that loops over
// this set does nothing at all when it comes back empty, and a loop over
// nothing PASSES — which is precisely how requirePublishedForgePkg came to
// probe a package the templates never imported while reporting green.
func TestForgePkgImportsAreRealPackages(t *testing.T) {
	got := ForgePkgImports()
	if len(got) == 0 {
		t.Fatal("ForgePkgImports returned nothing; a consumer looping over this set would vacuously pass")
	}

	root, err := filepath.Abs(filepath.Join("..", "..", "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("pkg/ tree not present: %v", err)
	}

	for _, name := range got {
		dir := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("templates import forge/pkg/%s but %s is not a directory in this repo", name, dir)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("read %s: %v", dir, err)
			continue
		}
		hasGo := false
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".go" {
				hasGo = true
				break
			}
		}
		if !hasGo {
			t.Errorf("forge/pkg/%s has no Go source; templates import a package that cannot resolve", name)
		}
	}
}
