// File: internal/codegen/composition_graph_commands.go
//
// One cobra command per root (docs/design/composition-graph.md §1, PR 4).
// The forge-owned cmd/<bin>/cmd/roots_gen.go is the projection of the
// //forge:root structs onto the command tree: `<bin> server` runs the Server
// root, `<bin> server api` the root declared `command=server,api`. Each
// command hands its root to runRoot — the PROCESS POLICY in the owned
// cmd/<bin>/cmd/serve.go (config, logger, boot probes, migrate, telemetry,
// the readiness gate and stop budget) — together with the two facts the
// policy cannot know without the graph, both read off the root's reach at
// generate time:
//
//   - Migrate: the closure reaches the database, so the process migrates
//     before building it (a worker root that never touches the database
//     never runs a migration);
//   - the probes: a root whose closure serves no g.Probes (no API listener)
//     gets a probe listener of its own from Run<Root> (graph_gen.go), so
//     every process answers /healthz and /readyz on the server address.
//
// roots_gen.go calls an owned function, so it is written only while the
// command tree declares it: a tree without runRoot (a serve.go from before
// the per-root commands) keeps compiling, and generate says what to add.

package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
)

// reservedRootCommands are the command tree's own top-level commands, which
// no root's command path may start with.
var reservedRootCommands = map[string]string{
	"version":    "version.go",
	"db":         "db.go",
	"metrics":    "metrics.go",
	"help":       "cobra",
	"completion": "cobra",
}

// checkRootCommands fails on two roots with one command path and on a path
// that takes a built-in command's name.
func checkRootCommands(roots []graphRoot) error {
	seen := map[string]string{}
	for _, r := range roots {
		if len(r.Command) == 0 {
			continue
		}
		if owner, reserved := reservedRootCommands[r.Command[0]]; reserved {
			return fmt.Errorf("%s:%d: root %s runs as `%s`, but %q is the command tree's own command (%s) — give the root another name or a command= option",
				r.File, r.Line, r.TypeName, strings.Join(r.Command, " "), r.Command[0], owner)
		}
		key := strings.Join(r.Command, " ")
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("%s:%d: roots %s and %s both run as `%s` — give one a command= option of its own", r.File, r.Line, prev, r.TypeName, key)
		}
		seen[key] = r.TypeName
	}
	return nil
}

// graphComposeInputReads reads which graph inputs (g.Ctx, g.Cfg, g.Log,
// g.ServerCfg, g.Probes) each node's new<X> method in compose.go reads.
func graphComposeInputReads(src string, nodes []*graphNode) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	file, err := parser.ParseFile(token.NewFileSet(), "compose.go", src, parser.SkipObjectResolution)
	if err != nil {
		return out
	}
	isNode := map[string]bool{}
	for _, n := range nodes {
		isNode[n.Name] = true
	}
	for name, fd := range graphComposeMethods(file) {
		x, ok := strings.CutPrefix(name, "new")
		if !ok || !isNode[x] || len(fd.Recv.List[0].Names) != 1 {
			continue
		}
		recv := fd.Recv.List[0].Names[0].Name
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv && reservedGraphNames[sel.Sel.Name] {
				if out[x] == nil {
					out[x] = map[string]bool{}
				}
				out[x][sel.Sel.Name] = true
			}
			return true
		})
	}
	return out
}

// markRootReach sets what each root's process policy reads off its reach:
// whether a node in it serves g.Probes, and whether it reaches the database.
func markRootReach(views []graphRootView, inputReads map[string]map[string]bool, byName map[string]*graphNode, u *graphUniverse) {
	for i := range views {
		v := &views[i]
		for _, name := range v.Reach {
			if inputReads[name]["Probes"] {
				v.ServesProbes = true
			}
			if n := byName[name]; n != nil && u.isDatabase(n) {
				v.Migrate = true
			}
		}
	}
}

// isDatabase reports whether n's value is a database pool: a *sql.DB, a
// pgxpool.Pool, or a struct embedding one (the scaffolded `type DB struct{
// *sql.DB }`). Untyped (mid-edit), the scaffolded DB provider is recognised
// by its type as written.
func (u *graphUniverse) isDatabase(n *graphNode) bool {
	if t, ok := u.produced[n.Name]; ok {
		return isDatabaseType(t, 0)
	}
	return n.Kind == nodeProvider && n.Type == "*DB"
}

