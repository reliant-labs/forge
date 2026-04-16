package generator

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// UpgradeStatus describes the outcome for each managed file.
type UpgradeStatus string

const (
	UpgradeUpToDate     UpgradeStatus = "up-to-date"
	UpgradeUpdated      UpgradeStatus = "updated"
	UpgradeUserModified UpgradeStatus = "user-modified"
	UpgradeSkipped      UpgradeStatus = "skipped"
)

// UpgradeResult holds the outcome for a single managed file.
type UpgradeResult struct {
	Path   string        // relative path in project (e.g. "cmd/server.go")
	Status UpgradeStatus // what happened
	Diff   string        // unified-style diff when file changed
}

// File ownership tiers.
const (
	// Tier1 files are always overwritten by forge generate and gitignored.
	// These are pure infrastructure, 100% derivable from forge.project.yaml.
	Tier1 = 1
	// Tier2 files are checksum-protected and committed to git.
	// Overwritten only if the user hasn't modified them.
	Tier2 = 2
)

// managedFile describes a frozen file that upgrade tracks.
type managedFile struct {
	templateName string // template name in project/ dir (e.g. "cmd-server.go.tmpl")
	destPath     string // relative destination path (e.g. "cmd/server.go")
	templated    bool   // true if template needs data rendering
	tier         int    // 1 = always overwrite (gitignored), 2 = checksum-protected
}

// managedFiles returns the list of frozen files that upgrade manages.
func managedFiles() []managedFile {
	return []managedFile{
		// ── Tier 1: Always overwritten by forge generate, gitignored ──

		// Templated cmd files
		{templateName: "cmd-server.go.tmpl", destPath: "cmd/server.go", templated: true, tier: Tier1},
		{templateName: "cmd-root.go.tmpl", destPath: "cmd/main.go", templated: true, tier: Tier1},
		{templateName: "cmd-version.go.tmpl", destPath: "cmd/version.go", templated: true, tier: Tier1},

		// Static cmd files
		{templateName: "otel.go", destPath: "cmd/otel.go", templated: false, tier: Tier1},

		// Middleware (static infrastructure)
		{templateName: "middleware-recovery.go", destPath: "pkg/middleware/recovery.go", templated: false, tier: Tier1},
		{templateName: "middleware-logging.go", destPath: "pkg/middleware/logging.go", templated: false, tier: Tier1},
		{templateName: "middleware-http.go", destPath: "pkg/middleware/http.go", templated: false, tier: Tier1},
		{templateName: "middleware-audit.go", destPath: "pkg/middleware/audit.go", templated: false, tier: Tier1},
		{templateName: "middleware-authz.go", destPath: "pkg/middleware/authz.go", templated: false, tier: Tier1},

		// ── Tier 2: Checksum-protected, committed to git ──

		// Templated config files
		{templateName: "Taskfile.yml.tmpl", destPath: "Taskfile.yml", templated: true, tier: Tier2},
		{templateName: "Dockerfile.tmpl", destPath: "Dockerfile", templated: true, tier: Tier2},
		{templateName: "docker-compose.yml.tmpl", destPath: "docker-compose.yml", templated: true, tier: Tier2},

		// Static config files
		{templateName: "golangci.yml.tmpl", destPath: ".golangci.yml", templated: true, tier: Tier2},
		{templateName: ".gitignore", destPath: ".gitignore", templated: false, tier: Tier2},

		// Middleware (commonly customized)
		{templateName: "middleware-cors.go", destPath: "pkg/middleware/cors.go", templated: false, tier: Tier2},
		{templateName: "middleware-auth.go", destPath: "pkg/middleware/auth.go", templated: false, tier: Tier2},
		{templateName: "middleware-claims.go", destPath: "pkg/middleware/claims.go", templated: false, tier: Tier2},
	}
}

// upgradeTemplateData is the data struct used to render frozen templates.
// Mirrors the anonymous struct in ProjectGenerator.Generate().
type upgradeTemplateData struct {
	Name           string
	ProtoName      string
	Module         string
	ServiceName    string
	ServicePort    int
	ProjectName    string
	FrontendName   string
	FrontendPort   int
	GoVersion      string
	GoVersionMinor string
}

