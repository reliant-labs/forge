package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// tsconfig.json is GENERATED AND COMMITTED, and CI re-runs `forge generate`
// and fails the build on any diff. So the hoisting decision must be a pure
// function of TRACKED FILES. If it also reads node_modules, the same commit
// generates two different files — one on a developer's machine, another on a
// CI runner that has not installed anything — and Verify Generated Code fails
// on a file nobody edited.
//
// This is not hypothetical. control-plane's root package.json IS forge's own
// dev web-runtime bridge and is gitignored, so a developer's tree had both a
// workspace root and hoisted node_modules ("../../node_modules/…") while CI
// had neither ("./node_modules/…"). All 14 pins flipped and the job failed.
//
// The guarantee under test: for one fixed set of tracked files, installing or
// removing node_modules must not change the answer.
func TestFrontendDepsAreHoistedIgnoresInstallState(t *testing.T) {
	tests := []struct {
		name string
		// declareWorkspaceRoot writes a TRACKED-shaped workspace root.
		declareWorkspaceRoot bool
		// linkRuntime marks the frontend a workspace member in its own
		// (tracked) package.json.
		linkRuntime bool
		want        bool
	}{
		{name: "workspace root declared", declareWorkspaceRoot: true, want: true},
		{name: "frontend links the runtime", linkRuntime: true, want: true},
		{name: "plain frontend, no workspace", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, installed := range []bool{false, true} {
				projectDir := t.TempDir()
				feDir := filepath.Join(projectDir, "frontends", "console")
				mustMkdirAll(t, feDir)

				if tt.declareWorkspaceRoot {
					mustWrite(t, filepath.Join(projectDir, "package.json"),
						`{"workspaces":["frontends/*"]}`)
				}
				fe := `{"dependencies":{"@reliantlabs/forge-web-runtime":"^0.3.1"}}`
				if tt.linkRuntime {
					fe = `{"dependencies":{"@reliantlabs/forge-web-runtime":"workspace:*"}}`
				}
				mustWrite(t, filepath.Join(feDir, "package.json"), fe)

				// The ONLY thing that varies across the two iterations.
				if installed {
					mustMkdirAll(t, filepath.Join(feDir, "node_modules", "@connectrpc", "connect"))
					mustMkdirAll(t, filepath.Join(projectDir, "node_modules", "@connectrpc", "connect"))
				}

				got := frontendDepsAreHoisted(projectDir, feDir)
				if got != tt.want {
					t.Errorf("frontendDepsAreHoisted(installed=%v) = %v, want %v — "+
						"the decision must come from tracked files only, or the same "+
						"commit generates a different tsconfig.json on CI than on a "+
						"developer machine and Verify Generated Code fails on a file "+
						"nobody touched",
						installed, got, tt.want)
				}
			}
		})
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
}
