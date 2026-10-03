package cli

// THE REMOVAL GUARD for the one transitional client-side cluster apply.
//
// applyClusterPendingLocalFlux exists because an env with no control plane has
// no reconciler: nothing installs Flux into its cluster and nothing writes the
// in-cluster OCIRepository that would point at its bundle, so if the deploy
// does not apply the promotion it just recorded, nothing ever will.
// F-FLUX-LOCAL closes that gap, and this function must then be DELETED
// outright — not kept as a fallback, because a fallback apply beside a working
// reconciler is two writers for one cluster.
//
// WHY A TEST AND NOT A COMMENT. The risk is not that the function exists; it
// has to today. The risk is that a SECOND caller appears. The moment one does,
// deleting it stops being a deletion and becomes a refactor across call sites
// that nobody has scheduled — which is how a path whose whole justification is
// "until F-FLUX-LOCAL" outlives F-FLUX-LOCAL by a year. So the guard is on the
// call count, and it fails on the change that makes removal expensive rather
// than on the removal itself.
//
// This test is expected to be deleted together with the function it guards.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// transitionalClusterApply is the function under guard, and the marker naming
// what deletes it.
const (
	transitionalClusterApply = "applyClusterPendingLocalFlux"
	transitionalDeletedBy    = "F-FLUX-LOCAL"
)

// TestTransitionalClusterApply_HasExactlyOneCaller fails when a second caller
// appears, and says what to do about it.
func TestTransitionalClusterApply_HasExactlyOneCaller(t *testing.T) {
	t.Parallel()

	callers := map[string]int{}
	forEachPackageFile(t, func(path string, file *ast.File, fset *token.FileSet) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != transitionalClusterApply {
				return true
			}
			callers[enclosingFuncName(file, fset, call.Pos())]++
			return true
		})
	})

	total := 0
	for _, n := range callers {
		total += n
	}
	if total != 1 {
		t.Errorf("%s has %d call site(s) in %v, want exactly 1 (followPromote).\n"+
			"  It is the ONE transitional client-side cluster apply, kept only until %s installs a\n"+
			"  reconciler for a control-plane-less env. A second caller turns its deletion from a\n"+
			"  deletion into a cross-site refactor nobody has scheduled — which is how a path\n"+
			"  justified by \"until %s\" outlives %s.\n"+
			"  If you need a cluster apply here, you almost certainly want the reconciler to do it:\n"+
			"  record the promotion and let the bundle converge.",
			transitionalClusterApply, total, keysOfCounts(callers),
			transitionalDeletedBy, transitionalDeletedBy, transitionalDeletedBy)
	}
	if n := callers["followPromote"]; n != 1 {
		t.Errorf("%s's caller is %v, want followPromote — the follow-through is the only place that "+
			"knows an env has no reconciler", transitionalClusterApply, keysOfCounts(callers))
	}
}

// TestTransitionalClusterApply_NamesWhatDeletesIt keeps the function's reason
// for existing attached to it. A transitional path whose docstring stops
// naming its own removal is indistinguishable from a permanent one, and the
// next reader has no way to know it was meant to go.
func TestTransitionalClusterApply_NamesWhatDeletesIt(t *testing.T) {
	t.Parallel()

	var doc string
	forEachPackageFile(t, func(path string, file *ast.File, _ *token.FileSet) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Name.Name == transitionalClusterApply && fn.Doc != nil {
				doc = fn.Doc.Text()
			}
		}
	})
	if doc == "" {
		// Gone, or undocumented. If F-FLUX-LOCAL landed and the
		// function was removed, delete this file too — that is the
		// intended end state, not a failure.
		t.Fatalf("%s has no doc comment (or no longer exists). If %s landed and it was removed, "+
			"delete this guard file with it.", transitionalClusterApply, transitionalDeletedBy)
	}
	if !strings.Contains(doc, transitionalDeletedBy) {
		t.Errorf("%s's doc comment does not name %s as what deletes it. A transitional path that "+
			"stops saying so reads as permanent, and nobody removes it.",
			transitionalClusterApply, transitionalDeletedBy)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

// forEachPackageFile parses every .go file in this package directory.
func forEachPackageFile(t *testing.T, visit func(path string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(".", e.Name())
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		visit(path, file, fset)
	}
}

// enclosingFuncName names the function a position falls inside, for an error
// message that points at a call site rather than a file.
func enclosingFuncName(file *ast.File, fset *token.FileSet, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Pos() <= pos && pos <= fn.End() {
			return fn.Name.Name
		}
	}
	return fset.Position(pos).String()
}

func keysOfCounts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