// buildTemplateData constructs the template data from a project config,
// matching what ProjectGenerator.Generate() would produce.
func buildTemplateData(cfg *config.ProjectConfig) upgradeTemplateData {
	goVersion := detectGoVersion()
	protoName := strings.ReplaceAll(cfg.Name, "-", "_")

	serviceName := "api"
	servicePort := 8080
	if len(cfg.Services) > 0 {
		serviceName = cfg.Services[0].Name
		if cfg.Services[0].Port != 0 {
			servicePort = cfg.Services[0].Port
		}
	}

	frontendName := ""
	frontendPort := 3000
	if len(cfg.Frontends) > 0 {
		frontendName = cfg.Frontends[0].Name
		if cfg.Frontends[0].Port != 0 {
			frontendPort = cfg.Frontends[0].Port
		}
	}

	return upgradeTemplateData{
		Name:           cfg.Name,
		ProtoName:      protoName,
		Module:         cfg.ModulePath,
		ServiceName:    serviceName,
		ServicePort:    servicePort,
		ProjectName:    cfg.Name,
		FrontendName:   frontendName,
		FrontendPort:   frontendPort,
		GoVersion:      goVersion,
		GoVersionMinor: goVersionMinor(goVersion),
	}
}

// renderManagedFile renders a managed file's template content.
func renderManagedFile(f managedFile, data upgradeTemplateData) ([]byte, error) {
	if f.templated {
		return templates.RenderProjectTemplate(f.templateName, data)
	}
	return templates.GetProjectTemplate(f.templateName)
}

// simpleDiff produces a minimal unified-style diff showing changed lines.
func simpleDiff(path string, old, new []byte) string {
	oldLines := strings.Split(string(old), "\n")
	newLines := strings.Split(string(new), "\n")

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("--- a/%s\n", path))
	buf.WriteString(fmt.Sprintf("+++ b/%s\n", path))

	// Simple line-by-line comparison showing context around changes
	maxLen := len(oldLines)
	if len(newLines) > maxLen {
		maxLen = len(newLines)
	}

	const contextLines = 3
	type hunk struct {
		startOld int
		startNew int
		old      []string
		new      []string
	}

	// Find changed regions
	type change struct {
		lineOld int
		lineNew int
	}
	var changes []change

	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		if oldLines[i] != newLines[j] {
			changes = append(changes, change{i, j})
		}
		i++
		j++
	}
	for ; i < len(oldLines); i++ {
		changes = append(changes, change{i, -1})
	}
	for ; j < len(newLines); j++ {
		changes = append(changes, change{-1, j})
	}

	if len(changes) == 0 {
		return ""
	}

	// Group changes into hunks with context
	type hunkRange struct {
		startOld, endOld int
		startNew, endNew int
	}
	var hunks []hunkRange

	for _, c := range changes {
		oLine := c.lineOld
		if oLine < 0 {
			oLine = len(oldLines)
		}
		nLine := c.lineNew
		if nLine < 0 {
			nLine = len(newLines)
		}

		startO := oLine - contextLines
		if startO < 0 {
			startO = 0
		}
		endO := oLine + contextLines + 1
		if endO > len(oldLines) {
			endO = len(oldLines)
		}
		startN := nLine - contextLines
		if startN < 0 {
			startN = 0
		}
		endN := nLine + contextLines + 1
		if endN > len(newLines) {
			endN = len(newLines)
		}

		if len(hunks) > 0 {
			last := &hunks[len(hunks)-1]
			if startO <= last.endOld || startN <= last.endNew {
				if endO > last.endOld {
					last.endOld = endO
				}
				if endN > last.endNew {
					last.endNew = endN
				}
				continue
			}
		}
		hunks = append(hunks, hunkRange{startO, endO, startN, endN})
	}

	for _, h := range hunks {
		oldCount := h.endOld - h.startOld
		newCount := h.endNew - h.startNew
		buf.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", h.startOld+1, oldCount, h.startNew+1, newCount))

		// Use a simple approach: show removed lines then added lines with context
		oi, ni := h.startOld, h.startNew
		for oi < h.endOld || ni < h.endNew {
			if oi < h.endOld && ni < h.endNew && oi < len(oldLines) && ni < len(newLines) && oldLines[oi] == newLines[ni] {
				buf.WriteString(" " + oldLines[oi] + "\n")
				oi++
				ni++
			} else if oi < h.endOld && oi < len(oldLines) {
				buf.WriteString("-" + oldLines[oi] + "\n")
				oi++
			} else if ni < h.endNew && ni < len(newLines) {
				buf.WriteString("+" + newLines[ni] + "\n")
				ni++
			} else {
				break
			}
		}
	}

	return buf.String()
}

