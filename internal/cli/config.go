package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/config"
)

// ErrProjectConfigNotFound is returned when forge.project.yaml does not exist.
var ErrProjectConfigNotFound = errors.New("forge.project.yaml not found in current directory (run 'forge new' to create a project)")

const defaultProjectConfigFile = "forge.project.yaml"

// findProjectConfigFile walks upward from the current working directory
// looking for forge.project.yaml, similar to how git/go locate their
// configuration. It returns the absolute path to the config file or
// ErrProjectConfigNotFound if no config is found before reaching the
// filesystem root.
func findProjectConfigFile() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	dir := cwd
	for {
		candidate := filepath.Join(dir, defaultProjectConfigFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrProjectConfigNotFound
		}
		dir = parent
	}
}

// loadProjectConfig reads forge.project.yaml, walking up from the
// current working directory until it finds one (or hits the filesystem
// root). Returns ErrProjectConfigNotFound when no config is found.
func loadProjectConfig() (*config.ProjectConfig, error) {
	path, err := findProjectConfigFile()
	if err != nil {
		return nil, err
	}
	return loadProjectConfigFrom(path)
}

// loadProjectConfigFrom reads and parses a project config from the given path.
// Returns ErrProjectConfigNotFound when the file does not exist.
func loadProjectConfigFrom(path string) (*config.ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrProjectConfigNotFound
		}
		return nil, fmt.Errorf("failed to read project config: %w", err)
	}

	var cfg config.ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse project config %s: %w", path, err)
	}

	if cfg.Name == "" {
		return nil, fmt.Errorf("project config %s is missing required field: name", path)
	}

	// Apply defaults for service paths and normalize enum casing. Older
	// generators wrote "GO_SERVICE"; new ones write "go_service". Accept
	// both by canonicalizing to lowercase snake_case on load.
	for i := range cfg.Services {
		if cfg.Services[i].Path == "" {
			cfg.Services[i].Path = "handlers/" + cfg.Services[i].Name
		}
		if cfg.Services[i].Type == "" {
			cfg.Services[i].Type = "go_service"
		} else {
			cfg.Services[i].Type = normalizeEnum(cfg.Services[i].Type)
		}
	}

	// Apply defaults for frontend paths
	for i := range cfg.Frontends {
		if cfg.Frontends[i].Path == "" {
			cfg.Frontends[i].Path = "frontends/" + cfg.Frontends[i].Name
		}
		if cfg.Frontends[i].Type == "" {
			cfg.Frontends[i].Type = "nextjs"
			continue
		}
		cfg.Frontends[i].Type = normalizeEnum(cfg.Frontends[i].Type)
	}

	// Normalize environment type casing (accept "LOCAL"/"CLOUD" from older
	// generators and canonicalize to lowercase).
	for i := range cfg.Envs {
		if cfg.Envs[i].Type != "" {
			cfg.Envs[i].Type = normalizeEnum(cfg.Envs[i].Type)
		}
	}

	// Normalize Kubernetes provider casing (accept "K3D" from older
	// generators and canonicalize to lowercase).
	if cfg.K8s.Provider != "" {
		cfg.K8s.Provider = normalizeEnum(cfg.K8s.Provider)
	}

	return &cfg, nil
}

// normalizeEnum canonicalizes an enum-like YAML string value to lowercase
// snake_case. This lets old projects using upper-case or hyphenated enum
// spellings continue to work alongside newly generated projects.
func normalizeEnum(v string) string {
	return strings.ToLower(strings.ReplaceAll(v, "-", "_"))
}

// findEnvironment looks up an environment by name from the project config.
func findEnvironment(c *config.ProjectConfig, name string) *config.EnvironmentConfig {
	for i := range c.Envs {
		if c.Envs[i].Name == name {
			return &c.Envs[i]
		}
	}
	return nil
}