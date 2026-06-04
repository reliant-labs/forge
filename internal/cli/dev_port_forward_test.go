package cli

import (
	"testing"
)

// TestDevPortForwardBackgroundFlagRegistered confirms the --background
// flag is wired into the port-forward command so orchestration scripts
// (cloud-dev.sh and friends) can detach without hand-rolling a `nohup &`.
func TestDevPortForwardBackgroundFlagRegistered(t *testing.T) {
	cmd := newDevPortForwardCmd()
	f := cmd.Flags().Lookup("background")
	if f == nil {
		t.Fatal("--background flag not registered on port-forward command")
	}
	if f.DefValue != "false" {
		t.Errorf("--background default = %q, want %q", f.DefValue, "false")
	}
}

// TestDevPortForwardStopSubcommandRegistered confirms `port-forward
// stop` exists as a child command and is the canonical way to tear
// down background forwards.
func TestDevPortForwardStopSubcommandRegistered(t *testing.T) {
	cmd := newDevPortForwardCmd()
	var stop = false
	for _, sub := range cmd.Commands() {
		if sub.Use == "stop" {
			stop = true
			break
		}
	}
	if !stop {
		t.Error("`stop` subcommand not registered on port-forward command")
	}
}

// TestDevPortForwardBackgroundParsesTrue ensures the flag parses
// without the long path through k3d/kubectl — guards against a typo
// in the flag wiring breaking script users.
func TestDevPortForwardBackgroundParsesTrue(t *testing.T) {
	cmd := newDevPortForwardCmd()
	if err := cmd.Flags().Parse([]string{"--background"}); err != nil {
		t.Fatalf("parse --background: %v", err)
	}
	val, err := cmd.Flags().GetBool("background")
	if err != nil {
		t.Fatalf("GetBool: %v", err)
	}
	if !val {
		t.Error("expected --background to be true after parsing")
	}
}
