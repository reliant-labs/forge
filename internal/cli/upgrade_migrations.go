package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/templates"
)

// Migration metadata + state lives alongside the existing template-drift
// upgrader (upgrade.go). The split is intentional: `forge upgrade` (the
// existing command) runs deterministic codemods + template-drift rewrites;
// `forge upgrade list` and `forge upgrade apply <id>` surface the
// LLM-readable migration skills under skills/forge/migrations/ and let
// the user (or an agent) record that a migration has been applied.
//
// Migration skills declare frontmatter:
//
//	---
//	name: dev-target-to-kcl-deploy
//	description: ...
//	applies-from: v0.5.0
//	applies-to:   v0.6.0
//	detection:    grep -l "dev_target" forge.yaml
//	---
//
// `applies-from` / `applies-to` form a half-open [from, to) range over
// the project's pinned forge_version. `detection` is an optional shell
// snippet — when present, it runs in the project root and the migration
// is treated as "needed" only if the command exits 0 (matched something).

// migrationSkillSources are the embedded-template path prefixes where
// migration skills live. We walk BOTH:
//
//   - "migrations" — the new convention (Agent A's kcl-schemas-to-module
//     and Agent D's dev-target-to-kcl-deploy land here). Every subdir is
//     a migration; nothing else lives in this tree.
//   - "migration" — the legacy tree carries v*-to-* migration skills
//     (e.g. v0.1-to-v0.2, v0.x-to-contractkit) alongside non-migration
//     sub-skills (cli, service, upgrade, top-level SKILL.md). We only
//     pick up subdirs that match the v*-to-* shape so the general
//     migration sub-skills don't leak into `forge upgrade list`.
//
// Both trees ship `name:` / `description:` / `applies-from:` /
// `applies-to:` / `detection:` frontmatter, so the parser is shared.
var migrationSkillSources = []migrationSource{
	{root: "migrations", filter: nil},          // new — accept every subdir
	{root: "migration", filter: isVersionDir},  // legacy — accept v*-to-* only
}

// migrationSource is one entry in migrationSkillSources: an embedded
// template root plus an optional dir-name filter applied to direct
// children. A nil filter means "accept every subdir".
type migrationSource struct {
	root   string
	filter func(dir string) bool
}

// isVersionDir reports whether `dir` looks like a v*-to-* migration
// directory (e.g. "v0.1-to-v0.2", "v0.x-to-contractkit"). Used to filter
// the legacy `migration/` tree down to actual migrations and drop the
// non-migration sub-skills that share the dir (cli, service, upgrade).
func isVersionDir(dir string) bool {
	return strings.HasPrefix(dir, "v") && strings.Contains(dir, "-to-")
}

// migrationsStateFile is the on-disk record of applied migrations,
// relative to the project root. The file is JSON; absent file means
// no migrations have been recorded yet.
const migrationsStateFile = ".forge/migrations.json"

// migrationMeta is the parsed frontmatter for one migration skill.
//
// AppliesFrom / AppliesTo are SemVer-ish strings ("v0.5.0", "0.6"); an
// empty AppliesFrom means "applies to any pre-AppliesTo project" and an
// empty AppliesTo means "applies to any project >= AppliesFrom".
type migrationMeta struct {
	// ID is the directory name (e.g. "dev-target-to-kcl-deploy") — the
	// stable identifier used by `forge upgrade apply <id>` and in the
	// applied-state JSON.
	ID string `json:"id"`
	// SkillPath is the path you'd pass to `forge skill load` to read
	// the migration's body (e.g. "migrations/dev-target-to-kcl-deploy").
	SkillPath string `json:"skill_path"`
	// Title is a human-readable name from frontmatter `name:`. Falls
	// back to ID when missing.
	Title string `json:"title"`
	// Description is the one-line summary from frontmatter `description:`.
	Description string `json:"description"`
	// AppliesFrom / AppliesTo bound the project version range this
	// migration applies to. Half-open: [AppliesFrom, AppliesTo).
	AppliesFrom string `json:"applies_from,omitempty"`
	AppliesTo   string `json:"applies_to,omitempty"`
	// Detection is an optional shell command run in the project root.
	// When present, the migration is filtered out unless the command
	// exits 0 (i.e. found something).
	Detection string `json:"detection,omitempty"`
}

