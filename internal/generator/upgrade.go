package generator

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
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
	// These are pure infrastructure, 100% derivable from forge.yaml.
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

// fileEnabledByFeatures reports whether a managed file should be included
// given the current feature flags AND project kind. Files that don't match
// any gated path are always included (backwards-compatible default).
//
// Kind gating: CLI and library projects don't ship the Connect-server stack
// (cmd/{server,otel,db,main}.go, pkg/middleware/*, Dockerfile,
// docker-compose, alloy-config). Listing them in the upgrade plan for those
// kinds produced 100+ line "would update" diffs against files that should
// not exist — noisy, and the fix recipe was always "ignore." Now they are
// excluded from the plan up-front for non-service kinds.
func fileEnabledByFeatures(f managedFile, cfg *config.ProjectConfig) bool {
	p := f.destPath
	kind := config.ProjectKindService
	if cfg != nil {
		kind = cfg.EffectiveKind()
	}
	isService := kind == config.ProjectKindService

	// Kind gates first — service-shape files don't apply to CLI/library.
	// The SCAFFOLD always emits these files for service-kind (deploy
	// derives on for service projects, and even a `features.deploy:
	// false` project keeps the tree on disk). upgrade therefore also
	// manages them for every service-kind project — gating on the flag
	// would strand opted-out scaffolds with un-upgradable Dockerfiles.
	switch {
	case strings.HasPrefix(p, "cmd/"):
		return isService
	case strings.HasPrefix(p, "pkg/middleware/"):
		return isService
	case p == "Dockerfile" || p == "docker-compose.yml":
		return isService
	case p == "deploy/alloy-config.alloy":
		return isService && cfg.Features.ObservabilityEnabled()
	}

	// Feature gates for files that aren't kind-restricted.
	switch {
	case strings.HasPrefix(p, "deploy/") && strings.HasSuffix(p, ".k"):
		return isService
	case p == ".air.toml" || p == ".air-debug.toml":
		return cfg.Features.HotReloadEnabled()
	case strings.HasPrefix(p, ".github/workflows/"):
		return cfg.Features.CIEnabled()
	}
	return true
}