// RegenerateInfraFiles regenerates all Tier 1 (always-overwrite) infrastructure
// files. Called by forge generate to keep infrastructure in sync with templates.
func RegenerateInfraFiles(projectDir string, cfg *config.ProjectConfig) error {
	data := buildTemplateData(cfg)
	for _, f := range managedFiles() {
		if f.tier != Tier1 {
			continue
		}
		content, err := renderManagedFile(f, data)
		if err != nil {
			return fmt.Errorf("render %s: %w", f.destPath, err)
		}
		fullPath := filepath.Join(projectDir, f.destPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(fullPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.destPath, err)
		}
	}
	return nil
}

// Upgrade checks all managed (frozen) files against the current templates
// and optionally applies updates.
//
// When checkOnly is true, no files are written — it only reports what would change.
// When force is true, user-modified files are overwritten without prompting.
func Upgrade(projectDir string, cfg *config.ProjectConfig, force bool, checkOnly bool) ([]UpgradeResult, error) {
	data := buildTemplateData(cfg)

	cs, err := LoadChecksums(projectDir)
	if err != nil {
		return nil, fmt.Errorf("load checksums: %w", err)
	}

	var results []UpgradeResult

	for _, f := range managedFiles() {
		// Render the expected content from the current template
		expected, err := renderManagedFile(f, data)
		if err != nil {
			return nil, fmt.Errorf("render template %s: %w", f.templateName, err)
		}

		// Read the existing file on disk
		diskPath := filepath.Join(projectDir, f.destPath)
		existing, err := os.ReadFile(diskPath)
		if err != nil {
			if os.IsNotExist(err) {
				// File doesn't exist — treat as needing update
				result := UpgradeResult{
					Path:   f.destPath,
					Status: UpgradeSkipped,
				}
				if !checkOnly {
					if writeErr := writeManagedFile(projectDir, f.destPath, expected, cs); writeErr != nil {
						return nil, fmt.Errorf("write %s: %w", f.destPath, writeErr)
					}
					result.Status = UpgradeUpdated
				} else {
					result.Status = UpgradeUpdated // would be updated
				}
				results = append(results, result)
				continue
			}
			return nil, fmt.Errorf("read %s: %w", f.destPath, err)
		}

		// Compare rendered template with what's on disk
		if bytes.Equal(existing, expected) {
			results = append(results, UpgradeResult{
				Path:   f.destPath,
				Status: UpgradeUpToDate,
			})
			continue
		}

		// Tier 1 files are always overwritten (they're gitignored)
		if f.tier == Tier1 {
			result := UpgradeResult{
				Path:   f.destPath,
				Status: UpgradeUpdated,
				Diff:   simpleDiff(f.destPath, existing, expected),
			}
			if !checkOnly {
				if writeErr := writeManagedFile(projectDir, f.destPath, expected, cs); writeErr != nil {
					return nil, fmt.Errorf("write %s: %w", f.destPath, writeErr)
				}
			}
			results = append(results, result)
			continue
		}

		// Tier 2: File differs — check if user has modified it
		diff := simpleDiff(f.destPath, existing, expected)
		storedChecksum, hasChecksum := cs.Files[f.destPath]

		if hasChecksum && HashContent(existing) == storedChecksum {
			// File matches stored checksum → user hasn't modified it → safe to auto-update
			result := UpgradeResult{
				Path:   f.destPath,
				Status: UpgradeUpdated,
				Diff:   diff,
			}
			if !checkOnly {
				if writeErr := writeManagedFile(projectDir, f.destPath, expected, cs); writeErr != nil {
					return nil, fmt.Errorf("write %s: %w", f.destPath, writeErr)
				}
			}
			results = append(results, result)
			continue
		}

		// User modified the file (or no checksum exists)
		if force {
			result := UpgradeResult{
				Path:   f.destPath,
				Status: UpgradeUpdated,
				Diff:   diff,
			}
			if !checkOnly {
				if writeErr := writeManagedFile(projectDir, f.destPath, expected, cs); writeErr != nil {
					return nil, fmt.Errorf("write %s: %w", f.destPath, writeErr)
				}
			}
			results = append(results, result)
		} else {
			results = append(results, UpgradeResult{
				Path:   f.destPath,
				Status: UpgradeUserModified,
				Diff:   diff,
			})
		}
	}

	// Save updated checksums (unless dry-run)
	if !checkOnly {
		if err := SaveChecksums(projectDir, cs); err != nil {
			return nil, fmt.Errorf("save checksums: %w", err)
		}
	}

	return results, nil
}

// writeManagedFile writes content to a file and records its checksum.
func writeManagedFile(root, relPath string, content []byte, cs *FileChecksums) error {
	fullPath := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(fullPath, content, 0644); err != nil {
		return err
	}
	cs.RecordFile(relPath, content)
	return nil
}