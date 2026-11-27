// The validate step fails a run for compile errors the run caused, not for
// ones that were already there. In a checkout several agents share, one
// agent's half-edited package rolled back every other agent's generate
// (Ranger friction #16): the compile check built the whole module and
// treated any error anywhere as the run's.
package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
)

// scopeFixture is a module whose "run" rewrote a/a.go: before the run it
// said preA, now it says the fixture's a/a.go.
type scopeFixture struct {
	dir   string
	scope *generateWriteScope
}

const preA = "package a\n\nfunc F() int { return 1 }\n"

func newScopeFixture(t *testing.T, files map[string]string) scopeFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("compiles a temp module with go list; runs in task test")
	}
	all := map[string]string{
		"go.mod": "module example.com/scope\n\ngo 1.22\n",
		// What the run wrote: F's signature is unchanged unless a case
		// overrides it.
		"a/a.go": "package a\n\nfunc F() int { return 2 }\n",
	}
	for k, v := range files {
		all[k] = v
	}
	dir := writeValidateFixture(t, all)
	// writeValidateFixture adds a root package; it compiles.
	return scopeFixture{dir: dir, scope: &generateWriteScope{
		written: []string{"a/a.go"},
		preRun: func(rel string) (checksums.PreRunState, bool) {
			if rel == "a/a.go" {
				return checksums.PreRunState{Existed: true, Content: []byte(preA)}, true
			}
			return checksums.PreRunState{}, false
		},
	}}
}

func (f scopeFixture) attribute(t *testing.T) (*foreignCompile, bool) {
	t.Helper()
	rep, pkgs, err := compileTreePackages(f.dir, true, nil, nil)
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	if rep.Build == "" {
		t.Fatal("fixture compiles; every case here has a compile error")
	}
	return f.scope.attribute(f.dir, pkgs)
}

const brokenB = "package b\n\nvar X int = \"half-edited\"\n"

// The Ranger case: another agent's package is mid-edit, and it imports
// nothing this run wrote. Its error is not the run's.
func TestValidateScope_ErrorInAPackageTheRunDidNotWriteIsForeign(t *testing.T) {
	f := newScopeFixture(t, map[string]string{"b/b.go": brokenB})
	fc, ok := f.attribute(t)
	if !ok {
		t.Fatal("a pre-existing error in a package the run never touched was attributed to the run")
	}
	if !slices.Equal(fc.packages, []string{"example.com/scope/b"}) || fc.errors != 1 {
		t.Errorf("foreign = %+v, want the one error in example.com/scope/b", fc)
	}
	if !strings.Contains(fc.output, `b/b.go:3:13: cannot use "half-edited"`) {
		t.Errorf("the foreign errors are not reported as go build prints them:\n%s", fc.output)
	}
	if len(fc.unverified) != 0 {
		t.Errorf("nothing the run wrote imports b, yet unverified = %v", fc.unverified)
	}

	// And the validate step passes over it, with the caveat that keeps
	// the closing line off a green check and the run off the memo.
	caveats, err := checkGoBuildScoped(f.dir, f.scope)
	if err != nil {
		t.Fatalf("the compile check failed the run over another package's error: %v", err)
	}
	if caveats.ForeignErrors != 1 || caveats.ForeignPackages != 1 || !caveats.any() {
		t.Errorf("caveats = %+v, want 1 foreign error in 1 package, counted as not compiling", caveats)
	}
	// Without a scope (--strict, or any caller outside generate) every
	// compile error still fails.
	if _, err := checkGoBuildScoped(f.dir, nil); err == nil {
		t.Error("a nil scope must fail on any compile error")
	}
}

