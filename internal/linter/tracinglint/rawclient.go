// File: rawclient.go — forge-tracing-raw-http-client and
// forge-tracing-raw-sql-open.
//
// An outbound call or a query made through a client forge did not build has
// no span, propagates no traceparent and never appears in the trace: the
// request looks fast and the external call it actually waited on is
// invisible. forge ships the traced constructors — observe.NewHTTPClient /
// observe.NewHTTPTransport for plain HTTP, dbtrace.Open / dbtrace.OpenDB for
// database/sql — and these rules flag the constructions that step around
// them.
//
// Both rules flag the DEFECT, not a spelling. An *http.Client whose
// Transport is set to something the rule cannot see into is left alone; only
// a client that demonstrably falls back to an untraced transport (no
// Transport, nil, http.DefaultTransport, a bare *http.Transport) is flagged.
// A pgx pool is flagged only when its function never sets a Tracer.

package tracinglint

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"

	"github.com/reliant-labs/forge/internal/linter/finding"
)

const httpPath = "net/http"

// rawHTTPClientFindings flags untraced *http.Client constructions and uses of
// the package-level default client.
func rawHTTPClientFindings(f *goFile) []finding.Finding {
	httpName := f.importName(httpPath)
	if httpName == "" {
		return nil
	}
	var out []finding.Finding
	flag := func(n ast.Node, what string) {
		out = append(out, finding.Finding{
			Rule:     RuleRawHTTPClient,
			Severity: finding.SeverityError,
			File:     f.rel,
			Line:     f.line(n.Pos()),
			Message:  what + ": its calls open no client span and propagate no traceparent, so the callee is invisible in the caller's trace",
			Remediation: "use observe.NewHTTPClient(observe.HTTPClientOptions{Timeout: …}), or wrap your own transport: " +
				"Transport: observe.NewHTTPTransport(base)",
		})
	}
	// Assignments to http.DefaultClient (installing a traced default) are
	// not uses of it.
	lhs := map[ast.Node]bool{}
	ast.Inspect(f.file, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, l := range as.Lhs {
				lhs[l] = true
			}
		}
		return true
	})
	forEachFunc(f.file, func(body ast.Node) {
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return false // visited as its own function
			case *ast.CompositeLit:
				if name, ok := pkgSelector(f, x.Type, httpPath); ok && name == "Client" {
					if why := untracedTransport(f, x, body); why != "" {
						flag(x, "an *http.Client with "+why)
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
					if name, ok := pkgSelector(f, x.Args[0], httpPath); ok && name == "Client" {
						flag(x, "new(http.Client) has no transport")
					}
				}
				if fn, ok := pkgSelector(f, x.Fun, httpPath); ok {
					switch fn {
					case "Get", "Head", "Post", "PostForm":
						flag(x, fmt.Sprintf("%s.%s uses http.DefaultClient", httpName, fn))
					}
				}
			case *ast.SelectorExpr:
				if name, ok := pkgSelector(f, x, httpPath); ok && name == "DefaultClient" && !lhs[x] {
					flag(x, "http.DefaultClient has no instrumented transport")
				}
			}
			return true
		})
	})
	return out
}

// untracedTransport explains why the client literal's transport is untraced,
// or returns "" when it is (or may be) traced.
func untracedTransport(f *goFile, lit *ast.CompositeLit, body ast.Node) string {
	var transport ast.Expr
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return "" // positional fields: not worth guessing
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Transport" {
			transport = kv.Value
		}
	}
	if transport == nil {
		return "no Transport (it falls back to http.DefaultTransport)"
	}
	if why := untracedTransportExpr(f, transport); why != "" {
		return why
	}
	id, ok := transport.(*ast.Ident)
	if !ok {
		return ""
	}
	// An identifier: untraced only when every value the function gives it is.
	var values []ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.AssignStmt:
			if len(st.Lhs) != len(st.Rhs) {
				return true
			}
			for i, l := range st.Lhs {
				if li, ok := l.(*ast.Ident); ok && li.Name == id.Name {
					values = append(values, st.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, n := range st.Names {
				if n.Name == id.Name && i < len(st.Values) {
					values = append(values, st.Values[i])
				}
			}
		}
		return true
	})
	if len(values) == 0 {
		return ""
	}
	for _, v := range values {
		if untracedTransportExpr(f, v) == "" {
			return ""
		}
	}
	return "an untraced Transport (" + id.Name + " is a bare transport)"
}

