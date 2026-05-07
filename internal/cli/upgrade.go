package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/generator"
)

func newUpgradeCmd() *cobra.Command {
	var (
		check     bool
		force     bool
		toVersion string
		dryRun    bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update frozen project files from latest Forge templates",
		Long: `Detect template drift on frozen files (files written at 'forge new' time but
not updated by 'forge generate') and apply updates from newer Forge templates.

Files that haven't been modified by the user are updated automatically.
User-modified files show a diff and are skipped unless --force is used.

After a successful upgrade the project's forge_version field in forge.yaml
is bumped to the current binary version (or to --to when provided). Any
per-version migration skills found at skills/forge/migration/v<from>-to-*
are surfaced so the LLM running upgrade can follow them step-by-step.

Examples:
  forge upgrade                # Upgrade to latest, run all needed migrations
  forge upgrade --to 1.5.0     # Upgrade to a specific target version
  forge upgrade --dry-run      # Show what would change (alias for --check)
  forge upgrade --check        # Dry-run: only show what would change
  forge upgrade --force        # Apply all updates, even for user-modified files`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --dry-run is an alias for --check; either toggles dry-run.
			return runUpgrade(check || dryRun, force, toVersion)
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "Dry-run: only show what would change, don't write files")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Alias for --check")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite user-modified files without prompting")
	cmd.Flags().StringVar(&toVersion, "to", "", "Target forge version (defaults to the current binary version)")

	return cmd
}

