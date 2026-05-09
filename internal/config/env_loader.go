package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// ErrEnvironmentNotFound is returned when an env name does not match any
// EnvironmentConfig in the project config and there is no sibling file
// to fall back on.
var ErrEnvironmentNotFound = errors.New("environment not found in forge.yaml or as a config.<env>.yaml sibling file")

// LoadEnvironmentConfig returns the merged per-environment config map for
// envName, layering the optional sibling file `config.<env>.yaml` on top
// of the inline `environments[<envName>].config` in forge.yaml.
//
// projectDir is the directory containing forge.yaml (used to locate the
// sibling file). cfg is the already-loaded ProjectConfig.
//
// Resolution order (later wins):
//  1. environments[<envName>].config from forge.yaml (the inline shape).
//  2. sibling file config.<envName>.yaml at projectDir.
//
// Both sources are optional. If neither exists and there is no matching
// EnvironmentConfig entry, [ErrEnvironmentNotFound] is returned. An empty
// non-nil map is returned when an env exists but defines no config.
func LoadEnvironmentConfig(cfg *ProjectConfig, projectDir, envName string) (map[string]any, error) {
	merged := map[string]any{}
	envExists := false

	// 1. Inline config from forge.yaml.
	for _, env := range cfg.Envs {
		if env.Name == envName {
			envExists = true
			for k, v := range env.Config {
				merged[k] = v
			}
			break
		}
	}

	// 2. Sibling file config.<env>.yaml (overrides inline).
	siblingPath := filepath.Join(projectDir, fmt.Sprintf("config.%s.yaml", envName))
	if data, err := os.ReadFile(siblingPath); err == nil {
		envExists = true
		var sibling map[string]any
		if err := yaml.Unmarshal(data, &sibling); err != nil {
			return nil, fmt.Errorf("parse %s: %w", siblingPath, err)
		}
		for k, v := range sibling {
			merged[k] = v
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", siblingPath, err)
	}

	if !envExists {
		return nil, fmt.Errorf("%w: %q", ErrEnvironmentNotFound, envName)
	}
	return merged, nil
}
