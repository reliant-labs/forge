package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvironmentConfig_InlineOnly(t *testing.T) {
	cfg := &ProjectConfig{
		Envs: []EnvironmentConfig{{
			Name: "dev",
			Type: "local",
			Config: map[string]any{
				"database_url": "postgres://localhost:5432/myapp",
				"log_level":    "debug",
			},
		}},
	}

	got, err := LoadEnvironmentConfig(cfg, t.TempDir(), "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["database_url"] != "postgres://localhost:5432/myapp" {
		t.Errorf("database_url = %v, want postgres://localhost:5432/myapp", got["database_url"])
	}
	if got["log_level"] != "debug" {
		t.Errorf("log_level = %v, want debug", got["log_level"])
	}
}

func TestLoadEnvironmentConfig_SiblingOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.prod.yaml"), []byte(`
database_url: ${DATABASE_URL_SECRET}
log_level: warn
extra_key: from-sibling
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &ProjectConfig{
		Envs: []EnvironmentConfig{{
			Name: "prod",
			Type: "cloud",
			Config: map[string]any{
				"database_url": "ignored-by-sibling",
				"log_level":    "info",
				"only_inline":  "kept",
			},
		}},
	}

	got, err := LoadEnvironmentConfig(cfg, dir, "prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got["database_url"] != "${DATABASE_URL_SECRET}" {
		t.Errorf("database_url = %v, want sibling value", got["database_url"])
	}
	if got["log_level"] != "warn" {
		t.Errorf("log_level = %v, want warn (sibling)", got["log_level"])
	}
	if got["only_inline"] != "kept" {
		t.Errorf("only_inline = %v, want kept (inline preserved when sibling absent)", got["only_inline"])
	}
	if got["extra_key"] != "from-sibling" {
		t.Errorf("extra_key = %v, want from-sibling", got["extra_key"])
	}
}

func TestLoadEnvironmentConfig_SiblingOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.staging.yaml"), []byte(`
database_url: postgres://staging-db:5432/app
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// No matching EnvironmentConfig — sibling file alone is enough.
	cfg := &ProjectConfig{}
	got, err := LoadEnvironmentConfig(cfg, dir, "staging")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["database_url"] != "postgres://staging-db:5432/app" {
		t.Errorf("database_url = %v, want sibling-only value", got["database_url"])
	}
}

func TestLoadEnvironmentConfig_EmptyEnvReturnsEmptyMap(t *testing.T) {
	cfg := &ProjectConfig{
		Envs: []EnvironmentConfig{{Name: "dev"}},
	}
	got, err := LoadEnvironmentConfig(cfg, t.TempDir(), "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty map, got %v", got)
	}
}

func TestLoadEnvironmentConfig_MissingEnv(t *testing.T) {
	cfg := &ProjectConfig{}
	_, err := LoadEnvironmentConfig(cfg, t.TempDir(), "nope")
	if !errors.Is(err, ErrEnvironmentNotFound) {
		t.Errorf("want ErrEnvironmentNotFound, got %v", err)
	}
}
