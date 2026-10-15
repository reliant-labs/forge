package cigate

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/affected"
)

func TestApplyAffected(t *testing.T) {
	unit := Check{Name: UnitTests, Argv: []string{"task", "test"}}
	other := check("lint", "lint")

	got := ApplyAffected([]Check{other, unit}, affected.Result{Packages: []string{"ex.com/m/a", "ex.com/m/b"}, Total: 9, BaseRef: "origin/main"})
	if argv := strings.Join(got[1].Argv, " "); argv != "task test -- ex.com/m/a ex.com/m/b" {
		t.Errorf("argv = %q", argv)
	}
	if !strings.Contains(got[1].Note, "2 of 9 packages") || got[0].Note != "" {
		t.Errorf("notes: %q / %q", got[1].Note, got[0].Note)
	}
	if strings.Join(unit.Argv, " ") != "task test" {
		t.Error("ApplyAffected modified its input")
	}

	none := ApplyAffected([]Check{unit}, affected.Result{BaseRef: "origin/main", Changed: []string{"nested/d/d.go"}})
	if none[0].NotApplicable == "" {
		t.Error("no package reached: the row is N/A, with the reason")
	}
	full := ApplyAffected([]Check{unit}, affected.Result{Full: true, Reason: "go.mod changed"})
	if strings.Join(full[0].Argv, " ") != "task test" || full[0].Note != "full suite: go.mod changed" {
		t.Errorf("full: %+v", full[0])
	}
}
