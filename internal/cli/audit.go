// Package cli — `forge audit` command.
//
// Audit produces a comprehensive snapshot of project state designed to
// orient an LLM (or human) without forcing them to grep ten different
// directories. It rolls up:
//
//   - Forge version pin (forge.yaml forge_version vs binary buildinfo).
//   - Project shape (kind, services + RPC counts, workers, operators,
//     frontends, installed packs).
//   - Convention compliance (rolled up forge lint counts per category).
//   - Codegen state (last generate timestamp via .forge/checksums.json,
//     orphan _gen files, uncommitted user edits to forge-space files).
//   - Pack health (each installed pack's version against the embedded
//     pack registry).
//   - Pack graph health (every installed pack's `depends_on` is also
//     installed; missing producers surface as errors).
//   - Proto-vs-migration alignment (entity tables vs db/migrations/).
//   - Migration safety summary (allowed_destructive count, latest
//     migration timestamp, destructive_change severity).
//   - Wire-coverage (unresolved Deps fields in pkg/app/wire_gen.go,
//     rolled up from `forge lint --wire-coverage`).
//   - FORGE_SCAFFOLD marker counts (P0 sharpening surface).
//   - Deps health (go.sum freshness vs go.mod, gen/ presence).
//
// JSON output groups checks by category with status: ok|warn|error so a
// sub-agent can branch on `.codegen.status == "warn"` directly.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/linter/dblint"
	"github.com/reliant-labs/forge/internal/linter/forgeconv"
	"github.com/reliant-labs/forge/internal/packs"
)

// AuditStatus is the per-category roll-up. We keep the wire enum tiny so
// the JSON shape is easy to grep / jq against.
type AuditStatus string

const (
	AuditStatusOK    AuditStatus = "ok"
	AuditStatusWarn  AuditStatus = "warn"
	AuditStatusError AuditStatus = "error"
)

// AuditCategory is one section of the audit report. The shape mirrors the
// "category, status, summary, details" scheme called for in the spec —
// kept deliberately simple so a sub-agent can pluck `.summary` for a
// human-readable snippet or `.details` for structured fix-up data.
type AuditCategory struct {
	Status  AuditStatus    `json:"status"`
	Summary string         `json:"summary"`
	Details map[string]any `json:"details,omitempty"`
}

// AuditReport is the top-level JSON structure emitted by `forge audit --json`.
// Field order is stable so diffing two audits is human-readable.
type AuditReport struct {
	ProjectName    string                   `json:"project_name"`
	ProjectKind    string                   `json:"project_kind"`
	BinaryVersion  string                   `json:"binary_version"`
	GeneratedAt    time.Time                `json:"generated_at"`
	Categories     map[string]AuditCategory `json:"categories"`
	OverallStatus  AuditStatus              `json:"overall_status"`
}

// auditCategoryOrder pins the print-order so human output stays stable
// regardless of map iteration. Categories not in this list fall back to
// alphabetical at the end.
var auditCategoryOrder = []string{
	"version",
	"shape",
	"conventions",
	"codegen",
	"packs",
	"pack_graph",
	"proto_migration_alignment",
	"migration_safety",
	"wire_coverage",
	"scaffold_markers",
	"deps",
}

func newAuditCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Print a comprehensive project state snapshot",
		Long: `Print a comprehensive snapshot of forge project state.

Audit reports forge version pin, project shape, lint roll-ups, codegen
state, pack health, proto vs migration alignment, scaffold markers, and
dep health. Use --json for machine-readable output (sub-agents).

Examples:
  forge audit            # human-readable
  forge audit --json     # machine-readable`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAudit(jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func runAudit(jsonOut bool) error {
	report, err := buildAuditReport(".")
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printAuditReport(os.Stdout, report)
	return nil
}

// buildAuditReport collects every category's data and rolls up the
// overall status. Errors in individual category collectors are folded
// into a "warn" status for that category — we never bail the whole audit
// because a single grep failed; partial information beats nothing.
func buildAuditReport(projectDir string) (*AuditReport, error) {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project dir: %w", err)
	}

	cfg, cfgErr := loadProjectConfigFrom(filepath.Join(abs, defaultProjectConfigFile))
	if cfgErr != nil && !errors.Is(cfgErr, ErrProjectConfigNotFound) {
		return nil, fmt.Errorf("load project config: %w", cfgErr)
	}

	report := &AuditReport{
		BinaryVersion: buildinfo.Version(),
		GeneratedAt:   time.Now().UTC(),
		Categories:    make(map[string]AuditCategory),
	}
	if cfg != nil {
		report.ProjectName = cfg.Name
		report.ProjectKind = cfg.EffectiveKind()
	} else {
		report.ProjectName = filepath.Base(abs)
		report.ProjectKind = "unknown"
	}

	report.Categories["version"] = auditVersion(cfg)
	report.Categories["shape"] = auditShape(cfg, abs)
	report.Categories["conventions"] = auditConventions(cfg, abs)
	report.Categories["codegen"] = auditCodegen(cfg, abs)
	report.Categories["packs"] = auditPacks(cfg)
	report.Categories["pack_graph"] = auditPackGraph(cfg)
	report.Categories["proto_migration_alignment"] = auditProtoMigration(cfg, abs)
	report.Categories["migration_safety"] = auditMigrationSafety(cfg, abs)
	report.Categories["wire_coverage"] = auditWireCoverage(abs)
	report.Categories["scaffold_markers"] = auditScaffoldMarkers(abs)
	report.Categories["deps"] = auditDeps(abs)

	report.OverallStatus = rollupStatus(report.Categories)
	return report, nil
}

// rollupStatus collapses per-category statuses into one overall verdict.
// "error" beats "warn" beats "ok", same precedence forge doctor uses.
func rollupStatus(cats map[string]AuditCategory) AuditStatus {
	worst := AuditStatusOK
	for _, c := range cats {
		switch c.Status {
		case AuditStatusError:
			return AuditStatusError
		case AuditStatusWarn:
			worst = AuditStatusWarn
		}
	}
	return worst
}

// auditVersion compares forge.yaml's pinned forge_version against the
// running binary. Mismatches surface as warnings (not errors) because
// running newer is usually fine — `forge upgrade` fixes it.
func auditVersion(cfg *config.ProjectConfig) AuditCategory {
	binv := buildinfo.Version()
	if cfg == nil {
		return AuditCategory{
			Status:  AuditStatusError,
			Summary: "no forge.yaml found — not a forge project",
			Details: map[string]any{"binary_version": binv},
		}
	}
	pinned := cfg.EffectiveForgeVersion()
	details := map[string]any{
		"pinned_version": pinned,
		"binary_version": binv,
	}
	if warning := forgeVersionMismatchWarning(cfg.ForgeVersion, binv); warning != "" {
		details["hint"] = fmt.Sprintf("run `%s upgrade` to align", CLIName())
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: warning,
			Details: details,
		}
	}
	return AuditCategory{
		Status:  AuditStatusOK,
		Summary: fmt.Sprintf("forge_version %s matches binary", pinned),
		Details: details,
	}
}

