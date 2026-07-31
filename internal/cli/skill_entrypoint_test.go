package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestSkillListNamesTheEntryPoint pins the pointer `forge skill list` prints
// after its table. An alphabetical listing of ninety skills answers "what
// exists" but not "which one first", and the measured cost of that gap was
// the largest single time bucket in a dogfood run. The pointer is only
// printed when the skill really ships, so this test also guards that it
// still does.
func TestSkillListNamesTheEntryPoint(t *testing.T) {
	skills, err := listForgeShippedSkills()
	if err != nil {
		t.Fatalf("listForgeShippedSkills: %v", err)
	}
	found := false
	for _, s := range skills {
		if s.Path == entryPointSkillPath {
			found = true
			if s.Description == "" {
				t.Errorf("entry-point skill %q has no description — it would list as a bare row", s.Path)
			}
		}
	}
	if !found {
		t.Fatalf("no shipped skill at %q — `forge skill list` would print no entry pointer, and the corpus would have no front door", entryPointSkillPath)
	}

	cmd := newSkillListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("skill list: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "skill load "+entryPointSkillPath) {
		t.Errorf("`skill list` output does not name the entry point:\n%s", got)
	}
	// The pointer must come after the table, not instead of it.
	if !strings.Contains(got, "PATH") || !strings.Contains(got, "db") {
		t.Errorf("`skill list` lost its table:\n%s", got)
	}
}
