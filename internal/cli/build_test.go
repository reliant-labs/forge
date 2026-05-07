package cli

import (
	"testing"
)

func TestBuildDebugFlagExists(t *testing.T) {
	cmd := newBuildCmd()

	f := cmd.Flags().Lookup("debug")
	if f == nil {
		t.Fatal("--debug flag not registered on build command")
	}

	if f.DefValue != "false" {
		t.Errorf("--debug default = %q, want %q", f.DefValue, "false")
	}
}

func TestBuildDebugFlagParsesTrue(t *testing.T) {
	cmd := newBuildCmd()

	if err := cmd.Flags().Parse([]string{"--debug"}); err != nil {
		t.Fatalf("failed to parse --debug: %v", err)
	}

	val, err := cmd.Flags().GetBool("debug")
	if err != nil {
		t.Fatalf("GetBool(\"debug\") error: %v", err)
	}
	if !val {
		t.Error("expected --debug to be true after parsing --debug")
	}
}

func TestBuildDebugFlagDefaultIsFalse(t *testing.T) {
	cmd := newBuildCmd()

	// Parse with no flags — debug should remain false.
	if err := cmd.Flags().Parse([]string{}); err != nil {
		t.Fatalf("failed to parse empty args: %v", err)
	}

	val, err := cmd.Flags().GetBool("debug")
	if err != nil {
		t.Fatalf("GetBool(\"debug\") error: %v", err)
	}
	if val {
		t.Error("expected --debug to default to false")
	}
}

func TestBuildAllFlagsRegistered(t *testing.T) {
	cmd := newBuildCmd()

	expected := []struct {
		name     string
		defValue string
	}{
		{"output", "bin"},
		{"target", "all"},
		{"parallel", "true"},
		{"docker", "false"},
		{"debug", "false"},
	}

	for _, tt := range expected {
		f := cmd.Flags().Lookup(tt.name)
		if f == nil {
			t.Errorf("flag --%s not registered", tt.name)
			continue
		}
		if f.DefValue != tt.defValue {
			t.Errorf("flag --%s default = %q, want %q", tt.name, f.DefValue, tt.defValue)
		}
	}
}