// auditShape inventories the project's structural elements: services
// (and their RPC counts), workers, operators, frontends, packs.
func auditShape(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	if cfg == nil {
		return AuditCategory{Status: AuditStatusError, Summary: "no forge.yaml"}
	}
	type svcInfo struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		RPCCount int    `json:"rpc_count"`
	}
	var services, workers, operators []svcInfo
	var frontends []map[string]string

	// Parse RPC counts when proto/services exists. Failure here is silent —
	// we still emit the structural shape from forge.yaml so the user gets
	// the inventory even when codegen is broken.
	rpcByService := map[string]int{}
	if dirExists(filepath.Join(projectDir, "proto", "services")) {
		if defs, err := codegen.ParseServicesFromProtos(filepath.Join(projectDir, "proto", "services"), projectDir); err == nil {
			for _, d := range defs {
				rpcByService[d.Name] = len(d.Methods)
			}
		}
	}

	for _, s := range cfg.Services {
		info := svcInfo{Name: s.Name, Type: s.Type}
		// match by ProtoService name suffix (Echo → EchoService)
		for protoName, count := range rpcByService {
			short := strings.TrimSuffix(protoName, "Service")
			if strings.EqualFold(short, s.Name) || strings.EqualFold(protoName, s.Name) {
				info.RPCCount = count
				break
			}
		}
		switch s.Type {
		case "worker":
			workers = append(workers, info)
		case "operator":
			operators = append(operators, info)
		default:
			services = append(services, info)
		}
	}
	for _, fe := range cfg.Frontends {
		frontends = append(frontends, map[string]string{"name": fe.Name, "type": fe.Type})
	}

	details := map[string]any{
		"services":  services,
		"workers":   workers,
		"operators": operators,
		"frontends": frontends,
		"packs":     cfg.Packs,
		"packages":  packageNames(cfg.Packages),
	}
	summary := fmt.Sprintf("kind=%s, %d service(s), %d worker(s), %d operator(s), %d frontend(s), %d pack(s)",
		cfg.EffectiveKind(), len(services), len(workers), len(operators), len(frontends), len(cfg.Packs))
	return AuditCategory{Status: AuditStatusOK, Summary: summary, Details: details}
}

func packageNames(pkgs []config.PackageConfig) []string {
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.Name)
	}
	return out
}

// auditConventions runs the lint linters whose results are amenable to
// programmatic roll-up: forgeconv, dblint. Anything that requires
// shelling to a Go subprocess (golangci, contractlint) is expensive and
// noisy in an audit context — we surface a hint to run `forge lint` for
// the full picture instead.
func auditConventions(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	counts := map[string]int{}
	hasErrors := false
	hasWarnings := false

	protoDir := filepath.Join(projectDir, "proto")
	if dirExists(protoDir) {
		if res, err := forgeconv.LintProtoTree(protoDir); err == nil {
			for _, f := range res.Findings {
				key := "conventions/" + string(f.Severity)
				counts[key]++
				if f.Severity == forgeconv.SeverityError {
					hasErrors = true
				} else {
					hasWarnings = true
				}
			}
		}
	}

	dbDir := filepath.Join(projectDir, "proto", "db")
	if dirExists(dbDir) {
		if res, err := dblint.LintProtoDir(dbDir); err == nil {
			for _, f := range res.Findings {
				key := "db/" + string(f.Severity)
				counts[key]++
				if f.Severity == dblint.SeverityWarning {
					hasWarnings = true
				}
			}
		}
	}

	status := AuditStatusOK
	switch {
	case hasErrors:
		status = AuditStatusError
	case hasWarnings:
		status = AuditStatusWarn
	}

	summary := "no convention violations"
	if hasErrors || hasWarnings {
		var bits []string
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			bits = append(bits, fmt.Sprintf("%s=%d", k, counts[k]))
		}
		summary = strings.Join(bits, ", ")
	}

	_ = cfg // reserved for future per-feature gating
	return AuditCategory{
		Status:  status,
		Summary: summary,
		Details: map[string]any{
			"counts": counts,
			"hint":   fmt.Sprintf("run `%s lint` for full output (golangci, contractlint, etc.)", CLIName()),
		},
	}
}

