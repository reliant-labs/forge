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
	"github.com/reliant-labs/forge/internal/templates"
)

// GenerateServiceStub generates service.go and handlers.go for a new service
// using the embedded FS templates. crudMethodNames lists methods that CRUD gen
// will implement; these are excluded from the initial handlers.go stubs.
func GenerateServiceStub(svc ServiceDef, targetDir string, crudMethodNames ...map[string]bool) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	// Derive projectDir from targetDir's <projectDir>/handlers/<svc> shape so
	// the test-helper-name collision check can probe internal/<pkg>. Day-0,
	// no caller passes a non-conventional targetDir.
	projectDir := filepath.Dir(filepath.Dir(targetDir))
	data := mapServiceDefToTemplateData(svc, projectDir)

	// Render service.go from embedded template
	serviceContent, err := templates.ServiceTemplates().Render("service.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render service.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "service.go"), serviceContent, 0644); err != nil {
		return err
	}

	// For handlers.go, filter out methods that CRUD gen will implement.
	var crudNames map[string]bool
	if len(crudMethodNames) > 0 {
		crudNames = crudMethodNames[0]
	}
	handlersData := data
	if len(crudNames) > 0 {
		var nonCRUD []MethodTemplateData
		for _, m := range data.Methods {
			if !crudNames[m.Name] {
				nonCRUD = append(nonCRUD, m)
			}
		}
		handlersData.Methods = nonCRUD
	}

	// Render handlers.go from embedded template only when there are real methods
	// to implement. With zero methods, handlers.go would just be a placeholder
	// comment; skip it and let the user (or subsequent forge generate runs) create
	// it with actual content.
	if len(handlersData.Methods) > 0 {
		handlersContent, err := templates.ServiceTemplates().Render("handlers.go.tmpl", handlersData)
		if err != nil {
			return fmt.Errorf("render handlers.go.tmpl: %w", err)
		}
		if err := os.WriteFile(filepath.Join(targetDir, "handlers.go"), handlersContent, 0644); err != nil {
			return err
		}
	}

	// Render handlers_scaffold_test.go from embedded template (same filter as handlers.go — skip CRUD methods).
	// The qualified filename frees the canonical handlers_test.go slot for user-owned tests; forge never
	// touches handlers_test.go.
	unitTestContent, err := templates.ServiceTemplates().Render("unit_test.go.tmpl", handlersData)
	if err != nil {
		return fmt.Errorf("render unit_test.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "handlers_scaffold_test.go"), unitTestContent, 0644); err != nil {
		return err
	}

	// NOTE: no one-shot integration_test.go scaffold is emitted — see
	// GenerateMissingHandlerStubs for the rationale (one test philosophy
	// per service).

	// Render authorizer.go from embedded template
	authzData := struct {
		Package     string
		ServiceName string
		Module      string
	}{
		Package:     data.ServiceName,
		ServiceName: data.HandlerName,
		Module:      data.Module,
	}
	authzContent, err := templates.ServiceTemplates().Render("authorizer.go.tmpl", authzData)
	if err != nil {
		return fmt.Errorf("render authorizer.go.tmpl: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "authorizer.go"), authzContent, 0644); err != nil {
		return err
	}

	return nil
}

// RegenerateServiceFile regenerates only service.go for an existing service
// directory, using the proto-derived HandlerName so that Connect RPC references
// (Unimplemented*Handler, New*Handler) match the actual proto service name.
func RegenerateServiceFile(svc ServiceDef, targetDir string) error {
	data := mapServiceDefToTemplateData(svc)

	serviceContent, err := templates.ServiceTemplates().Render("service.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render service.go.tmpl: %w", err)
	}
	return os.WriteFile(filepath.Join(targetDir, "service.go"), serviceContent, 0644)
}

// MissingHandlerResult holds the result of scanning for missing handler stubs.
type MissingHandlerResult struct {
	NewMethods  []string // names of methods that were generated
	AllUpToDate bool     // true if no new methods were needed
}

