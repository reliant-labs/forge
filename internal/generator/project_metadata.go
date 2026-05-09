package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/templates"
)

// writeProjectMetadata writes everything under .reliant/, the top-level
// memory file (whose name depends on MemoryFormat), and the project-level
// .mcp.json files.
//
// File ownership model:
//
//   - forge-owned (always overwritten on regeneration):
//     .reliant/project.json, .reliant/README.md.
//
//   - User-owned (written only if absent, never touched if present):
//     <memory-file>, .reliant/reliant.md, .mcp.json, .mcp.json.example.
//
// Skills and conventions are served via `forge skill list/load` from embedded
// templates — no files are written to disk for them.
func (g *ProjectGenerator) writeProjectMetadata() error {
	reliantDir := filepath.Join(g.Path, ".reliant")
	if err := os.MkdirAll(reliantDir, 0o755); err != nil {
		return fmt.Errorf("failed to create .reliant directory: %w", err)
	}

	if err := g.writeProjectJSON(reliantDir); err != nil {
		return err
	}

	if err := assets.WriteTemplateWithData(".reliant-README.md", filepath.Join(reliantDir, "README.md"), nil); err != nil {
		return fmt.Errorf("failed to write .reliant/README.md: %w", err)
	}

	templateData := struct {
		Name string
		CLI  string
	}{Name: g.Name, CLI: cliName()}

	// User-owned .reliant/reliant.md — project memory file. Write only if absent.
	// This is always generated regardless of --memory format (forge's own memory).
	if err := writeIfAbsent(filepath.Join(reliantDir, "reliant.md"), "reliant-reliant.md.tmpl", templateData); err != nil {
		return fmt.Errorf("failed to write .reliant/reliant.md: %w", err)
	}

	// User-owned top-level memory file — path depends on --memory format.
	memoryFile := g.MemoryFormat.MemoryFilePath()
	memoryDest := filepath.Join(g.Path, memoryFile)
	// Ensure parent directory exists (needed for copilot: .github/).
	if dir := filepath.Dir(memoryDest); dir != g.Path {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", memoryFile, err)
		}
	}
	if err := writeIfAbsent(memoryDest, "reliant.md.tmpl", templateData); err != nil {
		return fmt.Errorf("failed to write %s: %w", memoryFile, err)
	}

	// User-owned MCP config — write only if absent.
	if err := writeIfAbsent(filepath.Join(g.Path, ".mcp.json"), "mcp.json.tmpl", templateData); err != nil {
		return fmt.Errorf("failed to write .mcp.json: %w", err)
	}

	// Documentation file for opt-in MCP servers — write only if absent so a
	// user who deleted it intentionally isn't pestered.
	if err := writeIfAbsent(filepath.Join(g.Path, ".mcp.json.example"), "mcp.json.example.tmpl", templateData); err != nil {
		return fmt.Errorf("failed to write .mcp.json.example: %w", err)
	}

	return nil
}

// writeProjectJSON writes the immutable project metadata JSON under .reliant/.
func (g *ProjectGenerator) writeProjectJSON(reliantDir string) error {
	metadata := map[string]interface{}{
		"name":        g.Name,
		"module_path": g.ModulePath,
		"created_at":  time.Now().Format(time.RFC3339),
		"version":     "1.0.0",
		"generator":   "forge",
	}

	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(reliantDir, "project.json"), data, 0o644); err != nil {
		return fmt.Errorf("failed to write .reliant/project.json: %w", err)
	}
	return nil
}

// writeIfAbsent renders the given template to destPath only if destPath does
// not already exist. This is used for user-owned files (reliant.md, .mcp.json,
// .mcp.json.example) to avoid clobbering local edits on regeneration.
func writeIfAbsent(destPath, templateName string, data interface{}) error {
	if _, err := os.Stat(destPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat %s: %w", destPath, err)
	}
	return assets.WriteTemplateWithData(templateName, destPath, data)
}

func (g *ProjectGenerator) generateGolangciLint() error {
	data := struct{ Module string }{Module: g.ModulePath}
	content, err := templates.ProjectTemplates().Render("golangci.yml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render golangci.yml: %w", err)
	}
	destPath := filepath.Join(g.Path, ".golangci.yml")
	return os.WriteFile(destPath, content, 0644)
}

