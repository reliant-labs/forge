package lint

// Translating `forge lint --scope` into Go package patterns.
//
// golangci-lint (and go/packages under the contract linter) refuses a set of
// patterns that resolves to no package in the module: "no go files to
// analyze". That is not "nothing to report" — it fails the lane. So a scope
// path contributes a pattern only when a lint pass would actually compile Go
// under it. A `.go` file is not enough evidence: reliant's scripts/ held one
// shell script and one `//go:build ignore` program, the old check counted the
// .go file, and every scripts-only pull request failed golangci-lint.
//
// What no pass compiles, and so contributes no pattern:
//   - a file excluded by its build constraint on every pass (`ignore`, a
//     foreign GOOS/GOARCH, by `//go:build` or by file name);
//   - a nested module's source (a directory with its own go.mod is not this
//     module's package, whatever `./dir/...` looks like);
//   - the trees `go list ./...` itself skips (testdata, vendor, node_modules,
//     `.`- and `_`-prefixed directories).
//
// Two answers, because forge runs more than one golangci-lint pass
// (golangci_build_tags.go). goPackages is every pattern SOME pass compiles —
// the default pass or a per-tag pass — so a directory of integration-tagged
// tests stays in scope for the integration pass. defaultPassPackages is the
// subset the default (untagged) build compiles: what the default pass, the
// typed-config guardrail and the contract linter may be handed. A scope that
// reaches only tagged code skips the default pass and still runs the tagged
// one; a scope that reaches no compiled Go at all leaves the lane vacuous,
// and the engine skips it with a one-line note.

import (
	"go/build"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// goReach is which lint passes compile Go a scope path reaches.
type goReach struct {
	// inDefault: the default pass (host tags plus golangci's run.build-tags).
	inDefault bool
	// inAny: the default pass or one of the per-tag passes.
	inAny bool
}

// goPackages translates the scope into Go package patterns for the lanes
// that take them: `./<dir>/...` for a directory, the containing package for
// a .go file. A path no lint pass compiles Go under (proto/, db/migrations/,
// scripts/ holding only shell and `//go:build ignore` programs) contributes
// nothing, and an empty result means no Go package is in scope.
func (s *lintScope) goPackages() []string {
	return s.packagesWhere(func(r goReach) bool { return r.inAny })
}

// defaultPassPackages is goPackages narrowed to the patterns that reach Go the
// default build compiles on this host (with golangci's run.build-tags) — what
// a lane that does not add per-tag build tags may be handed.
func (s *lintScope) defaultPassPackages() []string {
	return s.packagesWhere(func(r goReach) bool { return r.inDefault })
}

func (s *lintScope) packagesWhere(keep func(goReach) bool) []string {
	if s == nil {
		return nil
	}
	tags := golangciConfigBuildTags(s.root)
	var out []string
	seen := map[string]bool{}
	for _, p := range s.paths {
		abs := filepath.Join(s.root, filepath.FromSlash(p))
		fi, err := os.Stat(abs)
		if err != nil {
			continue
		}
		var (
			pattern string
			reach   goReach
		)
		switch {
		case fi.IsDir():
			pattern = "./" + p + "/..."
			reach = s.scanGo(abs, true, tags)
		case strings.HasSuffix(p, ".go"):
			// A file names its package: golangci-lint lints the package, and
			// the scope filters its findings back to the file.
			pattern = "./" + path.Dir(p)
			if path.Dir(p) == "." {
				pattern = "."
			}
			reach = s.scanGo(filepath.Dir(abs), false, tags)
		default:
			continue
		}
		if keep(reach) && !seen[pattern] {
			seen[pattern] = true
			out = append(out, pattern)
		}
	}
	return out
}

// scanGo reports which passes compile a Go file in dir — under it too when
// recursive — walking as `go list ./dir/...` walks: no nested module, no
// testdata, vendor, node_modules, or `.`/`_`-prefixed directory.
func (s *lintScope) scanGo(dir string, recursive bool, tags []string) goReach {
	var r goReach
	if dir != s.root && hasGoMod(dir) {
		return r // another module: none of this module's packages
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is simply not counted
		}
		if d.IsDir() {
			if p == dir {
				return nil
			}
			name := d.Name()
			if !recursive || name == "node_modules" || name == "vendor" || name == "testdata" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || hasGoMod(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		inDefault, inAny := goFilePasses(filepath.Dir(p), d.Name(), tags)
		r.inDefault = r.inDefault || inDefault
		r.inAny = r.inAny || inAny
		if r.inDefault {
			return fs.SkipAll // the default pass implies every answer
		}
		return nil
	})
	return r
}

// goFilePasses reports whether the default pass compiles the Go file
// dir/name (go/build's own matcher: file-name GOOS/GOARCH suffixes and the
// //go:build line, with tags), and whether any pass does — the default, or
// the pass forge adds for one project-defined tag the file's constraint
// names, which it runs with tags plus that tag (collectGolangciLintJSON).
func goFilePasses(dir, name string, tags []string) (inDefault, inAny bool) {
	ctx := build.Default
	ctx.BuildTags = tags
	if ok, _ := ctx.MatchFile(dir, name); ok {
		return true, true
	}
	expr := goBuildExpr(filepath.Join(dir, name))
	if expr == nil {
		return false, false
	}
	for _, tag := range exprTags(expr) {
		if isToolchainTag(tag) {
			continue // `ignore`, a GOOS, tools.go's pin: forge runs no pass for it
		}
		ctx.BuildTags = append(append([]string{}, tags...), tag)
		if ok, _ := ctx.MatchFile(dir, name); ok {
			return false, true
		}
	}
	return false, false
}

func hasGoMod(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}
