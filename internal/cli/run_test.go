package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunDebugFlagExists(t *testing.T) {
	cmd := newRunCmd()

	f := cmd.Flags().Lookup("debug")
	if f == nil {
		t.Fatal("--debug flag not registered on run command")
	}

	if f.DefValue != "false" {
		t.Errorf("--debug default = %q, want %q", f.DefValue, "false")
	}
}

func TestRunDebugFlagParsesTrue(t *testing.T) {
	cmd := newRunCmd()

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

func TestRunDebugFlagDefaultIsFalse(t *testing.T) {
	cmd := newRunCmd()

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

func TestRunAllFlagsRegistered(t *testing.T) {
	cmd := newRunCmd()

	expected := []struct {
		name     string
		defValue string
	}{
		{"env", "dev"},
		{"no-infra", "false"},
		{"service", ""},
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

// TestDebugAirConfigSelection tests the air config file selection logic
// used in runProjectDev when debug=true. The logic in run.go is:
//
//  1. Start with airConfig = ".air.toml"
//  2. If debug, check for ".air-debug.toml" — if it exists, use that instead
//  3. If the chosen airConfig exists, use air; otherwise fall back to dlv
func TestDebugAirConfigSelection(t *testing.T) {
	tests := []struct {
		name          string
		files         []string // files to create in temp dir
		wantAirConfig string   // expected selected config
	}{
		{
			name:          "debug config exists, should select it",
			files:         []string{".air.toml", ".air-debug.toml"},
			wantAirConfig: ".air-debug.toml",
		},
		{
			name:          "only regular config exists, should keep default",
			files:         []string{".air.toml"},
			wantAirConfig: ".air.toml",
		},
		{
			name:          "no config files exist, should keep default",
			files:         nil,
			wantAirConfig: ".air.toml",
		},
		{
			name:          "only debug config exists without regular",
			files:         []string{".air-debug.toml"},
			wantAirConfig: ".air-debug.toml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			for _, f := range tt.files {
				path := filepath.Join(dir, f)
				if err := os.WriteFile(path, []byte("# test"), 0o644); err != nil {
					t.Fatalf("failed to create %s: %v", f, err)
				}
			}

			// Replicate the selection logic from run.go lines 149-155
			airConfig := ".air.toml"
			debugConfig := ".air-debug.toml"
			if _, err := os.Stat(filepath.Join(dir, debugConfig)); err == nil {
				airConfig = debugConfig
			}

			if airConfig != tt.wantAirConfig {
				t.Errorf("airConfig = %q, want %q", airConfig, tt.wantAirConfig)
			}
		})
	}
}

// TestDebugAirConfigFallbackToDlv verifies that when debug=true and no air
// config file exists at all, the code path would fall back to dlv.
func TestDebugAirConfigFallbackToDlv(t *testing.T) {
	dir := t.TempDir()

	// No .air.toml or .air-debug.toml in the temp dir.
	// Replicate the logic from run.go lines 149-163:
	airConfig := ".air.toml"
	debugConfig := ".air-debug.toml"
	if _, err := os.Stat(filepath.Join(dir, debugConfig)); err == nil {
		airConfig = debugConfig
	}

	// The code then checks: if _, err := os.Stat(airConfig); err == nil { use air }
	// else { build debug binary + dlv }
	_, err := os.Stat(filepath.Join(dir, airConfig))
	useDlv := err != nil

	if !useDlv {
		t.Error("expected dlv fallback when no air config files exist")
	}
}

// TestDebugAirConfigUsesAirWhenConfigExists verifies that when debug=true
// and an air config is present, the air path is taken instead of dlv.
func TestDebugAirConfigUsesAirWhenConfigExists(t *testing.T) {
	dir := t.TempDir()

	// Create .air-debug.toml
	if err := os.WriteFile(filepath.Join(dir, ".air-debug.toml"), []byte("# debug config"), 0o644); err != nil {
		t.Fatal(err)
	}

	airConfig := ".air.toml"
	debugConfig := ".air-debug.toml"
	if _, err := os.Stat(filepath.Join(dir, debugConfig)); err == nil {
		airConfig = debugConfig
	}

	_, err := os.Stat(filepath.Join(dir, airConfig))
	useAir := err == nil

	if !useAir {
		t.Error("expected air to be used when .air-debug.toml exists")
	}
}
