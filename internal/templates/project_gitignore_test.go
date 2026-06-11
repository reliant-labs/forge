// project_gitignore_test.go — pins the .forge/ handling in the project
// .gitignore template.
//
// The broad `.forge/` ignore is intentional (per-developer runtime
// state), but two files are shared project state and MUST be negated
// back into version control:
//
//   - checksums.json — generate/upgrade drift detection across clones
//   - friction.jsonl — the append-only generator-friction log written
//     by `forge friction add`; it travels with the repo so captured
//     friction survives worktrees, clones, and CI checkouts
//
// Losing either negation silently strands shared state on one machine,
// so the exact lines are asserted here.
package templates_test

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestProjectGitignore_ForgeStateNegations(t *testing.T) {
	content, err := templates.ProjectTemplates().Get(".gitignore")
	if err != nil {
		t.Fatalf("read project .gitignore template: %v", err)
	}
	lines := strings.Split(string(content), "\n")
	has := func(want string) bool {
		for _, l := range lines {
			if strings.TrimSpace(l) == want {
				return true
			}
		}
		return false
	}

	if !has(".forge/") {
		t.Error(".gitignore template must ignore .forge/ (per-developer state)")
	}
	for _, neg := range []string{"!.forge/checksums.json", "!.forge/friction.jsonl"} {
		if !has(neg) {
			t.Errorf(".gitignore template must negate %s back into version control", strings.TrimPrefix(neg, "!"))
		}
	}

	// Order matters for gitignore semantics: a negation only works when
	// it appears after the rule it carves out of.
	idx := func(want string) int {
		for i, l := range lines {
			if strings.TrimSpace(l) == want {
				return i
			}
		}
		return -1
	}
	dirRule := idx(".forge/")
	for _, neg := range []string{"!.forge/checksums.json", "!.forge/friction.jsonl"} {
		if n := idx(neg); n >= 0 && n < dirRule {
			t.Errorf("%s must come after the .forge/ ignore rule to take effect", neg)
		}
	}
}
