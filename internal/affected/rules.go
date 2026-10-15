package affected

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// selection is the answer to "which tests does this diff reach".
type selection struct {
	// full is non-empty when no subset can judge the diff; it is the reason.
	full     string
	packages []string
	// why says, per selected package, what selected it. claims says, per
	// changed file, what accounted for it.
	why    map[string]string
	claims map[string]string
	// tagged maps a build-tag lane (e2e, integration) to the packages whose
	// tagged tests the diff reaches.
	tagged map[string][]string
}

// selector accumulates what the changed files select.
type selector struct {
	g   *packageGraph
	mod string

	// changed: packages whose code changed — their own tests and every test
	// binary that links them run. own: packages whose own tests run, no more.
	changed map[string][]string
	own     map[string][]string
	// taggedOwn: a build-tagged file changed in the package, per lane.
	taggedOwn map[string]map[string]bool
	claims    map[string]string
}

func (s *selector) markChanged(pkg, cause string) { s.changed[pkg] = append(s.changed[pkg], cause) }
func (s *selector) markOwn(pkg, cause string)     { s.own[pkg] = append(s.own[pkg], cause) }

// selectAffected maps every changed file to what it selects, or to a reason
// the whole suite must run. A file is accounted for by (in order): an ignore
// glob; a rule that claims it (and may also say what it selects); the Go
// package that owns, embeds or holds it as testdata. A file nothing accounts
// for runs everything — nothing proves which tests read it.
func selectAffected(ctx context.Context, in Input, g *packageGraph, tagGraph func() *packageGraph, git func(context.Context, string, ...string) (string, error), changed []string) selection {
	s := &selector{
		g: g, changed: map[string][]string{}, own: map[string][]string{},
		taggedOwn: map[string]map[string]bool{}, claims: map[string]string{},
	}
	if err := validateRules(in.Rules); err != nil {
		return selection{full: err.Error()}
	}
	dirs := packageDirs(g)
	var resolveErr error
	resolve := func(patterns []string) []string {
		var out []string
		for _, p := range patterns {
			m := matchPackages(dirs, p)
			if len(m) == 0 && resolveErr == nil {
				resolveErr = fmt.Errorf("ci.gate.affected_rules names %q, which matches no package of this module", p)
			}
			out = append(out, m...)
		}
		return out
	}

	var readers *readerIndex
	for _, f := range changed {
		if ignoredByGlobs(f, in.Ignore) {
			s.claims[f] = "ci.gate.affected_ignore: no test reads it"
			continue
		}
		var matched []int
		for i := range in.Rules {
			if ruleMatches(in.Rules[i], f) {
				matched = append(matched, i)
			}
		}
		exact := false
		for _, i := range matched {
			if r := in.Rules[i]; r.Full != "" {
				return selection{full: fmt.Sprintf("%s changed: %s", f, r.Full)}
			}
			exact = exact || in.Rules[i].Exact
		}

		var notes []string
		claimed := false
		if o, ok := g.classify(f); ok && !exact {
			claimed = true
			switch o.kind {
			case kindCode:
				s.markChanged(o.pkg, f+" ("+o.how+")")
			case kindTest:
				s.markOwn(o.pkg, f+" ("+o.how+")")
			case kindTagged:
				for _, l := range o.lanes {
					if s.taggedOwn[l] == nil {
						s.taggedOwn[l] = map[string]bool{}
					}
					s.taggedOwn[l][o.pkg] = true
				}
			}
			notes = append(notes, o.how)
		}
		for _, i := range matched {
			r := in.Rules[i]
			for _, p := range resolve(r.Affects) {
				s.markChanged(p, f+" (rule "+ruleName(i, r)+")")
			}
			for _, p := range resolve(r.Tests) {
				if _, ok := g.testDeps[p]; !ok && resolveErr == nil {
					resolveErr = fmt.Errorf("ci.gate.affected_rules (%s) lists %s under tests, and it has no tests", ruleName(i, r), p)
				}
				s.markOwn(p, f+" (rule "+ruleName(i, r)+")")
			}
			if !r.Also {
				claimed = true
			}
			notes = append(notes, "rule "+ruleName(i, r))
		}
		if resolveErr != nil {
			return selection{full: resolveErr.Error()}
		}
		if !claimed {
			return selection{full: fmt.Sprintf("%s changed, and it is not Go source, an embedded file or testdata of a package, and no ci.gate.affected_rules entry claims it — nothing proves which tests read it", f)}
		}
		// A test that names the file reads it: select it as a safety net.
		// Readers never claim a file; they only add to what rules selected.
		if !exact && !strings.HasSuffix(f, ".go") {
			if readers == nil {
				readers = loadReaders(ctx, in.Root, git, g)
			}
			for _, p := range readers.of(f) {
				s.markOwn(p, f+" (its tests name this path)")
			}
		}
		s.claims[f] = strings.Join(notes, "; ")
	}

	sel := selection{claims: s.claims, why: map[string]string{}, tagged: map[string][]string{}}
	rel := func(p string) string { return relPkg(g, p) }
	for pkg, deps := range g.testDeps {
		switch {
		case len(s.changed[pkg]) > 0:
			sel.why[pkg] = firstCause(s.changed[pkg])
		case len(s.own[pkg]) > 0:
			sel.why[pkg] = firstCause(s.own[pkg])
		default:
			var hit []string
			for _, d := range deps {
				if len(s.changed[d]) > 0 {
					hit = append(hit, d)
				}
			}
			if len(hit) == 0 {
				continue
			}
			sort.Strings(hit)
			sel.why[pkg] = fmt.Sprintf("its tests link %s (%s)", rel(hit[0]), firstCause(s.changed[hit[0]]))
		}
		sel.packages = append(sel.packages, pkg)
	}
	sort.Strings(sel.packages)

	// Build-tag lanes: a package's tagged tests run when a tagged file of it
	// changed, or when their test binary (built with the tags) links a
	// package whose code changed.
	for pkg, lanes := range g.taggedLanes {
		for _, lane := range lanes {
			hit := s.taggedOwn[lane][pkg]
			if !hit && len(s.changed) > 0 {
				tg := tagGraph()
				if tg == nil {
					continue
				}
				for _, d := range tg.testDeps[pkg] {
					if len(s.changed[d]) > 0 {
						hit = true
						break
					}
				}
			}
			if hit {
				sel.tagged[lane] = append(sel.tagged[lane], pkg)
			}
		}
	}
	for _, v := range sel.tagged {
		sort.Strings(v)
	}
	return sel
}

