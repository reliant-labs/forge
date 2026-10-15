package affected

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// fakeGit answers the git calls Compute makes.
type fakeGit struct {
	branch    string
	noOrigin  bool
	changed   []string
	untracked []string
	tracked   []string
}

func (f fakeGit) run(_ context.Context, _ string, args ...string) (string, error) {
	switch strings.Join(args[:2], " ") {
	case "symbolic-ref --quiet":
		if len(args) == 3 && args[2] == "refs/remotes/origin/HEAD" {
			if f.noOrigin {
				return "", errors.New("no origin/HEAD")
			}
			return "refs/remotes/origin/main", nil
		}
		return f.branch, nil
	case "rev-parse --verify":
		return "", errors.New("absent")
	case "merge-base HEAD":
		return "0123456789abcdef0123", nil
	case "diff --name-only":
		return strings.Join(f.changed, "\n"), nil
	case "ls-files --others":
		return strings.Join(f.untracked, "\n"), nil
	case "ls-files --cached":
		return strings.Join(f.tracked, "\n"), nil
	}
	return "", errors.New("unexpected git " + strings.Join(args, " "))
}

// module is the synthetic project every table test runs over:
//
//	a        embeds tmpl/x.tmpl; its tests embed golden.txt; has an e2e-tagged test
//	b        imports a
//	c        alone; e2e-tagged test links b
//	nested/d no tests
//	kcl      embeds k.k, no tests of its own
//	render   tests link kcl (the "render" package)
//	tpl      embeds skills/s.md; imported by gen and cli
//	gen,cli  import tpl
//	guard    tests read every file (rule paths ** + also)
//	readme   tests name docs/guide.md in a path literal
//	cli_test imports nothing but is named by a rule
type module struct {
	root string
}

func newModule(t *testing.T) module {
	t.Helper()
	root := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("a/a_e2e_test.go", "//go:build e2e\n\npackage a\n")
	put("c/c_e2e_test.go", "//go:build e2e\n\npackage c\n")
	put("readme/readme_test.go", "package readme\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n)\n\nfunc root() string { _, _ = os.Getwd(); return \"..\" }\nvar _ = filepath.Join(root(), \"docs\", \"guide.md\")\n")
	return module{root: root}
}

