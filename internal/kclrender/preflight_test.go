package kclrender

import (
	"strings"
	"testing"
)

// TestPluginPreflightPassesWhenAvailable — the normal path must not add a
// gate. A false positive here would refuse every render on a good binary.
func TestPluginPreflightPassesWhenAvailable(t *testing.T) {
	if err := pluginPreflight(true, "v0.1.20"); err != nil {
		t.Fatalf("preflight refused a CGO binary: %v", err)
	}
}

// TestPluginPreflightIsActionableWhenUnavailable is the whole point of the
// preflight. Before it existed, a CGO-free forge failed with KCL's own
// message — "the plugin package 'kcl_plugin.forge' is not found ... confirm
// if plugin mode is enabled" — which names a knob forge does not have,
// never mentions CGO, and gave the user nothing to run. Each assertion
// below is one thing that message lacked.
func TestPluginPreflightIsActionableWhenUnavailable(t *testing.T) {
	err := pluginPreflight(false, "v0.1.20")
	if err == nil {
		t.Fatal("preflight allowed a render on a binary with no kcl_plugin.forge namespace")
	}
	msg := err.Error()

	for _, want := range []string{
		// Names the cause, which KCL's message does not.
		"without CGO",
		"kcl_plugin.forge",
		// Says what the consequence is, so the user does not go
		// hunting for a project-level misconfiguration.
		"no environment can be rendered",
		// The runbook shape forge errors follow: expected / found / Fix.
		"expected:",
		"found:",
		"Fix:",
		// The literal command to run — the part that makes this
		// actionable rather than merely descriptive.
		"CGO_ENABLED=1 go install github.com/reliant-labs/forge/cmd/forge@v0.1.20",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("preflight message missing %q\n--- message ---\n%s", want, msg)
		}
	}

	// It must not repeat KCL's misleading suggestion.
	if strings.Contains(msg, "plugin mode") {
		t.Errorf("preflight message suggests enabling 'plugin mode', a knob forge does not have:\n%s", msg)
	}
}

// TestPluginPreflightDevBuildFixIsRunnable — a "(devel)"/+dirty stamp
// names no ref a module proxy can serve, so `go install ...@<version>`
// would hand the user a command that fails. Dev builds get the
// contributor install instead. Mirrors kclvendor.DowngradeError's handling
// of the same hazard.
func TestPluginPreflightDevBuildFixIsRunnable(t *testing.T) {
	for _, version := range []string{"", "dev", "(devel)", "v0.1.20+dirty"} {
		msg := pluginPreflight(false, version).Error()
		if !strings.Contains(msg, "CGO_ENABLED=1 task install:dev") {
			t.Errorf("version %q: expected the contributor install hint\n%s", version, msg)
		}
		if strings.Contains(msg, "go install github.com/reliant-labs/forge/cmd/forge@") {
			t.Errorf("version %q: offered `go install ...@%s`, which no proxy can serve\n%s", version, version, msg)
		}
	}
}
