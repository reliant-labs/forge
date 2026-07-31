package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/templates"
)

// Migration metadata + state lives alongside the existing template-drift
// upgrader (upgrade.go). The split is intentional: `forge project upgrade` (the
// existing command) runs deterministic codemods + template-drift rewrites;
// `forge project upgrade list` and `forge project upgrade apply <id>` surface the
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
//
// Two further booleans take a migration out of the automatic worklist:
//
//	retired:  true   # the target shape no longer exists — never applicable
//	elective: true   # a valid architecture CHOICE, surfaced only on request
//
// Every migration must declare at least one of the four gates
// (range / detection / retired / elective): a migration that declares
// none claims to apply to every project ever generated, which is never
// true and turns the worklist into noise. TestMigrationSkills_DeclareAGate
// pins that invariant over the shipped set.

// migrationSkillsRoot is the embedded-template path prefix where every
// migration skill lives. Every direct subdirectory of this root is a
// migration; nothing else lives here. Migration skills ship `name:` /
// `description:` / `applies-from:` / `applies-to:` / `detection:`
// frontmatter, parsed by parseMigrationFrontmatter below.
const migrationSkillsRoot = "migrations"

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
	// stable identifier used by `forge project upgrade apply <id>` and in the
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
	// Retired marks a tombstoned migration: the shape it migrates TOWARD
	// no longer exists, so it applies to nothing and must never be
	// offered. The SKILL.md stays in the tree as the record of what
	// happened and where to go instead.
	Retired bool `json:"retired,omitempty"`
	// Elective marks a migration that is a legitimate architecture CHOICE
	// rather than drift off a shape forge stopped generating (e.g.
	// binary=per-service → binary=shared). Elective migrations are never
	// pushed by the worklist; they are found by reading the skills index
	// and loaded on purpose.
	Elective bool `json:"elective,omitempty"`
}

// migrationsState is the JSON shape written to .forge/migrations.json.
type migrationsState struct {
	// Applied maps migration ID -> ISO-8601 timestamp it was marked
	// applied. Keys are stable across forge versions (the migration
	// ID is the directory name, not a hash).
	Applied map[string]string `json:"applied"`
}

// pendingMigration is one row in `forge project upgrade list` output.
type pendingMigration struct {
	Meta    migrationMeta `json:"meta"`
	Applied bool          `json:"applied"`
	// AppliedAt is the ISO-8601 timestamp from migrations.json when
	// Applied is true; empty otherwise.
	AppliedAt string `json:"applied_at,omitempty"`
}

// attachMigrationSubcommands wires `forge project upgrade list` and
// `forge project upgrade apply <id>` onto the existing upgrade cobra command.
// The existing `forge project upgrade` (no args) keeps its template-drift +
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
and execute the steps. Use 'forge project upgrade apply <id>' to record a
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
job. 'apply' just records the outcome so later 'forge project upgrade list'
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

// computePendingMigrations is the core listing routine. It returns every
// migration that migrationApplies accepts for this project, tagged with
// whether it has already been applied (per .forge/migrations.json).
//
// Migrations that don't apply are simply omitted — the caller doesn't
// need to reason about why.
func computePendingMigrations() ([]pendingMigration, error) {
	projectRoot, err := findProjectRoot()
	if err != nil {
		return nil, err
	}
	if projectRoot == "" {
		return nil, cliutil.UserErr("forge project upgrade list",
			"no forge project found in this directory or any parent",
			"",
			"run 'forge project new' to create a project, or cd into one")
	}

	migrations, err := loadMigrationMetas()
	if err != nil {
		return nil, fmt.Errorf("load migration skills: %w", err)
	}

	state, err := readMigrationsState(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", migrationsStateFile, err)
	}

	out := applicableMigrations(migrations, projectBaselineVersion(), projectRoot)
	for i := range out {
		if ts, ok := state.Applied[out[i].Meta.ID]; ok {
			out[i].Applied = true
			out[i].AppliedAt = ts
		}
	}
	return out, nil
}