func (m module) listed(tagged bool) func(context.Context, string, []string) (io.Reader, error) {
	d := func(rel string) string { return filepath.Join(m.root, rel) }
	ex := "ex.com/m/"
	pkgs := []listedPackage{
		{ImportPath: "fmt", Standard: true, Dir: "/goroot/src/fmt"},
		{ImportPath: ex + "a", Name: "a", Dir: d("a"), EmbedFiles: []string{"tmpl/x.tmpl"}, EmbedPatterns: []string{"tmpl/*"}, IgnoredGoFiles: []string{"a_e2e_test.go"}},
		{ImportPath: ex + "b", Name: "b", Dir: d("b"), Deps: []string{ex + "a"}},
		{ImportPath: ex + "c", Name: "c", Dir: d("c"), IgnoredGoFiles: []string{"c_e2e_test.go"}},
		{ImportPath: ex + "nested/d", Name: "d", Dir: d("nested/d")},
		{ImportPath: ex + "kcl", Name: "kcl", Dir: d("kcl"), EmbedFiles: []string{"k.k"}, EmbedPatterns: []string{"*.k"}},
		{ImportPath: ex + "render", Name: "render", Dir: d("render"), Deps: []string{ex + "kcl"}},
		{ImportPath: ex + "tpl", Name: "tpl", Dir: d("tpl"), EmbedFiles: []string{"skills/s.md"}, EmbedPatterns: []string{"skills/*"}},
		{ImportPath: ex + "gen", Name: "gen", Dir: d("gen"), Deps: []string{ex + "tpl"}},
		{ImportPath: ex + "cli", Name: "cli", Dir: d("cli"), Deps: []string{ex + "tpl", ex + "gen"}},
		{ImportPath: ex + "guard", Name: "guard", Dir: d("guard")},
		{ImportPath: ex + "readme", Name: "readme", Dir: d("readme")},
		{ImportPath: ex + "a [" + ex + "a.test]", Name: "a", Dir: d("a"), ForTest: ex + "a", TestEmbedFiles: []string{"golden.txt"}},
		{ImportPath: ex + "a.test", Name: "main", Dir: d("a"), Deps: []string{ex + "a [" + ex + "a.test]", "fmt"}},
		{ImportPath: ex + "b.test", Name: "main", Dir: d("b"), Deps: []string{ex + "a", ex + "b [" + ex + "b.test]"}},
		{ImportPath: ex + "c.test", Name: "main", Dir: d("c"), Deps: []string{ex + "c [" + ex + "c.test]"}},
		{ImportPath: ex + "render.test", Name: "main", Dir: d("render"), Deps: []string{ex + "kcl", ex + "render [" + ex + "render.test]"}},
		{ImportPath: ex + "gen.test", Name: "main", Dir: d("gen"), Deps: []string{ex + "tpl", ex + "gen [" + ex + "gen.test]"}},
		{ImportPath: ex + "cli.test", Name: "main", Dir: d("cli"), Deps: []string{ex + "tpl", ex + "gen", ex + "cli [" + ex + "cli.test]"}},
		{ImportPath: ex + "guard.test", Name: "main", Dir: d("guard"), Deps: []string{ex + "guard [" + ex + "guard.test]"}},
		{ImportPath: ex + "readme.test", Name: "main", Dir: d("readme"), Deps: []string{ex + "readme [" + ex + "readme.test]"}},
		// A dependency outside the module (module cache) is never a candidate.
		{ImportPath: "other.com/x", Name: "x", Dir: "/gomodcache/other.com/x"},
		{ImportPath: "other.com/x.test", Name: "main", Dir: "/gomodcache/other.com/x", Deps: []string{ex + "a"}},
	}
	if tagged {
		// With -tags e2e the c test binary also links b (its e2e test imports it).
		for i := range pkgs {
			if pkgs[i].ImportPath == ex+"c.test" {
				pkgs[i].Deps = append(pkgs[i].Deps, ex+"b", ex+"a")
			}
		}
	}
	return func(context.Context, string, []string) (io.Reader, error) {
		pr, pw := io.Pipe()
		go func() {
			enc := json.NewEncoder(pw)
			for _, p := range pkgs {
				_ = enc.Encode(p)
			}
			_ = pw.Close()
		}()
		return pr, nil
	}
}

var testRules = []config.CIAffectedRule{
	{Paths: []string{"go.mod", "go.sum", "Taskfile.yml"}, Full: "a pin or the task definitions changed"},
	{Paths: []string{"kcl/tests/**"}, Affects: []string{"./kcl"}, Tests: []string{"./render"}},
	{Paths: []string{"docs/**", "**/*.md"}, Except: []string{"tpl/skills/**"}},
	{Paths: []string{"tpl/skills/**"}, Exact: true, Tests: []string{"./gen"}},
	{Paths: []string{"**"}, Also: true, Tests: []string{"./guard"}},
}

func (m module) compute(t *testing.T, g fakeGit, rules []config.CIAffectedRule, ignore ...string) Result {
	t.Helper()
	if g.branch == "" {
		g.branch = "feature"
	}
	return Compute(context.Background(), Input{
		Root: m.root, Git: g.run, GoList: m.listed(false), GoListTagged: m.listed(true), Ignore: ignore, Rules: rules,
	})
}

func pkgs(a Result) string {
	short := make([]string, 0, len(a.Packages))
	for _, p := range a.Packages {
		short = append(short, strings.TrimPrefix(p, "ex.com/m/"))
	}
	sort.Strings(short)
	return strings.Join(short, " ")
}