// untracedTransportExpr recognises the transports that are certainly
// untraced: nil, http.DefaultTransport (and a Clone of it), and a bare
// http.Transport literal.
func untracedTransportExpr(f *goFile, e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		if x.Name == "nil" {
			return "a nil Transport (it falls back to http.DefaultTransport)"
		}
	case *ast.SelectorExpr:
		if name, ok := pkgSelector(f, x, httpPath); ok && name == "DefaultTransport" {
			return "Transport: http.DefaultTransport"
		}
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return untracedTransportExpr(f, x.X)
		}
	case *ast.CompositeLit:
		if name, ok := pkgSelector(f, x.Type, httpPath); ok && name == "Transport" {
			return "a bare *http.Transport"
		}
	case *ast.CallExpr:
		// A Clone of the default transport, asserted to *http.Transport.
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Clone" {
			if ta, ok := sel.X.(*ast.TypeAssertExpr); ok && untracedTransportExpr(f, ta.X) != "" {
				return "a clone of http.DefaultTransport"
			}
		}
	}
	return ""
}

// sqlOpeners maps an import path predicate to the functions that open an
// untraced pool, and the remediation for each.
type sqlOpener struct {
	match       func(path string) bool
	funcs       map[string]bool
	remediation string
	// unlessTracer exempts a call whose enclosing function sets a Tracer
	// (pgx's own tracing hook).
	unlessTracer bool
}

var sqlOpeners = []sqlOpener{
	{
		match:       func(p string) bool { return p == "database/sql" },
		funcs:       map[string]bool{"Open": true, "OpenDB": true},
		remediation: "open it with dbtrace.Open(driver, dsn) or dbtrace.OpenDB(connector) (github.com/reliant-labs/forge/pkg/observe/dbtrace)",
	},
	{
		match:       func(p string) bool { return p == "github.com/jmoiron/sqlx" },
		funcs:       map[string]bool{"Open": true, "Connect": true, "ConnectContext": true, "MustOpen": true, "MustConnect": true},
		remediation: "wrap a traced pool: sqlx.NewDb(db, driver) with db from dbtrace.Open(driver, dsn)",
	},
	{
		match: func(p string) bool {
			return strings.HasPrefix(p, "github.com/jackc/pgx/") && strings.HasSuffix(p, "/stdlib")
		},
		funcs:       map[string]bool{"OpenDB": true, "OpenDBFromPool": true},
		remediation: "open it with dbtrace.OpenDB(stdlib.GetConnector(cfg))",
	},
	{
		match: func(p string) bool {
			return strings.HasPrefix(p, "github.com/jackc/pgx/") &&
				(strings.HasSuffix(p, "/pgxpool") || isDigits(strings.TrimPrefix(p, "github.com/jackc/pgx/v")))
		},
		funcs:        map[string]bool{"New": true, "NewWithConfig": true, "Connect": true, "ConnectConfig": true},
		remediation:  "set a tracer on the config before connecting (cfg.ConnConfig.Tracer, e.g. otelpgx.NewTracer()), or open through database/sql with dbtrace",
		unlessTracer: true,
	},
}

// rawSQLOpenFindings flags database pools opened without dbtrace.
func rawSQLOpenFindings(f *goFile) []finding.Finding {
	var out []finding.Finding
	forEachFunc(f.file, func(body ast.Node) {
		setsTracer := mentionsTracer(body)
		ast.Inspect(body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			path := f.imports[id.Name]
			for _, o := range sqlOpeners {
				if path == "" || !o.match(path) || !o.funcs[sel.Sel.Name] || (o.unlessTracer && setsTracer) {
					continue
				}
				out = append(out, finding.Finding{
					Rule:        RuleRawSQLOpen,
					Severity:    finding.SeverityError,
					File:        f.rel,
					Line:        f.line(call.Pos()),
					Message:     fmt.Sprintf("%s.%s opens an untraced database pool: its queries open no span, so a request's SQL is missing from its trace", id.Name, sel.Sel.Name),
					Remediation: o.remediation,
				})
			}
			return true
		})
	})
	return out
}

// mentionsTracer reports a `.Tracer = …` assignment or a `Tracer: …` field in
// body.
func mentionsTracer(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, l := range x.Lhs {
				if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "Tracer" {
					found = true
				}
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Tracer" {
				found = true
			}
		}
		return !found
	})
	return found
}

// forEachFunc calls fn with the body of every function in file — each
// declaration and each literal, separately — plus the file itself for
// package-level initialisers (with function bodies pruned by the visitors).
func forEachFunc(file *ast.File, fn func(body ast.Node)) {
	for _, decl := range file.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok {
			fn(gd)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Body != nil {
				fn(x.Body)
			}
		case *ast.FuncLit:
			fn(x.Body)
		}
		return true
	})
}
