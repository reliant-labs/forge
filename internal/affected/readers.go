package affected

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// readerIndex answers "which packages' tests name this repo path". It is a
// safety net under the rules, never a claim: a rule that says a file is inert
// (docs/**) is wrong the day a test starts reading one, and the index catches
// that by finding the path in the test's string literals. It over-approximates
// on purpose: a package selected for no reason costs seconds, one wrongly
// skipped costs a broken main.
type readerIndex struct {
	reads map[string][]string // package -> tracked paths its tests name
}

// of returns the packages whose test sources name f (or a directory above it).
func (ix *readerIndex) of(f string) []string {
	var out []string
	for pkg, paths := range ix.reads {
		for _, p := range paths {
			if f == p || strings.HasPrefix(f, p+"/") {
				out = append(out, pkg)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// rootMarkers are what a test writes to find the repository root. A package
// whose tests never do so reads only its own directory, which the import
// graph already covers.
var rootMarkers = []string{
	"repoRoot(", "RepoRoot(", "runtime.Caller(", `".."`, `"../`, "git rev-parse", "findRoot", `"go.mod"`, "projectRoot(",
}

func loadReaders(ctx context.Context, root string, git func(context.Context, string, ...string) (string, error), g *packageGraph) *readerIndex {
	ix := &readerIndex{reads: map[string][]string{}}
	out, err := git(ctx, root, "ls-files", "--cached")
	if err != nil {
		return ix
	}
	tr := newTrackedIndex(strings.Split(out, "\n"))
	for dir, pkg := range g.dirPkg {
		if cands := testCandidates(dir); len(cands) > 0 {
			if reads := tr.resolveAll(cands); len(reads) > 0 {
				ix.reads[pkg] = reads
			}
		}
	}
	return ix
}

// testCandidates lists the strings of one package's _test.go files that may
// name a repo path, when those files locate the repo root at all.
func testCandidates(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var cands []string
	locates := false
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		// A file behind the e2e or integration tag is not built by the unit
		// lane; the tagged lanes have their own selection.
		if len(fileLanes(filepath.Join(dir, e.Name()))) > 0 {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, m := range rootMarkers {
			if strings.Contains(string(src), m) {
				locates = true
				break
			}
		}
		f, err := parser.ParseFile(fset, e.Name(), src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				cands = append(cands, callPaths(x)...)
			case *ast.BasicLit:
				if s, ok := strLit(x); ok {
					cands = append(cands, s)
				}
			}
			return true
		})
	}
	if !locates {
		return nil
	}
	return cands
}

// callPaths joins the consecutive literal arguments of one call into the path
// they spell: filepath.Join(root, "kcl", "tests") names kcl/tests.
func callPaths(x *ast.CallExpr) []string {
	var cands, run []string
	flush := func() {
		if len(run) > 1 {
			cands = append(cands, strings.Join(run, "/"))
		}
		run = nil
	}
	for _, a := range x.Args {
		if s, ok := strLit(a); ok {
			run = append(run, s)
		} else {
			flush()
		}
	}
	flush()
	return cands
}

func strLit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

type trackedIndex struct {
	files, dirs map[string]bool
}

func newTrackedIndex(tracked []string) *trackedIndex {
	t := &trackedIndex{files: map[string]bool{}, dirs: map[string]bool{}}
	for _, f := range tracked {
		if f == "" {
			continue
		}
		t.files[f] = true
		for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
			t.dirs[d] = true
		}
	}
	return t
}

// resolveAll maps literals to the tracked files or directories they name. A
// bare word ("docs", "scripts") is also a label or a table name, so it counts
// only when it is a real file at the repo root.
func (t *trackedIndex) resolveAll(cands []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		if strings.ContainsAny(c, " \t\n") {
			continue // prose, not a path
		}
		for strings.HasPrefix(c, "../") || strings.HasPrefix(c, "./") {
			c = strings.TrimPrefix(strings.TrimPrefix(c, "../"), "./")
		}
		c = path.Clean(c)
		if c == "." || c == "" || strings.HasPrefix(c, "/") || strings.HasPrefix(c, "..") {
			continue
		}
		if i := strings.IndexAny(c, "*?[{%$"); i >= 0 {
			c = c[:i]
			j := strings.LastIndex(c, "/")
			if j < 0 {
				continue
			}
			c = c[:j]
		}
		if !t.files[c] && !t.dirs[c] {
			continue
		}
		if !strings.Contains(c, "/") && !t.files[c] {
			continue
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}
