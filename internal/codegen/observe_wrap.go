// File: internal/codegen/observe_wrap.go
//
// Detection for the OWNED per-package observability seam (observe_chain.go).
//
// `forge scaffold package` / `scaffold adapter` (and the package kinds) scaffold
// an owned observe_chain.go next to contract.go:
//
//	func newObserveChain() *observe.ComponentChain { ... }
//
// — the user-owned builder that assembles the in-process middleware chain
// (span / metrics / structured logging / panic-recovery) the component's
// GENERATED decorator (middleware_gen.go) routes every interface method
// through. The seam is the user's from day 0 (scaffold-once) and is THE
// extension point; the decorator that consumes it is forge-owned and
// regenerated from the Service interface every run (contract.WriteObservedDecorator),
// so adding a method to the contract grows the wrapper automatically — no
// hand-maintenance.
//
// The composition site opts a component into the wrapper AUTOMATICALLY when
// two facts hold on disk:
//
//  1. the package declares the owned seam `func newObserveChain() ...`
//     (DetectObserveChainSeam), and
//  2. the constructor RETURNS the Service-contract interface
//     (DetectConstructorType == the interface name — checked at the call
//     site). Concrete-returning handler packages are never wrapped —
//     otelconnect owns the RPC edge, and wrapping a *Service would change
//     the Components field type.
//
// compose.go / reconcileComposeAddComponents then emit the construction in
// wrapped form: `c.X = pkg.New<Concrete>WithForgeMiddleware(pkg.New(pkg.Deps{...}))`,
// where the wrapper constructor — named after the constructor's CONCRETE return
// type (ResolveMiddlewareWrapper), e.g. NewServiceWithForgeMiddleware — lives in
// the generated decorator.

package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// observeChainSeamFunc is the owned chain-builder whose presence opts a
// package into the generated observability decorator. Kept in sync with
// internal/generator/contract (observeChainSeamFunc) and the
// observe_chain.go.tmpl scaffold.
const observeChainSeamFunc = "newObserveChain"

// ShouldInstrumentComponent reports whether the component package at dir gets
// the generated observability decorator (and, at the composition site, the
// wrapped `pkg.New<Concrete>WithForgeMiddleware(pkg.New(...))` emission). It is THE single
// instrument decision — the compose assembler and the generate walk both route
// through it so they can never disagree.
//
// ctorType is DetectConstructorType(dir); ifaceName is the component's
// Service-contract interface name (serviceIfaceOrDefault). A package is
// instrumented when ALL hold:
//
//  1. the constructor RETURNS the Service interface (ctorType == ifaceName).
//     Handler-shaped packages return a concrete *Service and are never wrapped
//     — otelconnect owns the RPC edge, and wrapping *Service would change the
//     Components field type.
//  2. it has NOT opted out via a package-level `// forge:no-observe`
//     (HasPackageNoObserveDirective) — the opt-out wins over every opt-in.
//  3. it has opted IN — either the Phase-2 `// forge:constructor` marker
//     (HasConstructorMarker) OR, for backward compatibility, the legacy owned
//     observe_chain.go seam (DetectObserveChainSeam). Scaffolds stamp the
//     marker AND the seam, so new packages are born instrumented; the seam
//     keeps pre-marker projects working unchanged.
func ShouldInstrumentComponent(dir, ctorType, ifaceName string) bool {
	if ifaceName == "" {
		ifaceName = "Service"
	}
	if ctorType != ifaceName {
		return false
	}
	marker, noObserve := observePackageDecision(dir)
	if noObserve {
		return false
	}
	return marker || DetectObserveChainSeam(dir)
}

// DetectObserveChainSeam reports whether the component package at dir declares
// the OWNED observability seam `func newObserveChain(...) ...` in a non-
// generated, non-test source file. Its presence is the per-package opt-in
// signal for the generated decorator (and, paired with an interface-returning
// New, for the wrapped compose emission).
//
// Returns false for a missing/unparseable dir. Generated (_gen.go) and test
// files are ignored, so the generated decorator's own reference to the seam
// never counts as the seam itself.
func DetectObserveChainSeam(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
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
			if !ok || fn.Recv != nil || fn.Name == nil {
				continue
			}
			if fn.Name.Name == observeChainSeamFunc {
				return true
			}
		}
	}
	return false
}