// GenerateMissingHandlerStubs scans the existing service directory for implemented
// methods on *Service, compares against the proto ServiceDef, and generates stubs
// only for missing methods into handlers_gen.go.
// If all methods are already implemented, it returns AllUpToDate=true.
// If handlers_gen.go already exists, it is overwritten (it's generated code).
// crudMethodNames optionally lists method names that CRUD gen will implement;
// stubs are skipped for these even if they don't exist yet in the package.
//
// cs is the project's checksum tracker. Passing it ensures the generated
// handlers_gen.go is recorded so it doesn't show up as an orphan in `forge
// audit`. The placeholder-replacement of integration_test.go /
// handlers_scaffold_test.go does not record a checksum: those files become
// user-owned after the placeholder is filled in. The canonical
// handlers_test.go filename is reserved for the user. A nil cs is tolerated.
func GenerateMissingHandlerStubs(svc ServiceDef, projectDir, targetDir string, crudMethodNames map[string]bool, cs *checksums.FileChecksums) (*MissingHandlerResult, error) {
	existing, err := scanExistingMethods(targetDir, false)
	if err != nil {
		return nil, fmt.Errorf("scan existing methods: %w", err)
	}

	// handlers_crud.go is skipped by scanExistingMethods so its delegating
	// CRUD shims don't masquerade as "user implemented this RPC by hand" and
	// suppress regeneration of the very ops they delegate to. But the file is
	// also the user's own (the scaffold header says so), and a user can hand-
	// implement a non-CRUD RPC there (kalshi fr-fba0c4be8d: a custom-shape
	// ListSettlements with no entity behind it). That hand impl IS a real
	// implementation and MUST suppress the stub, or handlers_gen.go re-emits a
	// duplicate method and the package fails to compile. Discriminate by name:
	// a method in handlers_crud.go whose name is NOT a CRUD method is a hand
	// impl (the CRUD-shaped delegating shims are exactly crudMethodNames).
	for name := range scanHandlersCrudMethods(targetDir) {
		if !crudMethodNames[name] {
			existing[name] = true
		}
	}

	var missing []Method
	for _, m := range svc.Methods {
		if !existing[m.Name] && !crudMethodNames[m.Name] {
			missing = append(missing, m)
		}
	}

	handlersGenPath := filepath.Join(targetDir, "handlers_gen.go")
	if len(missing) == 0 {
		if err := os.Remove(handlersGenPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale handlers_gen.go: %w", err)
		}
		return &MissingHandlerResult{AllUpToDate: true}, nil
	}

	// Build a ServiceDef with only the missing methods for template rendering
	missingSvc := svc
	missingSvc.Methods = missing
	data := mapServiceDefToTemplateData(missingSvc, projectDir)

	// Disk-first: handlers_gen.go lands inside the EXISTING targetDir and
	// MUST declare the same package as the files already there — the
	// synthesized clause from mapServiceDefToTemplateData only holds for
	// fresh scaffolds. Parsing the live clause here keeps a snake_case
	// handler dir (or one whose clause differs from its dir name) from
	// getting a conflicting `package x` stamped into it on regenerate.
	// The import-path leaf for the *_test scaffolds likewise comes from
	// the real directory name.
	diskPkg, perr := ParsePackageClause(targetDir)
	if perr != nil {
		return nil, fmt.Errorf("generating handlers_gen.go: %w", perr)
	}
	applyDiskIdentity := func(d *ServiceTemplateData) {
		d.ServicePackage = diskPkg
		d.ServiceImportPath = filepath.Base(targetDir)
		d.TestHelperName = ComputeTestHelperName(diskPkg, projectDir)
	}
	applyDiskIdentity(&data)

	content, err := templates.ServiceTemplates().Render("handlers_gen.go.tmpl", data)
	if err != nil {
		return nil, fmt.Errorf("render handlers_gen.go.tmpl: %w", err)
	}

	relHandlersGen, err := filepath.Rel(projectDir, handlersGenPath)
	if err != nil {
		return nil, fmt.Errorf("compute relative path for handlers_gen.go: %w", err)
	}
	if _, err := checksums.WriteGeneratedFile(projectDir, relHandlersGen, content, cs, true); err != nil {
		return nil, err
	}

	// If integration_test.go / handlers_scaffold_test.go are still placeholders (no RPCs when
	// first generated), regenerate them with actual test scaffolding now that RPCs exist.
	// These files become user-owned after the placeholder is filled in, so we
	// don't checksum them — we want forge audit to leave them alone.
	fullData := mapServiceDefToTemplateData(svc, projectDir)
	applyDiskIdentity(&fullData)

	// Filter CRUD methods out of the unit-test scaffold so per-RPC rows
	// don't overlap with handlers_crud_test.go (the user-owned lifecycle
	// test that owns CRUD coverage). Same filter rule as the initial-gen
	// path in GenerateServiceStub — one source of truth per method, no
	// duplication.
	unitTestData := fullData
	if len(crudMethodNames) > 0 {
		var nonCRUD []MethodTemplateData
		for _, m := range fullData.Methods {
			if !crudMethodNames[m.Name] {
				nonCRUD = append(nonCRUD, m)
			}
		}
		unitTestData.Methods = nonCRUD
	}

	// NOTE: forge no longer emits a one-shot integration_test.go scaffold.
	// One test philosophy per service: the unit scaffold
	// (handlers_scaffold_test.go) owns per-RPC self-destructing rows, and
	// handlers_crud_integration_test.go owns the DB-bound CRUD surface.
	// Existing user-owned integration_test.go files are left untouched.

	handlersTestPath := filepath.Join(targetDir, "handlers_scaffold_test.go")
	if isPlaceholderUnitTest(handlersTestPath) {
		testContent, err := templates.ServiceTemplates().Render("unit_test.go.tmpl", unitTestData)
		if err != nil {
			return nil, fmt.Errorf("render unit_test.go.tmpl: %w", err)
		}
		if err := os.WriteFile(handlersTestPath, testContent, 0644); err != nil {
			return nil, fmt.Errorf("write handlers_scaffold_test.go: %w", err)
		}
	}

	var names []string
	for _, m := range missing {
		names = append(names, m.Name)
	}

	return &MissingHandlerResult{NewMethods: names}, nil
}

