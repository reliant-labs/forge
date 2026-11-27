package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/exitcode"
)

// `forge dev air-build` is mounted under the dev noun, runs as this forge in
// any project (an older pinned forge has no such command, so a hand-off
// would fail every air build), and exits with the build's status.
func TestDevAirBuildCommand(t *testing.T) {
	root := newTestRootCmd(t)
	cmd, _, err := root.Find([]string{"dev", "air-build"})
	if err != nil || cmd.Name() != "air-build" {
		t.Fatalf("forge dev air-build is not a command: %v", err)
	}
	if mode := handOffModeFor(commandRel(cmd, root), false); mode != runHere {
		t.Fatalf("forge dev air-build is handed to the pinned forge (mode %d)", mode)
	}

	run := func(args ...string) error {
		c := newDevAirBuildCmd()
		var out, errb bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&errb)
		c.SetArgs(args)
		return c.Execute()
	}
	out := filepath.Join(t.TempDir(), "app")
	if err := run("--", "go", "version"); err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("no --out: %v, want it named", err)
	}
	if err := run("--out", out, "--", filepath.Join(t.TempDir(), "no-such-build")); exitcode.Of(err) != 127 {
		t.Fatalf("a build that cannot start: %v (exit %d), want 127", err, exitcode.Of(err))
	}
}
