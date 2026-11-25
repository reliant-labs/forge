package cigate

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

func TestWithLockfileChecks(t *testing.T) {
	pnpmFE := templates.FrontendCIConfig{Name: "web", Path: "frontends/web"}
	cases := []struct {
		name     string
		files    map[string]string
		changed  []string
		wantRows string
		wantArgv string // suffix of the lockfile row's argv
		wantDir  string
	}{
		{
			name:     "npm manifest changed",
			files:    map[string]string{"frontends/web/package.json": "{}", "frontends/web/package-lock.json": "{}"},
			changed:  []string{"frontends/web/package.json"},
			wantRows: "lockfile-web deps-web unit-tests",
			wantArgv: "ci --dry-run --ignore-scripts --no-audit --no-fund",
			wantDir:  "frontends/web",
		},
		{
			name:     "pnpm lockfile changed",
			files:    map[string]string{"frontends/web/package.json": "{}", "frontends/web/pnpm-lock.yaml": "x"},
			changed:  []string{"frontends/web/pnpm-lock.yaml"},
			wantRows: "lockfile-web deps-web unit-tests",
			wantArgv: "install --frozen-lockfile --lockfile-only --ignore-scripts",
			wantDir:  "frontends/web",
		},
		{
			name:     "pnpm workspace root holds the lockfile",
			files:    map[string]string{"frontends/web/package.json": "{}", "pnpm-lock.yaml": "x", "pnpm-workspace.yaml": "x"},
			changed:  []string{"pnpm-workspace.yaml"},
			wantRows: "lockfile-web deps-web unit-tests",
			wantArgv: "install --frozen-lockfile --lockfile-only --ignore-scripts",
			wantDir:  "",
		},
		{
			name:     "source change only: N/A, no row",
			files:    map[string]string{"frontends/web/package.json": "{}", "frontends/web/package-lock.json": "{}"},
			changed:  []string{"frontends/web/src/App.tsx"},
			wantRows: "deps-web unit-tests",
		},
		{
			name:     "no lockfile: nothing to be out of sync with",
			files:    map[string]string{"frontends/web/package.json": "{}"},
			changed:  []string{"frontends/web/package.json"},
			wantRows: "deps-web unit-tests",
		},
		{
			name:     "full mode adds nothing",
			files:    map[string]string{"frontends/web/package.json": "{}", "frontends/web/package-lock.json": "{}"},
			wantRows: "deps-web unit-tests",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for f, body := range tc.files {
				write(t, root, f, body)
			}
			checks := []Check{{Name: "deps-web", Lane: LaneTest}, {Name: UnitTests, Lane: LaneTest}}
			got := WithLockfileChecks(checks, Affected{Changed: tc.changed}, root, []templates.FrontendCIConfig{pnpmFE})
			if strings.Join(names(got), " ") != tc.wantRows {
				t.Fatalf("rows = %v, want %s", names(got), tc.wantRows)
			}
			if tc.wantArgv == "" {
				return
			}
			c := find(t, got, "lockfile-web")
			if argv := strings.Join(c.Argv, " "); !strings.HasSuffix(argv, tc.wantArgv) {
				t.Errorf("argv = %q, want suffix %q", argv, tc.wantArgv)
			}
			if c.Dir != tc.wantDir || c.Lane != LaneTest || c.Note == "" {
				t.Errorf("row = %+v", c)
			}
		})
	}
}

// A frontend whose deps row is not selected (its lane is not running) gets
// no lockfile row either.
func TestWithLockfileChecks_FollowsTheDepsRowsLane(t *testing.T) {
	root := t.TempDir()
	write(t, root, "frontends/web/package.json", "{}")
	write(t, root, "frontends/web/package-lock.json", "{}")
	checks := []Check{{Name: UnitTests, Lane: LaneTest}}
	got := WithLockfileChecks(checks, Affected{Changed: []string{"frontends/web/package.json"}}, root, []templates.FrontendCIConfig{webFE})
	if strings.Join(names(got), " ") != UnitTests {
		t.Fatalf("rows = %v", names(got))
	}
}
