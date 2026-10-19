// File: internal/codegen/composition_graph.go
//
// The composition graph (docs/design/composition-graph.md §3): every part of
// a process is a component built from its Deps, and the graph that wires them
// is typed, generated Go. This file is the graph's MODEL and DISCOVERY:
//
//   - nodes: every component forge already discovers (handlers, packages,
//     workers, operators), every PROVIDER func in internal/app's owned files
//     (`func New<X>(<X>Deps) (T, error)` produces T), and the nodes forge
//     generates itself — one route set per audience (<Audience>Routes) and,
//     when the project has operators, the controller manager (Operators);
//   - roots: structs in internal/app marked `//forge:root <name>`. Each field
//     is a node, resolved by type; a root's process builds exactly the
//     closure its fields reach. A project that declares none gets the
//     generated default root, Server: every runnable node nothing else
//     depends on — today's `server` command.
//
// Resolution (composition_graph_resolve.go) and emission
// (composition_graph_emit.go) build on what is discovered here.
//
// A project is in GRAPH mode unless its internal/app still holds the legacy
// composition — the owned `type Infra struct` / `func NewComponents` that
// providers.go and compose.go were before the graph. Those projects keep the
// legacy generators byte-for-byte until they migrate (PR 7's codemod; the
// manual path is in the `architecture` skill).

package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/naming"
)

// GraphComposition reports whether the project composes with the composition
// graph: true for a new project and for one whose internal/app has no legacy
// composition (`type Infra struct` or `func NewComponents`) in an owned file.
func GraphComposition(projectDir string) bool {
	appDir := filepath.Join(projectDir, "internal", "app")
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(appDir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "NewComponents" {
					return false
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == "Infra" {
						if _, isStruct := ts.Type.(*ast.StructType); isStruct {
							return false
						}
					}
				}
			}
		}
	}
	return true
}

// nodeKind is where a graph node comes from.
type nodeKind int

const (
	nodeComponent nodeKind = iota // a handler / package / worker / operator package
	nodeProvider                  // a New<X>(<X>Deps) func in an owned internal/app file
	nodeRoutes                    // a generated <Audience>Routes route set
	nodeOperators                 // the generated controller manager
)

// lifecycle is the lifecycle methods a node's value has, read off its type by
// go/types: the method name to bind for each of lifecyclekit's four phases,
// "" for none.
type lifecycle struct {
	Run, Drain, Close, Ready string
}

func (l lifecycle) any() bool { return l.Run != "" || l.Drain != "" || l.Close != "" || l.Ready != "" }

// graphNode is one node of the composition graph.
type graphNode struct {
	// Name is the node's accessor on *Graph (g.<Name>()) and the suffix of
	// its compose.go constructor (newName). PascalCase, unique.
	Name string
	// LogName is the node's label in build errors and supervisor logs.
	LogName string
	Kind    nodeKind
	// Doc describes the node in one phrase, for generated comments.
	Doc string

	// Type is the Go type of the node's value as written in package app
	// ("*alpha.Service", "*APIListener", "*orm.Client").
	Type string
	// Imports are the alias → import path pairs Type and the constructor
	// call need in package app.
	Imports map[string]string

	// Ctor is the constructor call's function ("alpha.New", "NewDB"), and
	// DepsType its Deps literal type ("alpha.Deps", "DBDeps").
	Ctor     string
	DepsType string
	// DepsImport is the import path the Deps literal's selector names ("" for
	// a Deps type declared in package app).
	DepsImport string
	Fallible   bool
	// Wrapped components are constructed through their generated observe
	// middleware: `pkg.<MiddlewareCtor>(pkg.New(...))`.
	Wrapped        bool
	MiddlewareCtor string

	Deps      []DepsField
	DepsKeys  map[string]bool
	DepsFound bool
	// SrcImports are the imports the Deps struct's field types are written
	// against, for a node declared in package app (a provider's file, or a
	// generated route set's).
	SrcImports map[string]string

	Lifecycle lifecycle
	// NilGuard is true when the value can be nil (a pointer or an
	// interface), so the generated lifecycle literal checks it first.
	NilGuard bool

	// Comp is the discovered component behind a nodeComponent.
	Comp *BuildComponent
	// Role is a component's role root ("internal/workers", …); "" otherwise.
	Role string
	// Audience is a route set's audience.
	Audience string
	// Services are a route set's services, in Deps order.
	Services []*graphNode
	// Pos orders providers by their declaration in internal/app.
	Pos int
}

// graphRoot is one root: a struct in internal/app whose fields select the
// subgraph a process builds, or the generated default.
type graphRoot struct {
	// Name is the root's name — the `//forge:root <name>` argument, which
	// PR 4's commands are named after.
	Name string
	// TypeName is the root struct's Go name: Build<TypeName> / Run<TypeName>.
	TypeName string
	// Generated is the default root forge emits when none is declared.
	Generated bool
	Fields    []graphRootField
	// File/Line locate a declared root, for errors.
	File string
	Line int
}

// graphRootField is one field of a root, resolved to the node it holds.
type graphRootField struct {
	Name string
	Type string // as written
	Node *graphNode
}

// graphProvider is one provider func discovered in internal/app.
type graphProvider struct {
	Name     string // X in New<X>
	Result   string // T as written
	Fallible bool
	Deps     []DepsField
	DepsKeys map[string]bool
	Imports  map[string]string // the declaring file's imports, alias → path
	File     string
	Pos      int
}

