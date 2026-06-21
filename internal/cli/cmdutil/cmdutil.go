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
	"os"
	"path/filepath"
)

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