func runUpgrade(check, force bool, toVersion string) error {
	configPath, err := findProjectConfigFile()
	if err != nil {
		return err
	}

	cfg, err := loadProjectConfigFrom(configPath)
	if err != nil {
		return err
	}

	projectDir := filepath.Dir(configPath)

	// Determine target version. Default to the running binary's version,
	// honour --to when provided. We don't constrain --to to "newer than
	// current" — downgrade is a legitimate (if rare) operation.
	target := strings.TrimSpace(toVersion)
	if target == "" {
		target = buildinfo.Version()
	}

	from := cfg.EffectiveForgeVersion()

	// Pre-v0.1 baselines (the "0.0.0" sentinel from EffectiveForgeVersion
	// when forge_version is unset, or pseudoversion strings like
	// "v0.0.0-20260430002332-8f05b089372c+dirty" emitted by `go install`
	// against an untagged forge checkout) are the shape that shipped
	// before v0.2 introduced explicit codemods. Treat them as the v0.1
	// baseline for upgrade purposes — that's the first formal codemod
	// hop, and a project pinned to a v0.0.0-pseudoversion is by
	// definition pre-v0.2 (which is when forge_version started getting
	// pinned to a real tag). This keeps the minor-hop guard from
	// rejecting `forge upgrade --to v0.2` against real-world projects
	// that have never been bumped past their initial pseudoversion pin.
	if isPreV01Baseline(from) {
		from = "v0.1"
	}

	// Minor-hop enforcement. Per-version codemods are written for one
	// minor at a time (v0.1 → v0.2, v0.2 → v0.3, ...). Hopping multiple
	// minors at once almost always means an intermediate codemod runs
	// against a project that's already partially in the new shape — the
	// rewrite is guaranteed to be either a no-op or a corruption. We
	// require the user to step through one at a time so each codemod
	// runs against a clean baseline.
	//
	// This check only applies when:
	//   1. Both from/to parse cleanly as vMaj.Min(.patch).
	//   2. The hop spans the same major (cross-major upgrades aren't
	//      part of the codemod chain at all).
	//   3. The hop spans more than one minor.
	if hop := minorHopDistance(from, target); hop > 1 {
		return fmt.Errorf(
			"forge migrations are minor-hop only: cannot upgrade %s → %s in one step.\n"+
				"  Run: %s upgrade --to v%s\n"+
				"  Then: %s upgrade --to %s (and so on, one minor at a time).\n"+
				"This rule keeps each per-version codemod running against a clean baseline.",
			from, target,
			CLIName(), nextMinor(from),
			CLIName(), target,
		)
	}

	if check {
		fmt.Printf("%s upgrade --check (dry run): %s → %s\n", CLIName(), from, target)
	} else {
		fmt.Printf("%s upgrade: %s → %s\n", CLIName(), from, target)
	}
	fmt.Println()

	// Surface any per-version migration skills relevant to this jump
	// before doing destructive work, so the user (or LLM) can decide
	// whether to halt and load them first.
	if skills := relevantMigrationSkills(from, target); len(skills) > 0 {
		fmt.Println("📚 Per-version migration skills relevant to this upgrade:")
		for _, s := range skills {
			fmt.Printf("    - %s\n      %s\n      Load with: %s skill load %s\n",
				s.Path, s.Description, CLIName(), s.Path)
		}
		fmt.Println()
		fmt.Println("    The deterministic steps (regen, build) run automatically below.")
		fmt.Println("    Load each skill above and follow its 'Migration (manual part)' section")
		fmt.Println("    for any user-code adjustments needed for the version bump.")
		fmt.Println()
	}

	// Run the per-version codemod chain (deterministic AST rewrites)
	// BEFORE the template-update upgrade pass. Codemods rewrite
	// user-owned Tier-2 files (setup.go, handlers.go) which the
	// template upgrade leaves alone; doing them first keeps the two
	// concerns separate in the logs and means a codemod failure aborts
	// before template files get touched.
	var codemodReport CodemodReport
	if !check {
		report, err := runCodemodChain(projectDir, from, target)
		if err != nil {
			return fmt.Errorf("codemod chain: %w", err)
		}
		codemodReport = report
		if len(report.Auto) > 0 || len(report.Manual) > 0 {
			fmt.Printf("🔧 Applied %d codemod rewrites; %d items need LLM/manual review.\n",
				len(report.Auto), len(report.Manual))
			fmt.Println("    Detail in UPGRADE_NOTES.md (written at the end).")
			fmt.Println()
		}
	}

	results, err := generator.Upgrade(projectDir, cfg, force, check)
	if err != nil {
		return err
	}

	var updated, userModified, upToDate, skipped int
	for _, r := range results {
		switch r.Status {
		case generator.UpgradeUpToDate:
			upToDate++
			fmt.Fprintf(os.Stdout, "  %-35s up to date\n", r.Path)
		case generator.UpgradeUpdated:
			updated++
			if check {
				fmt.Fprintf(os.Stdout, "  %-35s would update\n", r.Path)
			} else {
				fmt.Fprintf(os.Stdout, "  %-35s updated\n", r.Path)
			}
		case generator.UpgradeUserModified:
			userModified++
			fmt.Fprintf(os.Stdout, "  %-35s user-modified (skipped)\n", r.Path)
			if r.Diff != "" {
				// Indent the diff for readability
				for _, line := range splitLines(r.Diff) {
					fmt.Fprintf(os.Stdout, "    %s\n", line)
				}
			}
		case generator.UpgradeSkipped:
			skipped++
			fmt.Fprintf(os.Stdout, "  %-35s skipped\n", r.Path)
		}
	}

	fmt.Println()

	// Summary
	parts := []string{}
	if updated > 0 {
		verb := "Updated"
		if check {
			verb = "Would update"
		}
		parts = append(parts, fmt.Sprintf("%s %d file(s)", verb, updated))
	}
	if userModified > 0 {
		parts = append(parts, fmt.Sprintf("%d user-modified (use --force to overwrite)", userModified))
	}
	if upToDate > 0 {
		parts = append(parts, fmt.Sprintf("%d up to date", upToDate))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}

	if len(parts) > 0 {
		for i, p := range parts {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Print(p)
		}
		fmt.Println()
	}

	// Bump the project's forge_version after a successful, non-dry-run
	// upgrade. We do this last so a partial failure above leaves the
	// existing pin in place rather than silently advancing it.
	if !check && target != "" && target != "dev" && target != "(devel)" {
		if cfg.ForgeVersion != target {
			cfg.ForgeVersion = target
			if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to bump forge_version in forge.yaml: %v\n", err)
			} else {
				fmt.Printf("\nforge_version → %s (forge.yaml updated)\n", target)
			}
		}
	}

	// Write the UPGRADE_NOTES.md at the project root so the user (or an
	// LLM running the upgrade) has a single canonical worklist of
	// auto-applied changes + items needing manual attention. Skip on
	// dry-run — the report would be misleading without the actual
	// rewrites having happened.
	if !check && (len(codemodReport.Auto) > 0 || len(codemodReport.Manual) > 0) {
		if err := writeUpgradeNotes(projectDir, from, target, codemodReport); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write UPGRADE_NOTES.md: %v\n", err)
		} else {
			fmt.Println()
			fmt.Println("📝 UPGRADE_NOTES.md written at the project root.")
			fmt.Println("    Review it for items needing LLM/manual attention, then delete the file once the upgrade lands.")
		}
	}

	return nil
}

