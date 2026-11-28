package cli

import (
	"strings"
	"testing"
)

var gtmSkillPaths = []string{
	"gtm",
	"gtm/positioning",
	"gtm/choose-strategies",
	"gtm/campaigns",
	"gtm/outreach",
	"gtm/events",
	"gtm/content",
	"gtm/measure",
	"gtm/automate",
}

// The gtm family is methodology for any team: it must ship, load by path,
// carry the general emit, and not need a forge project.
func TestGTMSkillsShipAndLoad(t *testing.T) {
	skills, err := listForgeShippedSkills()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]skillMeta{}
	for _, s := range skills {
		byPath[s.Path] = s
	}
	for _, p := range gtmSkillPaths {
		s, ok := byPath[p]
		if !ok {
			t.Errorf("skill %q is not shipped", p)
			continue
		}
		if s.Emit != SkillEmitGeneral {
			t.Errorf("skill %q emit = %q, want general", p, s.Emit)
		}
		if s.Applies != "" {
			t.Errorf("skill %q declares applies %q; gtm skills are methodology, listed under METHODOLOGY", p, s.Applies)
		}
		body, err := RenderSkillContentAt("", p, SkillAudienceAll, "forge")
		if err != nil || len(body) == 0 {
			t.Errorf("load %q: %v", p, err)
		}
	}
}

// Recurring work points at the tool-agnostic automation design, and every
// skill that can lead to an outward action says a human approves it.
func TestGTMSkillsCrossReferenceAndGateOutwardActions(t *testing.T) {
	read := func(p string) string {
		b, err := RenderSkillContentAt("", p, SkillAudienceAll, "forge")
		if err != nil {
			t.Fatalf("load %q: %v", p, err)
		}
		return string(b)
	}
	for _, p := range []string{"gtm/events", "gtm/outreach", "gtm/measure"} {
		if !strings.Contains(read(p), "gtm/automate") {
			t.Errorf("skill %q must point to gtm/automate for its recurring workflow", p)
		}
	}
	for _, p := range []string{"gtm", "gtm/choose-strategies", "gtm/campaigns", "gtm/outreach", "gtm/events", "gtm/content", "gtm/automate"} {
		if !strings.Contains(strings.ToLower(read(p)), "approv") {
			t.Errorf("skill %q must say outward actions need a human's approval", p)
		}
	}
	for _, p := range gtmSkillPaths {
		if body := read(p); strings.Contains(body, "@forge-only") {
			t.Errorf("skill %q has forge-only blocks; the gtm family is tool-agnostic", p)
		}
	}
}

// The automate skill lists runner options neutrally: Reliant is one of
// several, and the workflow docs are linked from that option only.
func TestGTMAutomateListsRunnersNeutrally(t *testing.T) {
	b, err := RenderSkillContentAt("", "gtm/automate", SkillAudienceAll, "forge")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{"Reliant workflow", "cron", "GitHub Actions", "n8n, Zapier or Make"} {
		if !strings.Contains(body, want) {
			t.Errorf("gtm/automate does not list the %q runner option", want)
		}
	}
	if n := strings.Count(body, "docs.reliantlabs.io"); n != 1 {
		t.Errorf("gtm/automate links Reliant docs %d times, want exactly once (the Reliant option only)", n)
	}
	for _, p := range gtmSkillPaths {
		text, err := RenderSkillContentAt("", p, SkillAudienceAll, "forge")
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(text))
		if strings.Contains(lower, "forge") {
			t.Errorf("skill %q mentions forge; the gtm family is tool-agnostic", p)
		}
		for _, line := range strings.Split(lower, "\n") {
			if strings.Contains(line, "reliant") && !(p == "gtm/automate" && strings.Contains(line, "a reliant workflow")) {
				t.Errorf("skill %q mentions reliant outside the runner option: %q", p, line)
			}
		}
	}
}

func TestGTMSkillsAreFindableAndListedAsMethodology(t *testing.T) {
	skills, err := listForgeShippedSkills()
	if err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]string{
		"positioning":  "gtm/positioning",
		"go-to-market": "gtm",
		"outreach":     "gtm/outreach",
		"hackathon":    "gtm/events",
		"north star":   "gtm/measure",
	} {
		found := false
		for _, r := range searchSkillsIn(skills, query) {
			if r.Skill.Path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("skill search %q does not find %q", query, want)
		}
	}

	got := runSkillList(t)
	methodAt := strings.Index(got, "METHODOLOGY")
	gtmAt := strings.Index(got, "\n  gtm ")
	if methodAt < 0 || gtmAt < methodAt {
		t.Errorf("gtm must be a group row under METHODOLOGY:\n%s", got)
	}
	if !strings.Contains(got, "gtm  ") || !strings.Contains(got, "+8") {
		t.Errorf("gtm must collapse its 8 sub-skills into a +8 count:\n%s", got)
	}
}
