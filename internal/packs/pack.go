// Package packs implements the pack system: pre-built, opinionated
// implementations that Forge can install into a project. Think of a
// pack like a Rails generator gem — it adds real, working code for a
// specific concern (auth, payments, email, etc.).
package packs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// Pack represents a loadable pack with its manifest and embedded templates.
type Pack struct {
	Name         string     `yaml:"name"`
	Description  string     `yaml:"description"`
	Version      string     `yaml:"version"`
	Config       PackConfig `yaml:"config"`
	Files        []PackFile `yaml:"files"`
	Dependencies []string   `yaml:"dependencies"`
	Generate     []PackFile `yaml:"generate"`
}

// PackConfig describes the configuration section a pack adds to
// forge.yaml.
type PackConfig struct {
	Section  string         `yaml:"section"`
	Defaults map[string]any `yaml:"defaults"`
}

// PackFile describes a single template→output file mapping.
type PackFile struct {
	Template    string `yaml:"template"`
	Output      string `yaml:"output"`
	Overwrite   string `yaml:"overwrite"`   // "always" | "once" | "never"
	Description string `yaml:"description"` // optional human description
}

// LoadPack loads a pack manifest from the embedded filesystem.
func LoadPack(name string) (*Pack, error) {
	data, err := packsFS.ReadFile(filepath.Join(name, "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("pack %q not found: %w", name, err)
	}

	var p Pack
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pack %q manifest: %w", name, err)
	}

	return &p, nil
}

// Install renders and writes pack files into the project, adds go
// dependencies, and records the pack in forge.yaml.
func (p *Pack) Install(projectDir string, cfg *config.ProjectConfig) error {
	// Check if already installed
	for _, installed := range cfg.Packs {
		if installed == p.Name {
			return fmt.Errorf("pack %q is already installed", p.Name)
		}
	}

	// Build template data from project config
	data := map[string]any{
		"ModulePath":  cfg.ModulePath,
		"ProjectName": cfg.Name,
		"PackConfig":  p.Config.Defaults,
	}

	// Render and write each file
	for _, f := range p.Files {
		if err := p.renderFile(f, projectDir, data); err != nil {
			return fmt.Errorf("render file %s: %w", f.Output, err)
		}
	}

	// Add go dependencies
	for _, dep := range p.Dependencies {
		fmt.Printf("  Adding dependency: %s\n", dep)
		cmd := exec.Command("go", "get", dep)
		cmd.Dir = projectDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go get %s: %w", dep, err)
		}
	}

	// Record pack in config
	cfg.Packs = append(cfg.Packs, p.Name)

	// Run go mod tidy
	fmt.Println("  Running go mod tidy...")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}

	return nil
}

// Remove deletes files created by the pack and removes it from the
// project config. Go dependencies are left in place since they may be
// used by other code.
func (p *Pack) Remove(projectDir string, cfg *config.ProjectConfig) error {
	// Delete files created by the pack
	for _, f := range p.Files {
		target := filepath.Join(projectDir, f.Output)
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			fmt.Printf("  Warning: could not remove %s: %v\n", f.Output, err)
		} else if err == nil {
			fmt.Printf("  Removed: %s\n", f.Output)
		}
	}

	// Also remove generate-hook outputs
	for _, f := range p.Generate {
		target := filepath.Join(projectDir, f.Output)
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			fmt.Printf("  Warning: could not remove %s: %v\n", f.Output, err)
		} else if err == nil {
			fmt.Printf("  Removed: %s\n", f.Output)
		}
	}

	// Remove from packs list
	filtered := cfg.Packs[:0]
	for _, name := range cfg.Packs {
		if name != p.Name {
			filtered = append(filtered, name)
		}
	}
	cfg.Packs = filtered

	return nil
}

// RenderGenerateFiles re-renders the pack's generate-hook templates.
// Called during `forge generate` to keep pack-generated code up to date.
func (p *Pack) RenderGenerateFiles(projectDir string, cfg *config.ProjectConfig) error {
	data := map[string]any{
		"ModulePath":  cfg.ModulePath,
		"ProjectName": cfg.Name,
		"PackConfig":  p.Config.Defaults,
	}

	for _, f := range p.Generate {
		if err := p.renderFile(f, projectDir, data); err != nil {
			return fmt.Errorf("render generate file %s: %w", f.Output, err)
		}
	}
	return nil
}

// renderFile renders a single template file and writes it to the project.
func (p *Pack) renderFile(f PackFile, projectDir string, data map[string]any) error {
	target := filepath.Join(projectDir, f.Output)

	// Check overwrite policy
	if f.Overwrite == "never" || f.Overwrite == "once" {
		if _, err := os.Stat(target); err == nil {
			if f.Overwrite == "never" {
				fmt.Printf("  Skipping (exists): %s\n", f.Output)
				return nil
			}
			// "once" means skip if already written by pack before
			// For simplicity, treat same as "never" on re-install
			fmt.Printf("  Skipping (already exists): %s\n", f.Output)
			return nil
		}
	}

	// Render template using the shared template engine
	basePath := filepath.Join(p.Name, "templates")
	content, err := templates.RenderFromFS(packsFS, basePath, f.Template, data)
	if err != nil {
		return fmt.Errorf("render template %s: %w", f.Template, err)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("create directory for %s: %w", f.Output, err)
	}

	// Write the file
	if err := os.WriteFile(target, content, 0644); err != nil {
		return fmt.Errorf("write %s: %w", f.Output, err)
	}

	fmt.Printf("  Created: %s\n", f.Output)
	return nil
}

// IsInstalled checks whether a pack is in the installed list.
func IsInstalled(name string, cfg *config.ProjectConfig) bool {
	for _, p := range cfg.Packs {
		if p == name {
			return true
		}
	}
	return false
}

// InstalledPacks returns the list of Pack structs for all installed packs.
func InstalledPacks(cfg *config.ProjectConfig) ([]*Pack, error) {
	var result []*Pack
	for _, name := range cfg.Packs {
		pack, err := LoadPack(name)
		if err != nil {
			// Pack was removed from Forge but still referenced in config
			fmt.Fprintf(os.Stderr, "  Warning: installed pack %q not found: %v\n", name, err)
			continue
		}
		result = append(result, pack)
	}
	return result, nil
}

// ValidPackName checks that a pack name contains only safe characters.
func ValidPackName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "_")
}