// migrationsState is the JSON shape written to .forge/migrations.json.
type migrationsState struct {
	// Applied maps migration ID -> ISO-8601 timestamp it was marked
	// applied. Keys are stable across forge versions (the migration
	// ID is the directory name, not a hash).
	Applied map[string]string `json:"applied"`
}

// pendingMigration is one row in `forge upgrade list` output.
type pendingMigration struct {
	Meta    migrationMeta `json:"meta"`
	Applied bool          `json:"applied"`
	// AppliedAt is the ISO-8601 timestamp from migrations.json when
	// Applied is true; empty otherwise.
	AppliedAt string `json:"applied_at,omitempty"`
}

// attachMigrationSubcommands wires `forge upgrade list` and
// `forge upgrade apply <id>` onto the existing upgrade cobra command.
// The existing `forge upgrade` (no args) keeps its template-drift +
// codemod behaviour — these subcommands are an additive surface for
// the migration-skill flow.
func attachMigrationSubcommands(upgrade *cobra.Command) {
	upgrade.AddCommand(newUpgradeListCmd())
	upgrade.AddCommand(newUpgradeApplyCmd())
}

func newUpgradeListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending forge migrations for this project",
		Long: `List forge migration skills whose version range covers this project's
pinned forge_version and whose detection script (if any) matches.

Migrations are LLM-readable playbooks under skills/forge/migrations/.
This command does NOT apply them — it surfaces the worklist so the user
(or an agent like Claude Code) can load each skill via 'forge skill load'
and execute the steps. Use 'forge upgrade apply <id>' to record a
migration as applied once you've finished its steps.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pending, err := computePendingMigrations()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				return writePendingMigrationsJSON(out, pending)
			}
			return writePendingMigrationsHuman(out, pending)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output JSON instead of human-readable text")
	return cmd
}

func newUpgradeApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply <migration-id>",
		Short: "Record a migration as applied (writes .forge/migrations.json)",
		Long: `Mark a migration as applied. This does NOT execute the migration —
loading the skill and running its steps is the user's (or an agent's)
job. 'apply' just records the outcome so later 'forge upgrade list'
runs hide migrations that have already been worked through.

The migration ID is the directory name under skills/forge/migrations/
(e.g. "dev-target-to-kcl-deploy"). Pass --force to record an apply
even when the migration is not in the pending list (rare, but useful
when an out-of-range migration was applied manually).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			return runUpgradeApply(cmd.OutOrStdout(), id)
		},
	}
}

// computePendingMigrations is the core listing routine. It returns
// every known migration tagged with whether it has been applied (per
// .forge/migrations.json), filtered to only those whose:
//   - applies-from / applies-to range covers the project's effective
//     forge_version (when both bounds are present), AND
//   - detection script (when present) exits 0 in the project root.
//
// Migrations whose version range or detection script excludes them are
// simply omitted — the caller doesn't need to reason about why.
func computePendingMigrations() ([]pendingMigration, error) {
	projectRoot, err := findProjectRoot()
	if err != nil {
		return nil, err
	}
	if projectRoot == "" {
		return nil, cliutil.UserErr("forge upgrade list",
			"no forge project found in this directory or any parent",
			"",
			"run 'forge new' to create a project, or cd into one")
	}

	// Read the project's pinned forge_version when forge.yaml is present.
	// We tolerate a missing/unparseable forge.yaml here — listing migrations
	// is useful even before a project has been formally pinned (the spec
	// explicitly calls out "project with no .forge/version.json lists every
	// migration as pending").
	projectVersion := ""
	if cfgPath, ferr := findProjectConfigFile(); ferr == nil {
		if cfg, lerr := loadProjectConfigFrom(cfgPath); lerr == nil {
			projectVersion = cfg.EffectiveForgeVersion()
		}
	}

	migrations, err := loadMigrationMetas()
	if err != nil {
		return nil, fmt.Errorf("load migration skills: %w", err)
	}

	state, err := readMigrationsState(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", migrationsStateFile, err)
	}

	var out []pendingMigration
	for _, m := range migrations {
		if !versionInRange(projectVersion, m.AppliesFrom, m.AppliesTo) {
			continue
		}
		if !runDetection(projectRoot, m.Detection) {
			continue
		}
		row := pendingMigration{Meta: m}
		if ts, ok := state.Applied[m.ID]; ok {
			row.Applied = true
			row.AppliedAt = ts
		}
		out = append(out, row)
	}
	// Stable sort by ID — keeps human + JSON output deterministic
	// across binary builds.
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.ID < out[j].Meta.ID })
	return out, nil
}

