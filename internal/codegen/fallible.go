package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// DetectDepsDBField checks whether the Deps struct in the given directory
// has a field of type orm.Context (indicating the service needs a database).
// It parses all non-test .go files and looks for a type Deps struct with a
// field whose type is orm.Context.
// Returns true if the Deps struct has a DB field, false otherwise.
func DetectDepsDBField(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}

		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != "Deps" {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structType.Fields.List {
					// Check for orm.Context type (selector expression)
					if sel, ok := field.Type.(*ast.SelectorExpr); ok {
						if ident, ok := sel.X.(*ast.Ident); ok {
							if ident.Name == "orm" && sel.Sel.Name == "Context" {
								return true, nil
							}
						}
					}
				}
			}
		}
	}

	return false, nil
}

// DetectFallibleConstructor checks whether the exported New function in the
// given directory returns an error as its last result (i.e. returns (T, error)).
// It parses all non-test .go files and looks for a top-level func New(...).
// Returns true if the constructor is fallible, false otherwise.
// If the directory doesn't exist or contains no New function, returns false.
func DetectFallibleConstructor(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue // skip unparseable files
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue // skip methods
			}
			if fn.Name == nil || fn.Name.Name != "New" {
				continue
			}

			// Found func New(...) — check return types
			results := fn.Type.Results
			if results == nil || len(results.List) < 2 {
				return false, nil // single return or no return
			}

			// Check if last return type is "error"
			lastField := results.List[len(results.List)-1]
			if ident, ok := lastField.Type.(*ast.Ident); ok && ident.Name == "error" {
				return true, nil
			}
			return false, nil
		}
	}

	return false, nil
}