package codegen

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ForgeDescriptor is the JSON structure written by protoc-gen-forge --mode=descriptor.
type ForgeDescriptor struct {
	Services []ServiceDef    `json:"services"`
	Entities []EntityDef     `json:"entities"`
	Configs  []ConfigMessage `json:"configs"`
}

// loadDescriptor reads and parses the forge_descriptor.json file from the gen/ directory.
func loadDescriptor(projectDir string) (*ForgeDescriptor, error) {
	descPath := filepath.Join(projectDir, "gen", "forge_descriptor.json")
	data, err := os.ReadFile(descPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No descriptor yet — not an error
		}
		return nil, fmt.Errorf("read forge descriptor: %w", err)
	}

	var desc ForgeDescriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return nil, fmt.Errorf("parse forge descriptor: %w", err)
	}
	return &desc, nil
}

// ParseServicesFromProtos reads service definitions from the forge descriptor.
// Falls back to empty if the descriptor does not exist yet.
func ParseServicesFromProtos(dir string, projectDir string) ([]ServiceDef, error) {
	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		log.Println("Info: forge_descriptor.json not found — run 'forge generate' with protoc-gen-forge to produce it")
		return nil, nil
	}

	// Set ModulePath on each service from the project's go.mod
	modulePath, modErr := GetModulePath(projectDir)
	if modErr == nil {
		for i := range desc.Services {
			desc.Services[i].ModulePath = modulePath
		}
	}

	return desc.Services, nil
}

// ParseEntityProtos reads entity definitions from the forge descriptor.
// Falls back to empty if the descriptor does not exist yet.
func ParseEntityProtos(projectDir string) ([]EntityDef, error) {
	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		log.Println("Info: forge_descriptor.json not found — run 'forge generate' with protoc-gen-forge to produce it")
		return nil, nil
	}
	return desc.Entities, nil
}

// ParseConfigProto reads config messages from the forge descriptor,
// filtering to those from a specific proto file path.
func ParseConfigProto(protoPath string) ([]ConfigMessage, error) {
	// Infer project dir from the proto path
	projectDir := protoPath
	for {
		parent := filepath.Dir(projectDir)
		if parent == projectDir {
			break
		}
		projectDir = parent
		if _, err := os.Stat(filepath.Join(projectDir, "go.mod")); err == nil {
			break
		}
	}

	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, nil
	}
	return desc.Configs, nil
}

// ParseConfigProtosFromDir reads all config messages from the forge descriptor.
// Falls back to empty if the descriptor does not exist yet.
func ParseConfigProtosFromDir(dir string) ([]ConfigMessage, error) {
	// Walk up from dir to find the project root (contains go.mod)
	projectDir := dir
	for {
		if _, err := os.Stat(filepath.Join(projectDir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(projectDir)
		if parent == projectDir {
			// Couldn't find go.mod — use the original dir
			projectDir = dir
			break
		}
		projectDir = parent
	}

	desc, err := loadDescriptor(projectDir)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		log.Println("Info: forge_descriptor.json not found — run 'forge generate' with protoc-gen-forge to produce it")
		return nil, nil
	}
	return desc.Configs, nil
}

// GetModulePath reads the module path from go.mod in the given directory.
func GetModulePath(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "module ") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "module ")), nil
		}
	}

	return "", fmt.Errorf("module directive not found in go.mod")
}

// projectBinaryShared best-effort reads forge.yaml and returns true when
// the project declares `binary: shared`. Used by the bootstrap generator
// to lazily construct services in the per-service cobra subcommand path.
// Empty file / parse error / missing field all fall back to false (the
// canonical per-service mode), so this is safe to call from any project
// shape including the initial scaffold pass before forge.yaml is fully
// populated.
func projectBinaryShared(projectDir string) bool {
	path := filepath.Join(projectDir, "forge.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// Lightweight inline parse so the codegen package doesn't take a
	// dependency on the `config` package (which would create an import
	// cycle: config → codegen via descriptor types). The spec is a single
	// top-level scalar key, so a string scan is sufficient.
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "binary:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, "binary:"))
		// Strip trailing comment + quotes.
		if idx := strings.Index(val, "#"); idx >= 0 {
			val = strings.TrimSpace(val[:idx])
		}
		val = strings.Trim(val, `"'`)
		return strings.EqualFold(val, "shared")
	}
	return false
}
