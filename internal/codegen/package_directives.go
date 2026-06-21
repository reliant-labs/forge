// File: internal/codegen/package_directives.go
//
// Package-level forge directives — comment markers a package declares in
// its own source (contract.go / doc.go / any non-test .go) to opt into or
// out of generator behaviour, as an alternative to a central forge.yaml
// list. They are the package-scoped siblings of the field-level
// `//forge:optional-dep` marker (see deps_parser.go) and follow the same
// recognition rules: both the spaced (`// forge:foo`) and unspaced
// (`//forge:foo`) forms are accepted, and the marker must be the WHOLE
// comment line (after stripping comment syntax + whitespace) so prose that
// merely mentions the directive is never mistaken for it.
//
// Two directives live here, deliberately separate because they mean two
// DIFFERENT things (FORGE_SHAPE_REDESIGN §1/§4, friction fr-b158e37541):
//
//   - `//forge:exclude-contract` — opt this package OUT of contract
//     codegen (mock/middleware/tracing/metrics scaffold). The per-package
//     equivalent of listing the package in forge.yaml `contracts.exclude`.
//     The package is NOT contract-shaped (or doesn't want a mock).
//
//   - `//forge:external-component` (alias `//forge:provided`) — this
//     component is HAND-CONSTRUCTED in providers.go / OpenInfra, NOT by the
//     type-topological Build injector. The injector skips it as a Build
//     node, but the package STILL gets its contract/mock codegen. This is
//     for a package that IS contract-shaped and WANTS its mock, but whose
//     construction is bespoke (adapter wrapping, two-phase setters, a
//     dialer nil'd on unset env) and so cannot be a plain New(Deps) node.
//
// Both are recognized as either a package doc comment (above the `package`
// clause) or a free-standing comment anywhere in the package's .go files —
// the latter so a package can carry the marker without disturbing its
// existing package doc.

package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Package directive spellings. exclude-contract has a single spelling;
// external-component accepts `provided` as a shorter synonym (same intent,
// "forge does not build this").
const (
	directiveExcludeContract    = "forge:exclude-contract"
	directiveExternalComponent  = "forge:external-component"
	directiveExternalComponent2 = "forge:provided"
)

// HasExcludeContractDirective reports whether the package rooted at dir
// declares `//forge:exclude-contract` in any of its non-test .go files.
// A package carrying this directive opts OUT of contract codegen — the
// per-package equivalent of forge.yaml `contracts.exclude`.
func HasExcludeContractDirective(dir string) bool {
	return packageHasDirective(dir, directiveExcludeContract)
}

// HasExternalComponentDirective reports whether the package rooted at dir
// declares `//forge:external-component` (or `//forge:provided`) in any of
// its non-test .go files. A package carrying this directive is skipped by
// the Build injector (it is hand-wired in providers.go / OpenInfra) but
// STILL gets its contract/mock codegen.
func HasExternalComponentDirective(dir string) bool {
	return packageHasDirective(dir, directiveExternalComponent) ||
		packageHasDirective(dir, directiveExternalComponent2)
}

// packageHasDirective scans every non-test .go file in dir for a comment
// whose whole text (after stripping comment markers + whitespace) equals
// needle. It recognizes both `// needle` and `//needle` (the directive
// form Go's CommentGroup.Text() would otherwise drop). Comments are scanned
// raw via the FileSet's comment list so package-doc, free-standing, and
// inline comments are all seen. Unparseable or unreadable files are
// skipped best-effort — a directive is an opt-in signal; failing to read
// it just means the default behaviour applies.
func packageHasDirective(dir, needle string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Generated files never carry hand-authored directives; skip them
		// both for speed and to avoid a regenerated file echoing a marker.
		if strings.HasSuffix(name, "_gen.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		for _, cg := range file.Comments {
			if cg == nil {
				continue
			}
			for _, c := range cg.List {
				if commentEquals(c, needle) {
					return true
				}
			}
		}
	}
	return false
}

// commentEquals reports whether a single *ast.Comment's inner text equals
// needle. Mirrors HasOptionalDepMarkerCommentGroup's per-comment rule:
// strip `//`, `/* */`, and surrounding whitespace, then exact-match — so a
// comment that merely references the directive inside surrounding prose is
// not a match.
func commentEquals(c *ast.Comment, needle string) bool {
	return trimCommentMarkers(c.Text) == needle
}