// auditCodegen reports on .forge/checksums.json freshness, hand-edits to
// forge-space files, and obvious orphan _gen files (those whose source
// proto / contract.go has been removed).
func auditCodegen(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	cs, err := generator.LoadChecksums(projectDir)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("could not load .forge/checksums.json: %v", err),
		}
	}

	details := map[string]any{
		"tracked_files":  len(cs.Files),
		"forge_version":  cs.ForgeVersion,
	}

	// .forge/checksums.json mtime as a rough "last generate" timestamp.
	checksumPath := filepath.Join(projectDir, ".forge", "checksums.json")
	if stat, statErr := os.Stat(checksumPath); statErr == nil {
		details["last_generate"] = stat.ModTime().UTC().Format(time.RFC3339)
	} else {
		details["last_generate"] = "never"
	}

	// User-edited gen files: tracked checksum but file content has drifted.
	var modified []string
	for rel := range cs.Files {
		if cs.IsFileModified(projectDir, rel) {
			modified = append(modified, rel)
		}
	}
	sort.Strings(modified)
	if len(modified) > 0 {
		details["user_edited_gen_files"] = modified
	}

	// Orphan _gen detection: walk the project for files ending in _gen.go
	// that aren't tracked by the checksum file at all. These are usually
	// safe to delete (cleanupStaleArtifacts would do it on next generate),
	// but flagging them at audit time tells the user without forcing a
	// regenerate.
	orphans := findOrphanGenFiles(projectDir, cs.Files)
	if len(orphans) > 0 {
		details["orphan_gen_files"] = orphans
	}

	_ = cfg
	status := AuditStatusOK
	summary := fmt.Sprintf("%d tracked, %d modified, %d orphans", len(cs.Files), len(modified), len(orphans))
	if len(modified) > 0 || len(orphans) > 0 {
		status = AuditStatusWarn
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// findOrphanGenFiles walks projectDir for *_gen.go files that aren't in
// the tracked-files map. The walk skips noisy roots (vendor/, gen/,
// node_modules/, .git/) so audit stays cheap on large projects.
func findOrphanGenFiles(projectDir string, tracked map[string]generator.FileChecksumEntry) []string {
	var orphans []string
	skip := map[string]struct{}{
		"vendor": {}, ".git": {}, "node_modules": {}, "gen": {}, ".forge": {},
	}
	_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, ok := skip[d.Name()]; ok {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, "_gen.go") && !strings.HasSuffix(name, "_gen_test.go") {
			return nil
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return nil
		}
		if _, ok := tracked[filepath.ToSlash(rel)]; ok {
			return nil
		}
		// Banner check: forge-banner files we generated but never tracked
		// (older versions of forge didn't checksum everything).
		if isForgeGeneratedBanner(path) {
			orphans = append(orphans, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(orphans)
	return orphans
}

// auditPacks compares each installed pack's version against the version
// embedded in the binary's pack registry. A mismatch (project pinned to
// "v0.1.0" but the binary ships "v0.2.0") surfaces as a warn.
func auditPacks(cfg *config.ProjectConfig) AuditCategory {
	if cfg == nil || len(cfg.Packs) == 0 {
		return AuditCategory{Status: AuditStatusOK, Summary: "no packs installed"}
	}
	type packEntry struct {
		Name             string `json:"name"`
		InstalledVersion string `json:"installed_version,omitempty"`
		LatestVersion    string `json:"latest_version,omitempty"`
		Status           string `json:"status"`
	}
	var entries []packEntry
	hasWarn := false
	for _, name := range cfg.Packs {
		// cfg.Packs is just a name list; we don't track per-project version
		// pins yet, so "installed" == whatever the binary ships.
		p, err := packs.GetPack(name)
		entry := packEntry{Name: name}
		if err != nil {
			entry.Status = "missing"
			entry.LatestVersion = "?"
			hasWarn = true
		} else {
			entry.LatestVersion = p.Version
			entry.InstalledVersion = p.Version
			entry.Status = "ok"
		}
		entries = append(entries, entry)
	}
	status := AuditStatusOK
	if hasWarn {
		status = AuditStatusWarn
	}
	return AuditCategory{
		Status:  status,
		Summary: fmt.Sprintf("%d pack(s) installed", len(entries)),
		Details: map[string]any{"packs": entries},
	}
}

// auditProtoMigration reports drift between proto-defined entities and
// the current SQL migrations. We give one of three verdicts:
//
//   - "proto entities authoritative" — entities exist; migrations exist;
//     every entity table appears in at least one migration.
//   - "migrations authoritative" — migrations exist; no proto entities
//     are defined (the project hand-writes migrations).
//   - "diverged (X tables in migrations not in proto / Y in proto not in
//     migrations)" — the two are out of sync.
func auditProtoMigration(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	migDir := filepath.Join(projectDir, "db", "migrations")
	if cfg != nil && cfg.Database.MigrationsDir != "" {
		migDir = filepath.Join(projectDir, cfg.Database.MigrationsDir)
	}
	hasMigrations := dirExists(migDir)

	var entities []codegen.EntityDef
	if dirExists(filepath.Join(projectDir, "proto")) {
		entities, _ = codegen.ParseEntityProtos(projectDir)
	}

	if !hasMigrations && len(entities) == 0 {
		return AuditCategory{Status: AuditStatusOK, Summary: "no proto entities, no migrations (n/a)"}
	}
	if !hasMigrations {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("%d proto entities but no migrations directory", len(entities)),
			Details: map[string]any{
				"hint": "run `forge generate` to scaffold an initial migration, or see the `proto` and `db` skills for the greenfield-vs-migrated boundary",
			},
		}
	}
	if len(entities) == 0 {
		return AuditCategory{Status: AuditStatusOK, Summary: "migrations authoritative (no proto entities)"}
	}

	migrationTables := tablesFromMigrations(migDir)
	entityTables := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		entityTables[e.TableName] = struct{}{}
	}

	var inMigNotProto, inProtoNotMig []string
	for t := range migrationTables {
		if _, ok := entityTables[t]; !ok {
			inMigNotProto = append(inMigNotProto, t)
		}
	}
	for t := range entityTables {
		if _, ok := migrationTables[t]; !ok {
			inProtoNotMig = append(inProtoNotMig, t)
		}
	}
	sort.Strings(inMigNotProto)
	sort.Strings(inProtoNotMig)

	if len(inMigNotProto) == 0 && len(inProtoNotMig) == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "proto entities authoritative — all entity tables present in migrations",
			Details: map[string]any{"entity_count": len(entities), "migration_table_count": len(migrationTables)},
		}
	}
	return AuditCategory{
		Status: AuditStatusWarn,
		Summary: fmt.Sprintf("diverged: %d table(s) in migrations not in proto, %d in proto not in migrations",
			len(inMigNotProto), len(inProtoNotMig)),
		Details: map[string]any{
			"in_migrations_not_in_proto": inMigNotProto,
			"in_proto_not_in_migrations": inProtoNotMig,
			"hint":                       "see the `proto` and `migration` skills for the greenfield-vs-migrated boundary; resolve with `forge db proto sync-from-db`, by dropping the proto entities, or by rolling a migration forward",
		},
	}
}