// A hand-written consumer that no longer matches what the run regenerated
// fails because of the run: it compiled against the pre-run bytes.
func TestValidateScope_ConsumerBrokenByTheRunsOutputIsTheRuns(t *testing.T) {
	f := newScopeFixture(t, map[string]string{
		"a/a.go": "package a\n\nfunc F() string { return \"\" }\n",
		"c/c.go": "package c\n\nimport \"example.com/scope/a\"\n\nvar Y int = a.F()\n",
	})
	if _, ok := f.attribute(t); ok {
		t.Fatal("a consumer the run's regenerated a/a.go broke was called pre-existing")
	}
}

// A package that was already broken, and that the run ALSO broke further,
// is the run's: one of its errors does not reproduce before the run.
func TestValidateScope_NewErrorBesideAnOldOneIsTheRuns(t *testing.T) {
	f := newScopeFixture(t, map[string]string{
		"a/a.go": "package a\n\nfunc F() string { return \"\" }\n",
		"c/c.go": "package c\n\nimport \"example.com/scope/a\"\n\nvar Y int = a.F()\n\nvar Z int = \"old\"\n",
	})
	if _, ok := f.attribute(t); ok {
		t.Fatal("a package with a new error beside a pre-existing one was called pre-existing")
	}
}

// An error inside a package the run wrote into is the run's, whatever file
// it cites.
func TestValidateScope_ErrorInAPackageTheRunWroteIsTheRuns(t *testing.T) {
	f := newScopeFixture(t, map[string]string{"a/hand.go": "package a\n\nvar H int = \"x\"\n"})
	if _, ok := f.attribute(t); ok {
		t.Fatal("an error in a package the run wrote into was called foreign")
	}
}

// A forge-owned file that does not compile is forge's to answer for, even
// in a package this run did not write.
func TestValidateScope_ErrorInAForgeOwnedFileIsTheRuns(t *testing.T) {
	f := newScopeFixture(t, map[string]string{
		"g/x_gen.go": stampedForgeOwned(t, "g/x_gen.go", "package g\n\nvar G int = \"x\"\n"),
	})
	if _, ok := f.attribute(t); ok {
		t.Fatal("an error in a forge-owned file was called foreign")
	}
}

// What the run reaches but cmd/go could not compile — it wrote into the
// package, or the package imports one it wrote into — because a broken
// foreign package sits under it, is named as unchecked.
func TestValidateScope_NamesWhatTheBrokenPackageKeptFromCompiling(t *testing.T) {
	f := newScopeFixture(t, map[string]string{
		"b/b.go": brokenB,
		"a/a.go": "package a\n\nimport \"example.com/scope/b\"\n\nvar _ = b.X\n\nfunc F() int { return 2 }\n",
		"d/d.go": "package d\n\nimport \"example.com/scope/a\"\n\nvar _ = a.F\n",
		"e/e.go": "package e\n\nimport \"example.com/scope/b\"\n\nvar _ = b.X\n",
	})
	// The pre-run a/a.go imports b too: b's error is still pre-existing.
	f.scope.preRun = func(string) (checksums.PreRunState, bool) {
		return checksums.PreRunState{Existed: true, Content: []byte("package a\n\nimport \"example.com/scope/b\"\n\nvar _ = b.X\n\nfunc F() int { return 1 }\n")}, true
	}
	fc, ok := f.attribute(t)
	if !ok {
		t.Fatal("b's pre-existing error was attributed to the run")
	}
	slices.Sort(fc.unverified)
	if want := []string{"example.com/scope/a", "example.com/scope/d"}; !slices.Equal(fc.unverified, want) {
		t.Errorf("unverified = %v, want %v (e imports only b: the run does not reach it)", fc.unverified, want)
	}
}

// A file the run created is absent before the run: the overlay removes it.
func TestValidateScope_FileTheRunCreatedIsAbsentBeforeIt(t *testing.T) {
	f := newScopeFixture(t, map[string]string{
		"a/new_gen.go": "package a\n\nfunc G() string { return \"\" }\n",
		"c/c.go":       "package c\n\nimport \"example.com/scope/a\"\n\nvar Y int = a.G()\n",
	})
	f.scope.written = []string{"a/new_gen.go"}
	f.scope.preRun = func(string) (checksums.PreRunState, bool) { return checksums.PreRunState{}, false }
	// Before the run c failed with "undefined: a.G", after it with a type
	// mismatch: a different error, so the run's.
	if _, ok := f.attribute(t); ok {
		t.Fatal("an error that changed with a file the run created was called pre-existing")
	}
}

