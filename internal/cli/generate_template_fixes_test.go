package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codemod"
)

// generate names each owned-file template fix still pending, with the files
// and the command that applies it; a fix with nothing to do, a codemod that
// is not Advise, and one that errors print nothing.
func TestAdviseTemplateFixes(t *testing.T) {
	t.Parallel()
	report := func(rep codemod.Report, err error) func(string, codemod.Options) (codemod.Report, error) {
		return func(string, codemod.Options) (codemod.Report, error) { return rep, err }
	}
	mods := []codemod.Codemod{
		{Name: "pending", Version: "v1.12.0", Short: "Fix the seam", Advise: true, Run: report(codemod.Report{Rewrites: []string{
			"internal/a/observe_chain.go: x", "internal/b/observe_chain.go: x", "internal/c/observe_chain.go: x",
			"internal/d/observe_chain.go: x", "internal/e/observe_chain.go: x",
		}}, nil)},
		{Name: "edited", Version: "v1.12.0", Short: "Fix the Taskfile", Advise: true, Run: report(codemod.Report{Next: []string{"Taskfile.yml: add FORGE"}}, nil)},
		{Name: "done", Version: "v1.12.0", Advise: true, Run: report(codemod.Report{}, nil)},
		{Name: "not-advise", Version: "v1.12.0", Run: report(codemod.Report{Rewrites: []string{"x.go: y"}}, nil)},
		{Name: "broken", Version: "v1.12.0", Advise: true, Run: report(codemod.Report{Rewrites: []string{"x.go: y"}}, errors.New("boom"))},
	}
	var buf bytes.Buffer
	adviseTemplateFixes(&buf, t.TempDir(), mods)
	out := buf.String()
	for _, want := range []string{
		"forge fixed the template these 5 owned files came from (v1.12.0, pending): Fix the seam",
		"internal/a/observe_chain.go, internal/b/observe_chain.go, internal/c/observe_chain.go, 2 more",
		"`" + Name() + " fix pending` applies it",
		"forge fixed the template this owned file came from (v1.12.0, edited): Fix the Taskfile",
		"Taskfile.yml",
		"Edited past what `" + Name() + " fix edited` can rewrite",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("advice lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"done", "not-advise", "broken", "x.go"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("advice names %q, which has nothing pending or is not advice:\n%s", unwanted, out)
		}
	}
}

// Every registered Advise codemod is one upgrade applies too: generate's
// advice and upgrade's crossing name the same fixes.
func TestAdviseCodemodsAreUpgradeCodemods(t *testing.T) {
	t.Parallel()
	n := 0
	for _, c := range codemod.All() {
		if !c.Advise {
			continue
		}
		n++
		if !c.Upgrade {
			t.Errorf("codemod %s advises in generate but `forge project upgrade` does not run it", c.Name)
		}
	}
	if n == 0 {
		t.Fatal("no Advise codemod is registered")
	}
}
