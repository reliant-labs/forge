package lint

import "testing"

// A scoped run that names no path inside any frontend skips the frontend
// typecheck (an install plus a whole-frontend tsc whose findings the scope
// would drop anyway); a scope inside or around a frontend still runs it.
// Forge bug report 77e3df15: `forge lint --scope internal/...` ran
// `pnpm install` and the web typecheck.
func TestLintScopeReachesAnyDir(t *testing.T) {
	scope := func(paths ...string) *lintScope { return &lintScope{root: "/p", paths: paths} }
	dirs := []string{"frontends/web", "./frontends/admin"}
	cases := []struct {
		name  string
		scope *lintScope
		want  bool
	}{
		{"whole project", nil, true},
		{"go packages only", scope("internal/integrations/githubqueue", "internal/llm"), false},
		{"a file in a frontend", scope("frontends/web/src/app.tsx"), true},
		{"the frontend itself", scope("frontends/admin"), true},
		{"a parent of the frontends", scope("frontends"), true},
		{"a sibling with a shared prefix", scope("frontends/webhooks"), false},
	}
	for _, c := range cases {
		if got := c.scope.reachesAnyDir(dirs); got != c.want {
			t.Errorf("%s: reachesAnyDir = %v, want %v", c.name, got, c.want)
		}
	}
}