// tablesFromMigrations naïvely greps "CREATE TABLE [IF NOT EXISTS]
// <name>" out of every .sql under migDir. It's a rough heuristic — the
// authoritative answer would parse SQL — but it captures the 95% case and
// the 5% it misses tend to be exotic forms (DDL inside CTE, etc.) that
// would never appear in a forge-generated migration.
var createTableRE = regexp.MustCompile(`(?i)CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+["` + "`" + `]?([a-zA-Z_][a-zA-Z0-9_]*)["` + "`" + `]?`)

func tablesFromMigrations(dir string) map[string]struct{} {
	out := map[string]struct{}{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".sql") {
			return nil
		}
		// Skip "down" migrations — they DROP TABLE, not CREATE.
		if strings.Contains(filepath.Base(path), ".down.") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range createTableRE.FindAllStringSubmatch(string(data), -1) {
			if len(m) > 1 {
				out[strings.ToLower(m[1])] = struct{}{}
			}
		}
		return nil
	})
	return out
}

// auditScaffoldMarkers counts unfilled FORGE_SCAFFOLD placeholders that
// have survived a commit. The semantic check matches `forge lint
// --scaffolds`: only line-start `// FORGE_SCAFFOLD:` comments count as
// real markers; mere references to the literal string in source, docs,
// or template bodies do not. Directories that are intentional homes for
// markers (linter testdata, project templates) are skipped, matching
// the lint walker's skipDir + scaffold-template carve-outs.
//
// Without this filter, the audit would warn on every project that
// includes the scaffold linter source itself, the analyzer fixtures, or
// the generator templates that EMIT markers — i.e. it would be noisy on
// forge's own tree (which is a forge-managed project).
func auditScaffoldMarkers(projectDir string) AuditCategory {
	skip := map[string]struct{}{
		"vendor": {}, ".git": {}, "node_modules": {}, "gen": {}, ".forge": {},
		// testdata: linter fixtures intentionally hold markers so the
		// analyzer suite can assert it fires on them.
		"testdata": {},
		// templates/: forge's own scaffold templates contain the
		// markers as their literal output — they're not unfilled
		// placeholders in *this* tree.
		"templates": {},
	}
	var files []string
	total := 0
	_ = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, ok := skip[d.Name()]; ok {
				return filepath.SkipDir
			}
			return nil
		}
		// Only scan files whose markers would be unfilled placeholders
		// (Go/proto/TS/YAML/SQL/templates). Markdown and JSON often
		// reference the marker syntax verbatim for documentation; we
		// don't want every README that mentions `FORGE_SCAFFOLD:` to
		// turn audit yellow.
		if !isMarkerScannable(d.Name()) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		count := countLineStartScaffoldMarkers(data)
		if count > 0 {
			rel, _ := filepath.Rel(projectDir, path)
			files = append(files, filepath.ToSlash(rel))
			total += count
		}
		return nil
	})
	sort.Strings(files)
	status := AuditStatusOK
	if total > 0 {
		status = AuditStatusWarn
	}
	return AuditCategory{
		Status:  status,
		Summary: fmt.Sprintf("%d FORGE_SCAFFOLD marker(s) across %d file(s)", total, len(files)),
		Details: map[string]any{"files": files, "total_markers": total},
	}
}

