// File: internal/cli/lint/lint_frontend_cache_deps.go
//
// The dependencies the frontend lint lane's pass cache keys by content. The
// install stamps (lint_frontend_cache.go) move on every install, but not when
// someone edits a resolved package in place — a locally patched eslint
// plugin, a `patch-package` fix applied by hand, a debugging edit to espree —
// and that edit changes what eslint decides. So the complete runtime
// dependency closure of what eslint loads is hashed file by file:
//
//   - the roots: eslint itself, typescript (typescript-eslint parses with
//     it), every package the eslint config imports directly
//     (eslint.config.* / .eslintrc.*), and every package the frontend's
//     package.json lists (dependencies, devDependencies) whose name is
//     eslint-shaped (eslintRelevantPackage);
//   - from each package in the closure, every name its package.json lists
//     under dependencies, optionalDependencies and peerDependencies, each
//     resolved the way Node resolves it from that package's real directory
//     (the nearest node_modules/<name> walking up, symlinks followed — a
//     pnpm store entry's dependencies are its siblings), deduplicated by
//     real path. A name that resolves to nothing (an optional or peer
//     dependency not installed) is keyed as absent, so installing it is a
//     change.
//
// Every file of every package in the closure is hashed, minus its nested
// node_modules: a dependency installed there is visited as its own package.

package lint

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// eslintRelevantPackage reports whether a package name is one eslint loads
// as configuration, a plugin, a parser or a resolver.
func eslintRelevantPackage(name string) bool {
	scope, base := "", name
	if strings.HasPrefix(name, "@") {
		if i := strings.Index(name, "/"); i > 0 {
			scope, base = name[:i], name[i+1:]
		}
	}
	switch {
	case name == "typescript", name == "typescript-eslint":
		return true
	case scope == "@eslint", scope == "@typescript-eslint":
		return true
	case strings.HasPrefix(name, "eslint"):
		return true
	case strings.Contains(base, "eslint-plugin"), strings.Contains(base, "eslint-config"):
		return true
	}
	return false
}

// eslintConfigFiles are the config file names eslint reads, flat and legacy.
var eslintConfigFiles = []string{
	"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs",
	"eslint.config.ts", "eslint.config.mts", "eslint.config.cts",
	".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yaml", ".eslintrc.yml", ".eslintrc",
}

// configImportRe finds module specifiers in a config: `from "x"`,
// `import "x"`, `import("x")`, `require("x")`, and the quoted strings of a
// legacy config's extends/plugins/parser (bare names only survive the
// filter in configImports).
var configImportRe = regexp.MustCompile(`(?:\bfrom\s*|\bimport\s*\(?\s*|\brequire\s*\(\s*)["']([^"']+)["']`)

// configImports are the package names dir's eslint config imports.
func configImports(dir string) []string {
	var out []string
	for _, f := range eslintConfigFiles {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		for _, m := range configImportRe.FindAllStringSubmatch(string(data), -1) {
			if name := packageNameOf(m[1]); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// packageNameOf is the package a bare module specifier names ("" for a
// relative, absolute or node: specifier): "eslint-config-next/typescript" is
// eslint-config-next, "@eslint/js" is @eslint/js.
func packageNameOf(spec string) string {
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || strings.Contains(spec, ":") {
		return ""
	}
	parts := strings.Split(spec, "/")
	if strings.HasPrefix(spec, "@") {
		if len(parts) < 2 || parts[1] == "" {
			return ""
		}
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// manifestDeps is the part of a package.json the dependency walk reads.
type manifestDeps struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

func readManifestDeps(dir string) manifestDeps {
	var m manifestDeps
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

// resolvePackage finds name the way Node does from dir — the nearest
// node_modules/<name> in dir or an ancestor — and returns its real
// directory (symlinks followed). ok is false when nothing resolves.
func resolvePackage(name, dir string) (string, bool) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		cand := filepath.Join(d, "node_modules", filepath.FromSlash(name))
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			real, err := filepath.EvalSymlinks(cand)
			if err != nil {
				return "", false
			}
			return real, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// lintDep is one resolved package the key covers.
type lintDep struct{ name, dir string }

// lintDependencies resolves the runtime dependency closure of what eslint
// loads for the frontend at dir (see the file header), and the names that
// resolved to nothing, each as "<name> (from <importer>)".
func lintDependencies(dir string) (deps []lintDep, absent []string) {
	type item struct{ name, from string }
	var queue []item
	m := readManifestDeps(dir)
	roots := map[string]bool{"eslint": true, "typescript": true}
	for _, set := range []map[string]string{m.Dependencies, m.DevDependencies} {
		for name := range set {
			if eslintRelevantPackage(name) {
				roots[name] = true
			}
		}
	}
	for _, name := range configImports(dir) {
		roots[name] = true
	}
	for name := range roots {
		queue = append(queue, item{name, dir})
	}

	seenDir := map[string]bool{}
	seenAbsent := map[string]bool{}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		real, ok := resolvePackage(it.name, it.from)
		if !ok {
			from := "the frontend"
			if it.from != dir {
				from = filepath.Base(it.from)
			}
			if key := it.name + " (from " + from + ")"; !seenAbsent[key] {
				seenAbsent[key] = true
				absent = append(absent, key)
			}
			continue
		}
		if seenDir[real] {
			continue
		}
		seenDir[real] = true
		deps = append(deps, lintDep{name: it.name, dir: real})
		pm := readManifestDeps(real)
		for _, set := range []map[string]string{pm.Dependencies, pm.OptionalDependencies, pm.PeerDependencies} {
			for name := range set {
				queue = append(queue, item{name, real})
			}
		}
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].name != deps[j].name {
			return deps[i].name < deps[j].name
		}
		return deps[i].dir < deps[j].dir
	})
	sort.Strings(absent)
	return deps, absent
}

// hashLintDeps adds every package lintDependencies resolves to h, by name,
// real directory and the content of each file under it (nested node_modules
// excluded: those are packages of the closure in their own right), and
// returns the number of files it read. A name nothing installed is keyed as
// absent, so installing it is a change.
func hashLintDeps(h io.Writer, dir string) (int, error) {
	// A local config import can lead outside the hashed trees, directly or
	// through further imports. Do not emulate Node's module resolution here:
	// conservatively leave these configs uncached, even for an in-tree import.
	for _, f := range eslintConfigFiles {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, m := range configImportRe.FindAllStringSubmatch(string(data), -1) {
			if strings.HasPrefix(m[1], ".") || strings.HasPrefix(m[1], "/") {
				return 0, fmt.Errorf("lint cache: %s imports local config %q", f, m[1])
			}
		}
	}
	deps, absent := lintDependencies(dir)
	files := 0
	for _, name := range absent {
		fmt.Fprintf(h, "dep-absent %s\n", name)
	}
	for _, d := range deps {
		fmt.Fprintf(h, "dep %s %s\n", d.name, filepath.ToSlash(d.dir))
		n, err := hashPackageTree(h, "dep:"+d.name, d.dir)
		if err != nil {
			return 0, err
		}
		files += n
	}
	return files, nil
}

// hashPackageTree hashes an installed package: every regular file (and
// directory) under dir by relative path and content, minus nested
// node_modules. Unlike hashTree it keeps dist/, out/ and the like, which is
// where a package's code lives.
func hashPackageTree(h io.Writer, label, dir string) (int, error) {
	files := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != dir && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			fmt.Fprintf(h, "dir %s:%s\n", label, rel)
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "link %s:%s -> %s\n", label, rel, target)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files++
		return hashFile(h, label+":"+rel, p)
	})
	return files, err
}
