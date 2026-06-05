package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadEnvironmentConfig_SiblingPresent confirms the sibling file
// at config.<env>.yaml is loaded and returned as the per-env config
// map.
func TestLoadEnvironmentConfig_SiblingPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.prod.yaml"), []byte(`
database_url: ${DATABASE_URL_SECRET}
log_level: warn
`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnvironmentConfig(dir, "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["database_url"] != "${DATABASE_URL_SECRET}" {
		t.Errorf("database_url = %v, want sibling value", got["database_url"])
	}
	if got["log_level"] != "warn" {
		t.Errorf("log_level = %v, want warn", got["log_level"])
	}
}

// TestLoadEnvironmentConfig_EmptyFile returns an empty (non-nil) map
// when the sibling file exists but is empty.
func TestLoadEnvironmentConfig_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.dev.yaml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEnvironmentConfig(dir, "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("want empty map, got %v", got)
	}
}

// TestLoadEnvironmentConfig_MissingFile reports ErrEnvironmentNotFound
// when no sibling file exists for the requested env.
func TestLoadEnvironmentConfig_MissingFile(t *testing.T) {
	_, err := LoadEnvironmentConfig(t.TempDir(), "nope")
	if !errors.Is(err, ErrEnvironmentNotFound) {
		t.Errorf("want ErrEnvironmentNotFound, got %v", err)
	}
}
