package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// factoryDeclarations includes in-package tests: factories compile into the
// augmented handler package, not a separate namespace. Do not read our previous
// output as ownership, or a second generate would erase its own factories.
func factoryDeclarations(dir, pkg string) (map[string]string, map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	declared, funcs := map[string]string{}, map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == entityFactoryFile || !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("read factory declarations in %s: %w", filepath.Join(dir, name), err)
		}
		if file.Name.Name != pkg {
			continue
		} // external tests have their own namespace
		for _, decl := range file.Decls {
			for _, ident := range factoryDeclNames(decl) {
				declared[ident] = name
			}
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = true
			}
		}
	}
	return declared, funcs, nil
}

// factoryDeclNames covers the entire package namespace, not just functions:
// an override type or a SQL constant can collide with user code too.
func factoryDeclNames(decl ast.Decl) []string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			return []string{d.Name.Name}
		}
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				for _, name := range s.Names {
					if name.Name != "_" {
						names = append(names, name.Name)
					}
				}
			}
		}
		return names
	}
	return nil
}

// factoryName keeps the historical API when free. Its fallback begins ForgeNew,
// never New, so it cannot steal another generated factory's default name. The
// selection depends only on owned declarations, not previous generated output
// or emission order, and is shared by factory emitters and scaffolded callers.
func factoryName(base string, declared map[string]string) string {
	if declared[base] == "" {
		return base
	}
	candidate := "Forge" + base
	for n := 2; declared[candidate] != ""; n++ {
		candidate = fmt.Sprintf("Forge%s%d", base, n)
	}
	return candidate
}

func prepareFactoryGroup(dir string, g factoryGroup) (factoryGroup, map[string]string, error) {
	declared, _, err := factoryDeclarations(dir, g.pkg)
	if err != nil {
		return g, nil, err
	}
	// Clone before choosing names: other emitters may reuse these specs.
	g.specs = append([]entityFactorySpec(nil), g.specs...)
	for i := range g.specs {
		g.specs[i].funcName = factoryName("New"+g.specs[i].goName, declared)
	}
	g.creates = append([]createRequestSpec(nil), g.creates...)
	for i := range g.creates {
		g.creates[i].funcName = factoryName(g.creates[i].funcName, declared)
	}
	return g, declared, nil
}

// Check the declarations actually rendered, so helper dependency changes cannot
// leave a hand-maintained list of reserved names out of date.
func checkFactoryDeclarations(content []byte, declared map[string]string) error {
	f, err := parser.ParseFile(token.NewFileSet(), entityFactoryFile, content, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse rendered factories: %w", err)
	}
	emitted := map[string]bool{}
	for _, decl := range f.Decls {
		for _, name := range factoryDeclNames(decl) {
			if file, exists := declared[name]; exists {
				return fmt.Errorf("%s would redeclare %s from %s; rename the owned declaration or provide the factory in owned code", entityFactoryFile, name, file)
			}
			if emitted[name] {
				return fmt.Errorf("%s would declare %s more than once", entityFactoryFile, name)
			}
			emitted[name] = true
		}
	}
	return nil
}