// countLineStartScaffoldMarkers counts lines whose first non-whitespace
// content is the comment-form scaffold marker. Mirrors
// internal/linter/scaffolds.countScaffoldMarkers — kept separate to
// avoid pulling the linter package into the cli audit dependency
// graph.
func countLineStartScaffoldMarkers(data []byte) int {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		// Match both Go-style `// FORGE_SCAFFOLD:` and YAML/shell-style
		// `# FORGE_SCAFFOLD:` (the lint analyzer is Go-only, but audit
		// is allowed to span more file types).
		if strings.HasPrefix(trimmed, "// FORGE_SCAFFOLD:") || strings.HasPrefix(trimmed, "# FORGE_SCAFFOLD:") {
			count++
		}
	}
	return count
}

// isLikelyTextFile keeps the scaffold-marker walk cheap by only opening
// files whose extension is plausibly source (Go, proto, TS, YAML, MD,
// SQL, KCL). Everything else (.png, .pdf, .so) is skipped.
func isLikelyTextFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".go", ".proto", ".ts", ".tsx", ".js", ".jsx", ".yaml", ".yml",
		".md", ".sql", ".k", ".sh", ".tmpl", ".toml", ".json":
		return true
	}
	return false
}

// isMarkerScannable narrows isLikelyTextFile down to file types whose
// FORGE_SCAFFOLD markers are real unfilled placeholders rather than
// documentation references. Markdown and JSON commonly cite the marker
// syntax in prose / fixtures, so they're excluded — those occurrences
// would otherwise generate noisy "scaffold present" warnings on every
// project that documents how scaffolds work.
func isMarkerScannable(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".go", ".proto", ".ts", ".tsx", ".js", ".jsx",
		".yaml", ".yml", ".sql", ".k", ".sh", ".tmpl", ".toml":
		return true
	}
	return false
}

