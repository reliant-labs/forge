package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// validateGeneratedProject checks for common post-generation issues and
// returns human-readable warnings. These are advisory — the subsequent
// `go build ./...` step catches hard errors. The warnings help users
// diagnose *why* the build fails.
//
// cfg/services/entities may be nil/empty (directory-scan fallback, or a
// project with no frontends) — every check here degrades to "find nothing"
// rather than erroring, matching the advisory contract the rest of this
// function already has.
func validateGeneratedProject(projectDir string, cfg *config.ProjectConfig, services []codegen.ServiceDef, entities []codegen.EntityDef) []string {
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

	// Check: entity pages exist that the user-owned nav.tsx never links to.
	// Same finding stepFrontendNav already printed as an inline ℹ️ line
	// (reportUnlinkedRoutes) — duplicated here because that line sits mid-run
	// among ~150 lines of ✅ output and gets missed, while this block is read
	// every time. See unlinkedRouteWarnings for why it re-derives rather than
	// reuses stepFrontendNav's result.
	warnings = append(warnings, unlinkedRouteWarnings(cfg, projectDir, services, entities)...)

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