// applicableMigrations filters a migration set down to the rows that
// apply at the given baseline, sorted by ID so human + JSON output stay
// deterministic across binary builds. Split from computePendingMigrations
// so both migration surfaces (and their tests) share one filter.
func applicableMigrations(migrations []migrationMeta, baseline, projectRoot string) []pendingMigration {
	var out []pendingMigration
	for _, m := range migrations {
		if !migrationApplies(m, baseline, projectRoot) {
			continue
		}
		out = append(out, pendingMigration{Meta: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.ID < out[j].Meta.ID })
	return out
}

// projectBaselineVersion reads the forge_version the project is pinned
// to, or "" when there is no readable forge.yaml. A missing/unparseable
// config is tolerated: migrations are worth listing even before a
// project has been formally pinned, and an unorderable baseline just
// leaves the detection script as the gate.
func projectBaselineVersion() string {
	cfgPath, err := findProjectConfigFile()
	if err != nil {
		return ""
	}
	store, err := loadProjectStoreFrom(cfgPath)
	if err != nil {
		return ""
	}
	return store.Meta().EffectiveForgeVersion()
}

// loadMigrationMetas enumerates every SKILL.md under
// skills/forge/migrations/ in the embedded templates and parses its
// frontmatter. Every direct subdirectory of migrationSkillsRoot is a
// migration — the tree is single-purpose, so no filtering is required.
func loadMigrationMetas() ([]migrationMeta, error) {
	// The skills tree is rooted under "forge" in the embedded FS, so
	// the relative scan path is "forge/<migrationSkillsRoot>".
	relRoot := path.Join("forge", migrationSkillsRoot)
	entries, err := templates.ProjectTemplates().List(path.Join("skills", relRoot))
	if err != nil {
		// Missing root — no migrations shipped. Not an error: an
		// installation might predate the migration-skill convention.
		return nil, nil
	}

	var out []migrationMeta
	for _, rel := range entries {
		// `entries` are paths relative to skills/forge/migrations/,
		// e.g. "dev-target-to-kcl-deploy/SKILL.md" or
		// "v0.1-to-v0.2/SKILL.md".
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

		content, err := templates.ProjectTemplates().Get(path.Join("skills", relRoot, rel))
		if err != nil {
			continue
		}
		m := parseMigrationFrontmatter(content)
		m.ID = id
		m.SkillPath = path.Join(migrationSkillsRoot, id)
		out = append(out, m)
	}
	return out, nil
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
		case "retired":
			m.Retired = v == "true"
		case "elective":
			m.Elective = v == "true"
		}
	}
	return m
}

// migrationApplies is the SINGLE decision about whether a migration is
// worth putting in front of a project. Every surface that offers
// migrations — `forge project upgrade`'s pre-flight banner and
// `forge project upgrade list` — routes through here, so the two can
// never drift into disagreeing about what applies.
//
// The gates, in order:
//
//  1. Retired: the target shape no longer exists. Applies to nothing.
//  2. Elective: a legitimate architecture choice, not drift. Never
//     pushed; loaded on purpose.
//  3. Version range: the half-open [applies-from, applies-to) window
//     over the project's pinned forge_version.
//  4. Detection: the project must actually EXHIBIT the old shape.
//
// When the baseline names no version at all (see baselineIsUnknown) the
// range gate is skipped and a detection script becomes REQUIRED. An
// unknown baseline is not evidence of being old — it is the absence of
// evidence — so answering it from version ordering means inventing a
// position on the timeline and then filtering against the invention. What
// the project contains is the only thing left that is actually true.
//
// baseline is the project's pinned forge_version (possibly a Go
// pseudo-version from a dev build); projectRoot is where detection runs.
func migrationApplies(m migrationMeta, baseline, projectRoot string) bool {
	if m.Retired || m.Elective {
		return false
	}
	if baselineIsUnknown(baseline) {
		if strings.TrimSpace(m.Detection) == "" {
			return false
		}
		return runDetection(projectRoot, m.Detection)
	}
	if !versionInRange(baseline, m.AppliesFrom, m.AppliesTo) {
		return false
	}
	return runDetection(projectRoot, m.Detection)
}