// isPreV01Baseline reports whether v is a "pre-v0.1" pin: the "0.0.0"
// sentinel returned by EffectiveForgeVersion when forge_version is
// unset, or a pseudoversion of the form "v0.0.0-<timestamp>-<sha>" that
// `go install` against an untagged forge checkout produces. These pins
// predate the v0.2 codemod chain and should be treated as the v0.1
// baseline so `forge upgrade --to v0.2` works on real-world projects
// that were created before the formal upgrade story landed.
func isPreV01Baseline(v string) bool {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "0.0.0" {
		return true
	}
	// Pseudoversions look like "0.0.0-20260430002332-8f05b089372c" or
	// "0.0.0-20260430002332-8f05b089372c+dirty". The "0.0.0-" prefix
	// is the tell — semver pre-release suffixes attached to a real
	// tag use a different major/minor.
	return strings.HasPrefix(v, "0.0.0-")
}

// nextMinor returns the next minor version after v (e.g. "0.1" → "0.2",
// "v1.4.3" → "1.5"). When v can't be parsed cleanly we fall back to
// the input string — the caller's error message is still informative
// even with the fallback. Used by the minor-hop guard's error message.
func nextMinor(v string) string {
	maj, min, ok := splitMinor(v)
	if !ok {
		return v
	}
	return fmt.Sprintf("%d.%d", maj, min+1)
}

// migrationSkillRef holds the metadata for a migration skill that's
// relevant to a given upgrade jump.
type migrationSkillRef struct {
	Path        string // skill load path, e.g. "migration/v0.x-to-contractkit"
	Description string
}

// relevantMigrationSkills walks the skill registry looking for migration
// skills whose name starts with "v<from-prefix>-to-...". The match is
// loose by design: the canonical example "v0.x-to-contractkit" is
// keyed on the major version family rather than an exact version, so
// the matcher works on prefix-by-major.
//
// from and to are SemVer-ish strings (e.g. "1.4.0", "0.0.0", "dev").
// When from is a sentinel ("0.0.0", "", "dev") we surface every
// migration skill — legacy projects benefit from seeing the full
// upgrade story, not nothing.
func relevantMigrationSkills(from, to string) []migrationSkillRef {
	skills, err := listSkills()
	if err != nil {
		return nil
	}

	fromMajor := majorVersionPrefix(from)
	wantAll := from == "" || from == "0.0.0" || from == "dev" || from == "(devel)"

	var out []migrationSkillRef
	for _, s := range skills {
		// Skill paths look like "migration/v0.x-to-contractkit". Anything
		// else is not a per-version migration skill.
		if !strings.HasPrefix(s.Path, "migration/v") {
			continue
		}

		leaf := strings.TrimPrefix(s.Path, "migration/")
		// leaf is like "v0.x-to-contractkit" — split on "-to-" to extract
		// the from-prefix.
		from, _, ok := strings.Cut(leaf, "-to-")
		if !ok {
			continue
		}
		from = strings.TrimPrefix(from, "v")

		if !wantAll && fromMajor != "" && !strings.HasPrefix(from, fromMajor) {
			continue
		}

		out = append(out, migrationSkillRef{
			Path:        s.Path,
			Description: s.Description,
		})
	}
	return out
}

// majorVersionPrefix returns the leading "<major>." of a SemVer-ish
// string, or "" if it can't be parsed. "1.4.0" → "1.", "0.9" → "0.",
// "dev" → "".
func majorVersionPrefix(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return ""
	}
	if i := strings.Index(v, "."); i > 0 {
		return v[:i+1]
	}
	return ""
}

// splitLines splits a string into lines, handling both \n and \r\n.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := make([]string, 0)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
