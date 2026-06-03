// Public-identifier extraction for generated Go files.
//
// The rename-detection pass (see RenameWarnings in this package) needs
// to know which exported names a Tier-1 Go file declared in its most
// recent render, so it can diff against the current render and flag
// dropped names whose callers may not have been updated. This file
// owns the AST parse that produces that list.
//
// Non-Go files (.tsx, .yaml, .k, …) return an empty exports list —
// rename detection is currently scoped to Go callers because Go is the
// language where forge's codegen emits public package-level symbols
// that other (hand-written) packages depend on by name. Extending to
// TypeScript or KCL is straightforward but not needed by the current
// FRICTION class.
package checksums

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// ExtractGoExports returns the sorted list of public top-level
// identifier names declared in the Go source `content`. Public is
// determined by the Go convention (first rune is uppercase).
// Functions, types, vars, and consts are all included; receiver
// methods are NOT (a method rename rarely orphans an external caller
// because it goes through an interface or value receiver).
//
// Returns (nil, "") for non-Go content (parse error) — callers should
// treat that as "no exports recorded" rather than as an error.
//
// The returned package name is the `package <name>` clause from the
// file, used by RenameWarnings to construct `pkg.Name` search
// patterns when grepping for stale callers.
func ExtractGoExports(content []byte) (exports []string, pkgName string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, parser.SkipObjectResolution)
	if err != nil {
		return nil, ""
	}
	pkgName = f.Name.Name
	seen := map[string]bool{}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			// Skip methods (Recv != nil) — see file doc.
			if d.Recv != nil {
				continue
			}
			if isExported(d.Name.Name) {
				seen[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if isExported(s.Name.Name) {
						seen[s.Name.Name] = true
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if isExported(n.Name) {
							seen[n.Name] = true
						}
					}
				}
			}
		}
	}
	exports = make([]string, 0, len(seen))
	for n := range seen {
		exports = append(exports, n)
	}
	sort.Strings(exports)
	return exports, pkgName
}

// isExported reports whether the identifier begins with an uppercase
// rune (Go's convention for package-level visibility). Replicated
// here instead of importing go/token's IsExported to keep this file's
// dependency surface tight.
func isExported(name string) bool {
	if name == "" {
		return false
	}
	r := name[0]
	return r >= 'A' && r <= 'Z'
}

// IsGoPath reports whether relPath is a Go source file that the
// exports extractor should attempt to parse. Used so the
// rename-detection wiring can short-circuit on non-Go Tier-1 files.
func IsGoPath(relPath string) bool {
	return strings.HasSuffix(relPath, ".go")
}