// loadMigrationMetas enumerates every SKILL.md under each root in
// migrationSkillSources in the embedded templates and parses its
// frontmatter. IDs are deduplicated across sources — the first source
// to declare a given ID wins, which keeps the new `migrations/` tree as
// the authoritative location when a migration moves from legacy to new.
func loadMigrationMetas() ([]migrationMeta, error) {
	var out []migrationMeta
	seen := map[string]bool{}
	for _, src := range migrationSkillSources {
		metas := loadMigrationMetasFrom(src)
		for _, m := range metas {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	return out, nil
}

// loadMigrationMetasFrom scans one migrationSource and returns the
// migrations found there. A missing root (e.g. `migrations/` in a build
// that predates Agent A) yields nil, not an error — the outer loop
// happily skips empty sources.
func loadMigrationMetasFrom(src migrationSource) []migrationMeta {
	// The skills tree is rooted under "forge" in the embedded FS, so
	// the relative scan path is "forge/<root>".
	relRoot := path.Join("forge", src.root)
	entries, err := templates.ProjectTemplates().List(path.Join("skills", relRoot))
	if err != nil {
		// Missing root is fine — the other source(s) may still have
		// migrations. Callers see an empty slice here.
		return nil
	}

	var out []migrationMeta
	for _, rel := range entries {
		// `entries` are paths relative to skills/forge/<root>, e.g.
		// "dev-target-to-kcl-deploy/SKILL.md" or "v0.1-to-v0.2/SKILL.md".
		if !strings.HasSuffix(rel, "/SKILL.md") {
			continue
		}
		id := strings.TrimSuffix(rel, "/SKILL.md")
		// Defence in depth: a nested SKILL.md (a/b/SKILL.md) would
		// produce an id of "a/b" which is not a valid filesystem-friendly
		// migration identifier. Skip any non-flat layout.
		if strings.Contains(id, "/") {
			continue
		}
		// Apply the source-specific filter — the legacy `migration/`
		// tree mixes general sub-skills (cli, service, upgrade) in with
		// the v*-to-* migrations, so the filter narrows it down.
		if src.filter != nil && !src.filter(id) {
			continue
		}

		content, err := templates.ProjectTemplates().Get(path.Join("skills", relRoot, rel))
		if err != nil {
			continue
		}
		m := parseMigrationFrontmatter(content)
		m.ID = id
		m.SkillPath = path.Join(src.root, id)
		out = append(out, m)
	}
	return out
}

// parseMigrationFrontmatter extracts the migration-skill-specific fields
// from a SKILL.md body. It is a focused parser separate from the generic
// parseFrontmatter in skill.go because migration skills carry extra
// fields (applies-from, applies-to, detection) that aren't part of the
// generic SkillMeta shape.
func parseMigrationFrontmatter(content []byte) migrationMeta {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return migrationMeta{}
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return migrationMeta{}
	}
	block := s[4 : 4+end]

	var m migrationMeta
	for _, line := range strings.Split(block, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Strip surrounding quotes — some authors quote values with
		// special chars in YAML; we don't need a full YAML parser to
		// handle the common case.
		v = strings.Trim(v, `"'`)
		switch k {
		case "name":
			m.Title = v
		case "description":
			m.Description = v
		case "applies-from":
			m.AppliesFrom = v
		case "applies-to":
			m.AppliesTo = v
		case "detection":
			m.Detection = v
		}
	}
	return m
}

// versionInRange reports whether `version` falls in the half-open
// [from, to) range, with the usual special cases:
//   - Empty `version` (no project pin): EVERY migration applies. This
//     matches the spec: "a project with no .forge/version.json lists
//     every migration as pending".
//   - Pre-v0.1 baselines per isPreV01Baseline (the "0.0.0" sentinel
//     OR a "v0.0.0-<timestamp>-<sha>" pseudoversion from `go install`
//     against an untagged checkout): every migration applies. These
//     projects predate the formal upgrade story and should see the
//     full worklist.
//   - Empty `from`: range is (-inf, to).
//   - Empty `to`:   range is [from, +inf).
//   - Both empty:   migration always applies.
//
// Versions are compared as SemVer-ish tuples after stripping the
// leading "v". Non-parseable versions fall through to the cmpVersion
// fallback, which treats unknown components lexicographically.
func versionInRange(version, from, to string) bool {
	if strings.TrimSpace(version) == "" {
		return true
	}
	// Pre-v0.1 baselines (sentinel + pseudoversions) predate the
	// codemod chain — see isPreV01Baseline in upgrade.go. Treating
	// them like "no pin" surfaces every migration so real-world
	// projects pinned to a pseudoversion (cp-forge, kalshi-trader)
	// see the full worklist instead of an empty list.
	if isPreV01Baseline(version) {
		return true
	}
	v := normaliseVersion(version)
	if from != "" {
		if cmpVersion(v, normaliseVersion(from)) < 0 {
			return false
		}
	}
	if to != "" {
		if cmpVersion(v, normaliseVersion(to)) >= 0 {
			return false
		}
	}
	return true
}

// normaliseVersion strips a leading "v" and trailing whitespace. It
// does NOT validate — non-parseable strings are passed through; cmpVersion
// handles the comparison fallback.
func normaliseVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

// cmpVersion compares two SemVer-ish strings ("0.5.0" vs "0.6"). It
// returns -1/0/1 like strings.Compare. Missing patch components are
// treated as 0. A non-numeric component compares lexicographically
// against another non-numeric component, and numerically against a
// numeric component (after a string-to-int conversion attempt).
//
// This is intentionally simple — full SemVer (pre-release, build) is
// overkill for the migration-range gate, which only needs to bracket
// minor versions.
func cmpVersion(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		// Missing components default to "0" so "0.5" compares equal to
		// "0.5.0" — SemVer's standard zero-fill convention.
		ai, bi := "0", "0"
		if i < len(as) && as[i] != "" {
			ai = as[i]
		}
		if i < len(bs) && bs[i] != "" {
			bi = bs[i]
		}
		// Try numeric compare first; fall back to string.
		ax, aerr := strconv.Atoi(ai)
		bx, berr := strconv.Atoi(bi)
		if aerr == nil && berr == nil {
			if ax < bx {
				return -1
			}
			if ax > bx {
				return 1
			}
			continue
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

// runDetection runs the migration's detection script (if present) in
// the project root with `sh -c`. Returns true if the migration should
// be considered "needed" (script absent, OR script exits 0).
//
// We deliberately do not pipe output to the user — the detection script
// is meant as a silent gate. If a script needs to log, it should be
// rewritten as a real check.
func runDetection(projectRoot, script string) bool {
	if strings.TrimSpace(script) == "" {
		return true
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = projectRoot
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// readMigrationsState reads .forge/migrations.json. Absent file is not
// an error — it just means no migrations have been recorded yet.
func readMigrationsState(projectRoot string) (migrationsState, error) {
	state := migrationsState{Applied: map[string]string{}}
	if projectRoot == "" {
		return state, nil
	}
	p := filepath.Join(projectRoot, migrationsStateFile)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, err
	}
	if len(data) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Applied == nil {
		state.Applied = map[string]string{}
	}
	return state, nil
}

// writeMigrationsState writes the applied-set back to disk, creating
// the .forge dir if missing. The file is JSON with sorted keys (Go's
// encoding/json sorts map keys by default) so diffs against a previous
// state are clean.
func writeMigrationsState(projectRoot string, state migrationsState) error {
	if projectRoot == "" {
		return fmt.Errorf("no project root")
	}
	dir := filepath.Join(projectRoot, ".forge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	p := filepath.Join(projectRoot, migrationsStateFile)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal migrations state: %w", err)
	}
	// Trailing newline keeps the file POSIX-friendly.
	data = append(data, '\n')
	return os.WriteFile(p, data, 0o644)
}

// runUpgradeApply is the body of `forge upgrade apply <id>`.
//
// Behaviour:
//   - Reads the project's current applied-state from .forge/migrations.json.
//   - Verifies the migration ID exists in the embedded skill set. Unknown
//     IDs return a UserErr — typo-friendly.
//   - Records the migration as applied with the current timestamp.
//   - Writes the state file back.
//
// Re-applying an already-applied migration is a no-op apart from
// refreshing the timestamp; we don't refuse, because the user might
// legitimately want to re-record after a partial migration.
func runUpgradeApply(out io.Writer, id string) error {
	projectRoot, err := findProjectRoot()
	if err != nil {
		return err
	}
	if projectRoot == "" {
		return cliutil.UserErr("forge upgrade apply",
			"no forge project found in this directory or any parent",
			"",
			"run 'forge new' to create a project, or cd into one")
	}

	known, err := loadMigrationMetas()
	if err != nil {
		return fmt.Errorf("load migration skills: %w", err)
	}
	var found *migrationMeta
	for i := range known {
		if known[i].ID == id {
			found = &known[i]
			break
		}
	}
	if found == nil {
		ids := make([]string, 0, len(known))
		for _, k := range known {
			ids = append(ids, k.ID)
		}
		sort.Strings(ids)
		hint := "run 'forge upgrade list' to see available migration IDs"
		if len(ids) > 0 {
			hint = "available IDs: " + strings.Join(ids, ", ")
		}
		return cliutil.UserErr(
			fmt.Sprintf("forge upgrade apply %s", id),
			fmt.Sprintf("migration %q not found", id),
			"",
			hint,
		)
	}

	state, err := readMigrationsState(projectRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", migrationsStateFile, err)
	}
	state.Applied[id] = time.Now().UTC().Format(time.RFC3339)
	if err := writeMigrationsState(projectRoot, state); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Recorded migration %q as applied at %s\n", id, state.Applied[id])
	return nil
}

// writePendingMigrationsHuman renders the pending list as
// human-readable text. The format mirrors the spec:
//
//	🔧 Pending migrations for this project (forge v0.5.0 → binary vX):
//	  - dev-target-to-kcl-deploy
//	    Title: Migrate forge.yaml dev_target → KCL Service.deploy
//	    Range: v0.5.0 → v0.6.0
//	    Load: forge skill load migrations/dev-target-to-kcl-deploy
//	    Apply once done: forge upgrade apply dev-target-to-kcl-deploy
//
// When the list is empty we print "Project is up to date." per the spec.
func writePendingMigrationsHuman(out io.Writer, pending []pendingMigration) error {
	if len(pending) == 0 {
		_, err := fmt.Fprintln(out, "Project is up to date.")
		return err
	}
	cliName := Name()
	binary := buildinfo.Version()
	_, _ = fmt.Fprintf(out, "Pending migrations (binary %s):\n\n", binary)
	for _, p := range pending {
		marker := "[ ]"
		if p.Applied {
			marker = "[x]"
		}
		_, _ = fmt.Fprintf(out, "  %s %s\n", marker, p.Meta.ID)
		if p.Meta.Title != "" {
			_, _ = fmt.Fprintf(out, "      Title:       %s\n", p.Meta.Title)
		}
		if p.Meta.Description != "" {
			_, _ = fmt.Fprintf(out, "      Description: %s\n", p.Meta.Description)
		}
		rng := versionRangeString(p.Meta.AppliesFrom, p.Meta.AppliesTo)
		if rng != "" {
			_, _ = fmt.Fprintf(out, "      Range:       %s\n", rng)
		}
		if p.Applied {
			_, _ = fmt.Fprintf(out, "      Applied:     %s\n", p.AppliedAt)
			continue
		}
		_, _ = fmt.Fprintf(out, "      To load:     %s skill load %s\n", cliName, p.Meta.SkillPath)
		_, _ = fmt.Fprintf(out, "      Once done:   %s upgrade apply %s\n", cliName, p.Meta.ID)
	}
	return nil
}

// versionRangeString renders an applies-from/applies-to pair as a
// human-readable range. Either end being empty produces an open
// bound on that side; both empty returns "" so the caller skips the
// line entirely.
func versionRangeString(from, to string) string {
	switch {
	case from != "" && to != "":
		return fmt.Sprintf("%s → %s", from, to)
	case from != "":
		return fmt.Sprintf(">= %s", from)
	case to != "":
		return fmt.Sprintf("< %s", to)
	default:
		return ""
	}
}

// writePendingMigrationsJSON emits the pending list as JSON. The shape
// is `{"pending": [...]}` so callers can extend with sibling fields
// (e.g. binary_version) without a breaking change.
func writePendingMigrationsJSON(out io.Writer, pending []pendingMigration) error {
	body := struct {
		BinaryVersion string             `json:"binary_version"`
		Pending       []pendingMigration `json:"pending"`
	}{
		BinaryVersion: buildinfo.Version(),
		Pending:       pending,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}
