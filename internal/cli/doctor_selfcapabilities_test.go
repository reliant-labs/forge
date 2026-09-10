package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/doctor"
)

// deployEnabledConfig is the project shape that needs a render, and so
// needs kcl_plugin.forge.
func deployEnabledConfig(t *testing.T) *config.ProjectConfig {
	t.Helper()
	cfg := &config.ProjectConfig{}
	enabled := true
	cfg.Features.Deploy = &enabled
	if !cfg.Features.DeployEnabled() {
		t.Fatalf("test setup: deploy feature did not enable; the FeaturesConfig shape has changed")
	}
	return cfg
}

func findCheck(results []doctor.CheckResult, name string) (doctor.CheckResult, bool) {
	for _, r := range results {
		if r.Name == name {
			return r, true
		}
	}
	return doctor.CheckResult{}, false
}

// TestSelfCapabilityFailsAndRemediatesWhenUnavailable is the check that
// would have caught the shipped defect. Doctor previously verified eleven
// external tools and reported all-clear on a CGO-free binary that could
// not render any environment. A failing binary must now say so, and say
// what to run.
func TestSelfCapabilityFailsAndRemediatesWhenUnavailable(t *testing.T) {
	caps := defaultSelfCapabilities()
	for i := range caps {
		if caps[i].Name == "kcl-plugin" {
			caps[i].Available = func() bool { return false }
		}
	}

	results := runSelfCapabilityChecks(caps, deployEnabledConfig(t), t.TempDir(), "v0.1.20")
	got, ok := findCheck(results, "forge: kcl-plugin")
	if !ok {
		t.Fatalf("no kcl-plugin check in report: %+v", results)
	}
	if got.Status != doctor.StatusFail {
		t.Fatalf("status = %q, want %q (a binary that cannot render must not report healthy)", got.Status, doctor.StatusFail)
	}
	for _, want := range []string{
		"without CGO",
		"kcl_plugin.forge",
		"CGO_ENABLED=1 go install github.com/reliant-labs/forge/cmd/forge@v0.1.20",
	} {
		if !strings.Contains(got.Message+"\n"+got.Evidence, want) {
			t.Errorf("report missing %q\n--- message ---\n%s\n--- evidence ---\n%s", want, got.Message, got.Evidence)
		}
	}
}

// TestSelfCapabilityPassesWhenAvailable — the check must not be a
// permanent red mark on the binaries forge actually ships.
func TestSelfCapabilityPassesWhenAvailable(t *testing.T) {
	caps := defaultSelfCapabilities()
	for i := range caps {
		caps[i].Available = func() bool { return true }
	}
	results := runSelfCapabilityChecks(caps, deployEnabledConfig(t), t.TempDir(), "v0.1.20")
	for _, r := range results {
		if r.Status != doctor.StatusPass {
			t.Errorf("%s: status = %q, want %q (message: %s)", r.Name, r.Status, doctor.StatusPass, r.Message)
		}
	}
}

// TestSelfCapabilitySkippedWhenDeployDisabled — a project that never
// renders genuinely does not need the plugin, and reporting a fail there
// would be noise that trains users to ignore the section.
func TestSelfCapabilitySkippedWhenDeployDisabled(t *testing.T) {
	caps := defaultSelfCapabilities()
	for i := range caps {
		caps[i].Available = func() bool { return false }
	}
	disabled := false
	cfg := &config.ProjectConfig{}
	cfg.Features.Deploy = &disabled

	results := runSelfCapabilityChecks(caps, cfg, t.TempDir(), "v0.1.20")
	got, ok := findCheck(results, "forge: kcl-plugin")
	if !ok {
		t.Fatalf("no kcl-plugin check in report: %+v", results)
	}
	if got.Status != doctor.StatusSkip {
		t.Fatalf("status = %q, want %q for a project with deploy disabled", got.Status, doctor.StatusSkip)
	}
}

// TestSelfCapabilityChecksSkipUnderSignalFilter mirrors the tool checks:
// `--signal` selects among deploy/metrics/traces/logs/profiles, and there
// is no self-capability signal, so a filtered run emits nothing here
// rather than smuggling an unrelated section into a scoped report.
func TestSelfCapabilityChecksSkipUnderSignalFilter(t *testing.T) {
	if got := runSelfCapabilityDoctorChecks(t.Context(), deployEnabledConfig(t), t.TempDir(), "deploy"); got != nil {
		t.Fatalf("filtered run emitted %d checks, want none", len(got))
	}
}
