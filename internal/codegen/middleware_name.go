// File: internal/codegen/middleware_name.go
//
// The SINGLE source of truth for the generated in-process middleware
// decorator's identity (middleware_gen.go): the exact constructor + struct
// names and the op-namespace segment its wrapper methods stamp on spans and
// metrics. Both the generator (via generate_middleware, which passes the
// resolved names into contract.WriteObservedDecorator) and the compose
// assembler (which emits `<alias>.<Constructor>(<alias>.New(...))`) resolve the
// name HERE, so the emitted call is byte-identical to the generated declaration
// — a mismatch would be `undefined: <alias>.<Constructor>` at
// generate-validation. The contract generator lives in a package that must not
// import codegen (import cycle), so it consumes this resolver's output through
// the CLI rather than calling it directly; there is still exactly ONE
// implementation, so the two can never drift.
//
// The wrapper is keyed off the constructor's CONCRETE return type, not the
// Service interface it returns. The constructor's SIGNATURE returns the
// interface (`func New(Deps) Service`), so the concrete type is read from the
// return EXPRESSION (`return &service{…}` → "service", title-cased → "Service",
// so the canonical single-impl package is UNCHANGED: NewServiceWithForgeMiddleware).
// Concrete-type keying is what lets sibling impls of one interface get DISTINCT
// wrappers instead of colliding on a single New<Iface>WithForgeMiddleware.

package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// MiddlewareWrapper is the resolved identity of one component's generated
// middleware decorator.
type MiddlewareWrapper struct {
	// Constructor is the exported wrapper constructor, e.g.
	// "NewServiceWithForgeMiddleware". Compose emits `<alias>.<Constructor>(…)`;
	// the generator declares `func <Constructor>(inner <Iface>) <Iface>`.
	Constructor string
	// Struct is the unexported decorator struct, e.g. "forgeMiddlewareService".
	Struct string
	// OpSegment is the op-namespace infix. Empty for a package with a single
	// wrapped constructor (the op stays the clean "<pkg>.<Method>"); the
	// disambiguator token otherwise, so sibling impls are distinguishable in
	// traces/metrics ("<pkg>.<OpSegment>.<Method>").
	OpSegment string
}

// wrappedConstructor pairs an exported constructor whose SIGNATURE returns the
// wrapped interface with the concrete impl type its body constructs (read from
// the return EXPRESSION). Concrete is "" when it could not be resolved from a
// composite-literal return.
type wrappedConstructor struct {
	Ctor     string
	Concrete string
}

// resolveMiddlewareWrappers assigns each wrapped constructor a DISTINCT wrapper
// identity, keyed by constructor name. It keys off the concrete return type
// (title-cased) so sibling impls of one interface never collide. Two
// constructors that build the SAME concrete type (or a constructor whose
// concrete type could not be resolved) fall back to the constructor NAME, which
// is guaranteed unique because Go forbids duplicate func names in a package —
// so the result is always a set of distinct, valid, exported names (never a
// hard error). The op segment is empty when the package has a single wrapped
// constructor (the common case: the op namespace stays "<pkg>.<Method>") and
// the disambiguator otherwise.
//
// Pure (no filesystem) so the multi-impl and same-concrete-collision cases are
// unit-testable directly — independent of whether the generate pipeline can
// even emit more than one wrapped constructor per package (it currently emits
// exactly one, for `New`).
func resolveMiddlewareWrappers(ctors []wrappedConstructor) map[string]MiddlewareWrapper {
	byConcrete := map[string]int{}
	for _, c := range ctors {
		if c.Concrete != "" {
			byConcrete[c.Concrete]++
		}
	}
	single := len(ctors) <= 1
	out := make(map[string]MiddlewareWrapper, len(ctors))
	for _, c := range ctors {
		var w MiddlewareWrapper
		if c.Concrete == "" || byConcrete[c.Concrete] > 1 {
			// Unresolvable concrete type OR a same-concrete collision: key off
			// the (unique) constructor name. The constructor already begins with
			// "New" (e.g. "NewReadOnly"), so the wrapper is
			// <Ctor>WithForgeMiddleware, not New<Ctor>WithForgeMiddleware.
			seg := upperFirst(c.Ctor)
			w = MiddlewareWrapper{
				Constructor: c.Ctor + "WithForgeMiddleware",
				Struct:      "forgeMiddleware" + seg,
				OpSegment:   seg,
			}
		} else {
			seg := upperFirst(c.Concrete)
			w = MiddlewareWrapper{
				Constructor: "New" + seg + "WithForgeMiddleware",
				Struct:      "forgeMiddleware" + seg,
				OpSegment:   seg,
			}
		}
		if single {
			w.OpSegment = ""
		}
		out[c.Ctor] = w
	}
	return out
}

// ResolveMiddlewareWrapper returns the generated middleware decorator's identity
// for the component package at dir whose canonical `New` constructor returns the
// wrapped interface ifaceName. This is what BOTH the generate walk (which passes
// the result into contract.WriteObservedDecorator) and the compose assembler
// call, so the emitted constructor call matches the generated declaration
// exactly.
//
// The generate pipeline wraps exactly one constructor per package (`New`), so
// the resolver keys the wrapper off that single constructor's concrete return
// type and keeps the clean "<pkg>.<Method>" op namespace. The multi-constructor
// disambiguation and same-concrete fallback live in resolveMiddlewareWrappers
// and are exercised by its unit tests.
func ResolveMiddlewareWrapper(dir, ifaceName string) MiddlewareWrapper {
	if ifaceName == "" {
		ifaceName = "Service"
	}
	concrete := concreteReturnType(dir, "New", ifaceName)
	return resolveMiddlewareWrappers([]wrappedConstructor{{Ctor: "New", Concrete: concrete}})["New"]
}

// concreteReturnType reads the concrete impl type the constructor named ctor in
// dir builds, from its return EXPRESSION — `return &service{…}` /
// `return service{…}` → "service". Only a constructor whose SIGNATURE returns
// ifaceName as its first result is considered (the wrapped shape). Returns ""
// when the constructor is absent, does not return ifaceName first, returns the
// interface via a non-composite expression (a delegated call or a local var),
// or the dir can't be parsed — the caller then degrades to a
// constructor-name-keyed wrapper. Non-test, non-generated files only.
func concreteReturnType(dir, ctor, ifaceName string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil || fn.Name.Name != ctor {
				continue
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
				return ""
			}
			// The wrapped shape returns the interface FIRST (value-first,
			// error-last). Its concrete impl is that first result's expression.
			if printType(fset, fn.Type.Results.List[0].Type) != ifaceName {
				return ""
			}
			if fn.Body == nil {
				return ""
			}
			return firstConcreteReturn(fn.Body)
		}
	}
	return ""
}

// firstConcreteReturn walks body for the first `return <composite>, …` whose
// FIRST result is a composite literal (optionally address-of) over a
// same-package type, and returns that type's name. Address-of (`&service{}`) and
// bare (`service{}`) composites both count; nil / call / identifier returns are
// skipped (unresolvable here — e.g. `return nil, err` in a fallible
// constructor, which is passed over in favor of the real `return &service{…}`).
func firstConcreteReturn(body *ast.BlockStmt) string {
	var found string
	ast.Inspect(body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		expr := ret.Results[0]
		if u, ok := expr.(*ast.UnaryExpr); ok && u.Op == token.AND {
			expr = u.X
		}
		cl, ok := expr.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := cl.Type.(*ast.Ident); ok {
			found = id.Name
			return false
		}
		return true
	})
	return found
}
