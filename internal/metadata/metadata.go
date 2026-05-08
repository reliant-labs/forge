package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProjectMetadata holds project configuration metadata
type ProjectMetadata struct {
	Name       string `json:"name"`
	ModulePath string `json:"module_path"`
	CreatedAt  string `json:"created_at"`
	Version    string `json:"version"`
	Generator  string `json:"generator"`
}

// Load reads project metadata from .reliant/project.json
func Load(projectRoot string) (*ProjectMetadata, error) {
	metadataPath := filepath.Join(projectRoot, ".reliant", "project.json")
	
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata: %w", err)
	}

	var meta ProjectMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return &meta, nil
}

// LoadFromCwd loads project metadata from current working directory
func LoadFromCwd() (*ProjectMetadata, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get current directory: %w", err)
	}
	
	return Load(cwd)
}

// FindProjectRoot searches up the directory tree for .reliant/project.json
func FindProjectRoot(startPath string) (string, error) {
	currentPath, err := filepath.Abs(startPath)
	if err != nil {
		return "", err
	}

	for {
		metadataPath := filepath.Join(currentPath, ".reliant", "project.json")
		if _, err := os.Stat(metadataPath); err == nil {
			return currentPath, nil
		}

		parent := filepath.Dir(currentPath)
		if parent == currentPath {
			// Reached root directory
			return "", fmt.Errorf("no .reliant/project.json found")
		}
		currentPath = parent
	}
}

// LoadFromCurrentOrParent loads metadata, searching up the directory tree
func LoadFromCurrentOrParent() (*ProjectMetadata, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get current directory: %w", err)
	}

	root, err := FindProjectRoot(cwd)
	if err != nil {
		return nil, "", err
	}

	meta, err := Load(root)
	if err != nil {
		return nil, "", err
	}

	return meta, root, nil
}
