package cigate

import (
	"context"
	"slices"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// A Go test that names a file runs when it changes, even when a rule claims
// the file or affected_ignore lists it: claiming and ignoring decide only
// whether an unplaced file forces the full suite.
func TestAffectedReaders_SelectClaimedAndIgnoredFiles(t *testing.T) {
	cases := []struct {
		name   string
		ignore []string
		rules  []config.CIGateAffectedRule
	}{
		{name: "claimed by a rule", rules: []config.CIGateAffectedRule{{Paths: []string{"config/**"}, Packages: []string{"./a"}}}},
		{name: "listed in affected_ignore", ignore: []string{"config/**"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, tracked := readerRoot(t)
			g := readerGit{fakeGit: fakeGit{branch: "f", changed: []string{"config/settings.yaml"}}, tracked: tracked}
			a := ComputeAffected(context.Background(), AffectedInput{Root: root, Git: g.run, GoList: listed(root),
				Readers: true, Ignore: tc.ignore, Rules: tc.rules})
			if a.Full {
				t.Fatalf("full (%s), want narrowed", a.Reason)
			}
			if !slices.Contains(a.Packages, "ex.com/m/c") {
				t.Errorf("packages = %v, want ex.com/m/c (its test names config/settings.yaml)", a.Packages)
			}
		})
	}
}

// A package that owns a changed file (here, embeds it) runs even when an
// affected_ignore glob matches the file.
func TestComputeAffected_OwnerBeatsIgnore(t *testing.T) {
	root := t.TempDir()
	a := ComputeAffected(context.Background(), AffectedInput{Root: root, GoList: listed(root),
		Git: fakeGit{branch: "f", changed: []string{"a/tmpl/x.tmpl"}}.run, Ignore: []string{"*.tmpl"}})
	if a.Full {
		t.Fatalf("full (%s), want narrowed", a.Reason)
	}
	if !slices.Contains(a.Packages, "ex.com/m/a") || !slices.Contains(a.Packages, "ex.com/m/b") {
		t.Errorf("packages = %v, want ex.com/m/a (embeds it) and ex.com/m/b (imports a)", a.Packages)
	}
}

// An `on: go-source` rule (a tree-wide walker) fires for a Go change inside
// a nested module affected_modules: auto splits off.
func TestComputeAffected_GoSourceRuleSeesNestedModules(t *testing.T) {
	root, list := modulesFixture(t)
	a := ComputeAffected(context.Background(), AffectedInput{Root: root, Modules: AffectedModulesAuto, GoList: list,
		Git:   fakeGit{branch: "f", changed: []string{"sdk/go/util/u.go"}}.run,
		Rules: []config.CIGateAffectedRule{{On: config.CIGateOnGoSource, Packages: []string{"./other"}}}})
	if a.Full {
		t.Fatalf("full (%s), want narrowed", a.Reason)
	}
	if !slices.Contains(a.Packages, "ex.com/m/other") {
		t.Errorf("packages = %v, want ex.com/m/other (the go-source walker)", a.Packages)
	}
}

// --plan --json reports the nested modules' tests, always as a list.
func TestDocument_ReportsModules(t *testing.T) {
	d := (&Affected{}).Document()
	if d.Modules == nil {
		t.Fatal("modules is nil, want []")
	}
	d = (&Affected{Modules: []ModuleTests{{Dir: "sdk/go", Path: "ex.com/sdk", Packages: []string{"ex.com/sdk/x"}}}}).Document()
	if len(d.Modules) != 1 || d.Modules[0].Dir != "sdk/go" {
		t.Errorf("modules = %+v", d.Modules)
	}
}
