package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/doctor"
)

const goModPinned = `module github.com/example/demo

go 1.26

require (
	github.com/reliant-labs/forge/pkg v0.3.0
	github.com/example/demo/gen v0.0.0
)

replace github.com/example/demo/gen => ./gen
`

const goModVendored = `module github.com/example/demo

go 1.26

require github.com/reliant-labs/forge/pkg v0.0.0

replace github.com/example/demo/gen => ./gen

replace github.com/reliant-labs/forge/pkg => ./.forge-pkg
`

const goModAbsReplace = `module github.com/example/demo

go 1.26

replace github.com/reliant-labs/forge/pkg => /home/dev/src/forge/pkg
`

// TestPkgPinCheck covers every branch of the pure decision core. The
// load-bearing case is "stuck on the dev path": a .forge-pkg replace in
// a project while the running forge release publishes a real pkg
// version must WARN (not fail — the project still builds) with actionable
// switch-over commands.
func TestPkgPinCheck(t *testing.T) {
	cases := []struct {
		name       string
		goMod      string
		published  string
		wantStatus doctor.Status
		wantInMsg  string
		wantInEv   string
	}{
		{
			name:       "pinned release + released forge: pass",
			goMod:      goModPinned,
			published:  "v0.3.0",
			wantStatus: doctor.StatusPass,
			wantInMsg:  "published-release mode",
		},
		{
			name:       "pinned release + dev forge: pass (no replace present)",
			goMod:      goModPinned,
			published:  "",
			wantStatus: doctor.StatusPass,
		},
		{
			name:       "vendored + dev forge: pass (coherent dev wiring)",
			goMod:      goModVendored,
			published:  "",
			wantStatus: doctor.StatusPass,
			wantInMsg:  "dev build",
		},
		{
			name:       "vendored + released forge: warn, stuck on dev path",
			goMod:      goModVendored,
			published:  "v0.3.0",
			wantStatus: doctor.StatusWarn,
			wantInMsg:  "publishes pkg v0.3.0",
			wantInEv:   "go mod edit -dropreplace=github.com/reliant-labs/forge/pkg",
		},
		{
			name:       "host-absolute replace + released forge: warn",
			goMod:      goModAbsReplace,
			published:  "v0.3.0",
			wantStatus: doctor.StatusWarn,
			wantInMsg:  "/home/dev/src/forge/pkg",
			wantInEv:   "go get github.com/reliant-labs/forge/pkg@v0.3.0",
		},
		{
			name:       "empty go.mod: pass (nothing to inspect)",
			goMod:      "",
			published:  "v0.3.0",
			wantStatus: doctor.StatusPass,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := pkgPinCheck(c.goMod, c.published)
			if res.Name != pkgPinCheckName {
				t.Errorf("Name = %q, want %q", res.Name, pkgPinCheckName)
			}
			if res.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q (message: %s)", res.Status, c.wantStatus, res.Message)
			}
			if c.wantInMsg != "" && !strings.Contains(res.Message, c.wantInMsg) {
				t.Errorf("Message %q missing %q", res.Message, c.wantInMsg)
			}
			if c.wantInEv != "" && !strings.Contains(res.Evidence, c.wantInEv) {
				t.Errorf("Evidence %q missing %q", res.Evidence, c.wantInEv)
			}
		})
	}
}

// TestRunPkgPinDoctorChecksSignalFilter pins the wrapper's signal
// behavior: nil for telemetry-signal filters, active for "" and "tools".
func TestRunPkgPinDoctorChecksSignalFilter(t *testing.T) {
	dir := t.TempDir() // no go.mod → skip result when the check runs
	if got := runPkgPinDoctorChecks(nil, dir, "traces"); got != nil {
		t.Errorf("signal=traces: want nil, got %v", got)
	}
	for _, signal := range []string{"", "tools"} {
		got := runPkgPinDoctorChecks(nil, dir, signal)
		if len(got) != 1 || got[0].Status != doctor.StatusSkip {
			t.Errorf("signal=%q: want one skip result (no go.mod), got %v", signal, got)
		}
	}
}
