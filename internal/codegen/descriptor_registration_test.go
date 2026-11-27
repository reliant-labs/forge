package codegen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDescriptorBuilderRegistrationLastWins(t *testing.T) {
	oldBuilder, oldPolicyBuilder := descriptorBuilder, descriptorContextBuilder
	t.Cleanup(func() {
		descriptorBuilder, descriptorContextBuilder = oldBuilder, oldPolicyBuilder
	})

	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, "proto"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "proto", "test.proto"), []byte("syntax = \"proto3\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	legacyErr := errors.New("legacy builder called")
	policyErr := errors.New("policy builder called")
	legacy := func(dir string) error {
		if dir != projectDir {
			t.Errorf("builder directory = %q, want %q", dir, projectDir)
		}
		return legacyErr
	}
	policy := func(dir string, quiet bool) error {
		if dir != projectDir || !quiet {
			t.Errorf("policy builder arguments = (%q, %v), want (%q, true)", dir, quiet, projectDir)
		}
		return policyErr
	}

	t.Run("legacy then policy", func(t *testing.T) {
		RegisterDescriptorBuilder(legacy)
		RegisterDescriptorPolicyBuilder(policy)
		if descriptorBuilder != nil {
			t.Fatal("policy registration retained the legacy builder")
		}
		if _, err := LoadDescriptorWithQuiet(projectDir, true); !errors.Is(err, policyErr) {
			t.Fatalf("load error = %v, want policy builder error", err)
		}
	})

	t.Run("policy then legacy", func(t *testing.T) {
		RegisterDescriptorPolicyBuilder(policy)
		RegisterDescriptorBuilder(legacy)
		if descriptorContextBuilder != nil {
			t.Fatal("legacy registration retained the policy builder")
		}
		if _, err := LoadDescriptorWithQuiet(projectDir, true); !errors.Is(err, legacyErr) {
			t.Fatalf("load error = %v, want legacy builder error", err)
		}
	})
}
