// File: internal/codegen/composition_graph_mixed.go
//
// Adopting the composition graph one root at a time (forge#2285).
//
// A project on the legacy Infra / NewComponents composition used to be all or
// nothing: any `type Infra struct` or `func NewComponents` turned graph mode
// off for the whole project, and `forge fix composition-graph` converts
// everything at once. The largest projects cannot convert in one step.
//
// A root marked `adopt` (`//forge:root public command=public-api adopt`) is
// the explicit, per-root opt-in. In such a project (CompositionMixed):
//
//   - the legacy composition is generated exactly as before (compose.go,
//     lifecycle.go, mounts_services_gen.go, the per-component commands);
//   - beside it, the graph is generated for the adopted roots ONLY: their
//     constructor methods in MixedComposeFile (owned, reconciled like
//     compose.go), graph_gen.go and routes_gen.go holding exactly what those
//     roots reach (outOfMixedReach), and roots_gen.go's command per root once
//     the command tree declares runRoot;
//   - no default Server root is generated: the legacy server is the server;
//   - a root may reach nothing that builds the legacy composition
//     (validateMixedReach), so no command constructs both.
//
// Not yet projected for adopted roots: deploy/kcl's ROOTS and
// pkg/config/roots_gen.go (a workload keeps the legacy env until the
// project is in graph mode). The last step is `forge fix composition-graph`.

package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

// adoptedRoots is the roots marked adopt.
func adoptedRoots(roots []graphRoot) []graphRoot {
	var out []graphRoot
	for _, r := range roots {
		if r.Adopt {
			out = append(out, r)
		}
	}
	return out
}

// outOfMixedReach is the reachScope beside a legacy composition: every node
// the adopted roots do not reach is left out — graph_gen.go has no accessor
// for it and MixedComposeFile no constructor — and a problem on a node they
// do reach fails generate, as it does in graph mode.
func (m *graphModel) outOfMixedReach(roots []graphRoot, draft string, problems []graphProblem) (map[string]unbuiltNode, error) {
	edges, lazy := graphComposeEdgesLazy(draft, m.nodes)
	// graphRootViews resolves each root's fields in place; this is a draft.
	copied := make([]graphRoot, len(roots))
	for i, r := range roots {
		r.Fields = append([]graphRootField(nil), r.Fields...)
		copied[i] = r
	}
	views, err := graphRootViews(copied, m.nodes, m.u, edges, lazy)
	if err != nil {
		return nil, err
	}
	reached := map[string]bool{}
	for _, v := range views {
		for _, n := range v.Reach {
			reached[n] = true
		}
	}
	var inReach []graphProblem
	unbuilt := map[string]unbuiltNode{}
	for _, p := range problems {
		if reached[p.Node] {
			inReach = append(inReach, p)
			continue
		}
		u := unbuilt[p.Node]
		u.Problems = append(u.Problems, p)
		unbuilt[p.Node] = u
	}
	if len(inReach) > 0 {
		return nil, graphProblemsError(inReach)
	}
	for _, n := range m.nodes {
		if !reached[n.Name] {
			if _, ok := unbuilt[n.Name]; !ok {
				unbuilt[n.Name] = unbuiltNode{}
			}
		}
	}
	return unbuilt, nil
}

// legacyCompositionNames are the declarations of the legacy composition a
// node an adopted root reaches must not build on.
var legacyCompositionNames = map[string]bool{"Infra": true, "OpenInfra": true, "NewComponents": true, "Components": true}

// validateMixedReach refuses an adopted root that reaches a node whose
// construction — its constructor method, or its provider func — names the
// legacy composition: that root's process would build the legacy composition
// too, and with it every resource it opens, which is exactly what adopting a
// root one at a time must not do.
func (m *graphModel) validateMixedReach(projectDir string, providers []graphProvider) error {
	methods := map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	if file, err := parser.ParseFile(fset, MixedComposeFile, m.compose, parser.SkipObjectResolution); err == nil {
		methods = graphComposeMethods(file)
	}
	providerFile := map[string]string{}
	for _, p := range providers {
		providerFile[p.Name] = p.File
	}
	parsed := map[string]*ast.File{}
	providerFunc := func(name string) *ast.FuncDecl {
		rel := providerFile[name]
		if rel == "" {
			return nil
		}
		f, ok := parsed[rel]
		if !ok {
			f, _ = parser.ParseFile(token.NewFileSet(), filepath.Join(projectDir, filepath.FromSlash(rel)), nil, parser.SkipObjectResolution)
			parsed[rel] = f
		}
		if f == nil {
			return nil
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "New"+name {
				return fd
			}
		}
		return nil
	}
	var bad []string
	for _, v := range m.rootViews {
		for _, name := range v.Reach {
			where := ""
			if fd := methods["new"+name]; fd != nil && namesLegacyComposition(fd) != "" {
				where = fmt.Sprintf("its constructor (%s's new%s) names %s", MixedComposeFile, name, namesLegacyComposition(fd))
			} else if fd := providerFunc(name); fd != nil && namesLegacyComposition(fd) != "" {
				where = fmt.Sprintf("its provider (New%s, %s) names %s", name, providerFile[name], namesLegacyComposition(fd))
			}
			if where != "" {
				bad = append(bad, fmt.Sprintf("root %s reaches %s, and %s", v.Name, name, where))
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("an adopted root may not build the legacy composition, or its process would open everything Infra and NewComponents open:\n  - %s\n"+
		"Give the node a provider of its own that builds what it needs from its Deps (`func New<X>(<X>Deps) (T, error)`), the way the graph builds everything else",
		strings.Join(bad, "\n  - "))
}

// namesLegacyComposition is the first legacy composition declaration fd
// refers to by name (not as a field or method of something else, nor a
// composite literal's key), "" when none.
func namesLegacyComposition(fd *ast.FuncDecl) string {
	sel := map[*ast.Ident]bool{}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			sel[x.Sel] = true
		case *ast.CompositeLit:
			for _, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						sel[id] = true
					}
				}
			}
		}
		return true
	})
	found := ""
	ast.Inspect(fd, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && !sel[id] && legacyCompositionNames[id.Name] && found == "" {
			found = id.Name
		}
		return found == ""
	})
	return found
}
