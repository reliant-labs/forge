package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// validateGeneratedProject checks for common post-generation issues and
// returns human-readable warnings. These are advisory — the subsequent
// `go build ./...` step catches hard errors. The warnings help users
// diagnose *why* the build fails.
func validateGeneratedProject(projectDir string) []string {
	var warnings []string

	// Check: if the primary binary's cmd/<bin>/cmd/serve.go imports
	// pkg/config, config.go must exist.
	bin := bootstrapBinaryName(projectDir)
	servePath := filepath.Join(projectDir, "cmd", bin, "cmd", "serve.go")
	if fileImportsPackage(servePath, "pkg/config") {
		if !fileExists(filepath.Join(projectDir, "pkg", "config", "config.go")) {
			warnings = append(warnings,
				"cmd/"+bin+"/cmd/serve.go imports pkg/config but pkg/config/config.go was not generated. "+
					"Check your proto/config/ annotations.")
		}
	}

	return warnings
}

// fileImportsPackage returns true if the Go source file at path contains
// an import path ending with the given suffix (e.g. "pkg/config").
func fileImportsPackage(path, pkgSuffix string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// Simple heuristic: look for the suffix inside a quoted import.
	return strings.Contains(string(data), pkgSuffix)
}
