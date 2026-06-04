package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// ErrProjectConfigNotFound is returned when forge.yaml does not exist.
var ErrProjectConfigNotFound = errors.New("forge.yaml not found in current directory (run 'forge new' to create a project)")

const defaultProjectConfigFile = "forge.yaml"

// findProjectConfigFile walks upward from the current working directory
// looking for forge.yaml, similar to how git/go locate their
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

// loadProjectConfig reads forge.yaml, walking up from the
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

	parsed, err := config.LoadStrict(data, path)
	if err != nil {
		return nil, err
	}
	cfg := *parsed

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
		// Frontend type canonical forms are hyphenated ("react-native",
		// "vite-spa"). Lowercase but keep hyphens; downstream comparisons
		// use EqualFold against the hyphenated literal.
		cfg.Frontends[i].Type = strings.ToLower(strings.TrimSpace(cfg.Frontends[i].Type))
		// Accept legacy snake_case spellings from earlier forge generators.
		switch cfg.Frontends[i].Type {
		case "react_native":
			cfg.Frontends[i].Type = "react-native"
		case "vite_spa":
			cfg.Frontends[i].Type = "vite-spa"
		}
	}

	// Normalize environment type casing (accept "LOCAL"/"CLOUD" from older
	// generators and canonicalize to lowercase).
	for i := range cfg.Envs {
		if cfg.Envs[i].Type != "" {
			cfg.Envs[i].Type = normalizeEnum(cfg.Envs[i].Type)
		}
	}

	// Deprecation notice — emitted once per process at load time so
	// the warning doesn't spam every reader. Suppressed when the
	// FORGE_SUPPRESS_ENVIRONMENTS_DEPRECATION env var is set (CI
	// pipelines that already track the migration via their backlog).
	maybeWarnEnvironmentsDeprecated(&cfg)

	return &cfg, nil
}

// environmentsDeprecationWarned tracks whether the deprecation notice
// has already fired in this process so it doesn't spam every call
// site that reads forge.yaml.
var environmentsDeprecationWarned bool

func maybeWarnEnvironmentsDeprecated(cfg *config.ProjectConfig) {
	if environmentsDeprecationWarned {
		return
	}
	if len(cfg.Envs) == 0 {
		return
	}
	if os.Getenv("FORGE_SUPPRESS_ENVIRONMENTS_DEPRECATION") != "" {
		return
	}
	environmentsDeprecationWarned = true
	fmt.Fprintln(os.Stderr,
		"[forge] notice: `environments[]` in forge.yaml is deprecated.\n"+
			"        Move env-wide deploy config (cluster/namespace/registry/domain)\n"+
			"        onto per-service `forge.K8sCluster` blocks in KCL — see the\n"+
			"        `environments-to-kcl` migration skill. Suppress this notice with\n"+
			"        FORGE_SUPPRESS_ENVIRONMENTS_DEPRECATION=1.")
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