func TestSelectsWhatTheChangeReaches(t *testing.T) {
	m := newModule(t)
	for _, tc := range []struct {
		name    string
		changed []string
		want    string
	}{
		{"source of a dependency reaches its importers' tests", []string{"a/a.go"}, "a b guard"},
		{"a leaf package is tested alone", []string{"b/b.go"}, "b guard"},
		{"an embedded file is its package's source", []string{"a/tmpl/x.tmpl"}, "a b guard"},
		{"a test-only embed reaches only that package's tests", []string{"a/golden.txt"}, "a guard"},
		{"a package's own testdata reaches only that package", []string{"c/testdata/in/case.json"}, "c guard"},
		{"a test file reaches only its package", []string{"a/a_test.go"}, "a guard"},
		{"a package with no tests reaches no test binary", []string{"nested/d/d.go"}, "guard"},
		{"a deleted file in a package that embeds files is an embed change", []string{"a/tmpl/gone.tmpl"}, "a b guard"},
		{"a tpl change reaches everything linking tpl", []string{"tpl/tpl.go"}, "cli gen guard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := m.compute(t, fakeGit{changed: tc.changed}, testRules)
			if a.Full {
				t.Fatalf("fell back to the full suite: %s", a.Reason)
			}
			if got := pkgs(a); got != tc.want {
				t.Fatalf("packages = %q, want %q", got, tc.want)
			}
			if a.Total != 8 {
				t.Errorf("Total = %d, want the module's 8 test packages", a.Total)
			}
			for _, p := range a.Packages {
				if a.Why[p] == "" {
					t.Errorf("%s is selected with no reason", p)
				}
			}
		})
	}
}