// filterManagedFiles returns only the managed files whose features are enabled.
func filterManagedFiles(files []managedFile, cfg *config.ProjectConfig) []managedFile {
	filtered := make([]managedFile, 0, len(files))
	for _, f := range files {
		if fileEnabledByFeatures(f, cfg) {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

// managedFiles returns the list of frozen files that upgrade manages.
//
// `binary: shared` projects swap cmd/main.go's source template from
// cmd-root.go.tmpl to cmd-shared-main.go.tmpl. The per-service cobra
// subcommand file (cmd/services_gen.go) is intentionally NOT in this
// list: it is a projection of the pkg/app/services.go registration
// rows, owned by the generate pipeline (stepCmdSubcommands), which has
// the registry parse that upgrade lacks.
func managedFiles() []managedFile {
	return managedFilesFor(config.ProjectBinaryPerService)
}

// managedFilesForCfg is like managedFiles but consults the project
// config to choose the right per-kind / per-binary templates. Callers
// that already have the project config should prefer this so the right
// template is used during forge upgrade and forge generate's Tier-1
// regeneration sweep.
//
// Kind sensitivity: the Taskfile template differs by kind (service has
// the full task verb set; CLI has cobra-shaped tasks; library is leaner).
// Without this, `forge upgrade` on a CLI/library project produced a
// 100+ line diff that would have replaced the kind-correct Taskfile
// with the service one — diff was correctly skipped (file was
// "user-modified" from upgrade's perspective) but the dry-run output
// was unparseable.
//
// Binary sensitivity: `binary: shared` projects swap cmd/main.go's
// source from cmd-root.go.tmpl to cmd-shared-main.go.tmpl.
func managedFilesForCfg(cfg *config.ProjectConfig) []managedFile {
	binary := config.ProjectBinaryPerService
	kind := config.ProjectKindService
	if cfg != nil {
		binary = cfg.EffectiveBinary()
		kind = cfg.EffectiveKind()
	}
	return managedFilesForKindBinary(kind, binary)
}

// managedFilesFor returns the file plan for an explicit binary mode at
// the canonical service kind. Extracted so callers without a
// *ProjectConfig (e.g. legacy tests) can still get a canonical file
// list. New callers should prefer managedFilesForKindBinary so kind
// branches (Taskfile.{cli,library}.yml.tmpl, etc.) are honored.
func managedFilesFor(binary string) []managedFile {
	return managedFilesForKindBinary(config.ProjectKindService, binary)
}

// managedFilesForKindBinary returns the file plan for an explicit kind
// + binary mode. The kind selects the correct Taskfile template
// (service / CLI / library); the binary selects cmd/main.go's source.
func managedFilesForKindBinary(kind, binary string) []managedFile {
	mainTmpl := "cmd-root.go.tmpl"
	if binary == config.ProjectBinaryShared {
		mainTmpl = "cmd-shared-main.go.tmpl"
	}
	taskfileTmpl := "Taskfile.yml.tmpl"
	switch kind {
	case config.ProjectKindCLI:
		taskfileTmpl = "Taskfile.cli.yml.tmpl"
	case config.ProjectKindLibrary:
		taskfileTmpl = "Taskfile.library.yml.tmpl"
	}
	return []managedFile{
		// ── Tier 1: Always overwritten by forge generate, gitignored ──

		// Templated cmd files
		{templateName: "cmd-server.go.tmpl", destPath: "cmd/server.go", templated: true, tier: Tier1},
		{templateName: mainTmpl, destPath: "cmd/main.go", templated: true, tier: Tier1},
		{templateName: "cmd-db.go.tmpl", destPath: "cmd/db.go", templated: true, tier: Tier1},
		{templateName: "cmd-version.go.tmpl", destPath: "cmd/version.go", templated: true, tier: Tier1},

		// Static cmd files
		{templateName: "otel.go", destPath: "cmd/otel.go", templated: false, tier: Tier1},

		// buf.yaml is templated against `api.rest` so the googleapis BSR
		// dep is added/removed in lockstep with the runtime vanguard wrap.
		// Tier 1 (regenerated) because the dep choice is fully derived from
		// forge.yaml and users shouldn't be hand-editing it.
		{templateName: "buf.yaml.tmpl", destPath: "buf.yaml", templated: true, tier: Tier1},

		// ── Tier 2: Checksum-protected, committed to git ──

		// Templated config files
		{templateName: taskfileTmpl, destPath: "Taskfile.yml", templated: true, tier: Tier2},
		{templateName: "Dockerfile.tmpl", destPath: "Dockerfile", templated: true, tier: Tier2},
		{templateName: "docker-compose.yml.tmpl", destPath: "docker-compose.yml", templated: true, tier: Tier2},

		// Static config files
		{templateName: "golangci.yml.tmpl", destPath: ".golangci.yml", templated: true, tier: Tier2},
		{templateName: ".gitignore", destPath: ".gitignore", templated: false, tier: Tier2},

		// Middleware — the thin auth-policy file + its policy-wiring
		// test. Scaffolded once, then owned by the user; committed to
		// git and protected by checksum so `forge upgrade` leaves user
		// edits alone. The middleware MECHANISMS (auth modes, CORS,
		// security headers, rate limiting, etc.) live in the forge
		// libraries (pkg/authn, pkg/authz, pkg/middleware, pkg/observe)
		// — projects scaffolded before the library split keep their old
		// pkg/middleware/*.go copies; those files are user-owned and
		// simply stop being managed here (see the
		// migrations/v0.x-to-middleware-lib skill for hand-adoption).
		{templateName: "middleware.go", destPath: "pkg/middleware/middleware.go", templated: false, tier: Tier2},
		{templateName: "middleware_test.go", destPath: "pkg/middleware/middleware_test.go", templated: false, tier: Tier2},

		// cmd/commands.go — the user-owned cobra extension point the
		// Tier-1 cmd/main.go consumes (userCommands()). Scaffolded once,
		// then owned by the user; listed here so `forge upgrade` CREATES
		// it on pre-M6 trees (whose regenerated cmd/main.go now
		// references userCommands) and never stomps an edited copy.
		{templateName: "cmd-commands.go.tmpl", destPath: "cmd/commands.go", templated: true, tier: Tier2},

		// Alloy config — Tier 1 since it's fully derived from forge.yaml services.
		{templateName: "alloy-config.alloy.tmpl", destPath: "deploy/alloy-config.alloy", templated: true, tier: Tier1},
	}
}

// UpgradeManagedPaths returns the set of project-relative paths that
// `forge upgrade` (not `forge generate`) is responsible for emitting.
// Used by `forge generate`'s stale-artifact sweep to exclude these
// paths from the "stale codegen" candidate list: they're tracked in
// `.forge/checksums.json` but only re-rendered by upgrade, so seeing
// them missing from this run's WrittenThisRun set is the expected
// state, not a stale signal.
//
// The set is the union over every (kind, binary) combination. Forge
// only ships a small number of these combinations so the union is
// cheap; computing the union (rather than asking the caller for the
// project's specific kind/binary) keeps the helper signature simple
// and means a kind/binary mismatch in detection doesn't accidentally
// flag a managed file as stale.
//
// FRICTION 2026-06-05 (cp-forge audit-cleanup agent): `forge generate`
// warned 7 "stale" files — .github/CODEOWNERS, .golangci.yml,
// cmd/main.go, cmd/db.go, cmd/version.go, .github/workflows/e2e.yml,
// .github/pull_request_template.md — all of which are managed by
// `forge upgrade`. The user worked around it by hand-flipping
// `forked: true` in checksums.json, which silenced the warnings but
// also disconnected the files from the upgrade pipeline. The right
// fix is for the stale-sweep to know about the upgrade-managed set.
func UpgradeManagedPaths() map[string]bool {
	out := map[string]bool{}
	for _, kind := range []string{
		config.ProjectKindService,
		config.ProjectKindCLI,
		config.ProjectKindLibrary,
	} {
		for _, binary := range []string{
			config.ProjectBinaryPerService,
			config.ProjectBinaryShared,
		} {
			for _, f := range managedFilesForKindBinary(kind, binary) {
				out[f.destPath] = true
			}
		}
	}
	// Files emitted by ProjectGenerator outside the managedFiles list —
	// these still belong to the upgrade lane (templates that scaffold
	// once and stay user-owned, or one-shot Tier-2 metadata that
	// `forge generate` never touches). Without the additions below the
	// stale-sweep would re-flag them with the same false positive the
	// FRICTION note above describes. Add new upgrade-owned scaffolds
	// here when surfaces emerge.
	for _, p := range []string{
		// .github/* templates emitted by project_metadata.go's GitHub
		// scaffold pass — Tier-1 in checksums but `forge generate` never
		// re-emits them; `forge upgrade` does on version bumps.
		".github/CODEOWNERS",
		".github/pull_request_template.md",
		".github/dependabot.yml",
		".github/workflows/e2e.yml",
	} {
		out[p] = true
	}
	return out
}

// Tier2ManagedPaths returns the set of project-relative paths whose
// canonical template tier is Tier-2 (scaffold-once, user-owned after the
// first write). It is the source of truth for `forge generate`'s
// tier-migration step (generate_tier_migrate.go in internal/cli): a
// `.forge/checksums.json` entry for one of these paths that still
// carries tier=1 (or the legacy unset tier=0) predates the template's
// reclassification and must be flipped to tier=2 so the file stops
// being drift-guarded and stops surfacing as a "fork".
//
// Two sources:
//
//   - The managed-file registry entries tagged Tier2. A destPath's tier
//     is invariant across the (kind, binary) matrix — only the source
//     template varies — so the union over combinations is safe (same
//     posture as UpgradeManagedPaths).
//   - The one-shot .github scaffolds written once at `forge new` time
//     (project_ci.go) and never re-emitted by `forge generate`.
//     CODEOWNERS even carries the `yours: scaffolded once ... (starter)`
//     banner; recording them as Tier-1 was a historical accident that
//     made hand-editing your own CODEOWNERS trip the Tier-1 stomp
//     guard. (FRICTION 2026-06-05, cp-forge: users worked around the
//     misclassification by hand-flipping `forked: true`.)
//
// Deliberately NOT in this set: .github/workflows/e2e.yml and
// .github/dependabot.yml — those are re-rendered by `forge generate`'s
// CI step when enabled, so Tier-1 is their honest tier.
func Tier2ManagedPaths() map[string]bool {
	out := map[string]bool{}
	for _, kind := range []string{
		config.ProjectKindService,
		config.ProjectKindCLI,
		config.ProjectKindLibrary,
	} {
		for _, binary := range []string{
			config.ProjectBinaryPerService,
			config.ProjectBinaryShared,
		} {
			for _, f := range managedFilesForKindBinary(kind, binary) {
				if f.tier == Tier2 {
					out[f.destPath] = true
				}
			}
		}
	}
	for _, p := range []string{
		".github/CODEOWNERS",
		".github/pull_request_template.md",
	} {
		out[p] = true
	}
	return out
}

// ServiceInfo holds the name and port of a service for template rendering.
type ServiceInfo struct {
	Name string
	Port int
}

// upgradeTemplateData is the data struct used to render frozen templates.
// Mirrors the anonymous struct in ProjectGenerator.Generate().
type upgradeTemplateData struct {
	Name                   string
	ProtoName              string
	Module                 string
	ServiceName            string
	ServicePort            int
	ProjectName            string
	FrontendName           string
	FrontendPort           int
	GoVersion              string
	GoVersionMinor         string
	DockerBuilderGoVersion string
	Services               []ServiceInfo
	ConfigFields           map[string]bool
	// LocalForgePkgVendored — true when <projectDir>/.forge-pkg/go.mod
	// exists, signalling that the project is using the dev-mode local
	// vendoring of forge/pkg. The Dockerfile template uses this to emit
	// a corresponding `COPY .forge-pkg/ ./.forge-pkg/` line so docker
	// builds resolve the same replace target as host builds.
	LocalForgePkgVendored bool
	// RESTEnabled mirrors forge.yaml's `api.rest` toggle. Used by the
	// buf.yaml template to add the googleapis BSR dep when REST is on
	// (so vanguard's `google/api/annotations.proto` imports resolve).
	RESTEnabled bool
}

// buildTemplateData constructs the template data from a project config,
// matching what ProjectGenerator.Generate() would produce.
//
// projectDir (when non-empty) is used to read the project's go.mod `go`
// directive so upgrade doesn't silently retarget the project to the host's
// Go version. When projectDir is empty or go.mod can't be parsed, we fall
// back to the host's detected version.
func buildTemplateData(cfg *config.ProjectConfig, projectDir string) upgradeTemplateData {
	goVersion := goVersionFromGoMod(projectDir)
	if goVersion == "" {
		goVersion = detectGoVersion()
	}
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

	// Build the services list for templates like alloy-config.
	// The first service maps to docker-compose name "app".
	var services []ServiceInfo
	for i, svc := range cfg.Services {
		name := svc.Name
		if i == 0 {
			name = "app" // docker-compose service name for the primary service
		}
		port := svc.Port
		if port == 0 {
			port = 8080
		}
		services = append(services, ServiceInfo{Name: name, Port: port})
	}
	if len(services) == 0 {
		services = []ServiceInfo{{Name: "app", Port: 8080}}
	}

	// Parse config fields from proto/config/ so templates can conditionally
	// include code blocks that reference specific config fields.
	configFields := codegen.DefaultConfigFieldNames()
	if projectDir != "" {
		if msgs, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto/config")); err == nil && len(msgs) > 0 {
			configFields = codegen.ConfigFieldNamesFromMessages(msgs)
		}
	}

	// Detect whether the project is in the dev-mode local-vendor state for
	// forge/pkg. The Dockerfile template gates its COPY .forge-pkg/ line on
	// this so production-published projects (no .forge-pkg/ on disk) keep
	// their canonical Dockerfile and dev-mode projects get the COPY line
	// without the user editing the file by hand.
	localForgePkgVendored := false
	if projectDir != "" {
		if _, err := os.Stat(filepath.Join(projectDir, ".forge-pkg", "go.mod")); err == nil {
			localForgePkgVendored = true
		}
	}

	return upgradeTemplateData{
		Name:                   cfg.Name,
		ProtoName:              protoName,
		Module:                 cfg.ModulePath,
		ServiceName:            serviceName,
		ServicePort:            servicePort,
		ProjectName:            cfg.Name,
		FrontendName:           frontendName,
		FrontendPort:           frontendPort,
		GoVersion:              goVersion,
		GoVersionMinor:         goVersionMinor(goVersion),
		DockerBuilderGoVersion: dockerBuilderGoVersion(goVersion),
		Services:               services,
		ConfigFields:           configFields,
		LocalForgePkgVendored:  localForgePkgVendored,
		RESTEnabled:            cfg.API.REST,
	}
}

// renderManagedFile renders a managed file's template content.
func renderManagedFile(f managedFile, data upgradeTemplateData) ([]byte, error) {
	var content []byte
	var err error
	if f.templated {
		content, err = templates.ProjectTemplates().Render(f.templateName, data)
	} else {
		content, err = templates.ProjectTemplates().Get(f.templateName)
	}
	if err != nil {
		return nil, err
	}
	// gofmt Go renders. The generate pipeline runs goimports over
	// everything it writes, but the upgrade lane historically wrote raw
	// template output — so conditional templates (cmd-server.go.tmpl's
	// ConfigFields-gated struct literal) produced misaligned code that
	// diffed against the on-disk gofmt'd file and surfaced as phantom
	// "would update"/fork noise. format.Source can't reproduce
	// goimports' import-group reordering, but it eliminates the
	// alignment class entirely. Unformattable output (template bug)
	// falls through unformatted rather than failing the render.
	if strings.HasSuffix(f.destPath, ".go") {
		if formatted, ferr := format.Source(content); ferr == nil {
			content = formatted
		}
	}
	// Canonicalize trailing newline. gofmt-formatted Go files (and most
	// editor-on-save defaults across yaml/json/md) end with exactly one
	// `\n`. Templates checked into the repo sometimes don't, which made
	// drift detection report user-modified for files the user never
	// touched — they just got a `\n` appended on their first editor save.
	// Normalize at render time so byte-equal comparison and the on-disk
	// write both end with a single newline.
	return ensureTrailingNewline(content), nil
}

// ensureTrailingNewline appends exactly one trailing `\n` to text content,
// trimming any extras. Empty inputs are left empty.
func ensureTrailingNewline(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	end := len(b)
	for end > 0 && b[end-1] == '\n' {
		end--
	}
	out := make([]byte, end+1)
	copy(out, b[:end])
	out[end] = '\n'
	return out
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
//
// In `binary: shared` projects this picks cmd-shared-main.go.tmpl as the
// source for cmd/main.go (instead of the canonical cmd-root.go.tmpl) so the
// shared-binary scaffold survives generate cycles.
func RegenerateInfraFiles(projectDir string, cfg *config.ProjectConfig) error {
	return RegenerateInfraFilesTracked(projectDir, cfg, nil)
}

// RegenerateInfraFilesTracked is RegenerateInfraFiles routed through the
// checksums chokepoint. With a non-nil cs every Tier-1 infra write:
//
//   - honors disowned entries (the user ran `forge disown`: the write
//     is skipped while the file exists — the raw os.WriteFile path this
//     replaces violated the "forge never regenerates user-owned files"
//     contract for cmd/*.go and friends);
//   - records the render hash + WrittenThisRun so the stale sweep and
//     the next run's drift guard see an accurate manifest;
//   - tags the entry Tier-1 (these files ARE regenerated every run).
//
// force=true preserves the historical always-overwrite semantics for
// forge-owned files: the Tier-1 stomp guard ran earlier in the pipeline,
// so any surviving drift was already adjudicated (--force / disown).
//
// A nil cs falls back to untracked writes (legacy callers).
func RegenerateInfraFilesTracked(projectDir string, cfg *config.ProjectConfig, cs *FileChecksums) error {
	data := buildTemplateData(cfg, projectDir)
	filtered := filterManagedFiles(managedFilesForCfg(cfg), cfg)
	for _, f := range filtered {
		if f.tier != Tier1 {
			continue
		}
		content, err := renderManagedFile(f, data)
		if err != nil {
			return fmt.Errorf("render %s: %w", f.destPath, err)
		}
		if _, err := checksums.WriteGeneratedFileTier1(projectDir, f.destPath, content, cs, true); err != nil {
			return fmt.Errorf("write %s: %w", f.destPath, err)
		}
	}
	// The Tier-1 cmd/main.go just (re)rendered above references the
	// user-owned userCommands() extension point. Ensure cmd/commands.go
	// exists (write-once; never overwrites) so a pre-M6 tree whose
	// main.go gained the reference this run still compiles — the
	// codegen pipeline's stepCmdSubcommands does the same, but this
	// path also runs for service projects with features.codegen=false.
	for _, f := range filtered {
		if f.destPath == "cmd/main.go" {
			if err := codegen.GenerateCmdCommands(projectDir); err != nil {
				return fmt.Errorf("scaffold cmd/commands.go: %w", err)
			}
			break
		}
	}
	return nil
}

// hasLegacyMiddlewareLayout reports whether the project's
// pkg/middleware still has the pre-library-split shape: legacy
// mechanism files present (auth.go / claims.go are the sentinels —
// every old scaffold had both) and no thin middleware.go yet. Upgrade
// must not emit the thin policy pair into such a package — the symbol
// sets collide.
func hasLegacyMiddlewareLayout(projectDir string) bool {
	if _, err := os.Stat(filepath.Join(projectDir, "pkg", "middleware", "middleware.go")); err == nil {
		return false // already on the thin layout
	}
	for _, sentinel := range []string{"auth.go", "claims.go"} {
		if _, err := os.Stat(filepath.Join(projectDir, "pkg", "middleware", sentinel)); err == nil {
			return true
		}
	}
	return false
}

// Upgrade checks all managed (frozen) files against the current templates
// and optionally applies updates.
//
// When checkOnly is true, no files are written — it only reports what would change.
// When force is true, user-modified files are overwritten without prompting.
func Upgrade(projectDir string, cfg *config.ProjectConfig, force bool, checkOnly bool) ([]UpgradeResult, error) {
	data := buildTemplateData(cfg, projectDir)

	cs, err := LoadChecksums(projectDir)
	if err != nil {
		return nil, fmt.Errorf("load checksums: %w", err)
	}

	var results []UpgradeResult

	// Pre-library-split projects still carry the old pkg/middleware
	// mechanism files (auth.go, claims.go, …). Those declare the same
	// symbols as the thin policy pair (Claims, NewAuthInterceptor,
	// Authorizer, …), so dropping middleware.go next to them would stop
	// the package compiling. Their copies are user-owned and keep
	// working; converging on the library is the user-driven
	// migrations/v0.x-to-middleware-lib path, never an upgrade side
	// effect.
	legacyMiddleware := hasLegacyMiddlewareLayout(projectDir)

	for _, f := range filterManagedFiles(managedFilesForCfg(cfg), cfg) {
		if legacyMiddleware && strings.HasPrefix(f.destPath, "pkg/middleware/") {
			results = append(results, UpgradeResult{
				Path:   f.destPath,
				Status: UpgradeSkipped,
			})
			continue
		}
		// Disowned entries (and legacy fork-era ones) are user-owned:
		// upgrade never touches them while the file exists. A missing
		// file falls through — deletion is the documented re-adoption
		// path, and upgrade re-emitting it is the same contract as
		// `forge generate`.
		if entry, ok := cs.Files[f.destPath]; ok && (entry.Disowned || entry.Forked) {
			if _, statErr := os.Stat(filepath.Join(projectDir, f.destPath)); statErr == nil {
				results = append(results, UpgradeResult{
					Path:   f.destPath,
					Status: UpgradeSkipped,
				})
				continue
			}
		}

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

		// Tier 2: File differs — check if user has modified it.
		//
		// "Modified" means the on-disk content matches neither the
		// current checksum nor any prior render forge has produced for
		// this path. The history check is what closes the stale-codegen
		// gap: when a template is updated and the on-disk file is a
		// prior render (matches a checksum in history but not the
		// current one), forge treats it as auto-updateable rather than
		// flagging it as user-modified. See FileChecksums docs in
		// checksums.go.
		diff := simpleDiff(f.destPath, existing, expected)
		entry, hasChecksum := cs.Files[f.destPath]
		matchesKnownRender := hasChecksum && cs.MatchesAnyKnownRender(f.destPath, existing)
		_ = entry // entry retained for future per-entry diagnostics

		if matchesKnownRender {
			// File matches stored checksum or a prior render → user
			// hasn't modified it → safe to auto-update.
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
	// A write through the upgrade chokepoint means forge owns the result
	// again — the only way a disowned entry reaches here is the deletion
	// re-adoption path (Upgrade skips disowned entries whose file
	// exists), so clear the user-ownership markers.
	entry := cs.Files[relPath]
	if entry.Disowned || entry.Forked {
		entry.Disowned = false
		entry.DisownedAt = ""
		entry.Forked = false
		entry.Accepted = false
		entry.ForkedAt = ""
		cs.Files[relPath] = entry
	}
	return nil
}