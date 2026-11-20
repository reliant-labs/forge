package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// lifecycleTestData builds the lifecycle test template's data with the
// disk-resolved facts GenerateCRUDTests and generateEntityLifecycleTests
// share. Package + TestHelperName are overridden with the disk-resolved
// package clause so the emitted test file always matches the directory it
// lands in AND calls the `NewTest<X>` factory the bootstrap testing
// generator (which uses the same resolver) actually emitted.
func lifecycleTestData(svc ServiceDef, methods []CRUDMethod, modulePath, projectDir, pkg, targetDir, relDir string, fix *crudTestFixtures) CRUDTestTemplateData {
	data := buildCRUDTestTemplateData(svc, methods, modulePath, projectDir, fix)
	data.Package = pkg
	data.TestHelperName = ComputeTestHelperName(pkg, projectDir)
	data.UnfilledDeps = UnfilledDepsFor(targetDir)
	// relDir is internal/handlers/<leaf>; the import needs the leaf.
	data.ServiceImportPath = filepath.ToSlash(filepath.Base(relDir))
	data.ConnectV2 = connectV2For(svc, projectDir)
	// The same gate the test harness uses to declare NewMigratedTestDB.
	data.HasMigrationsFS = ProjectHasSQLMigrations(projectDir)
	return data
}

// lifecycleNeedsFieldMask mirrors the template's gates for the AIP-134
// masked-update block, the one user of the fieldmaskpb import.
func lifecycleNeedsFieldMask(e CRUDTestEntityData) bool {
	return e.HasCreate && e.HasGet && e.PkGoType == "string" &&
		e.HasUpdate && e.MutableStringField != "" &&
		e.UpdateMethod.UpdateMaskField != ""
}

// entityLifecycleTestRel is the per-entity lifecycle test file of an entity
// a service gained after its handlers_crud_test.go was scaffolded.
func entityLifecycleTestRel(relDir, entity string) string {
	return filepath.Join(relDir, "handlers_crud_"+naming.ToSnakeCase(entity)+"_test.go")
}

// generateEntityLifecycleTests scaffolds handlers_crud_<entity>_test.go for
// each qualifying entity (Create + Get over a string primary key) that no
// test file of the package covers yet — no TestCRUD_<Entity>_Lifecycle —
// and whose file the birth ledger allows. handlers_crud_test.go is
// scaffold-once, so without this an entity added to a service later got its
// regenerated factories and no test that ever called them.
//
// Each file is scaffold-once on its own: deleting it is final, like
// deleting handlers_crud_test.go. It declares only the helpers (see
// CRUDTestTemplateData.OmitAssert) no other file of the external test
// package declares, so it compiles beside the user's own files.
func generateEntityLifecycleTests(svc ServiceDef, methods []CRUDMethod, hasCreate, hasGet map[string]bool, modulePath, projectDir, pkg, targetDir, relDir string, cs *checksums.FileChecksums) (bool, error) {
	// The common regenerate path stops here: every entity is covered.
	var pending []string
	seen := map[string]bool{}
	var decls testFuncDecls
	scanned := false
	for _, cm := range methods {
		name := cm.Entity.Name
		if seen[name] || !hasCreate[name] || !hasGet[name] || cm.Entity.PkGoType != "string" {
			continue
		}
		seen[name] = true
		if !scanned {
			var err error
			if decls, err = scanTestFuncDecls(targetDir, pkg+"_test"); err != nil {
				return false, err
			}
			scanned = true
		}
		if decls.any["TestCRUD_"+name+"_Lifecycle"] {
			continue
		}
		if !checksums.ScaffoldOnceDecision(projectDir, entityLifecycleTestRel(relDir, name)) {
			continue
		}
		pending = append(pending, name)
	}
	if len(pending) == 0 {
		return false, nil
	}

	fix := buildCRUDTestFixtures(projectDir, nil)
	defer fix.close()
	all := lifecycleTestData(svc, methods, modulePath, projectDir, pkg, targetDir, relDir, fix)
	byName := map[string]CRUDTestEntityData{}
	for _, e := range all.Entities {
		byName[e.EntityName] = e
	}

	wrote := false
	for _, name := range pending {
		ent, ok := byName[name]
		if !ok {
			continue
		}
		data := all
		data.Entities = []CRUDTestEntityData{ent}
		data.CRUDMethods = nil
		data.NeedsFieldMask = lifecycleNeedsFieldMask(ent)
		data.PerEntity = true
		data.OmitAssert = decls.external["assertCleanError"]
		data.OmitDBHelpers = decls.external["crudTestDB"]

		content, err := templates.ServiceTemplates().Render("handlers_crud_test.go.tmpl", data)
		if err != nil {
			return wrote, fmt.Errorf("render lifecycle test for %s: %w", name, err)
		}
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return wrote, err
		}
		rel := entityLifecycleTestRel(relDir, name)
		if err := writeUserScaffold(filepath.Join(projectDir, rel), content); err != nil {
			return wrote, fmt.Errorf("write %s: %w", rel, err)
		}
		checksums.RecordScaffold(projectDir, rel)
		recordTier2(cs, rel, content)
		wrote = true
		// The next file of this run sees the helpers this one declared.
		decls.external["assertCleanError"] = true
		if !data.HasMigrationsFS {
			decls.external["crudTestDB"] = true
		}
	}
	return wrote, nil
}

// testFuncDecls is the top-level funcs a package directory's _test.go files
// declare: any — in either test package, for "is this test written" — and
// external — in the external test package alone, the namespace a scaffolded
// lifecycle test file's helpers share.
type testFuncDecls struct {
	any      map[string]bool
	external map[string]bool
}

// scanTestFuncDecls parses dir's _test.go files (declarations only matter,
// so a file that does not parse is skipped: the compiler reports it).
func scanTestFuncDecls(dir, externalPkg string) (testFuncDecls, error) {
	d := testFuncDecls{any: map[string]bool{}, external: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return d, fmt.Errorf("scan tests in %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		external := f.Name.Name == externalPkg
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			d.any[fn.Name.Name] = true
			if external {
				d.external[fn.Name.Name] = true
			}
		}
	}
	return d, nil
}
