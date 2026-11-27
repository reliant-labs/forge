// File: internal/cli/lint/lint_frontend_cache_deps.go
//
// The dependencies the frontend lint lane's pass cache keys by content. The
// install stamps (lint_frontend_cache.go) move on every install, but not when
// someone edits a resolved package in place — a locally patched eslint
// plugin, a `patch-package` fix applied by hand — and that edit changes what
// eslint decides. So the packages eslint actually loads are hashed file by
// file, the way the frontend's own files are:
//
//   - every package the frontend's package.json lists (dependencies,
//     devDependencies) whose name is eslint-shaped (eslintRelevantPackage),
//     plus typescript, which typescript-eslint parses with;
//   - every package the eslint config imports directly (eslint.config.* /
//     .eslintrc.*: `eslint-config-next`, `eslint-plugin-import`, …);
//   - transitively, every eslint-shaped dependency of those packages (a
//     shared config brings its own plugins and resolvers:
//     eslint-config-next → eslint-plugin-react-hooks,
//     eslint-import-resolver-typescript, @next/eslint-plugin-next).
//
// Each name resolves the way Node resolves it — the nearest node_modules/<name>
// walking up from the importer — and through symlinks to the package's real
// directory (a pnpm store entry), whose files are hashed minus its nested
// node_modules. Other packages a plugin requires (espree, minimatch, …) are
// covered by the install stamps only: an in-place edit there is not seen.

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

// lintDependencies resolves the packages eslint loads for the frontend at
// dir (see the file header), and the names that resolved to nothing.
func lintDependencies(dir string) (deps []lintDep, absent []string) {
	type item struct{ name, from string }
	var queue []item
	m := readManifestDeps(dir)
	roots := map[string]bool{"typescript": true}
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
			if !seenAbsent[it.name] {
				seenAbsent[it.name] = true
				absent = append(absent, it.name)
			}
			continue
		}
		if seenDir[real] {
			continue
		}
		seenDir[real] = true
		deps = append(deps, lintDep{name: it.name, dir: real})
		pm := readManifestDeps(real)
		for _, set := range []map[string]string{pm.Dependencies, pm.PeerDependencies, pm.OptionalDependencies} {
			for name := range set {
				if eslintRelevantPackage(name) {
					queue = append(queue, item{name, real})
				}
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
// excluded), and returns the number of files it read. A package a peer or
// optional dependency names but nothing installed is keyed as absent, so
// installing it is a change.
func hashLintDeps(h io.Writer, dir string) (int, error) {
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
