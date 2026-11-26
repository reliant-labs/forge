// File: reach.go — which code a traced process runs, and which code another
// module can reach.
//
// The tracing rules exist to keep a request inside ONE trace, so they only
// mean something in code a traced process runs. Options.LibraryOnly answers
// that for a whole project (observability off); this file answers it per
// package, from the import graph of the Go source on disk under the project
// root — the module and any module nested in it (forge's gen/ is one):
//
//   - A `package main` is a TRACED process when its local import closure
//     (itself included) holds a package that may install telemetry: one that
//     imports forge's pkg/serverkit or any go.opentelemetry.io/ package, or
//     calls forge's observe.Setup. A closure that imports a path under one of
//     the local module paths that is not on disk is unknown, and counts as
//     traced.
//   - A package is EXTERNALLY REACHABLE when another module can run it: it is
//     importable (not main, no internal/ segment below its module root), or
//     an importable package imports it, directly or not.
//   - A package is UNTRACED when it is not externally reachable, at least one
//     main reaches it, and every main that reaches it is untraced: a load
//     generator, a one-off tool, an example client. No process that runs it
//     records a trace, so there is no trace for its log lines or clients to
//     cut, and Lint leaves it out of every rule.
//
// Every ambiguity keeps the current behaviour: a package no main reaches
// (dead code, test-only helpers) stays linted, as does any package another
// module can reach, since forge cannot see the processes that import it.
// Build constraints are ignored (every file's imports count), which only
// adds edges, and so only keeps more code in scope. The signal reads the
// local source only: a main that boots through a third-party framework that
// installs a tracer provider the program never names would be taken for
// untraced. Such a program almost always touches OpenTelemetry itself, which
// is why the signal is any go.opentelemetry.io/ import rather than only the
// SDK's.

package tracinglint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	forgeModule     = "github.com/reliant-labs/forge"
	serverkitPath   = forgeModule + "/pkg/serverkit"
	observePath     = forgeModule + "/pkg/observe"
	otelPathPrefix  = "go.opentelemetry.io/"
	internalSegment = "internal"
)

// pkgNode is one package (directory) on disk.
type pkgNode struct {
	path   string // import path
	modRel string // directory relative to its module root, slash-separated
	// main is set when every non-test file is package main. A directory
	// mixing a library with a `//go:build ignore` program is a library.
	main      bool
	imports   map[string]bool
	telemetry bool // may install telemetry (see the file doc)
}

// reach is the per-package answer, keyed by absolute directory.
type reach struct {
	external map[string]bool
	untraced map[string]bool
}

// buildReach reads the import graph under root. A tree without a module path
// yields an empty reach: nothing is exempt.
func buildReach(root, module string) (reach, error) {
	r := reach{external: map[string]bool{}, untraced: map[string]bool{}}
	if module == "" {
		return r, nil
	}
	nodes, modules, err := loadPkgGraph(root)
	if err != nil {
		return r, err
	}
	byPath := make(map[string]string, len(nodes)) // import path → dir
	for dir, n := range nodes {
		byPath[n.path] = dir
	}
	local := func(p string) bool {
		for _, m := range modules {
			if p == m || strings.HasPrefix(p, m+"/") {
				return true
			}
		}
		return false
	}
	// closure returns the dirs reachable from start, and whether it imports
	// a local path that is not on disk.
	closure := func(start []string) (map[string]bool, bool) {
		seen := map[string]bool{}
		unknown := false
		stack := append([]string(nil), start...)
		for len(stack) > 0 {
			dir := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[dir] {
				continue
			}
			seen[dir] = true
			for p := range nodes[dir].imports {
				if next, ok := byPath[p]; ok {
					stack = append(stack, next)
				} else if local(p) {
					unknown = true
				}
			}
		}
		return seen, unknown
	}

	var importable []string
	for dir, n := range nodes {
		if !n.main && !hasInternalSegment(n.modRel) {
			importable = append(importable, dir)
		}
	}
	r.external, _ = closure(importable)

	tracedBy, untracedBy := map[string]bool{}, map[string]bool{}
	for dir, n := range nodes {
		if !n.main {
			continue
		}
		run, unknown := closure([]string{dir})
		traced := unknown
		for d := range run {
			traced = traced || nodes[d].telemetry
		}
		for d := range run {
			if traced {
				tracedBy[d] = true
			} else {
				untracedBy[d] = true
			}
		}
	}
	for dir := range untracedBy {
		if !tracedBy[dir] && !r.external[dir] {
			r.untraced[dir] = true
		}
	}
	return r, nil
}

// hasInternalSegment reports a directory (relative to its module root) under
// an internal/ element, which the go tool lets only its own module import.
// The module path itself is not consulted: an internal element there admits
// other modules.
func hasInternalSegment(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == internalSegment {
			return true
		}
	}
	return false
}

// loadPkgGraph reads the package clause and imports of every non-test Go file
// under root — generated ones, gen/ and nested modules included, since they
// are packages a binary links — keyed by absolute directory, and returns the
// module paths found. It skips what the go tool skips (testdata, ., _
// directories), vendored and node_modules trees, and a nested module whose
// go.mod names no module. A file that does not parse is skipped.
func loadPkgGraph(root string) (map[string]*pkgNode, []string, error) {
	nodes := map[string]*pkgNode{}
	notMain := map[string]bool{}
	modRoots := map[string]string{} // module root dir → module path
	var modules []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "vendor" || name == "testdata" || name == "node_modules" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				m := modulePath(path)
				if m == "" {
					return filepath.SkipDir
				}
				modRoots[path] = m
				modules = append(modules, m)
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		modDir := dir
		for modRoots[modDir] == "" && modDir != root && modDir != filepath.Dir(modDir) {
			modDir = filepath.Dir(modDir)
		}
		module := modRoots[modDir]
		if module == "" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			return nil
		}
		n := nodes[dir]
		if n == nil {
			rel, err := filepath.Rel(modDir, dir)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			n = &pkgNode{path: module, modRel: rel, imports: map[string]bool{}}
			if rel != "." {
				n.path = module + "/" + rel
			}
			nodes[dir] = n
		}
		if file.Name.Name != "main" {
			notMain[dir] = true
		}
		observeName := ""
		for _, spec := range file.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			n.imports[p] = true
			if strings.HasPrefix(p, otelPathPrefix) || p == serverkitPath || strings.HasPrefix(p, serverkitPath+"/") {
				n.telemetry = true
			}
			if p == observePath {
				observeName = "observe"
				if spec.Name != nil {
					observeName = spec.Name.Name
				}
			}
		}
		if !n.telemetry && observeName != "" && observeName != "_" {
			n.telemetry = callsSetup(path, src, observeName)
		}
		return nil
	})
	for dir, n := range nodes {
		n.main = !notMain[dir]
	}
	return nodes, modules, err
}

// callsSetup reports a file that calls <observe>.Setup, observe being the
// local name of forge's pkg/observe (a dot import counts on a bare Setup).
func callsSetup(path string, src []byte, observeName string) bool {
	file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		return true // cannot tell: assume it may
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok && id.Name == observeName && fn.Sel.Name == "Setup" {
				found = true
			}
		case *ast.Ident:
			if observeName == "." && fn.Name == "Setup" {
				found = true
			}
		}
		return !found
	})
	return found
}