// auditPackGraph checks that every installed pack's declared `depends_on`
// is also installed. Surfaces "missing producer" cases — e.g. someone
// hand-edited cfg.Packs to remove audit-log while leaving api-key in
// place, or installed an older project on a newer forge that introduced
// a new dep edge. Returns ok when no installed pack declares a dep, or
// when every dep is satisfied.
func auditPackGraph(cfg *config.ProjectConfig) AuditCategory {
	if cfg == nil || len(cfg.Packs) == 0 {
		return AuditCategory{Status: AuditStatusOK, Summary: "no packs installed (n/a)"}
	}
	missing := packs.MissingDependencies(cfg.Packs)
	// Also build the full edge list for the details payload — useful for
	// LLM consumers that want to render the graph without a second
	// round-trip to `forge pack list --deps`.
	edges := map[string][]string{}
	for _, name := range cfg.Packs {
		p, err := packs.GetPack(name)
		if err != nil || len(p.DependsOn) == 0 {
			continue
		}
		edges[name] = append([]string(nil), p.DependsOn...)
	}
	details := map[string]any{
		"installed_packs": cfg.Packs,
		"declared_edges":  edges,
	}
	if len(missing) > 0 {
		details["missing_dependencies"] = missing
		details["hint"] = "run `forge pack add <name>` for each missing dep, or remove the consuming pack to drop the requirement"
		return AuditCategory{
			Status:  AuditStatusError,
			Summary: fmt.Sprintf("%d missing pack dependency(ies): %s", len(missing), strings.Join(missing, ", ")),
			Details: details,
		}
	}
	return AuditCategory{
		Status:  AuditStatusOK,
		Summary: fmt.Sprintf("%d pack(s) installed; %d declared edge(s) all satisfied", len(cfg.Packs), len(edges)),
		Details: details,
	}
}

// auditMigrationSafety summarises the project's migration_safety
// configuration: number of allowlisted destructive globs, the
// destructive_change severity setting, and the timestamp of the most
// recent migration. Surfaces as warn when allowed_destructive is
// non-empty (informational — user has consciously opted out of the
// destructive-change guard for some files), error when the directory
// has migrations but none are parseable.
func auditMigrationSafety(cfg *config.ProjectConfig, projectDir string) AuditCategory {
	migDir := filepath.Join(projectDir, "db", "migrations")
	if cfg != nil && cfg.Database.MigrationsDir != "" {
		migDir = filepath.Join(projectDir, cfg.Database.MigrationsDir)
	}
	hasMigrations := dirExists(migDir)
	if !hasMigrations {
		return AuditCategory{Status: AuditStatusOK, Summary: "no migrations directory (n/a)"}
	}

	// Count migrations + find the latest mtime.
	var latestMtime time.Time
	latestName := ""
	migCount := 0
	entries, _ := os.ReadDir(migDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		migCount++
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latestMtime) {
			latestMtime = info.ModTime()
			latestName = e.Name()
		}
	}

	details := map[string]any{
		"migration_count":   migCount,
		"migrations_dir":    migDir,
	}
	if latestName != "" {
		details["latest_migration"] = latestName
		details["latest_migration_mtime"] = latestMtime.UTC().Format(time.RFC3339)
	}

	allowedCount := 0
	severity := "error"
	if cfg != nil {
		allowedCount = len(cfg.Database.MigrationSafety.AllowedDestructive)
		severity = cfg.Database.MigrationSafety.EffectiveDestructiveChange()
		if allowedCount > 0 {
			details["allowed_destructive"] = cfg.Database.MigrationSafety.AllowedDestructive
		}
		details["destructive_change_severity"] = severity
	}

	status := AuditStatusOK
	summary := fmt.Sprintf("%d migration(s); %d allowed_destructive; destructive_change=%s",
		migCount, allowedCount, severity)
	if allowedCount > 0 {
		// Informational warn — surface that the project has explicit
		// destructive carve-outs the user should re-review periodically.
		status = AuditStatusWarn
		details["hint"] = "review allowed_destructive entries periodically; once a destructive migration ships, remove its allowlist entry to re-enable the guard"
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// auditWireCoverage rolls up unresolved Deps fields in pkg/app/wire_gen.go
// — the same surface as `forge lint --wire-coverage`, but as a count and
// per-component breakdown rather than per-finding output. Useful for an
// audit-level "is wire complete?" yes/no without making the user shell
// to lint.
func auditWireCoverage(projectDir string) AuditCategory {
	path := filepath.Join(projectDir, "pkg", "app", "wire_gen.go")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "no pkg/app/wire_gen.go (n/a — library project or pre-generate)",
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("could not open wire_gen.go: %v", err),
		}
	}
	defer f.Close()

	findings, err := scanWireGen(f, path, projectDir)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("scan failed: %v", err),
		}
	}

	if len(findings) == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "wire coverage clean — no unresolved Deps fields",
		}
	}

	// Aggregate by component (wire*Deps function) for the breakdown.
	byComponent := map[string][]string{}
	for _, f := range findings {
		comp := f.Function
		if comp == "" {
			comp = "(unattributed)"
		}
		byComponent[comp] = append(byComponent[comp], f.Field)
	}
	components := make([]string, 0, len(byComponent))
	for k := range byComponent {
		components = append(components, k)
	}
	sort.Strings(components)

	details := map[string]any{
		"unresolved_count":      len(findings),
		"affected_components":   components,
		"by_component":          byComponent,
		"hint":                  fmt.Sprintf("run `%s lint --wire-coverage` for the full per-line report", CLIName()),
	}
	return AuditCategory{
		Status:  AuditStatusWarn,
		Summary: fmt.Sprintf("%d unresolved Deps field(s) across %d component(s)", len(findings), len(components)),
		Details: details,
	}
}

