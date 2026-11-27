package generator

// The packages a frontend's eslint config imports for its import rules.
//
// The Next.js scaffold's eslint.config.mjs lints imports with
// eslint-plugin-import-x and resolves them with
// eslint-import-resolver-typescript (it used eslint-plugin-import, whose
// no-cycle was ~89% of a fresh project's eslint time). That config is
// upgrade-managed: `forge project upgrade` rewrites an unedited copy to the
// new one. The new config imports two packages the old package.json never
// declared, so without this step the first lint after an upgrade fails with
// ERR_MODULE_NOT_FOUND. `forge generate` therefore declares each of them, at
// the version forge pins (templates.ESLintImportPackages), in any frontend
// whose eslint config imports it.
//
// Additive only: an entry already in dependencies or devDependencies, at any
// version, is the project's and is left alone, and nothing is ever removed
// (an upgraded project keeps its eslint-plugin-import entry until it drops
// it). A frontend whose config imports neither package is not touched.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/templates"
)

// eslintConfigNames are the flat-config file names ESLint 9 looks up in a
// frontend's root.
var eslintConfigNames = []string{
	"eslint.config.mjs", "eslint.config.js", "eslint.config.cjs",
	"eslint.config.ts", "eslint.config.mts", "eslint.config.cts",
}

// moduleSpecifierRe matches the specifier of an ES import (`from "x"`, a bare
// `import "x"`, a dynamic `import("x")`) or a CommonJS `require("x")`.
var moduleSpecifierRe = regexp.MustCompile(`(?:\bfrom|\bimport|\brequire)\s*\(?\s*["']([^"'\s]+)["']`)

// eslintConfigImports returns the set of package names the frontend's eslint
// config files at feDir import. A subpath import (`pkg/sub`, `@s/pkg/sub`)
// counts as its package.
func eslintConfigImports(feDir string) map[string]bool {
	out := map[string]bool{}
	for _, name := range eslintConfigNames {
		b, err := os.ReadFile(filepath.Join(feDir, name))
		if err != nil {
			continue
		}
		for _, m := range moduleSpecifierRe.FindAllStringSubmatch(string(b), -1) {
			out[packageOfSpecifier(m[1])] = true
		}
	}
	return out
}

// packageOfSpecifier is the package a bare module specifier names.
func packageOfSpecifier(spec string) string {
	parts := strings.SplitN(spec, "/", 3)
	if strings.HasPrefix(spec, "@") && len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// missingESLintImportPackages returns, in ESLintImportPackages order, each
// pinned package the frontend's eslint config imports that its manifest text
// declares in neither dependencies nor devDependencies. ok is false when the
// manifest does not parse.
func missingESLintImportPackages(feDir, text string) (missing []templates.ESLintImportPackage, ok bool) {
	imported := eslintConfigImports(feDir)
	if len(imported) == 0 {
		return nil, true
	}
	var doc manifestScopes
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, false
	}
	for _, p := range templates.ESLintImportPackages {
		if !imported[p.Name] {
			continue
		}
		if _, ok := doc.Dependencies[p.Name]; ok {
			continue
		}
		if _, ok := doc.DevDependencies[p.Name]; ok {
			continue
		}
		missing = append(missing, p)
	}
	return missing, true
}

// EnsureESLintImportDependencies declares, in <feRelDir>/package.json's
// devDependencies, each package of templates.ESLintImportPackages that the
// frontend's eslint config imports and the manifest does not declare. It
// prints one line per package it adds and reports whether it rewrote the
// manifest. Surgical like EnsureTelemetryDependencies: one entry inserted per
// package, a run that changes nothing writes nothing, and every failure is a
// warning.
func EnsureESLintImportDependencies(projectDir, feRelDir, feName string) bool {
	feDir := filepath.Join(projectDir, feRelDir)
	pkgPath := filepath.Join(feDir, "package.json")
	info, err := os.Stat(pkgPath)
	if err != nil {
		return false
	}
	original, err := os.ReadFile(pkgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", pkgPath, err)
		return false
	}
	missing, ok := missingESLintImportPackages(feDir, string(original))
	if !ok || len(missing) == 0 {
		return false
	}
	text := string(original)
	// Each insert lands first in the object, so inserting in reverse leaves
	// the new entries in ESLintImportPackages' (alphabetical) order.
	for i := len(missing) - 1; i >= 0; i-- {
		p := missing[i]
		text, err = insertDevDependency(text, p.Name, p.Version)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not declare %s in %s: %v\n", p.Name, pkgPath, err)
			return false
		}
	}
	if !json.Valid([]byte(text)) {
		fmt.Fprintf(os.Stderr, "warning: declaring the eslint import packages in %s would have produced invalid JSON; left unchanged\n", pkgPath)
		return false
	}
	checksums.RecordPreWriteAbs(pkgPath)
	if err := atomicfile.WriteFile(pkgPath, []byte(text), info.Mode().Perm()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", pkgPath, err)
		return false
	}
	for _, p := range missing {
		fmt.Printf("  + frontend %s: declared %s@%s in devDependencies (its eslint config imports it)\n", feName, p.Name, p.Version)
	}
	return true
}

// insertDevDependency adds name@spec to the top-level devDependencies object,
// creating the object when the manifest has none.
func insertDevDependency(text, name, spec string) (string, error) {
	openRe := sectionOpenRe(sectionDevDependencies)
	if openRe.MatchString(text) {
		return insertIntoObject(text, openRe, sectionDevDependencies, name, spec)
	}
	end := strings.LastIndexByte(text, '}')
	if end < 0 {
		return "", fmt.Errorf("no top-level object")
	}
	body := strings.TrimRight(text[:end], " \t\r\n")
	sep := ","
	if strings.HasSuffix(body, "{") {
		sep = "" // an empty manifest object
	}
	obj := "\n  " + jsonString(sectionDevDependencies) + ": {\n    " + jsonString(name) + ": " + jsonString(spec) + "\n  }\n"
	return body + sep + obj + text[end:], nil
}

// eslintImportDependencyNotices is EnsureESLintImportDependencies' read-only
// counterpart for a disowned manifest: one line per package it would declare.
func eslintImportDependencyNotices(feDir, text string) []string {
	missing, _ := missingESLintImportPackages(feDir, text)
	out := make([]string, 0, len(missing))
	for _, p := range missing {
		out = append(out, fmt.Sprintf("the eslint config imports %s, which package.json does not declare — `npm install --save-dev %s@%s`",
			p.Name, p.Name, p.Version))
	}
	return out
}
