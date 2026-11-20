package frontenddeps

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkspaceInstallDir locates a missing dependency tree's install directory
// without inspecting node_modules. Only workspaces inside boundary that adopt
// dir are eligible; otherwise the frontend installs locally.
func WorkspaceInstallDir(dir, boundary string) string {
	member, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	limit, err := filepath.Abs(boundary)
	if err != nil {
		return dir
	}
	rel, err := filepath.Rel(limit, member)
	if err != nil || !filepath.IsLocal(rel) {
		return dir
	}

	// A forge-managed bridge is installed by EnsureTo from the member:
	// it may first need to create that member's tracked registry lockfile.
	for p := member; p != limit; {
		p = filepath.Dir(p)
		if _, bridge := pnpmBridgeAdopts(p, member); bridge {
			return dir
		}
	}
	if fileExists(filepath.Join(member, "pnpm-workspace.yaml")) {
		return dir // this frontend is its own workspace
	}
	for p := member; p != limit; {
		p = filepath.Dir(p)
		if body, err := os.ReadFile(filepath.Join(p, "pnpm-workspace.yaml")); err == nil {
			if pnpmWorkspaceAdopts(p, member, body) {
				return p
			}
			return dir // pnpm stops at the nearest workspace file
		}
		if workspaceAdopts(p, member) {
			return p
		}
	}
	return dir
}

func pnpmWorkspaceAdopts(root, member string, body []byte) bool {
	var ws struct {
		Packages []string `yaml:"packages"`
	}
	if yaml.Unmarshal(body, &ws) != nil {
		return false
	}
	matched := false
	for _, pattern := range ws.Packages {
		excluded := strings.HasPrefix(pattern, "!")
		if !matchesMember(root, strings.TrimPrefix(pattern, "!"), member) {
			continue
		}
		if excluded {
			return false
		}
		matched = true
	}
	return matched
}
