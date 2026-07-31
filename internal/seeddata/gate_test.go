package seeddata

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The applier lives in internal/seeddata (forge's root module). Generated
// applications import only github.com/reliant-labs/forge/pkg/... — a separate
// module — so they CANNOT reach this package: Go's module boundary plus
// internal-visibility make seeding capability structurally absent from every
// shipped server binary. This test proves the shipped runtime module has ZERO
// transitive dependency on the applier. The gate is structural, not
// conventional.
func TestApplierNotReachableFromShippedModule(t *testing.T) {
	if testing.Short() {
		t.Skip("shells `go list`; skipped under -short")
	}
	pkgDir, err := filepath.Abs(filepath.Join("..", "..", "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pkgDir, "go.mod")); err != nil {
		t.Skipf("pkg module not found at %s: %v", pkgDir, err)
	}
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = pkgDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps in pkg module: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "reliant-labs/forge/internal/seeddata") {
		t.Fatal("the shipped pkg module transitively imports internal/seeddata — " +
			"the seed applier must never be reachable from a production/app binary")
	}
}

// The generated app's migrate path (pkg/app/migrate.go, from this template)
// must gain NO seed code path: golang-migrate is the only thing that runs
// against non-dev databases, and it must not be able to apply seeds.
func TestGeneratedMigrateTemplateHasNoSeedPath(t *testing.T) {
	tmpl := filepath.Join("..", "templates", "project", "migrate.go.tmpl")
	data, err := os.ReadFile(tmpl)
	if err != nil {
		t.Skipf("migrate template not found: %v", err)
	}
	if strings.Contains(strings.ToLower(string(data)), "seed") {
		t.Fatal("the generated migrate.go template must contain no seed code path")
	}
}
