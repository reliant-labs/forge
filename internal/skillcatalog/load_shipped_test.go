package skillcatalog

import (
	"errors"
	"io/fs"
	"testing"
)

// traversalSkillPaths are skill paths that are not a clean relative path
// beneath a skill root. Each must be refused before a template path is built:
// path.Join cleans "..", so "../general/code-review" read a general skill
// through the forge root, and enough ".." elements address any embedded file
// that the joined suffix names.
var traversalSkillPaths = []string{
	"",
	".",
	"..",
	"../general/code-review",
	"forge/../general/code-review",
	"../../project",
	"db/../../general/code-review",
	"db/../api",
	"./db",
	"db/",
	"db//migrations",
	"/db",
	"/etc/passwd",
	`..\general\code-review`,
	`db\migrations`,
	"forge/",
}

// LoadShipped refuses every path that could leave the skill root, with
// ErrInvalidSkillPath (which also reads as "does not exist").
func TestLoadShippedRefusesPathsOutsideTheSkillRoot(t *testing.T) {
	for _, p := range traversalSkillPaths {
		body, err := LoadShipped(p)
		if !errors.Is(err, ErrInvalidSkillPath) || !errors.Is(err, fs.ErrNotExist) || body != nil {
			t.Errorf("LoadShipped(%q) = %d bytes, err %v; want ErrInvalidSkillPath", p, len(body), err)
		}
	}
}

// The check refuses only bad paths: the entry point, a forge skill, a nested
// forge skill, the "forge/" prefix and a general skill still load.
func TestLoadShippedLoadsValidPaths(t *testing.T) {
	for _, p := range []string{"forge", "db", "forge/db", "db/seeding", "code-review"} {
		if body, err := LoadShipped(p); err != nil || len(body) == 0 {
			t.Errorf("LoadShipped(%q) = %d bytes, err %v; want the skill", p, len(body), err)
		}
	}
}

// The traversal the check closes, observed: before it, "../general/code-review"
// joined to skills/forge/../general/code-review and read a general skill
// through the forge root, and "forge/../general/..." did the same past the
// prefix trim.
func TestLoadShippedReadsNothingThroughDotDot(t *testing.T) {
	for _, p := range []string{"../general/code-review", "forge/../general/code-review", "db/../api"} {
		if body, err := LoadShipped(p); err == nil || body != nil {
			t.Errorf("LoadShipped(%q) = %d bytes, err %v; want it refused", p, len(body), err)
		}
	}
}