// The rule table: what each kind of rule does to a non-Go path.
func TestRules(t *testing.T) {
	m := newModule(t)
	for _, tc := range []struct {
		name    string
		changed []string
		want    string
	}{
		{"affects selects the package and its importers' tests, tests selects readers", []string{"kcl/tests/x.k"}, "guard render"},
		{"affects on a rule-only path (no Go package owns the file)", []string{"kcl/tests/deep/y.k"}, "guard render"},
		{"a doc is claimed and selects only what names it", []string{"docs/other.md"}, "guard"},
		{"a doc a test names in a path literal selects that test", []string{"docs/guide.md"}, "guard readme"},
		{"exact: a skill is read by gen alone, not by everything linking tpl", []string{"tpl/skills/s.md"}, "gen guard"},
		{"a README next to code is claimed by the **/*.md rule", []string{"b/README.md"}, "guard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := fakeGit{changed: tc.changed, tracked: []string{"docs/guide.md", "docs/other.md"}}
			a := m.compute(t, g, testRules)
			if a.Full {
				t.Fatalf("fell back to the full suite: %s", a.Reason)
			}
			if got := pkgs(a); got != tc.want {
				t.Fatalf("packages = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFallsBackToTheFullSuite(t *testing.T) {
	m := newModule(t)
	for _, tc := range []struct {
		name  string
		g     fakeGit
		rules []config.CIAffectedRule
		why   string
	}{
		{"go.mod is a pin", fakeGit{changed: []string{"a/a.go", "go.mod"}}, testRules, "a pin or the task definitions changed"},
		{"go.sum is a pin", fakeGit{changed: []string{"go.sum"}}, testRules, "go.sum changed"},
		{"the Taskfile defines the lanes", fakeGit{changed: []string{"Taskfile.yml"}}, testRules, "Taskfile.yml changed"},
		{"a file nothing claims", fakeGit{changed: []string{"scripts/x.sh"}}, testRules, "scripts/x.sh changed"},
		{"a path only an also-rule matches stays unclaimed", fakeGit{changed: []string{"weird.bin"}}, testRules, "weird.bin changed"},
		{"an untracked unclaimed file", fakeGit{untracked: []string{"notes.txt"}}, testRules, "notes.txt changed"},
		{"Go source in no package (a deleted package)", fakeGit{changed: []string{"gone/gone.go"}}, testRules, "gone/gone.go changed"},
		{"with no rules at all a doc is unclaimed", fakeGit{changed: []string{"docs/x.md"}}, nil, "docs/x.md changed"},
		{"a rule naming no package is a config error, not skipped tests", fakeGit{changed: []string{"kcl/tests/x.k"}}, []config.CIAffectedRule{{Paths: []string{"kcl/**"}, Affects: []string{"./nope"}}}, "matches no package"},
		{"a rule with no paths", fakeGit{changed: []string{"a/a.go"}}, []config.CIAffectedRule{{Affects: []string{"./a"}}}, "has no paths"},
		{"the default branch", fakeGit{branch: "main", changed: []string{"a/a.go"}}, testRules, "on the default branch"},
		{"nothing changed", fakeGit{branch: "feature"}, testRules, "nothing changed since origin/main"},
		{"no origin", fakeGit{noOrigin: true}, testRules, "no origin/<default branch>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := m.compute(t, tc.g, tc.rules)
			if !a.Full || !strings.Contains(a.Reason, tc.why) {
				t.Fatalf("Full=%v Reason=%q, want the full suite because %q", a.Full, a.Reason, tc.why)
			}
			if !strings.HasPrefix(a.Line(), "unit tests: full suite — ") {
				t.Errorf("Line = %q", a.Line())
			}
		})
	}
	forced := Compute(context.Background(), Input{Root: m.root, Force: "CI_FULL=1", Git: fakeGit{branch: "f", changed: []string{"a/a.go"}}.run, GoList: m.listed(false)})
	if !forced.Full || forced.Reason != "CI_FULL=1" {
		t.Errorf("forced: %+v", forced)
	}
}

// affected_ignore names files no Go test reads; a change to one keeps the
// branch in affected mode without consulting any test.
func TestIgnoreGlobs(t *testing.T) {
	m := newModule(t)
	a := m.compute(t, fakeGit{changed: []string{"NOTES.md", "docs/pic.png", "b/b.go"}}, nil, "*.md", "docs/**")
	if a.Full || pkgs(a) != "b" {
		t.Fatalf("Full=%v (%s) packages=%q", a.Full, a.Reason, pkgs(a))
	}
	if !ignoredByGlobs("deploy/kcl/dev/main.k", []string{"deploy/kcl/*/main.k"}) || ignoredByGlobs("deploy/kcl/main.k", []string{"deploy/kcl/*/main.k"}) {
		t.Error("a glob with a slash matches the whole path")
	}
}

// A replay judges a file list instead of the work tree.
func TestFilesOverrideGit(t *testing.T) {
	m := newModule(t)
	a := Compute(context.Background(), Input{
		Root: m.root, Files: []string{"b/b.go", "docs/guide.md"}, Rules: testRules,
		Git: fakeGit{tracked: []string{"docs/guide.md"}}.run, GoList: m.listed(false), GoListTagged: m.listed(true),
	})
	if a.Full || pkgs(a) != "b guard readme" {
		t.Fatalf("Full=%v (%s) packages=%q", a.Full, a.Reason, pkgs(a))
	}
}

// Build-tagged tests are another lane: a change reaches them through the
// tagged test binary's links, not the unit lane's.
func TestTaggedLane(t *testing.T) {
	m := newModule(t)
	a := m.compute(t, fakeGit{changed: []string{"b/b.go"}}, testRules)
	if got := strings.Join(a.Tagged["e2e"], " "); got != "ex.com/m/c" {
		t.Errorf("e2e packages after changing b = %q, want c (its e2e test links b)", got)
	}
	a = m.compute(t, fakeGit{changed: []string{"a/a_e2e_test.go"}}, testRules)
	if got := strings.Join(a.Tagged["e2e"], " "); !strings.Contains(got, "ex.com/m/a") {
		t.Errorf("e2e packages after changing a's e2e test = %q, want a", got)
	}
	if pkgs(a) != "guard" {
		t.Errorf("a tagged file selects no unit test beyond the guard; got %q", pkgs(a))
	}
	a = m.compute(t, fakeGit{changed: []string{"nested/d/d.go"}}, testRules)
	if len(a.Tagged["e2e"]) != 0 {
		t.Errorf("e2e = %v for a package nothing links", a.Tagged["e2e"])
	}
}

func TestValidateRules(t *testing.T) {
	for name, r := range map[string]config.CIAffectedRule{
		"full with affects":   {Paths: []string{"x"}, Full: "why", Affects: []string{"./a"}},
		"exact and also":      {Paths: []string{"x"}, Exact: true, Also: true},
		"full with exact":     {Paths: []string{"x"}, Full: "why", Exact: true},
		"no paths at all":     {},
		"full with tests too": {Paths: []string{"x"}, Full: "why", Tests: []string{"./a"}},
	} {
		if err := validateRules([]config.CIAffectedRule{r}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := validateRules(testRules); err != nil {
		t.Errorf("the test rules are valid: %v", err)
	}
}

func TestMatchGlob(t *testing.T) {
	for _, tc := range []struct {
		pat, p string
		want   bool
	}{
		{"kcl/**", "kcl/a/b.k", true},
		{"kcl/**", "kcl", true},
		{"kcl/**", "kclx/a", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "a/b/c.mdx", false},
		{"**", "any/thing", true},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/d/c", false},
		{"./go.mod", "go.mod", true},
	} {
		if got := matchGlob(tc.pat, tc.p); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pat, tc.p, got, tc.want)
		}
	}
}

func TestMatchPackages(t *testing.T) {
	dirs := map[string]string{".": "ex.com/m", "kcl": "ex.com/m/kcl", "internal/cli": "ex.com/m/internal/cli", "internal/cli/lint": "ex.com/m/internal/cli/lint", "internal/clix": "ex.com/m/internal/clix"}
	for pat, want := range map[string]string{
		"./kcl":              "ex.com/m/kcl",
		"./internal/cli/...": "ex.com/m/internal/cli ex.com/m/internal/cli/lint",
		"./...":              "ex.com/m ex.com/m/internal/cli ex.com/m/internal/cli/lint ex.com/m/internal/clix ex.com/m/kcl",
		"./nope":             "",
	} {
		if got := strings.Join(matchPackages(dirs, pat), " "); got != want {
			t.Errorf("matchPackages(%q) = %q, want %q", pat, got, want)
		}
	}
}

// The real thing, once: git and `go list` over a small module, on a branch.
func TestRealModule(t *testing.T) {
	if testing.Short() {
		t.Skip("runs git and `go list -deps -test` subprocesses; runs in task test")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	for rel, body := range map[string]string{
		"go.mod":      "module ex.com/m\n\ngo 1.22\n",
		"a/a.go":      "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { _ = A() }\n",
		"b/b.go":      "package b\n\nimport \"ex.com/m/a\"\n\nfunc B() int { return a.A() }\n",
		"b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { _ = B() }\n",
		"c/c.go":      "package c\n",
		"c/c_test.go": "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n",
	} {
		write(rel, body)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	run("checkout", "-q", "-b", "feature")
	write("a/a.go", "package a\n\nfunc A() int { return 2 }\n")
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	a := Compute(context.Background(), Input{Root: root, Env: env})
	if a.Full {
		t.Fatalf("full suite: %s", a.Reason)
	}
	if got := strings.Join(a.Packages, " "); got != "ex.com/m/a ex.com/m/b" || a.Total != 3 {
		t.Fatalf("packages %q of %d, want a and b of 3", got, a.Total)
	}
	if !strings.Contains(a.Line(), "2 of 3 test packages") {
		t.Errorf("Line = %q", a.Line())
	}
}
