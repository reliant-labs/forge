// Package cmdutil holds cross-cutting helpers shared by forge's own CLI across
// MORE THAN ONE command group (internal/cli and its dir-nested subpackages).
// It is a leaf package — it imports only neutral internal packages — so any
// command group can depend on it without an import cycle back to internal/cli.
//
// Helpers used by a single command stay with that command; trivial stdlib
// wrappers (a one-line os.Stat check) are duplicated locally rather than
// shared. Only genuinely shared logic lives here.
package cmdutil

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/cliutil"
)

// Name returns the command name users should type to invoke Forge. When the
// binary is "forge" (standalone install) it returns "forge"; when embedded in
// another binary (e.g. "reliant") it returns "reliant forge". Shared so group
// commands can print copy-pasteable next-step hints without importing
// internal/cli.
func Name() string {
	base := filepath.Base(os.Args[0])
	if base == "forge" {
		return "forge"
	}
	return base + " " + "forge"
}

// ErrProjectConfigNotFound is returned when forge.yaml does not exist. The
// canonical sentinel lives here (the shared leaf package) so both internal/cli
// and the dir-nested command groups compare against the same value;
// internal/cli's config.ErrProjectConfigNotFound aliases this.
var ErrProjectConfigNotFound = errors.New("forge.yaml not found in current directory (run 'forge new' to create a project)")

// ProjectRoot finds the project root by looking for forge.yaml in the cwd
// (NOT a walk-up — see FindProjectRoot for that). Returns a user-facing error
// when forge.yaml is absent from the current directory.
func ProjectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	configPath := filepath.Join(cwd, "forge.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return "", cliutil.UserErr("forge",
			"forge.yaml not found in current directory",
			"",
			"cd into your project root, or run 'forge new <name>' to scaffold a new project")
	}
	return cwd, nil
}

// FindProjectRoot walks upward from the cwd looking for a forge.yaml. Returns
// the directory or "" when no project is found. Mirrors the loadProjectConfig
// walk-up behavior in config.go.
func FindProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "forge.yaml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}
