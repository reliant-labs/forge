// project_gitignore_test.go — pins the .forge/ handling in the project
// .gitignore template.
//
// The broad .forge ignore is intentional (per-developer runtime
// state), but two files are shared project state and MUST be negated
// back into version control:
//
//   - disowned.json — one-way ownership transfers (`forge disown`);
//     travels with the repo so forge never regenerates a disowned file
//     in any clone or worktree
//   - hashes.json — scoped render hashes for comment-incapable
//     generated formats (JSON outputs)
//   - friction.jsonl — the append-only generator-friction log written
//     by `forge friction add`; it travels with the repo so captured
//     friction survives worktrees, clones, and CI checkouts
//
// (checksums.json is DEAD: generated files certify themselves via the
// embedded forge:hash marker; the negation must NOT come back.)
//
// Losing a negation silently strands shared state on one machine,
// so the exact lines are asserted here.
//
// CRITICAL gitignore semantics: the ignore rule must be `.forge/*`
// (children), NOT `.forge/` (the directory). Git cannot re-include a
// file whose parent DIRECTORY is excluded — with `.forge/` the
// negations are silently dead and the state files never get committed.
// (Bitten in cp-forge; its .gitignore fork carried the fix before the
// template did.)
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

	if !has(".forge/*") {
		t.Error(".gitignore template must ignore .forge/* (children rule — per-developer state)")
	}
	if has(".forge/") {
		t.Error(".gitignore template must NOT use the `.forge/` directory rule: " +
			"git cannot re-include files under an excluded directory, so the " +
			"disowned.json/hashes.json/friction.jsonl negations would be silently dead")
	}
	if has("!.forge/checksums.json") {
		t.Error(".gitignore template must NOT negate the dead checksums.json manifest back in")
	}
	for _, neg := range []string{"!.forge/disowned.json", "!.forge/hashes.json", "!.forge/friction.jsonl"} {
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
	childRule := idx(".forge/*")
	for _, neg := range []string{"!.forge/disowned.json", "!.forge/hashes.json", "!.forge/friction.jsonl"} {
		if n := idx(neg); n >= 0 && n < childRule {
			t.Errorf("%s must come after the .forge/* ignore rule to take effect", neg)
		}
	}
}