// rootDirectiveRE matches `//forge:root` with an optional name and options.
var rootDirectiveRE = regexp.MustCompile(`^//\s*forge:root(?:\s+([A-Za-z][\w-]*))?`)

// isGraphGeneratedFile reports whether name is one of forge's generated
// internal/app files, which discovery never reads as owned declarations.
func isGraphGeneratedFile(name string) bool {
	return strings.HasSuffix(name, "_gen.go") || name == "compose.go"
}

// discoverAppDecls parses the owned internal/app files (everything except
// compose.go, *_gen.go and tests) for provider funcs and root structs.
func discoverAppDecls(projectDir string) (providers []graphProvider, roots []graphRoot, err error) {
	appDir := filepath.Join(projectDir, "internal", "app")
	entries, rerr := os.ReadDir(appDir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, nil, nil
		}
		return nil, nil, rerr
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || isGraphGeneratedFile(n) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	pos := 0
	for _, n := range names {
		path := filepath.Join(appDir, n)
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			return nil, nil, fmt.Errorf("parse internal/app/%s: %w", n, perr)
		}
		imports := fileImportAliases(file)
		structs := map[string]*ast.StructType{}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				st, isStruct := ts.Type.(*ast.StructType)
				if !isStruct {
					continue
				}
				structs[ts.Name.Name] = st
				doc := ts.Doc
				if doc == nil && len(gd.Specs) == 1 {
					doc = gd.Doc
				}
				if rootName, isRoot := rootDirective(doc); isRoot {
					if rootName == "" {
						rootName = naming.ToKebabCase(ts.Name.Name)
					}
					r := graphRoot{Name: rootName, TypeName: ts.Name.Name, File: "internal/app/" + n, Line: fset.Position(ts.Pos()).Line}
					for _, f := range st.Fields.List {
						typ := printType(fset, f.Type)
						if len(f.Names) == 0 {
							return nil, nil, fmt.Errorf("%s:%d: root %s embeds %s; give each root field a name — the field order is the stop order", r.File, fset.Position(f.Pos()).Line, ts.Name.Name, typ)
						}
						for _, fn := range f.Names {
							r.Fields = append(r.Fields, graphRootField{Name: fn.Name, Type: typ})
						}
					}
					roots = append(roots, r)
				}
			}
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "New") || len(fd.Name.Name) == 3 {
				continue
			}
			x := strings.TrimPrefix(fd.Name.Name, "New")
			if !ast.IsExported(x) {
				continue
			}
			params := flattenFieldTypes(fd.Type.Params)
			if len(params) != 1 || !isIdent(params[0], x+"Deps") {
				continue
			}
			results := flattenFieldTypes(fd.Type.Results)
			if len(results) == 0 || len(results) > 2 || (len(results) == 2 && !isIdent(results[1], "error")) {
				continue
			}
			st := structs[x+"Deps"]
			if st == nil {
				st = findAppStruct(projectDir, names, x+"Deps")
			}
			if st == nil {
				continue
			}
			pos++
			keys := map[string]bool{}
			for _, f := range st.Fields.List {
				for _, fn := range f.Names {
					keys[fn.Name] = true
				}
				if len(f.Names) == 0 {
					if en := embeddedFieldName(f.Type); en != "" {
						keys[en] = true
					}
				}
			}
			providers = append(providers, graphProvider{
				Name:     x,
				Result:   printType(fset, results[0]),
				Fallible: len(results) == 2,
				Deps:     collectDepsFields(fset, st),
				DepsKeys: keys,
				Imports:  imports,
				File:     "internal/app/" + n,
				Pos:      pos,
			})
		}
	}
	return providers, roots, nil
}

// findAppStruct finds a struct declared in another owned internal/app file.
func findAppStruct(projectDir string, names []string, want string) *ast.StructType {
	for _, n := range names {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(projectDir, "internal", "app", n), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		if st := findStructType(file, want); st != nil {
			return st
		}
	}
	return nil
}

// rootDirective reads `//forge:root [name]` off a type's doc comment.
func rootDirective(doc *ast.CommentGroup) (string, bool) {
	if doc == nil {
		return "", false
	}
	for _, c := range doc.List {
		if m := rootDirectiveRE.FindStringSubmatch(c.Text); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// fileImportAliases maps each import's name in file (explicit alias, or the
// path's last element) to its path. Blank and dot imports are skipped.
func fileImportAliases(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			name = imp.Name.Name
		} else if isMajorVersionSuffix(name) {
			trimmed := strings.TrimSuffix(path, "/"+name)
			name = trimmed[strings.LastIndex(trimmed, "/")+1:]
		}
		out[name] = path
	}
	return out
}

// typeExprImports returns the alias → path pairs a type expression's
// package selectors name, looked up in imports. A selector whose alias is not
// in imports is reported in unknown.
func typeExprImports(expr string, imports map[string]string) (map[string]string, []string) {
	out := map[string]string{}
	var unknown []string
	e, err := parser.ParseExpr(expr)
	if err != nil {
		return out, nil
	}
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			if p, ok := imports[id.Name]; ok {
				out[id.Name] = p
			} else {
				unknown = append(unknown, id.Name)
			}
		}
		return false
	})
	return out, unknown
}

// audienceRoutesName is the generated route set's node name for an audience:
// user → UserRoutes, daemon → DaemonRoutes.
func audienceRoutesName(audience string) string {
	return naming.ToPascalCase(audience) + "Routes"
}

// logName is a node's label in errors and logs: its accessor in snake_case.
func logName(name string) string { return naming.ToSnakeCase(name) }