// isPlaceholderUnitTest checks if handlers_scaffold_test.go is still the auto-generated
// placeholder with no real tests.
func isPlaceholderUnitTest(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), `forge-unit-test-placeholder`)
}

// scanExistingMethods reads all .go files in dir and returns a set of
// method names that are already implemented on *Service. It uses
// go/parser so that multi-line receivers, comments, and strings
// containing "*Service" are handled correctly.
//
// This is the dedup that lets a user's `handlers.go` claim a method
// (e.g. `func (s *Service) CreateUser(...) ...`) and have the next
// `forge generate` automatically drop the matching stub from
// `handlers_gen.go`. Same shape closes the FORGE_REVIEW_PROCESS.md §2.3
// git_credential drift class — gen-files and user-files share the
// `*Service` receiver, so a method declared in either is sufficient
// signal that the proto RPC is implemented.
//
// An individual file that fails to parse is skipped with a warning
// rather than failing the whole pass: a transient syntax error in a
// sibling file must not brick the dedup for the entire package, since
// losing dedup means the user's just-written `CreateUser` would be
// re-stubbed in handlers_gen.go and the package would fail to compile
// (duplicate method).
func scanExistingMethods(dir string, includeGeneratedStubs bool) (map[string]bool, error) {
	existing := make(map[string]bool)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		// Skip test files
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if !includeGeneratedStubs && (entry.Name() == "handlers_gen.go" || entry.Name() == "handlers_crud_gen.go" ||
			entry.Name() == "handlers_crud.go" || entry.Name() == "handlers_crud_ops_gen.go") {
			// handlers_crud.go holds the forge-scaffolded thin CRUD shims:
			// its methods delegate to generated ops, so they must not count
			// as "user implemented this RPC by hand" (that would suppress
			// regeneration of the very ops they delegate to).
			continue
		}

		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			// Intentional soft warning (no --strict promotion): per-file
			// parse errors mustn't unwind the dedup — a transient
			// syntax error elsewhere in the package would otherwise
			// strand the user with no scaffold regen. See func doc for
			// the full rationale. Lives in internal/codegen so no
			// pipelineContext reach.
			fmt.Fprintf(os.Stderr, "Warning: scanExistingMethods skipping %s (parse error): %v\n", path, err)
			continue
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			// Receiver must be a pointer: *Service
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			ident, ok := star.X.(*ast.Ident)
			if !ok || ident.Name != "Service" {
				continue
			}
			if fn.Name != nil && fn.Name.Name != "" {
				existing[fn.Name.Name] = true
			}
		}
	}

	return existing, nil
}

// scanHandlersCrudMethods returns the set of *Service method names declared in
// handlers_crud.go specifically. scanExistingMethods skips that file wholesale
// (its delegating shims must not suppress ops regen); this lets the stub
// generator look inside it to find HAND-WRITTEN (non-CRUD) impls that DO need
// to suppress a duplicate stub. Returns an empty set if the file is absent or
// unparseable — losing this signal only risks a duplicate-method compile error
// surfacing at the validate step, never a silent wrong result.
func scanHandlersCrudMethods(dir string) map[string]bool {
	out := map[string]bool{}
	path := filepath.Join(dir, "handlers_crud.go")
	if _, err := os.Stat(path); err != nil {
		return out
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: scanHandlersCrudMethods skipping %s (parse error): %v\n", path, err)
		return out
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "Service" {
			continue
		}
		if fn.Name != nil && fn.Name.Name != "" {
			out[fn.Name.Name] = true
		}
	}
	return out
}