func isDatabaseType(t types.Type, depth int) bool {
	if depth > 3 {
		return false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	if obj := named.Obj(); obj.Pkg() != nil {
		switch obj.Pkg().Path() + "." + obj.Name() {
		case "database/sql.DB", "github.com/jackc/pgx/v5/pgxpool.Pool", "github.com/jackc/pgx/v4/pgxpool.Pool":
			return true
		}
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := 0; i < st.NumFields(); i++ {
		if f := st.Field(i); f.Embedded() && isDatabaseType(f.Type(), depth+1) {
			return true
		}
	}
	return false
}

// rootCmdView is one command of roots_gen.go: a root's command, or a group
// command that only holds others (`server` above `server api`).
type rootCmdView struct {
	Var, Use, Short, Long string
	Parent                string
	Root                  *graphRootView
}

type rootsGenView struct {
	Module   string
	Commands []rootCmdView
	Top      []string
}

// rootCommandViews lays the roots' command paths out as a cobra tree: one
// command per distinct path prefix, parents before children, in path order.
func rootCommandViews(roots []graphRootView) ([]rootCmdView, []string) {
	byPath := map[string]*graphRootView{}
	prefixes := map[string]bool{}
	for i := range roots {
		r := &roots[i]
		byPath[strings.Join(r.Command, " ")] = r
		for j := 1; j <= len(r.Command); j++ {
			prefixes[strings.Join(r.Command[:j], " ")] = true
		}
	}
	paths := make([]string, 0, len(prefixes))
	for p := range prefixes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	varOf := func(path string) string {
		var b strings.Builder
		for _, w := range strings.Fields(path) {
			b.WriteString(naming.ToPascalCase(w))
		}
		s := b.String()
		return strings.ToLower(s[:1]) + s[1:] + "Cmd"
	}
	var out []rootCmdView
	var top []string
	for _, p := range paths {
		words := strings.Fields(p)
		v := rootCmdView{Var: varOf(p), Use: words[len(words)-1]}
		if len(words) > 1 {
			v.Parent = varOf(strings.Join(words[:len(words)-1], " "))
		} else {
			top = append(top, v.Var)
		}
		if r := byPath[p]; r != nil {
			v.Root = r
			var fields []string
			for _, f := range r.Fields {
				fields = append(fields, logName(f.Accessor))
			}
			if len(fields) == 0 {
				v.Short = fmt.Sprintf("Run the %s root (it holds no nodes)", r.Name)
			} else {
				v.Short = fmt.Sprintf("Run the %s root: %s", r.Name, strings.Join(fields, ", "))
			}
			v.Long = fmt.Sprintf("Run the %s root of the composition graph (internal/app): build every node its fields reach, dependencies first, then supervise them until a signal or a failing node. Start order (stop is the reverse): %s.", r.Name, r.Plan)
		} else {
			v.Short = fmt.Sprintf("The %s roots", p)
		}
		out = append(out, v)
	}
	return out, top
}

// rootsGenFile is the generated command file, in the command tree's dir.
const rootsGenFile = "roots_gen.go"

// emitRootCommands writes cmd/<bin>/cmd/roots_gen.go for the primary binary,
// or retires it — with a warning — when the tree has no runRoot to call.
func emitRootCommands(projectDir, module string, roots []graphRootView, cs *checksums.FileChecksums, warn io.Writer) error {
	treeDir := primaryCmdTreeDir(projectDir)
	if treeDir == "" {
		return nil
	}
	rel := filepath.Join(treeDir, rootsGenFile)
	policy, registered := cmdTreeRootSeams(filepath.Join(projectDir, treeDir))
	if !policy {
		RetireGenerated(projectDir, rel, cs)
		names := make([]string, len(roots))
		for i, r := range roots {
			names[i] = "`" + strings.Join(r.Command, " ") + "`"
		}
		fmt.Fprintf(warn, "  ⚠️  %s declares no runRoot, the process policy every root's command runs through, so the commands for this project's roots (%s) are not generated: add `func runRoot(ctx context.Context, deps Deps, root rootSpec) error` to %s (forge skill load architecture — \"Roots and commands\") and register `rootCommands(deps)` in root.go\n",
			filepath.ToSlash(treeDir), strings.Join(names, ", "), filepath.ToSlash(filepath.Join(treeDir, "serve.go")))
		return nil
	}
	if !registered {
		fmt.Fprintf(warn, "  ⚠️  %s never calls rootCommands(deps), so none of the roots' commands is on the command tree: add `root.AddCommand(rootCommands(deps)...)` to newRootCmd\n",
			filepath.ToSlash(filepath.Join(treeDir, "root.go")))
	}
	cmds, top := rootCommandViews(roots)
	src, err := renderGoTemplate("roots_gen.go.tmpl", rootsGenView{Module: module, Commands: cmds, Top: top})
	if err != nil {
		return err
	}
	if err := writeForgeOwned(projectDir, rel, src, cs); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}

// cmdTreeRootSeams reports whether the command tree's owned files declare
// runRoot (the process policy roots_gen.go calls) and call rootCommands (so
// the generated commands reach the tree).
func cmdTreeRootSeams(treeAbs string) (policy, registered bool) {
	entries, err := os.ReadDir(treeAbs)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == rootsGenFile {
			continue
		}
		file, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(treeAbs, n), nil, parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.FuncDecl:
				if x.Recv == nil && x.Name.Name == "runRoot" {
					policy = true
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "rootCommands" {
					registered = true
				}
			}
			return true
		})
	}
	return policy, registered
}
