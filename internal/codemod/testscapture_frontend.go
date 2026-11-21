package codemod

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The frontend half of tests-capture: each frontends/<name> that runs
// Vitest. Its config writes results and v8 coverage into the capture lane's
// directory when — and only when — FORGE_TEST_LANE_DIR is set, so a bare
// `npm test` is unchanged; @vitest/coverage-v8 is the provider that needs.

var (
	pkgVitestRe   = regexp.MustCompile(`(?m)^([ \t]*)"vitest":[ \t]*"([^"]+)"(,?)[ \t]*$`)
	tsTestBlockRe = regexp.MustCompile(`(?m)^( *)test:\s*\{[ \t]*$`)
	tsExportRe    = regexp.MustCompile(`(?m)^export default defineConfig\(`)
	tsPeersRe     = regexp.MustCompile(`(?m)^const webRuntimePeers\b.*;\n\n`)
	tsNamedJoinRe = regexp.MustCompile(`import\s*\{[^}]*\bjoin\b[^}]*\}\s*from\s*["']node:path["']`)
	tsPathRe      = regexp.MustCompile(`import\s+path\s+from\s*["']node:path["']`)
)

func (r *tcRun) frontends() {
	for _, fe := range tcSortedDirs(r.dir, "frontends/*") {
		pkgRel := fe + "/package.json"
		pkg, ok := r.read(pkgRel)
		if !ok {
			continue
		}
		m := pkgVitestRe.FindStringSubmatchIndex(pkg)
		if m == nil {
			continue
		}
		r.vitestConfig(fe)
		if strings.Contains(pkg, `"@vitest/coverage-v8"`) {
			continue
		}
		indent, spec, comma := pkg[m[2]:m[3]], pkg[m[4]:m[5]], pkg[m[6]:m[7]]
		entry := indent + `"@vitest/coverage-v8": "` + spec + `"`
		var next string
		if comma == "," {
			next = pkg[:m[1]] + "\n" + entry + "," + pkg[m[1]:]
		} else {
			next = pkg[:m[6]] + "," + pkg[m[6]:m[1]] + "\n" + entry + pkg[m[1]:]
		}
		r.set(pkgRel, next, "@vitest/coverage-v8 "+spec+" (vitest's own range: it peers on the exact vitest release)")
		pm := "npm install"
		if _, err := os.Stat(filepath.Join(r.dir, filepath.FromSlash(fe), "pnpm-lock.yaml")); err == nil {
			pm = "pnpm install"
		}
		r.next("install %s's new devDependency: `%s` in %s (forge project upgrade's locks phase does it), and commit the lockfile", fe, pm, fe)
	}
}

// vitestConfig adds the lane block to a frontend's Vitest config: the
// Next.js scaffold's vitest.config.ts, or the Vite SPA's vite.config.ts.
func (r *tcRun) vitestConfig(fe string) {
	variant := "next"
	rel := fe + "/vitest.config.ts"
	src, ok := r.read(rel)
	if !ok {
		variant = "vite"
		rel = fe + "/vite.config.ts"
		if src, ok = r.read(rel); !ok {
			return
		}
	}
	if strings.Contains(src, "FORGE_TEST_LANE_DIR") {
		return
	}
	skip := func(why string) {
		r.next("%s: %s, so Vitest records no results or coverage for this frontend's lane. "+
			"Add the lane block by hand: forge skill load migrations/v1.9.0/tests, section \"Frontend lanes\"", rel, why)
	}
	// The block joins paths with node:path's join, by whichever name the
	// config imports it.
	joinFn := ""
	switch {
	case variant == "next" && tsNamedJoinRe.MatchString(src), variant == "vite" && !tsPathRe.MatchString(src) && tsNamedJoinRe.MatchString(src):
		joinFn = "join("
	case tsPathRe.MatchString(src):
		joinFn = "path.join("
	}
	if joinFn == "" {
		skip("it imports neither `join` nor `path` from node:path")
		return
	}
	exp := tsExportRe.FindStringIndex(src)
	tests := tsTestBlockRe.FindAllStringSubmatchIndex(src, -1)
	if exp == nil || len(tests) != 1 {
		skip("it has no `export default defineConfig(` with one `test: {` block")
		return
	}
	t := tests[0]
	testIndent := t[3] - t[2]
	closing := "\n" + strings.Repeat(" ", testIndent) + "}"
	end := strings.Index(src[t[1]:], closing)
	if end < 0 {
		skip("its `test: {` block does not close at its own indent")
		return
	}
	end += t[1] + 1 // the closing line's start
	before := strings.TrimRight(src[:end], " \t\n")
	if !strings.HasSuffix(before, ",") {
		skip("the last entry of its `test` block has no trailing comma")
		return
	}

	spread := tcBlock("vitest_" + variant + "_spread")
	constBlock := tcBlock("vitest_" + variant + "_const")
	spreadIndent := 4
	if variant == "vite" {
		spreadIndent = 6
	}
	spread = tcReindent(spread, spreadIndent, testIndent+2)
	if joinFn == "path.join(" {
		spread = strings.ReplaceAll(spread, " join(", " path.join(")
	} else {
		spread = strings.ReplaceAll(spread, "path.join(", "join(")
	}

	next := src[:end] + spread + src[end:]
	if variant == "vite" {
		comment := tcReindent(tcBlock("vitest_vite_comment"), 4, testIndent)
		if !tcLineBeforeHas(next, t[0], "//") {
			comment = strings.TrimPrefix(comment, strings.Repeat(" ", testIndent)+"//\n")
		}
		next = next[:t[0]] + comment + next[t[0]:]
	}
	// The const sits where a new scaffold has it: after the web runtime's
	// peers, else above the config and the comment that heads it.
	at := -1
	if p := tsPeersRe.FindStringIndex(next); len(p) == 2 && p[1] <= exp[0] {
		at = p[1]
	}
	if at < 0 {
		at = exp[0]
		for at > 0 {
			prev := strings.LastIndexByte(next[:at-1], '\n') + 1
			if !strings.HasPrefix(strings.TrimSpace(next[prev:at-1]), "//") {
				break
			}
			at = prev
		}
	}
	next = next[:at] + constBlock + "\n" + next[at:]
	r.set(rel, next, "Vitest writes its results and v8 coverage into the capture lane's directory when FORGE_TEST_LANE_DIR is set (a bare test run is unchanged)")
}