// baselineIsUnknown reports whether a project's forge_version names no
// version at all: absent, one of the "dev"/"(devel)" sentinels, the
// "0.0.0" stand-in EffectiveForgeVersion returns for an unset field, or
// anything SemVer cannot place.
//
// "0.0.0" is a sentinel, not a measurement — a project can be unpinned
// and perfectly modern. A REAL pin whose base tag happens to be v0.0.0
// (the `v0.0.0-<timestamp>-<sha>` pseudo-version `go install` produces
// against a repo with no tags) is a different thing: it names a specific
// commit, it is genuinely ancient, and it orders normally.
func baselineIsUnknown(v string) bool {
	switch strings.TrimSpace(v) {
	case "", "dev", "(devel)", "0.0.0", "v0.0.0":
		return true
	}
	return semverKey(v) == ""
}

// versionInRange reports whether `version` falls in the half-open
// [from, to) range:
//
//   - Empty `from`: range is (-inf, to).
//   - Empty `to`:   range is [from, +inf).
//   - Both empty:   every version is in range (the migration is gated by
//     its detection script instead).
//
// Ordering is real SemVer precedence (golang.org/x/mod/semver), which
// matters because the versions forge actually produces are not simple
// vX.Y.Z triples. A build from a local checkout stamps a Go
// pseudo-version — `v0.0.4-0.20260724212501-dfb85daf8474+dirty` — and
// SemVer says that is a PRE-RELEASE of v0.0.4: after v0.0.3, before
// v0.0.4, and unambiguously below v0.1.0. A component-wise string
// comparison gets that wrong in both directions, which is how a dev
// build ends up sorted against migrations it has nothing to do with.
//
// A version that cannot be ordered at all (unpinned projects, the "dev"
// sentinel) is not excluded by the range — the detection script is the
// only honest gate left for it.
func versionInRange(version, from, to string) bool {
	v := semverKey(version)
	if v == "" {
		return true
	}
	if f := semverKey(from); f != "" && semver.Compare(v, f) < 0 {
		return false
	}
	if t := semverKey(to); t != "" && semver.Compare(v, t) >= 0 {
		return false
	}
	return true
}

// semverKey canonicalises a SemVer-ish forge version into a string
// golang.org/x/mod/semver can order, or "" when it is not orderable at
// all (the "0.0.0" unpinned sentinel is orderable; "dev" is not).
//
// Two normalisations: a missing leading "v" is added (forge.yaml pins
// are written both ways), and build metadata is dropped — SemVer gives
// build metadata no precedence, so `v0.0.4-…+dirty` orders exactly
// where `v0.0.4-…` does. Partial versions ("0.5") are accepted and
// zero-filled by semver itself.
func semverKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
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
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
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

// runUpgradeApply is the body of `forge project upgrade apply <id>`.
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
		return cliutil.UserErr("forge project upgrade apply",
			"no forge project found in this directory or any parent",
			"",
			"run 'forge project new' to create a project, or cd into one")
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
		hint := "run 'forge project upgrade list' to see available migration IDs"
		if len(ids) > 0 {
			hint = "available IDs: " + strings.Join(ids, ", ")
		}
		return cliutil.UserErr(
			fmt.Sprintf("forge project upgrade apply %s", id),
			fmt.Sprintf("migration %q not found", id),
			"",
			hint,
		)
	}

	if found.Retired {
		return cliutil.UserErr(
			fmt.Sprintf("forge project upgrade apply %s", id),
			fmt.Sprintf("migration %q is retired — the shape it migrates toward no longer exists", id),
			"",
			fmt.Sprintf("read it with '%s skill load %s'; it names the migration that replaced it", Name(), found.SkillPath),
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
//	    Apply once done: forge project upgrade apply dev-target-to-kcl-deploy
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
		_, _ = fmt.Fprintf(out, "      Once done:   %s project upgrade apply %s\n", cliName, p.Meta.ID)
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