func firstCause(c []string) string {
	if len(c) == 0 {
		return ""
	}
	sorted := append([]string(nil), c...)
	sort.Strings(sorted)
	if len(sorted) == 1 {
		return sorted[0]
	}
	return fmt.Sprintf("%s, +%d more", sorted[0], len(sorted)-1)
}

func relPkg(g *packageGraph, pkg string) string {
	for dir, p := range g.dirPkg {
		if p == pkg {
			if r := relDir(g, dir); r != "." {
				return r
			}
			return "."
		}
	}
	return pkg
}

func ruleName(i int, r config.CIAffectedRule) string {
	if len(r.Paths) > 0 {
		return fmt.Sprintf("#%d %s", i+1, r.Paths[0])
	}
	return fmt.Sprintf("#%d", i+1)
}

// validateRules rejects a rule that could not mean anything, so a typo in
// forge.yaml surfaces as a full run with the reason, not as skipped tests.
func validateRules(rules []config.CIAffectedRule) error {
	for i, r := range rules {
		if len(r.Paths) == 0 {
			return fmt.Errorf("ci.gate.affected_rules[%d] has no paths", i)
		}
		if r.Full != "" && (len(r.Affects)+len(r.Tests) > 0 || r.Exact || r.Also) {
			return fmt.Errorf("ci.gate.affected_rules[%d] (%s) sets full together with affects/tests/exact/also; full is the whole decision", i, r.Paths[0])
		}
		if r.Exact && r.Also {
			return fmt.Errorf("ci.gate.affected_rules[%d] (%s) is both exact and also; exact claims the file, also never does", i, r.Paths[0])
		}
	}
	return nil
}

func ruleMatches(r config.CIAffectedRule, f string) bool {
	hit := false
	for _, p := range r.Paths {
		if matchGlob(p, f) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, p := range r.Except {
		if matchGlob(p, f) {
			return false
		}
	}
	return true
}

// matchGlob matches a slash path against a pattern whose segments are
// path.Match patterns; a `**` segment matches zero or more segments.
func matchGlob(pattern, p string) bool {
	return matchSegments(strings.Split(strings.TrimPrefix(pattern, "./"), "/"), strings.Split(p, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// packageDirs maps each module package's directory (project-relative, "." for
// the root) to its import path.
func packageDirs(g *packageGraph) map[string]string {
	out := make(map[string]string, len(g.dirPkg))
	for dir, pkg := range g.dirPkg {
		out[relDir(g, dir)] = pkg
	}
	return out
}

func relDir(g *packageGraph, abs string) string {
	r := strings.TrimPrefix(strings.TrimPrefix(abs, g.root), "/")
	if r == "" {
		return "."
	}
	return r
}

// matchPackages resolves a `./dir` or `./dir/...` pattern to import paths.
func matchPackages(dirs map[string]string, pattern string) []string {
	p := strings.TrimPrefix(pattern, "./")
	var out []string
	switch {
	case p == "...":
		for _, pkg := range dirs {
			out = append(out, pkg)
		}
	case strings.HasSuffix(p, "/..."):
		prefix := strings.TrimSuffix(p, "/...")
		for d, pkg := range dirs {
			if d == prefix || strings.HasPrefix(d, prefix+"/") {
				out = append(out, pkg)
			}
		}
	default:
		if p == "" {
			p = "."
		}
		if pkg, ok := dirs[p]; ok {
			out = append(out, pkg)
		}
	}
	sort.Strings(out)
	return out
}
