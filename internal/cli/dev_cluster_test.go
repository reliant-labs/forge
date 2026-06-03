package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadK3dClusterName_FromConfig verifies the canonical happy path:
// when deploy/k3d.yaml exists with a metadata.name, we read it.
func TestReadK3dClusterName_FromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "k3d.yaml")
	cfg := []byte("apiVersion: k3d.io/v1alpha5\nkind: Simple\nmetadata:\n  name: cp-forge\nservers: 1\n")
	if err := os.WriteFile(cfgPath, cfg, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	got, err := readK3dClusterName(cfgPath)
	if err != nil {
		t.Fatalf("readK3dClusterName: %v", err)
	}
	if got != "cp-forge" {
		t.Fatalf("want %q, got %q", "cp-forge", got)
	}
}

// TestReadK3dClusterName_MissingFile returns empty (no error) so the
// caller can fall back to forge.yaml's project name.
func TestReadK3dClusterName_MissingFile(t *testing.T) {
	got, err := readK3dClusterName(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("missing file should not be an error, got: %v", err)
	}
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

// TestReadK3dClusterName_EmptyMetadataName returns empty when the file
// exists but doesn't carry metadata.name.
func TestReadK3dClusterName_EmptyMetadataName(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "k3d.yaml")
	if err := os.WriteFile(cfgPath, []byte("servers: 1\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	got, err := readK3dClusterName(cfgPath)
	if err != nil {
		t.Fatalf("readK3dClusterName: %v", err)
	}
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

// TestNewDevCmd_Subtree confirms the dev parent registers every
// subcommand spec'd in the dev tree.
func TestNewDevCmd_Subtree(t *testing.T) {
	cmd := newDevCmd()
	want := map[string]bool{
		"cluster":      false,
		"status":       false,
		"logs":         false,
		"info":         false,
		"port-forward": false,
		"instances":    false,
	}
	for _, sub := range cmd.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("forge dev: missing %q subcommand", name)
		}
	}
}

// TestNewDevClusterCmd_Subtree confirms the cluster subtree registers
// every subcommand.
func TestNewDevClusterCmd_Subtree(t *testing.T) {
	cmd := newDevClusterCmd()
	want := map[string]bool{
		"up":     false,
		"down":   false,
		"status": false,
		"reset":  false,
		"reload": false,
	}
	for _, sub := range cmd.Commands() {
		if _, ok := want[sub.Name()]; ok {
			want[sub.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("forge dev cluster: missing %q subcommand", name)
		}
	}
}

// TestBoolUpDown covers the trivial formatter used in status output.
func TestBoolUpDown(t *testing.T) {
	if boolUpDown(true) != "up" {
		t.Errorf("true should render as 'up'")
	}
	if boolUpDown(false) != "down" {
		t.Errorf("false should render as 'down'")
	}
}