// generateExamplesReadme scaffolds an examples/ directory with a README that
// documents the convention for client-side demos. We don't ship a concrete
// example because what's appropriate depends on the project's shape; the
// README gives agents and contributors a stable home to drop one into.
func (g *ProjectGenerator) generateExamplesReadme() error {
	data := struct {
		Name string
	}{Name: g.Name}
	content, err := templates.ProjectTemplates().Render("examples-README.md.tmpl", data)
	if err != nil {
		return fmt.Errorf("render examples/README.md: %w", err)
	}
	examplesDir := filepath.Join(g.Path, "examples")
	if err := os.MkdirAll(examplesDir, 0o755); err != nil {
		return fmt.Errorf("create examples dir: %w", err)
	}
	return os.WriteFile(filepath.Join(examplesDir, "README.md"), content, 0o644)
}

// generatePkgMiddleware writes Connect-compatible interceptors into pkg/middleware/.
func (g *ProjectGenerator) generatePkgMiddleware() error {
	middlewareFiles := []struct {
		templateName string
		destName     string
	}{
		{"middleware-recovery.go", "recovery.go"},
		{"middleware-recovery_test.go", "recovery_test.go"},
		{"middleware-logging.go", "logging.go"},
		{"middleware-logging_test.go", "logging_test.go"},
		{"middleware-auth.go", "auth.go"},
		{"middleware-auth_test.go", "auth_test.go"},
		{"middleware-authz.go", "authz.go"},
		{"middleware-permissive-authz.go", "permissive_authz.go"},
		{"middleware-claims.go", "claims.go"},
		{"middleware-audit.go", "audit.go"},
		{"middleware-http.go", "http.go"},
		{"middleware-cors.go", "cors.go"},
		{"middleware-cors_test.go", "cors_test.go"},
		{"middleware-security-headers.go", "security_headers.go"},
		{"middleware-security-headers_test.go", "security_headers_test.go"},
		{"middleware-ratelimit.go", "ratelimit.go"},
		{"middleware-ratelimit_test.go", "ratelimit_test.go"},
		{"middleware-requestid.go", "requestid.go"},
		{"middleware-requestid_test.go", "requestid_test.go"},
		{"middleware-idempotency.go", "idempotency.go"},
		{"middleware-idempotency_test.go", "idempotency_test.go"},
		{"middleware-redact.go", "redact.go"},
		{"middleware-redact_test.go", "redact_test.go"},
		{"middleware-logevents.go", "logevents.go"},
		{"middleware-trace-handler.go", "trace_handler.go"},
	}

	for _, f := range middlewareFiles {
		content, err := templates.ProjectTemplates().Get(f.templateName)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(g.Path, "pkg", "middleware", f.destName)
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.destName, err)
		}
	}
	return nil
}

// recordFrozenChecksums records checksums for all frozen files managed by
// `forge upgrade`. This must be called after the frozen files have been
// written to disk so that new projects have baseline checksums.
func (g *ProjectGenerator) recordFrozenChecksums(templateData interface{}) error {
	return RecordFrozenChecksums(g.Path, g.effectiveBinary(), g.effectiveKind())
}

// RecordFrozenChecksums records checksums for all managed files at
// projectDir. Exposed publicly so callers outside the scaffold path
// (e.g. `forge new` after `bootstrapGeneratedCode` runs goimports
// and reformats Tier-2 files) can re-record the post-formatting bytes
// — otherwise the checksums baked at scaffold time would not match
// the on-disk content, and `forge upgrade --dry-run` would flag every
// formatted file as user-modified.
func RecordFrozenChecksums(projectDir, binary, kind string) error {
	cs, err := LoadChecksums(projectDir)
	if err != nil {
		return fmt.Errorf("load checksums: %w", err)
	}
	cs.ForgeVersion = buildinfo.Version()

	for _, f := range managedFilesForKindBinary(kind, binary) {
		fullPath := filepath.Join(projectDir, f.destPath)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read %s for checksum: %w", f.destPath, err)
		}
		cs.RecordFile(f.destPath, content)
	}
	return SaveChecksums(projectDir, cs)
}