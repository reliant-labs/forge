package skillcatalog

import (
	"strings"
	"testing"
)

// TestShippedSkillDirectivesRender renders every forge-shipped skill that
// carries an `@benefits` directive and checks none survives: a directive the
// registry cannot render would otherwise fail `skill load` for a user.
func TestShippedSkillDirectivesRender(t *testing.T) {
	skills, err := ListShipped()
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, m := range skills {
		raw, err := LoadShipped(m.Path)
		if err != nil {
			t.Fatalf("%s: %v", m.Path, err)
		}
		if !strings.Contains(string(raw), benefitsDirectiveOpen) {
			continue
		}
		found++
		out, err := ExpandBenefitDirectives(raw)
		if err != nil {
			t.Errorf("skill %s: %v", m.Path, err)
			continue
		}
		if strings.Contains(string(out), benefitsDirectiveOpen) {
			t.Errorf("skill %s: an @benefits directive survived rendering", m.Path)
		}
	}
	if found == 0 {
		t.Fatal("no shipped skill carries an @benefits directive; the entry skills should open with a glance")
	}
}
