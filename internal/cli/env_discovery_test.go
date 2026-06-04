package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestListEnvs_FromFilesystem confirms the kcl-dir walker finds every
// directory containing a `main.k` file — sorted alphabetically and
// deduped against ones without main.k.
func TestListEnvs_FromFilesystem(t *testing.T) {
	dir := t.TempDir()
	for _, env := range []string{"dev", "staging", "prod"} {
		envDir := filepath.Join(dir, "deploy", "kcl", env)
		if err := os.MkdirAll(envDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(envDir, "main.k"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One env dir without a main.k — should be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "deploy", "kcl", "no-main"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ListEnvs(dir)
	if err != nil {
		t.Fatalf("ListEnvs: %v", err)
	}
	want := []string{"dev", "prod", "staging"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("envs[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestListEnvs_MissingKCLDir returns an empty list (no error) when
// the deploy/kcl dir doesn't exist. Brand-new projects, library-kind
// projects, and projects mid-migration all land here.
func TestListEnvs_MissingKCLDir(t *testing.T) {
	got, err := ListEnvs(t.TempDir())
	if err != nil {
		t.Fatalf("ListEnvs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %v", got)
	}
}

// TestEnvExists confirms the boolean variant — true when main.k is
// present, false otherwise.
func TestEnvExists(t *testing.T) {
	dir := t.TempDir()
	envDir := filepath.Join(dir, "deploy", "kcl", "dev")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "main.k"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	ok, err := EnvExists(dir, "dev")
	if err != nil || !ok {
		t.Errorf("expected dev=true, got (%v, %v)", ok, err)
	}
	ok, err = EnvExists(dir, "nope")
	if err != nil || ok {
		t.Errorf("expected nope=false, got (%v, %v)", ok, err)
	}
}

// TestListEnvsForConfig_KCLPreferred confirms that when KCL declares
// envs, those win over forge.yaml. The yaml entries are merged in for
// the in-flight migration case but kcl is the source of truth.
func TestListEnvsForConfig_KCLPreferred(t *testing.T) {
	dir := t.TempDir()
	for _, env := range []string{"dev", "prod"} {
		envDir := filepath.Join(dir, "deploy", "kcl", env)
		if err := os.MkdirAll(envDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(envDir, "main.k"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.ProjectConfig{
		Envs: []config.EnvironmentConfig{
			{Name: "dev"},
			{Name: "staging"}, // yaml-only env — picked up via merge
		},
	}
	got := ListEnvsForConfig(dir, cfg)
	want := map[string]bool{"dev": true, "prod": true, "staging": true}
	if len(got) != 3 {
		t.Fatalf("len: got %v want 3", got)
	}
	for _, e := range got {
		if !want[e] {
			t.Errorf("unexpected env %q", e)
		}
	}
}

// TestListEnvsForConfig_FallbackToYAML confirms forge.yaml is the
// fallback when no KCL envs are present (legacy projects).
func TestListEnvsForConfig_FallbackToYAML(t *testing.T) {
	cfg := &config.ProjectConfig{
		Envs: []config.EnvironmentConfig{
			{Name: "dev"}, {Name: "prod"},
		},
	}
	got := ListEnvsForConfig(t.TempDir(), cfg)
	if len(got) != 2 {
		t.Fatalf("want 2 envs, got %v", got)
	}
}
