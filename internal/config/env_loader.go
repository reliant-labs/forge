package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// ErrEnvironmentNotFound is returned when no sibling `config.<env>.yaml`
// file exists for envName.
var ErrEnvironmentNotFound = errors.New("environment not found: no config.<env>.yaml sibling file")

// LoadEnvironmentConfig returns the per-environment config map loaded
// from the sibling file `config.<env>.yaml` next to forge.yaml.
//
// projectDir is the directory containing forge.yaml. envName names the
// environment (`dev`, `staging`, `prod`, …).
//
// Returns [ErrEnvironmentNotFound] when no `config.<env>.yaml` is
// present. Returns an empty map (and nil error) when the file is
// present but empty.
//
// Per-env app config (runtime AppConfig values keyed by snake_case
// proto field names) lives exclusively in sibling files now —
// forge.yaml `environments[]` is gone. Per-env deploy config
// (cluster/namespace/registry/domain) lives in KCL `forge.K8sCluster`
// blocks.
func LoadEnvironmentConfig(projectDir, envName string) (map[string]any, error) {
	siblingPath := filepath.Join(projectDir, fmt.Sprintf("config.%s.yaml", envName))
	data, err := os.ReadFile(siblingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %q", ErrEnvironmentNotFound, envName)
		}
		return nil, fmt.Errorf("read %s: %w", siblingPath, err)
	}
	merged := map[string]any{}
	if len(data) == 0 {
		return merged, nil
	}
	if err := yaml.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("parse %s: %w", siblingPath, err)
	}
	if merged == nil {
		merged = map[string]any{}
	}
	return merged, nil
}
