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

	// Check: if cmd/server.go imports pkg/config, config.go must exist.
	serverPath := filepath.Join(projectDir, "cmd", "server.go")
	if fileImportsPackage(serverPath, "pkg/config") {
		if !fileExists(filepath.Join(projectDir, "pkg", "config", "config.go")) {
			warnings = append(warnings,
				"cmd/server.go imports pkg/config but pkg/config/config.go was not generated. "+
					"Check your proto/config/ annotations.")
		}
	}

	// Check: if any authorizer.go references GeneratedAuthorizer, the
	// corresponding authorizer_gen.go must exist.
	handlersDir := filepath.Join(projectDir, "handlers")
	entries, err := os.ReadDir(handlersDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			authPath := filepath.Join(handlersDir, entry.Name(), "authorizer.go")
			genPath := filepath.Join(handlersDir, entry.Name(), "authorizer_gen.go")
			if fileContains(authPath, "GeneratedAuthorizer") && !fileExists(genPath) {
				warnings = append(warnings,
					"handlers/"+entry.Name()+"/authorizer.go references GeneratedAuthorizer "+
						"but authorizer_gen.go was not generated.")
			}
		}
	}

	return warnings
}

// fileContains returns true if the file at path contains the substring.
// Returns false if the file cannot be read.
func fileContains(path, substr string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), substr)
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