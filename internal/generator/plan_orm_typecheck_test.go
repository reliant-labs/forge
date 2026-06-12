package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/reliant-labs/forge/internal/config"
)

// TestGeneratePlanORM_OutputTypeChecks COMPILES the emitted ORM code —
// the gate the kalshi escapes proved was missing. fr-3fba9166ba shipped
// because every unit test asserted on substrings of the render while
// the render itself could never pass the type checker (`undefined:
// time`, IsZero on a string). The entity matrix here covers every
// stamping/PK branch the emitter has:
//
//   - string PK + tenant + soft-delete + declared NOT NULL time columns
//   - legacy TEXT created_at/updated_at (string stamps)
//   - nullable managed columns (pointer stamps), time and string
//   - server-allocated int64 and int32 PKs
//   - unstampable epoch-integer "timestamps" (no stamping emitted)
//   - id-only integer-PK table (DEFAULT VALUES, no strings import)
//
// The generated files are type-checked in-module via a go/packages
// overlay (they import forge/pkg/orm, otel, ulid — all dependencies of
// this repo), so this is a real compile, not a substring proxy.
// Full-mode only: loading the dependency graph takes seconds.
func TestGeneratePlanORM_OutputTypeChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checking generated output loads the full dependency graph (>2s)")
	}

	root := t.TempDir()
	entities := []config.PlanEntity{
		{
			Name: "Project", Timestamps: true, SoftDelete: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true, NotNull: true},
				{Name: "org_id", Type: "string", TenantKey: true, NotNull: true},
				{Name: "name", Type: "string", NotNull: true},
				{Name: "tags", Type: "[]string"},
				{Name: "created_at", Type: "time", NotNull: true},
				{Name: "updated_at", Type: "time", NotNull: true},
			},
		},
		{
			Name: "Trade", Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true, NotNull: true},
				{Name: "ticker", Type: "string", NotNull: true},
				{Name: "created_at", Type: "string", NotNull: true},
				{Name: "updated_at", Type: "string", NotNull: true},
			},
		},
		{
			Name: "Audit", Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true, NotNull: true},
				{Name: "created_at", Type: "time"},
				{Name: "updated_at", Type: "time"},
			},
		},
		{
			Name: "Legacy", Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true, NotNull: true},
				{Name: "created_at", Type: "string"},
				{Name: "updated_at", Type: "string"},
			},
		},
		{
			Name: "Hypothesis", Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "int64", PrimaryKey: true, NotNull: true},
				{Name: "title", Type: "string", NotNull: true},
				{Name: "created_at", Type: "time", NotNull: true},
				{Name: "updated_at", Type: "time", NotNull: true},
			},
		},
		{
			Name: "Tick",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "int32", PrimaryKey: true, NotNull: true},
				{Name: "label", Type: "string", NotNull: true},
			},
		},
		{
			Name: "Epoch", Timestamps: true,
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "string", PrimaryKey: true, NotNull: true},
				{Name: "created_at", Type: "int64", NotNull: true},
				{Name: "updated_at", Type: "int64", NotNull: true},
			},
		},
		{
			Name: "Only",
			Fields: []config.PlanEntityField{
				{Name: "id", Type: "int64", PrimaryKey: true, NotNull: true},
			},
		},
	}

	if err := GeneratePlanORM(root, "example.com/app", "api", entities); err != nil {
		t.Fatalf("GeneratePlanORM: %v", err)
	}

	dbDir := filepath.Join(root, "internal", "db")
	files, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	// Overlay the generated files into a synthetic package inside THIS
	// module so all their imports resolve through forge's own go.mod.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	synthDir := filepath.Join(repoRoot, "internal", "generator", "zz_orm_typecheck", "db")
	overlay := map[string][]byte{}
	var goFiles []string
	for _, f := range files {
		content, rerr := os.ReadFile(filepath.Join(dbDir, f.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", f.Name(), rerr)
		}
		// github.com/oklog/ulid/v2 is a dependency of generated PROJECTS,
		// not of the forge module itself, so the overlay can't resolve
		// it. Substitute the one expression that uses it; everything
		// else type-checks for real. (The ULID chokepoint's emission is
		// pinned by TestGeneratePlanORM_Basic.)
		src := strings.ReplaceAll(string(content), "\t\"github.com/oklog/ulid/v2\"\n", "")
		src = strings.ReplaceAll(src, "ulid.Make().String()", `"ulid-stub"`)
		path := filepath.Join(synthDir, f.Name())
		overlay[path] = []byte(src)
		goFiles = append(goFiles, path)
	}

	cfg := &packages.Config{
		Mode:    packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
		Dir:     repoRoot,
		Overlay: overlay,
	}
	pkgs, err := packages.Load(cfg, "file="+goFiles[0])
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("packages.Load returned no packages")
	}
	failed := false
	for _, p := range pkgs {
		for _, e := range p.Errors {
			failed = true
			t.Errorf("generated ORM code does not compile: %v", e)
		}
	}
	if failed {
		for _, f := range files {
			content, _ := os.ReadFile(filepath.Join(dbDir, f.Name()))
			t.Logf("==== %s ====\n%s", f.Name(), content)
		}
	}
}
