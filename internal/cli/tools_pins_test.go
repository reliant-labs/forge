package cli

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// TestToolPinsNameForgesGolangciLint: `forge tools pins golangci-lint` is
// how a CI workflow reads the golangci-lint `forge lint` gates with, so it
// must be the very constant forge lint builds and installs.
func TestToolPinsNameForgesGolangciLint(t *testing.T) {
	pins := toolPins()
	if got := pins["golangci-lint"]; got != templates.GolangciLintVersion {
		t.Errorf("pins[golangci-lint] = %q, want %q", got, templates.GolangciLintVersion)
	}
	if got := pins["buf"]; got != templates.BufVersion {
		t.Errorf("pins[buf] = %q, want %q", got, templates.BufVersion)
	}
	if got := pins["syft"]; got != templates.SyftVersion {
		t.Errorf("pins[syft] = %q, want %q", got, templates.SyftVersion)
	}
	if got := pins["grype"]; got != templates.GrypeVersion {
		t.Errorf("pins[grype] = %q, want %q", got, templates.GrypeVersion)
	}

	cmd := newToolsPinsCmd()
	cmd.SetArgs([]string{"no-such-tool"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "golangci-lint") {
		t.Errorf("an unknown tool must fail naming the known ones, got %v", err)
	}
}

// TestToolsInstallKnowsTheLintTools: golangci-lint and buf are installable by
// name, and an unknown name fails before anything is installed.
func TestToolsInstallKnowsTheLintTools(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	err := runToolsInstall(context.Background(), "", false, "no-such-tool")
	if err == nil {
		t.Fatal("an unknown tool name installed nothing and passed")
	}
	for _, want := range []string{"golangci-lint", "buf", "protoc-gen-go", "syft", "grype"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must list %q among the installable tools: %v", want, err)
		}
	}
}
