package templates_test

import (
	"strings"
	"testing"
)

// TestAPISkillTeachesDomainSentinels pins the api skill's answer to forge
// friction #37 (Ranger): a domain sentinel written as its own type that
// unwraps to svcerr.ErrNotFound reached the client as `not found: not found`,
// its own text dropped, and nothing said that only text handed to svcerr
// crosses the wire or how to declare a named sentinel that carries its text.
func TestAPISkillTeachesDomainSentinels(t *testing.T) {
	t.Parallel()
	api := shippedSkills(t)["forge/api/SKILL.md"]
	for _, want := range []string{
		"Only text handed to svcerr reaches the client",
		"svcerr.Sentinel(svcerr.ErrNotFound, \"job was deleted\")",
	} {
		if !strings.Contains(api, want) {
			t.Errorf("api skill must say %q", want)
		}
	}
}
