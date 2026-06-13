package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/projectstore"
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

// loadProjectStore reads forge.yaml (walking up from cwd) and returns a
// projectstore.ProjectStore — the single read+mutate surface consumers
// route through. It is the store-returning sibling of loadProjectConfig;
// new code should prefer it so nothing outside the store impl holds a
// *config.ProjectConfig.
func loadProjectStore() (projectstore.ProjectStore, error) {
	cfg, err := loadProjectConfig()
	if err != nil {
		return nil, err
	}
	return projectstore.New(cfg), nil
}

// loadProjectStoreFrom reads and wraps a project config at the given path
// in a ProjectStore. Sibling of loadProjectConfigFrom.
func loadProjectStoreFrom(path string) (projectstore.ProjectStore, error) {
	cfg, err := loadProjectConfigFrom(path)
	if err != nil {
		return nil, err
	}
	return projectstore.New(cfg), nil
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

	// Per-component entities live in the project-root components.json
	// sibling of forge.yaml (forge.yaml is global-only). Read it if
	// present; absent is valid (a project with no components is a library).
	componentsJSON, err := readComponentsJSONFile(filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	parsed, err := config.LoadProject(data, componentsJSON, path)
	if err != nil {
		return nil, err
	}
	cfg := *parsed

	// Apply kind-derived path defaults and normalize the kind enum
	// casing. An empty kind defaults to "server" (the historical
	// type: go_service default).
	for i := range cfg.Components {
		if cfg.Components[i].Kind == "" {
			cfg.Components[i].Kind = config.ComponentKindServer
		} else {
			cfg.Components[i].Kind = normalizeEnum(cfg.Components[i].Kind)
		}
		if cfg.Components[i].Path == "" {
			cfg.Components[i].Path = defaultServicePath(cfg.Components[i])
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

	return &cfg, nil
}

// readComponentsJSONFile reads the project-root components.json from dir
// (the directory holding forge.yaml). A missing file is not an error — it
// returns nil bytes, which config.LoadProject treats as "no components"
// (a pure library, or a fresh service before its first `forge add`).
func readComponentsJSONFile(dir string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, config.ComponentsFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", config.ComponentsFileName, err)
	}
	return data, nil
}

// normalizeEnum canonicalizes an enum-like YAML string value to lowercase
// snake_case. This lets old projects using upper-case or hyphenated enum
// spellings continue to work alongside newly generated projects.
func normalizeEnum(v string) string {
	return strings.ToLower(strings.ReplaceAll(v, "-", "_"))
}
