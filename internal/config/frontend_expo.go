package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// hasExpoDependency reads metadata only: discovery never evaluates app.config
// code or invokes a package manager. A script or package name is not evidence.
func hasExpoDependency(dir string) bool {
	body, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(body, &pkg) != nil {
		return false
	}
	return strings.TrimSpace(pkg.Dependencies["expo"]) != "" || strings.TrimSpace(pkg.DevDependencies["expo"]) != ""
}
