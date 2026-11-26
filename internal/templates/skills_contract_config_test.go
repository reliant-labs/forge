package templates_test

import (
	"regexp"
	"testing"
)

func TestContractSkillsDoNotRecommendRemovedSettings(t *testing.T) {
	t.Parallel()
	skills := shippedSkills(t)
	// Match setting syntax, not prose that tells users which keys to remove.
	removed := regexp.MustCompile(`(?m)^\s*(?:- \x60)?(?:contracts\.)?(?:strict|allow_exported_vars|allow_exported_funcs):\s*(?:true|false)\b`)
	for _, name := range []string{"contracts", "migration", "migration-cli", "migration-service"} {
		path := "forge/" + name + "/SKILL.md"
		if bad := removed.FindString(skills[path]); bad != "" {
			t.Errorf("%s recommends a removed setting: %s", path, bad)
		}
	}
}
