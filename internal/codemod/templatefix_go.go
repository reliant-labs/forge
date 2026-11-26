package codemod

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"strconv"
	"strings"
)

// templatefix_go.go — the template fixes to owned Go files.

func init() {
	registerTemplateFix(templateFix{
		name:    "observe-chain-logger",
		version: "v1.12.0",
		short:   "Let each contract package's observe_chain.go log to the component's own Deps.Logger, not slog.Default (forge#2927)",
		long: `Point each internal package's owned observe_chain.go at the component's own logger.

Until v1.12.0 the scaffolded seam built its recovery and log middlewares on
slog.Default():

	logger := slog.Default()

so a component's calls logged to the process default, never to the Deps.Logger
it was constructed with. The template now leaves the logger nil, which the
generated decorator binds to the component's Deps.Logger (else slog.Default):

	var logger *slog.Logger

This rewrites exactly that line inside newObserveChain. A seam that calls a
method on logger itself (logger.Info, ...) is listed, not rewritten: a nil
*slog.Logger would panic there.`,
		candidates: ownedGoFilesNamed("observe_chain.go"),
		rewrite:    rewriteObserveChainLogger,
	})
	registerTemplateFix(templateFix{
		name:    "cmd-server-config",
		version: "v1.12.0",
		short:   "Add the exported cmd.ServerConfig to the command tree's owned serve.go, so a test can boot the server root in-process (forge#2903)",
		long: `Add ServerConfig to cmd/<bin>/cmd/serve.go.

Since v1.12.0 the scaffolded serve.go of a composition-graph project exports

	func ServerConfig(cfg *config.Config) (serverkit.Config, error)

the projection runRoot applies, so an in-process test of the composition graph
boots the server root the way '<bin> server' does (testkit.NewMigratedPostgresURL
and the testing/patterns skill). serve.go is yours from birth, so a project
scaffolded earlier has no ServerConfig. This adds it above serverkitConfig,
calling it the way the template does. A serve.go with no serverkitConfig (one
rewritten past the template's shape) is listed, not rewritten.`,
		candidates: globCandidates("cmd/*/cmd/serve.go"),
		rewrite:    rewriteCmdServerConfig,
	})
}

// observeChainOldLogger is the line the observe_chain.go template wrote
// before v1.12.0, and observeChainNewLogger what it writes now.
const (
	observeChainOldLogger = "\tlogger := slog.Default()\n"
	observeChainNewLogger = "\t// nil is the component's own logger: its Deps.Logger, which the\n" +
		"\t// generated decorator binds (observe.ComponentChain.WithLogger), else\n" +
		"\t// slog.Default(). Set one here to pin this package's log elsewhere.\n" +
		"\tvar logger *slog.Logger\n"
)

func rewriteObserveChainLogger(rel string, src []byte) (out []byte, note, next string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, "", ""
	}
	fn := findFunc(file, "newObserveChain")
	if fn == nil || fn.Body == nil {
		return nil, "", ""
	}
	start, end := fset.Position(fn.Body.Lbrace).Offset, fset.Position(fn.Body.Rbrace).Offset
	body := string(src[start:end])
	if strings.Count(body, observeChainOldLogger) != 1 {
		return nil, "", ""
	}
	if callsMethodOn(fn.Body, "logger") {
		return nil, "", "newObserveChain calls a method on logger, so it cannot become nil; " +
			"to log to the component's Deps.Logger, pass nil to observe.RecoverMiddleware and observe.LogMiddleware instead of logger"
	}
	newBody := strings.Replace(body, observeChainOldLogger, observeChainNewLogger, 1)
	out = append(append(append([]byte{}, src[:start]...), newBody...), src[end:]...)
	return out, "logger := slog.Default() → var logger *slog.Logger (the component's Deps.Logger)", ""
}

func rewriteCmdServerConfig(rel string, src []byte) (out []byte, note, next string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, "", ""
	}
	// Only the composition graph's serve.go (runRoot) carries ServerConfig.
	if findFunc(file, "runRoot") == nil || findFunc(file, "ServerConfig") != nil {
		return nil, "", ""
	}
	sk := findFunc(file, "serverkitConfig")
	if sk == nil {
		return nil, "", "no serverkitConfig to project from; add an exported " +
			"`func ServerConfig(cfg *config.Config) (serverkit.Config, error)` returning the serverkit.Config runRoot runs with"
	}
	var call string
	switch n := sk.Type.Params.NumFields(); n {
	case 1:
		call = "serverkitConfig(cfg)"
	case 2:
		call = "serverkitConfig(cfg, rootConfigs{})"
	default:
		return nil, "", "serverkitConfig takes " + strconv.Itoa(n) + " parameters, not the template's; add an exported " +
			"`func ServerConfig(cfg *config.Config) (serverkit.Config, error)` returning the serverkit.Config runRoot runs with"
	}
	at := sk.Pos()
	if sk.Doc != nil {
		at = sk.Doc.Pos()
	}
	off := fset.Position(at).Offset
	// rel is cmd/<bin>/cmd/serve.go: the binary is the grandparent leaf.
	bin := path.Base(path.Dir(path.Dir(rel)))
	block := cmdServerConfigBlock(bin, call)
	out = append(append(append([]byte{}, src[:off]...), block...), src[off:]...)
	return out, "added func ServerConfig (the serverkit.Config runRoot runs with, for in-process tests)", ""
}

// cmdServerConfigBlock is ServerConfig as cmd-tree-serve-graph.go.tmpl
// writes it, followed by a blank line.
func cmdServerConfigBlock(bin, call string) string {
	return `// ServerConfig is the serverkit.Config a process runs with for cfg — the
// projection runRoot applies — exported so an in-process test of the
// composition graph boots the server root the way ` + "`" + bin + ` server` + "`" + ` does.
// Start from config.Defaults(), point its database URL at a database of the
// test's own (testkit.NewMigratedPostgresURL(t, forgedb.Source())) and its
// port at a free one (pgtest.ReserveLoopbackPort), then:
//
//	skCfg, err := cmd.ServerConfig(cfg)
//	drainer := serverkit.NewDrainer(skCfg)
//	g := &app.Graph{Ctx: ctx, Cfg: cfg, Log: logger, ServerCfg: skCfg,
//		Probes: &serverkit.Probes{Drainer: drainer, Logger: logger}}
//
// and run the server root's method of g (graph_gen.go) in a goroutine, under
// lifecyclekit.Policy{Drainer: drainer, Logger: logger}, until the test
// cancels ctx. The test package imports this one, so it is an external test
// package (package app_test). Change the projection in serverkitConfig, below.
func ServerConfig(cfg *config.Config) (serverkit.Config, error) {
	return ` + call + `
}

`
}

// findFunc returns the top-level function (not method) named name.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// callsMethodOn reports whether body calls a method on the identifier name
// (name.M(...)).
func callsMethodOn(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
