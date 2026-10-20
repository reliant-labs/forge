package codegen

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestRootCommandPath_NameOrCommandOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []string
		want string
		err  string
	}{
		{name: "worker", want: "worker"},
		{name: "api", opts: []string{"command=server,api"}, want: "server api"},
		{name: "gw", opts: []string{"bogus"}, err: "unknown //forge:root option"},
		{name: "gw", opts: []string{"command=Server"}, err: "not a cobra command name"},
		{name: "gw", opts: []string{"command="}, err: "not a cobra command name"},
	} {
		got, err := rootCommandPath(tc.name, tc.opts)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s %v: err = %v, want %q", tc.name, tc.opts, err, tc.err)
			}
			continue
		}
		if err != nil || strings.Join(got, " ") != tc.want {
			t.Errorf("%s %v = %v, %v; want %q", tc.name, tc.opts, got, err, tc.want)
		}
	}
}

func TestDiscoverAppDecls_RootCommandOption(t *testing.T) {
	dir := t.TempDir()
	writeAppFile(t, dir, "roots.go", `package app

// API is the user-facing half.
//
//forge:root api command=server,api
type API struct {
	L *APIListener
}

//forge:root
type Gateway struct{}
`)
	_, roots, err := discoverAppDecls(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range roots {
		got[r.Name] = strings.Join(r.Command, " ")
	}
	if got["api"] != "server api" || got["gateway"] != "gateway" {
		t.Fatalf("command paths = %v", got)
	}

	writeAppFile(t, dir, "roots.go", "package app\n\n//forge:root api comand=x\ntype API struct{}\n")
	if _, _, err := discoverAppDecls(dir); err == nil || !strings.Contains(err.Error(), "internal/app/roots.go:4") {
		t.Fatalf("a bad option must fail discovery with its position, got %v", err)
	}
}

func TestCheckRootCommands_ReservedAndDuplicatePaths(t *testing.T) {
	if err := checkRootCommands([]graphRoot{{TypeName: "Version", Command: []string{"version"}}}); err == nil || !strings.Contains(err.Error(), "version.go") {
		t.Fatalf("a root named after a built-in command was accepted: %v", err)
	}
	err := checkRootCommands([]graphRoot{
		{TypeName: "A", Command: []string{"server", "api"}},
		{TypeName: "B", Command: []string{"server", "api"}},
	})
	if err == nil || !strings.Contains(err.Error(), "both run as `server api`") {
		t.Fatalf("two roots with one path were accepted: %v", err)
	}
	if err := checkRootCommands([]graphRoot{
		{TypeName: "Server", Command: []string{"server"}},
		{TypeName: "API", Command: []string{"server", "api"}},
	}); err != nil {
		t.Fatalf("a root nested under another's command: %v", err)
	}
}

func TestRootCommandViews_NestedPathsBecomeACobraTree(t *testing.T) {
	roots := []graphRootView{
		{Name: "worker", TypeName: "Worker", Command: []string{"worker"}, Plan: "tick"},
		{Name: "api", TypeName: "API", Command: []string{"serve", "api"}, Plan: "api_listener"},
		{Name: "gateway", TypeName: "Gateway", Command: []string{"serve", "gateway"}, Plan: "daemon_listener"},
	}
	cmds, top := rootCommandViews(roots)
	var got []string
	for _, c := range cmds {
		run := "-"
		if c.Root != nil {
			run = c.Root.TypeName
		}
		got = append(got, c.Var+"<"+c.Parent+">"+run)
	}
	want := "serveCmd<>-, serveAPICmd<serveCmd>API, serveGatewayCmd<serveCmd>Gateway, workerCmd<>Worker"
	if strings.Join(got, ", ") != want {
		t.Fatalf("commands = %s\nwant       %s", strings.Join(got, ", "), want)
	}
	if strings.Join(top, ",") != "serveCmd,workerCmd" {
		t.Fatalf("top = %v", top)
	}
}

// cmdTreeFixture writes a minimal command tree for emitRootCommands.
func cmdTreeFixture(t *testing.T, serve string) string {
	t.Helper()
	dir := t.TempDir()
	tree := filepath.Join(dir, "cmd", "demo", "cmd")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	root := "package cmd\n\nfunc newRootCmd(deps Deps) { _ = rootCommands(deps) }\n"
	for name, src := range map[string]string{"root.go": root, "serve.go": serve} {
		if err := os.WriteFile(filepath.Join(tree, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestEmitRootCommands_WritesOneCommandPerRoot(t *testing.T) {
	dir := cmdTreeFixture(t, "package cmd\n\nfunc runRoot(ctx context.Context, deps Deps, root rootSpec) error { return nil }\n")
	var warn bytes.Buffer
	roots := []graphRootView{
		{Name: "server", TypeName: "Server", Command: []string{"server"}, Migrate: true, Fields: []graphRootFieldView{{Accessor: "APIListener"}}, Plan: "db → api_listener"},
		{Name: "worker", TypeName: "Worker", Command: []string{"worker"}, Fields: []graphRootFieldView{{Accessor: "Tick"}}, Plan: "tick"},
	}
	if err := emitRootCommands(dir, "example.com/demo", roots, nil, &warn); err != nil {
		t.Fatal(err)
	}
	if warn.Len() != 0 {
		t.Fatalf("unexpected warning: %s", warn.String())
	}
	src, err := os.ReadFile(filepath.Join(dir, "cmd", "demo", "cmd", "roots_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := squash(string(src))
	for _, want := range []string{
		`rootSpec{Name: "server", Migrate: true, Run: (*app.Graph).RunServer}`,
		`rootSpec{Name: "worker", Migrate: false, Run: (*app.Graph).RunWorker}`,
		`Short: "Run the worker root: tick"`,
		"return []*cobra.Command{serverCmd, workerCmd}",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("roots_gen.go lacks %q:\n%s", want, src)
		}
	}
}

func TestEmitRootCommands_ATreeWithoutRunRootKeepsCompiling(t *testing.T) {
	// A serve.go from before the per-root commands: `server` runs Serve.
	dir := cmdTreeFixture(t, "package cmd\n\nfunc Serve(ctx context.Context, deps Deps) error { return nil }\n")
	stale := filepath.Join(dir, "cmd", "demo", "cmd", "roots_gen.go")
	if err := os.WriteFile(stale, []byte("// Code generated by forge. DO NOT EDIT.\npackage cmd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	roots := []graphRootView{{Name: "worker", TypeName: "Worker", Command: []string{"worker"}}}
	if err := emitRootCommands(dir, "example.com/demo", roots, nil, &warn); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("roots_gen.go was kept for a tree with no runRoot to call (%v)", err)
	}
	if !strings.Contains(warn.String(), "declares no runRoot") || !strings.Contains(warn.String(), "`worker`") {
		t.Fatalf("warning = %q", warn.String())
	}
}

func TestGraphComposeInputReads_ProbesServedByTheListener(t *testing.T) {
	src := `package app

func (g *Graph) newAPIListener() (*APIListener, error) {
	return NewAPIListener(APIListenerDeps{Probes: g.Probes, Logger: g.Log})
}

func (g *Graph) newTick() (*tick.Worker, error) { return tick.New(tick.Deps{Logger: g.Log}), nil }
`
	nodes := []*graphNode{{Name: "APIListener"}, {Name: "Tick"}}
	reads := graphComposeInputReads(src, nodes)
	if !reads["APIListener"]["Probes"] || reads["Tick"]["Probes"] || !reads["Tick"]["Log"] {
		t.Fatalf("reads = %v", reads)
	}
}

func TestIsDatabaseType_PoolsAndStructsEmbeddingThem(t *testing.T) {
	fset := token.NewFileSet()
	sqlFile, err := parser.ParseFile(fset, "sql.go", "package sql\n\ntype DB struct{}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	sqlPkg, err := (&types.Config{}).Check("database/sql", fset, []*ast.File{sqlFile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(fset, "app.go", `package app

import "database/sql"

type DB struct{ *sql.DB }
type Wrapped struct{ DB }
type Client struct{ db *sql.DB }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	imp := importerFunc(func(path string) (*types.Package, error) {
		if path == "database/sql" {
			return sqlPkg, nil
		}
		return nil, os.ErrNotExist
	})
	pkg, err := (&types.Config{Importer: imp}).Check("app", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	typ := func(name string) types.Type { return types.NewPointer(pkg.Scope().Lookup(name).Type()) }
	for name, want := range map[string]bool{"DB": true, "Wrapped": true, "Client": false} {
		if got := isDatabaseType(typ(name), 0); got != want {
			t.Errorf("isDatabaseType(*%s) = %v, want %v", name, got, want)
		}
	}
	if !isDatabaseType(types.NewPointer(sqlPkg.Scope().Lookup("DB").Type()), 0) {
		t.Error("*sql.DB is a database")
	}
}

func TestServeGraphTemplate_RunRootIsThePolicyEveryRootRunsUnder(t *testing.T) {
	render := func(fields map[string]bool) string {
		t.Helper()
		out, err := templates.ProjectTemplates().Render("cmd-tree-serve-graph.go.tmpl", CmdServerTemplateData{Module: "example.com/proj", Name: "proj", ConfigFields: fields})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := format.Source(out); err != nil {
			t.Fatalf("serve.go does not format: %v\n%s", err, out)
		}
		return string(out)
	}
	full := render(DefaultConfigFieldNames())
	for _, want := range []string{
		"func runRoot(ctx context.Context, deps Deps, root rootSpec) error {",
		"if root.Migrate {",
		"return root.Run(g, ctx, lifecyclekit.Policy{",
		"skCfg.ShutdownTimeout = cfg.ShutdownTimeout.AsDuration()",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("serve.go lacks %q", want)
		}
	}
	if strings.Contains(full, "drainbudget") || strings.Contains(full, "RunServer") {
		t.Error("a config with the drain fields reads them from config, and no root is named in serve.go")
	}
	// A config without the drain fields still honours PRE_STOP_DELAY and
	// SHUTDOWN_TIMEOUT, through the one reader of them.
	bare := render(map[string]bool{"Port": true})
	if !strings.Contains(bare, "drainbudget.FromEnv(os.Getenv)") {
		t.Errorf("a config without the drain fields must read the drain budget from the environment:\n%s", bare)
	}
}
