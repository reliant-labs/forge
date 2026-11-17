package commitpolicy

import (
	"strings"
	"testing"
)

// The fix hint follows the rule that excludes the path. Replacing `.forge/`
// with `.forge/*` is the fix only when the directory itself is ignored; a
// .gitignore that already has `.forge/*` is missing a negation, and telling it
// to make a replacement it has already made fixes nothing.
func TestForgeStateFix_FollowsTheRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, pattern string
		want, reject  []string
	}{
		{"directory", ".forge/", []string{"replace `.forge/` with `.forge/*`"}, nil},
		{"directory, no slash", ".forge", []string{"replace `.forge` with `.forge/*`"}, nil},
		{"directory, anchored", "/.forge/", []string{"replace `/.forge/` with `.forge/*`"}, nil},
		{"children, negation missing", ".forge/*",
			[]string{"keep `.forge/*`", "add `!.forge/scaffolded/`", ".gitignore", "`git add .forge/scaffolded/`"},
			[]string{"replace"}},
		{"children, anchored", "/.forge/*", []string{"keep `/.forge/*`", "add `!.forge/scaffolded/`"}, []string{"replace"}},
		{"broader rule", "scaffolded/",
			[]string{"add `!.forge/scaffolded/` on a line below `scaffolded/` (.gitignore:7)"}, []string{"replace"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := forgeStateFix(".forge/scaffolded/", ignoreMatch{source: ".gitignore", line: "7", pattern: tc.pattern})
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("fix %q: want it to contain %q", got, w)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(got, r) {
					t.Errorf("fix %q: must not contain %q", got, r)
				}
			}
		})
	}
}