// auditDeps surfaces dep-shaped risks: missing go.sum, gen/ unstable
// (would `go mod tidy` produce a diff?). We don't actually run `go mod
// tidy` because that's expensive and would mutate state — we just check
// for go.sum presence and report.
func auditDeps(projectDir string) AuditCategory {
	details := map[string]any{}
	hasWarn := false

	if _, err := os.Stat(filepath.Join(projectDir, "go.mod")); err == nil {
		details["go_mod"] = "present"
		if _, err := os.Stat(filepath.Join(projectDir, "go.sum")); os.IsNotExist(err) {
			details["go_sum"] = "missing — run `go mod tidy`"
			hasWarn = true
		} else {
			details["go_sum"] = "present"
		}
	} else {
		details["go_mod"] = "missing"
	}

	if _, err := os.Stat(filepath.Join(projectDir, "gen", "go.mod")); err == nil {
		details["gen_go_mod"] = "present"
	}

	status := AuditStatusOK
	summary := "deps look healthy"
	if hasWarn {
		status = AuditStatusWarn
		summary = "deps need attention"
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}

// printAuditReport renders the human-readable audit. Layout: one line
// header, then one block per category in auditCategoryOrder, then a
// trailing overall verdict.
func printAuditReport(w *os.File, r *AuditReport) {
	fmt.Fprintf(w, "Forge audit — %s (kind=%s, binary=%s)\n", r.ProjectName, r.ProjectKind, r.BinaryVersion)
	fmt.Fprintf(w, "Generated at %s\n\n", r.GeneratedAt.Format(time.RFC3339))

	printed := map[string]struct{}{}
	for _, key := range auditCategoryOrder {
		if cat, ok := r.Categories[key]; ok {
			printAuditCategory(w, key, cat)
			printed[key] = struct{}{}
		}
	}
	// Fallthrough: any categories not in the canonical order, alphabetical.
	var extras []string
	for k := range r.Categories {
		if _, ok := printed[k]; !ok {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		printAuditCategory(w, k, r.Categories[k])
	}

	fmt.Fprintf(w, "Overall: %s\n", strings.ToUpper(string(r.OverallStatus)))
}

func printAuditCategory(w *os.File, key string, cat AuditCategory) {
	icon := "✓"
	switch cat.Status {
	case AuditStatusWarn:
		icon = "⚠"
	case AuditStatusError:
		icon = "✗"
	}
	fmt.Fprintf(w, "%s %s — %s\n", icon, key, cat.Summary)
	if len(cat.Details) > 0 {
		// Print details indented; primitives inline, slices/maps collapsed.
		keys := make([]string, 0, len(cat.Details))
		for k := range cat.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := cat.Details[k]
			fmt.Fprintf(w, "    %s: %s\n", k, formatDetailValue(v))
		}
	}
	fmt.Fprintln(w)
}

func formatDetailValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		if len(t) == 0 {
			return "[]"
		}
		if len(t) <= 5 {
			return "[" + strings.Join(t, ", ") + "]"
		}
		return fmt.Sprintf("[%s, ... (%d total)]", strings.Join(t[:5], ", "), len(t))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		s := string(b)
		if len(s) > 200 {
			s = s[:197] + "..."
		}
		return s
	}
}