// Changed inputs without journaled pre-run bytes must prevent attribution,
// even when they are hand-written, missing, or no longer regular files.
func TestWriteScopeOf_UnjournaledHandwrittenChangesFailClosed(t *testing.T) {
	for _, change := range []string{"edit", "create", "delete", "non-regular"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			rel := "hand.go"
			file := filepath.Join(dir, rel)
			if change != "create" {
				if err := os.WriteFile(file, []byte("package hand\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotWorkTree(dir)
			if before == nil {
				t.Fatal("could not snapshot fixture")
			}
			switch change {
			case "edit", "create":
				if err := os.WriteFile(file, []byte("package hand\nvar X int = \"broken\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "delete", "non-regular":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if change == "non-regular" {
					if err := os.Mkdir(file, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			scope := writeScopeOf(&pipelineContext{AbsPath: dir, preRun: before})
			if scope == nil || !slices.Equal(scope.unrestorable, []string{rel}) {
				t.Fatalf("scope = %+v; want %s unrestorable", scope, rel)
			}
			pkgs := []*listedPackage{{ImportPath: "hand", Dir: dir, Error: &listedPackageError{Err: "hand.go:2:1: compile error"}}}
			if _, ok := scope.attribute(dir, pkgs); ok {
				t.Fatal("classified a failure as pre-existing without reconstructing the pre-run input")
			}
		})
	}
}

// Fail closed: a build input that changed outside the journal
// cannot be rewound for the comparison, so nothing is attributed.
func TestValidateScope_UnrestorableChangeAttributesNothing(t *testing.T) {
	s := &generateWriteScope{unrestorable: []string{"internal/x/x_gen.go"}}
	if _, ok := s.attribute(t.TempDir(), []*listedPackage{{ImportPath: "p", Dir: "/x", Error: &listedPackageError{Err: "boom"}}}); ok {
		t.Error("attributed a failure while a forge-owned file changed outside the journal")
	}
	var none *generateWriteScope
	if _, ok := none.attribute(t.TempDir(), nil); ok {
		t.Error("a nil scope attributed a failure")
	}
}

// The whole pipeline, with the real compile check: a run that rewrites its
// own package while another package in the module is mid-edit keeps its
// output and exits 0, and the closing line says the project does not
// compile. Before, it rolled back over an error it had nothing to do with.
// Under --strict it still fails and rolls back.
func TestGenerate_ForeignCompileErrorKeepsTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real compile check over a temp module; runs in task test")
	}
	const rel = "a/mock_gen.go"
	setup := func(t *testing.T) string {
		dir := writeValidateFixture(t, map[string]string{
			"go.mod": "module example.com/scope\n\ngo 1.22\n",
			rel:      stampedForgeOwned(t, rel, "package a\n\nfunc F() int { return 1 }\n"),
			"b/b.go": brokenB,
		})
		regen := GenStep{
			Name: "regenerate a's mock",
			Gate: always,
			Tag:  "codegen",
			Run: func(ctx *pipelineContext) error {
				_, err := checksums.WriteGeneratedFile(ctx.AbsPath, rel, []byte("package a\n\nfunc F() int { return 2 }\n"), &checksums.FileChecksums{}, true)
				return err
			},
		}
		withPipelineSteps(t, []GenStep{regen, productionStep(t, goBuildValidateStep)})
		return dir
	}

	dir := setup(t)
	out, err := runPipelineCapturingOutput(t, dir)
	if err != nil {
		t.Fatalf("another package's pre-existing error failed the run: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, rel)); !strings.Contains(string(got), "return 2") {
		t.Errorf("the run's output was not kept:\n%s", got)
	}
	for _, want := range []string{
		"1 compile error(s) in 1 package(s) this run did not write: example.com/scope/b",
		"— but the project does NOT compile.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}

	strictDir := setup(t)
	stderr, restore := captureStderr(t)
	var strictErr error
	captureStdout(t, func() { strictErr = runGeneratePipelineFlags(strictDir, pipelineFlags{Strict: true}) })
	restore()
	if strictErr == nil {
		t.Fatalf("--strict must fail on any compile error:\n%s", stderr.String())
	}
	if got, _ := os.ReadFile(filepath.Join(strictDir, rel)); strings.Contains(string(got), "return 2") {
		t.Error("--strict must roll the run back")
	}
}

func TestValidateScope_RequiresMappedHandwrittenErrors(t *testing.T) {
	dir := writeValidateFixture(t, map[string]string{
		"b/b.go": "package b\n",
		"b/generated.go": "// Code generated by test. DO NOT EDIT.\npackage b\n",
		"b/reconciled.go": "// forge-scaffold (USER-OWNED, RECONCILED)\npackage b\n",
		"other.go": "package fixture\n",
	})
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"handwritten", "b/b.go:2:1: undefined: x", true},
		{"no location", "compiler failed", false},
		{"header only", "# example.com/b\n", false},
		{"empty path", ":2:1: undefined: x", false},
		{"mixed", "b/b.go:2:1: undefined: x\ncompiler failed", false},
		{"outside package", "other.go:2:1: undefined: x", false},
		{"missing input", "b/missing.go:2:1: undefined: x", false},
		{"generated", "b/generated.go:2:1: undefined: x", false},
		{"reconciled", "b/reconciled.go:2:1: undefined: x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &listedPackage{
				ImportPath: "example.com/b",
				Dir:        filepath.Join(dir, "b"),
				GoFiles:    []string{"b.go", "generated.go", "reconciled.go", "missing.go"},
				Error:      &listedPackageError{Err: tc.text},
			}
			if _, ok := (&generateWriteScope{}).attribute(dir, []*listedPackage{p}); ok != tc.want {
				t.Errorf("attribute = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestErrorsReproduce(t *testing.T) {
	before := "# example.com/b\nb/b.go:3:13: cannot use x\nb/b.go:4:1: undefined: y\n"
	for _, tc := range []struct {
		after string
		want  bool
	}{
		{"# example.com/b\nb/b.go:3:13: cannot use x\n", true},
		{before, true},
		{"# example.com/b\nb/b.go:3:13: cannot use x\nb/b.go:9:1: too many errors\n", true},
		{"# example.com/b\nb/b.go:5:2: undefined: z\n", false},
	} {
		if got := errorsReproduce(tc.after, before); got != tc.want {
			t.Errorf("errorsReproduce(%q) = %v, want %v", tc.after, got, tc.want)
		}
	}
	if errorsReproduce("b/b.go:3:13: cannot use x\n", "") {
		t.Error("a package that compiled before the run cannot have had its errors")
	}
}

func TestProjectRelDir(t *testing.T) {
	root := filepath.Join(string(os.PathSeparator), "p", "proj")
	for dir, want := range map[string]string{
		filepath.Join(root, "internal", "x"): "internal/x",
		root:                                 ".",
	} {
		if got, ok := projectRelDir(root, dir); !ok || got != want {
			t.Errorf("projectRelDir(%s) = %q, %v; want %q", dir, got, ok, want)
		}
	}
	for _, dir := range []string{"", filepath.Join(string(os.PathSeparator), "p", "other"), filepath.Join(root, "..", "proj2")} {
		if _, ok := projectRelDir(root, dir); ok {
			t.Errorf("projectRelDir(%q) is inside the project", dir)
		}
	}
}
