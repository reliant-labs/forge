// skills_size_test.go — structural guard for shipped skill paths.
package templates_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestShippedSkillPathsAreWellFormed keeps a split skill reachable: every
// SKILL.md must sit under skills/forge/... or skills/general/..., because
// listForgeShippedSkills derives a skill's path from exactly those two
// roots and skips anything else. A subtopic dropped in the wrong place
// would vanish from the catalog rather than fail loudly.
func TestShippedSkillPathsAreWellFormed(t *testing.T) {
	t.Parallel()

	for rel, content := range shippedSkills(t) {
		if !strings.HasPrefix(rel, "forge/") && !strings.HasPrefix(rel, "general/") {
			t.Errorf("skills/%s: shipped skills must live under skills/forge/ or skills/general/ — anything else is skipped by the catalog", rel)
		}
		if filepath.Base(rel) != "SKILL.md" {
			t.Errorf("skills/%s: expected a SKILL.md leaf", rel)
		}
		if !strings.HasPrefix(content, "---\n") {
			t.Errorf("skills/%s: must open with YAML frontmatter at byte 0 (the skill loader requires it)", rel)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// maxSkillBytes mirrors reliant's MaxSkillBodySize: a larger skill is
// truncated when loaded, losing its tail (sub-skill pointers).
const maxSkillBytes = 24_000

func TestShippedSkillsFitLoadBudget(t *testing.T) {
	t.Parallel()
	for rel, content := range shippedSkills(t) {
		if len(content) > maxSkillBytes {
			t.Errorf("skills/%s: %d bytes exceeds the %d-byte load cap; split a separable topic into a sibling sub-skill", rel, len(content), maxSkillBytes)
		}
	}
}
