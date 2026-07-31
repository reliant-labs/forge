package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
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
		Use:   "upgrade [--force <path>...]",
		Short: "Update frozen project files from latest Forge templates",
		Long: `Detect template drift on frozen files (files written at 'forge project new' time but
not updated by 'forge generate') and apply updates from newer Forge templates.

Files that haven't been modified by the user are updated automatically.
User-modified files show a diff and are skipped unless --force is used.

--force is per-path. Naming paths after --force restricts the overwrite to
exactly those files, so you can adopt one new template without stomping every
other file you have customized. Bare --force keeps its whole-project meaning.

A file you have deliberately taken over is better claimed than defended:
'forge project disown <path> --reason ...' records the ownership transfer, and
upgrade then skips the file permanently — even under --force.

Scaffold-once files (each frontend's shared mechanism modules under src/lib and
src/hooks, plus the .github starters) get their own section and are NEVER written
implicitly. They are yours from birth; upgrade only reports what their templates
gained, how far behind you are in lines, and how many lines in your copy no
template accounts for. Adopt one by naming it:
'forge project upgrade --force <path>'. Bare --force does not reach them.

After a successful upgrade the project's forge_version field in forge.yaml
is bumped to the current binary version (or to --to when provided). Migrations
whose declared version range AND detection script both match this project are
surfaced first, so the LLM running upgrade can follow them step-by-step.

Examples:
  forge project upgrade                        # Upgrade to latest, run all needed migrations
  forge project upgrade --to 1.5.0             # Upgrade to a specific target version
  forge project upgrade --dry-run              # Show what would change (alias for --check)
  forge project upgrade --check                # Dry-run: only show what would change
  forge project upgrade --force                # Apply all updates, even for user-modified files
  forge project upgrade --force buf.yaml       # Overwrite ONLY buf.yaml; leave other edits alone`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --dry-run is an alias for --check; either toggles dry-run.
			return runUpgrade(check || dryRun, force, args, toVersion)
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "Dry-run: only show what would change, don't write files")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Alias for --check")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite user-modified files. Name paths after it to overwrite only those")
	cmd.Flags().StringVar(&toVersion, "to", "", "Target forge version (defaults to the current binary version)")

	// `forge project upgrade list` and `forge project upgrade apply <id>` cover the
	// migration-skill surface (see upgrade_migrations.go). They share
	// the upgrade noun with the template-drift upgrader above but are
	// independent: list/apply work even on projects whose forge_version
	// is unpinned, and don't touch any user-owned files.
	attachMigrationSubcommands(cmd)

	return cmd
}

func runUpgrade(check, force bool, forcePaths []string, toVersion string) error {
	configPath, err := findProjectConfigFile()
	if err != nil {
		return err
	}

	store, err := loadProjectStoreFrom(configPath)
	if err != nil {
		return err
	}
	cfg := store.Config()

	projectDir := filepath.Dir(configPath)

	selection, err := resolveForceSelection(cfg, force, forcePaths)
	if err != nil {
		return err
	}

	// Determine target version. Default to the running binary's version,
	// honour --to when provided. We don't constrain --to to "newer than
	// current" — downgrade is a legitimate (if rare) operation.
	target := strings.TrimSpace(toVersion)
	if target == "" {
		target = buildinfo.Version()
	}

	// The baseline is reported and compared EXACTLY as the project pinned
	// it. A dev build's pseudo-version is real, orderable SemVer (a
	// pre-release of the next patch), so nothing has to be substituted for
	// it to take part in version-gated migration selection — and
	// substituting a different version, as this once did, is what fed
	// projects the migrations of a release they were never on.
	from := store.Meta().EffectiveForgeVersion()

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
		return cliutil.UserErr(
			fmt.Sprintf("forge project upgrade --to %s", target),
			fmt.Sprintf("minor-hop only: cannot upgrade %s → %s in one step (each per-version codemod must run against a clean baseline)", from, target),
			"",
			fmt.Sprintf("run '%s project upgrade --to v%s' first, then re-run '%s project upgrade --to %s' (one minor at a time)",
				Name(), nextMinor(from), Name(), target),
		)
	}

	if check {
		fmt.Printf("%s project upgrade --check (dry run): %s → %s\n", Name(), describeVersion(from), describeVersion(target))
	} else {
		fmt.Printf("%s project upgrade: %s → %s\n", Name(), describeVersion(from), describeVersion(target))
	}
	if isPreV01Baseline(from) {
		fmt.Println("  Baseline is pre-v0.1: forge has not declared backwards compatibility yet, so expect template churn.")
	}
	fmt.Println()

	// Surface the migrations this project actually needs before doing
	// destructive work, so the user (or LLM) can decide whether to halt
	// and load them first.
	printApplicableMigrations(projectDir, from)

	// Run the per-version codemod chain (deterministic AST rewrites)
	// BEFORE the template-update upgrade pass. Codemods rewrite
	// user-owned Tier-2 files (setup.go, handlers.go) which the
	// template upgrade leaves alone; doing them first keeps the two
	// concerns separate in the logs and means a codemod failure aborts
	// before template files get touched.
	var codemodReport CodemodReport
	if !check {
		report, err := runUpgradeCodemods(projectDir, from, target)
		if err != nil {
			return err
		}
		codemodReport = report
	}

	// One-time legacy .forge/checksums.json migration (same conversion
	// `forge generate` performs; see generate_legacy_migrate.go). Unlike
	// the pipeline, upgrade has no emitters to side-render against, so
	// unverifiable entries get the unverified-legacy sentinel directly —
	// the next generate's guard names them with the standard remedies.
	// Skipped under --check/--dry-run (read-only contract).
	if !check {
		if err := migrateLegacyChecksums(projectDir); err != nil {
			return err
		}
	}

	results, err := generator.UpgradeSelection(projectDir, cfg, selection, check)
	if err != nil {
		return err
	}

	counts := tallyAndPrintUpgradeResults(results, check)

	fmt.Println()

	printUpgradeSummary(counts, check)
	printUserModifiedRemedies(counts.userModifiedPaths)

	// The scaffold-once tier. Separate pass, separate section, and
	// advisory by contract: these files are the user's from birth, so the
	// run reports what their templates gained and writes nothing unless a
	// path was named after --force. Runs in both modes — a --check that
	// stayed silent about them is how a retried-4xx query client shipped.
	if _, err := runAdvisoryPass(projectDir, cfg, selection, check); err != nil {
		return err
	}

	// Bump the project's forge_version after a successful, non-dry-run
	// upgrade. We do this last so a partial failure above leaves the
	// existing pin in place rather than silently advancing it.
	//
	// Hard error: silently failing to bump leaves the project pinned to
	// the old version, so the next `forge generate` runs the wrong
	// template set against an already-migrated tree.
	if err := bumpForgeVersion(cfg, configPath, target, check); err != nil {
		return err
	}

	// Write the UPGRADE_NOTES.md at the project root so the user (or an
	// LLM running the upgrade) has a single canonical worklist of
	// auto-applied changes + items needing manual attention. Skip on
	// dry-run — the report would be misleading without the actual
	// rewrites having happened.
	//
	// Hard error: this report is the canonical worklist for manual
	// follow-up items. Silently dropping it strands the user with no
	// record of what still needs hand-attention.
	if !check && (len(codemodReport.Auto) > 0 || len(codemodReport.Manual) > 0) {
		if err := writeUpgradeNotes(projectDir, from, target, codemodReport); err != nil {
			return fmt.Errorf("write UPGRADE_NOTES.md: %w", err)
		}
		fmt.Println()
		fmt.Println("📝 UPGRADE_NOTES.md written at the project root.")
		fmt.Println("    Review it for items needing LLM/manual attention, then delete the file once the upgrade lands.")
	}

	return nil
}

// printApplicableMigrations surfaces the migrations this project actually
// needs BEFORE the destructive work, so the user (or LLM) can decide whether
// to halt and load them first.
//
// "Actually needs" is migrationApplies (upgrade_migrations.go) — the same
// decision `forge project upgrade list` makes: version range AND detection
// script, minus anything already recorded in .forge/migrations.json. Silence
// means nothing applies, which is a real answer; a project that exhibits none
// of the old shapes should be told so rather than handed the catalogue.
func printApplicableMigrations(projectRoot, baseline string) {
	metas, err := loadMigrationMetas()
	if err != nil {
		return
	}
	state, err := readMigrationsState(projectRoot)
	if err != nil {
		return
	}
	var rows []pendingMigration
	for _, row := range applicableMigrations(metas, baseline, projectRoot) {
		if _, done := state.Applied[row.Meta.ID]; done {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return
	}
	fmt.Println("📚 Migrations this project still needs:")
	for _, row := range rows {
		fmt.Printf("    - %s\n      %s\n      Load with: %s skill load %s\n",
			row.Meta.SkillPath, row.Meta.Description, Name(), row.Meta.SkillPath)
	}
	fmt.Println()
	fmt.Println("    The deterministic steps (regen, build) run automatically below.")
	fmt.Println("    Load each skill above and follow its 'Migration (manual part)' section")
	fmt.Println("    for any user-code adjustments needed for the version bump.")
	fmt.Println()
}

// describeVersion renders a forge version for human output, labelling
// anything that is not a published tag. A project scaffolded by a local
// checkout pins a Go pseudo-version; that string is honest identity but
// reads like a release, so say which it is at the point it's shown.
func describeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "(unpinned)"
	}
	if buildinfo.IsDevVersion(v) {
		return v + " (dev build)"
	}
	return v
}

// resolveForceSelection turns the --force flag plus any paths named after it
// into the generator's per-file overwrite selection.
//
//	(no --force)                → overwrite nothing the user has touched
//	--force                     → overwrite every user-modified managed file
//	--force <path>...           → overwrite exactly those, leave the rest alone
//
// Paths without --force are refused rather than guessed at: a path list is
// only ever a narrowing of a destructive act, so arming it must be explicit.
// Unknown paths are refused too — silently ignoring a typo would report
// "nothing to force" while the user believes the file was adopted.
//
// The valid set is the Tier-2 managed files PLUS the scaffold-once advisory
// rows (upgrade_advisory.go). Bare --force still means only the Tier-2 set:
// the advisory tier is adoptable one named path at a time and never in bulk,
// because those files are the user's from birth.
func resolveForceSelection(cfg *config.ProjectConfig, force bool, paths []string) (generator.ForceSelection, error) {
	if len(paths) == 0 {
		if force {
			return generator.ForceAll(), nil
		}
		return generator.ForceNone(), nil
	}
	if !force {
		return generator.ForceNone(), cliutil.UserErr(
			fmt.Sprintf("%s project upgrade %s", Name(), strings.Join(paths, " ")),
			"paths select which user-modified files to overwrite, but nothing armed the overwrite",
			"",
			fmt.Sprintf("run '%s project upgrade --force %s' to adopt those files, or drop the paths to see what would change",
				Name(), strings.Join(paths, " ")),
		)
	}

	managed := generator.ManagedPathsFor(cfg)
	advisory, err := generator.AdvisoryFilesFor(cfg)
	if err != nil {
		return generator.ForceNone(), err
	}
	managed = append(managed, generator.AdvisoryPaths(advisory)...)
	known := map[string]bool{}
	for _, p := range managed {
		known[p] = true
	}
	selected := make([]string, 0, len(paths))
	var unknown []string
	for _, raw := range paths {
		rel := filepath.Clean(strings.TrimSpace(raw))
		if !known[rel] {
			unknown = append(unknown, raw)
			continue
		}
		selected = append(selected, rel)
	}
	if len(unknown) > 0 {
		sort.Strings(managed)
		return generator.ForceNone(), cliutil.UserErr(
			fmt.Sprintf("%s project upgrade --force %s", Name(), strings.Join(paths, " ")),
			fmt.Sprintf("not upgrade-managed: %s", strings.Join(unknown, ", ")),
			"",
			fmt.Sprintf("upgrade only owns these paths in this project:\n    %s", strings.Join(managed, "\n    ")),
		)
	}
	return generator.ForcePaths(selected...), nil
}

// runUpgradeCodemods runs the per-version codemod chain (deterministic AST
// rewrites) and reports how many rewrites applied / need manual review. A
// codemod failure is wrapped as an actionable user error. Run BEFORE the
// template-update pass so a codemod failure aborts before template files get
// touched.
func runUpgradeCodemods(projectDir, from, target string) (CodemodReport, error) {
	report, err := runCodemodChain(projectDir, from, target)
	if err != nil {
		return CodemodReport{}, cliutil.WrapUserErr(
			fmt.Sprintf("forge project upgrade --to %s", target),
			"codemod chain failed",
			"",
			"inspect UPGRADE_NOTES.md (when written) and the codemod log; fix the offending file then re-run upgrade",
			err)
	}
	if len(report.Auto) > 0 || len(report.Manual) > 0 {
		fmt.Printf("🔧 Applied %d codemod rewrites; %d items need LLM/manual review.\n",
			len(report.Auto), len(report.Manual))
		fmt.Println("    Detail in UPGRADE_NOTES.md (written at the end).")
		fmt.Println()
	}
	return report, nil
}

// migrateLegacyChecksums performs the one-time legacy .forge/checksums.json
// migration (the same conversion `forge generate` performs). Unlike the
// pipeline, upgrade has no emitters to side-render against, so unverifiable
// entries get the unverified-legacy sentinel directly — the next generate's
// guard names them with the standard remedies. A missing/unreadable manifest is
// a no-op (nothing to migrate).
func migrateLegacyChecksums(projectDir string) error {
	mcs, lerr := generator.LoadChecksums(projectDir)
	if lerr != nil {
		return nil
	}
	outcome, merr := checksums.MigrateLegacyManifest(projectDir, mcs, legacyMigrationStampable)
	if merr != nil {
		return fmt.Errorf("legacy checksums migration: %w", merr)
	}
	if outcome == nil {
		return nil
	}
	fmt.Printf("\U0001F4DC Migrated off the legacy .forge/checksums.json (%d entries) — generated files are self-certifying now.\n", outcome.Total())
	for _, p := range outcome.Unverified {
		checksums.StampUnverified(projectDir, p)
		fmt.Fprintf(os.Stderr, "   ? %s — matches nothing the legacy manifest recorded; the next `forge generate` will name it (resolve with --force or `forge project disown`)\n", p)
	}
	if serr := generator.SaveChecksums(projectDir, mcs); serr != nil {
		return fmt.Errorf("save .forge ownership state: %w", serr)
	}
	return nil
}

// upgradeCounts tallies the per-file outcomes of a template-upgrade pass.
type upgradeCounts struct {
	updated      int
	userModified int
	upToDate     int
	skipped      int
	// userModifiedPaths are the paths reported as user-modified, in
	// output order — the exact argument list the remedy hints quote back.
	userModifiedPaths []string
}

// tallyAndPrintUpgradeResults prints one line per upgraded file (and the
// indented diff for user-modified files) and returns the aggregate counts.
func tallyAndPrintUpgradeResults(results []generator.UpgradeResult, check bool) upgradeCounts {
	var c upgradeCounts
	for _, r := range results {
		switch r.Status {
		case generator.UpgradeUpToDate:
			c.upToDate++
			_, _ = fmt.Fprintf(os.Stdout, "  %-35s up to date\n", r.Path)
		case generator.UpgradeUpdated:
			c.updated++
			_, _ = fmt.Fprintf(os.Stdout, "  %-35s %s\n", r.Path, updatedLabel(r.Forced, check))
		case generator.UpgradeUserModified:
			c.userModified++
			c.userModifiedPaths = append(c.userModifiedPaths, r.Path)
			_, _ = fmt.Fprintf(os.Stdout, "  %-35s user-modified (skipped)\n", r.Path)
			if r.Diff != "" {
				// Indent the diff for readability
				for _, line := range splitLines(r.Diff) {
					_, _ = fmt.Fprintf(os.Stdout, "    %s\n", line)
				}
			}
		case generator.UpgradeSkipped:
			c.skipped++
			_, _ = fmt.Fprintf(os.Stdout, "  %-35s skipped\n", r.Path)
		}
	}
	return c
}

// updatedLabel names what happened (or would happen) to a file the upgrade
// wrote. Overwriting a file the user edited is a different act from refreshing
// a pristine render, so it gets a different word.
func updatedLabel(forced, check bool) string {
	switch {
	case forced && check:
		return "would overwrite your edits (--force)"
	case forced:
		return "overwrote your edits (--force)"
	case check:
		return "would update"
	default:
		return "updated"
	}
}

// printUserModifiedRemedies names both ways out of a user-modified file, at
// the moment the user is looking at the diff.
//
// The two are not interchangeable. --force <path> adopts the new template and
// discards your edits, which is right when the edit was incidental or the
// file's provenance was simply unknown. disown is the durable answer when you
// meant to take the file over: it records the ownership transfer, and upgrade
// then skips the file forever — including under a later bare --force. Until
// someone runs it, "I edited a frozen file" is an implicit state that only
// survives as long as nobody forces.
//
// Both are printed and neither is applied: a user-modified verdict is not
// evidence of intent (it also fires on files whose provenance predates the
// self-certifying marker), and disown is a one-way door.
func printUserModifiedRemedies(paths []string) {
	if len(paths) == 0 {
		return
	}
	joined := strings.Join(paths, " ")
	fmt.Println()
	fmt.Printf("Two ways to resolve the %d user-modified file(s):\n", len(paths))
	fmt.Println("  Adopt the new template for specific files (your edits there are discarded):")
	fmt.Printf("    %s project upgrade --force %s\n", Name(), joined)
	fmt.Println("  Or claim a file as yours — upgrade never touches it again, even with --force:")
	fmt.Printf("    %s project disown %s --reason \"<what the template can't express>\"\n", Name(), paths[0])
}

// printUpgradeSummary prints the one-line, comma-joined summary of the upgrade
// counts. No-op when nothing was updated/skipped/etc.
func printUpgradeSummary(c upgradeCounts, check bool) {
	parts := []string{}
	if c.updated > 0 {
		verb := "Updated"
		if check {
			verb = "Would update"
		}
		parts = append(parts, fmt.Sprintf("%s %d file(s)", verb, c.updated))
	}
	if c.userModified > 0 {
		parts = append(parts, fmt.Sprintf("%d user-modified (skipped)", c.userModified))
	}
	if c.upToDate > 0 {
		parts = append(parts, fmt.Sprintf("%d up to date", c.upToDate))
	}
	if c.skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", c.skipped))
	}
	if len(parts) == 0 {
		return
	}
	for i, p := range parts {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(p)
	}
	fmt.Println()
}

// bumpForgeVersion pins the project's forge_version after a successful,
// non-dry-run upgrade. Done last so a partial failure upstream leaves the
// existing pin in place rather than silently advancing it. Skipped under
// --check and for the dev/(devel)/empty sentinel targets. A write failure is a
// hard error — a silent no-bump strands the project pinned to the old version,
// so the next `forge generate` runs the wrong template set.
func bumpForgeVersion(cfg *config.ProjectConfig, configPath, target string, check bool) error {
	if check || target == "" || target == "dev" || target == "(devel)" {
		return nil
	}
	if cfg.ForgeVersion == target {
		return nil
	}
	cfg.ForgeVersion = target
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("bump forge_version in forge.yaml: %w", err)
	}
	fmt.Printf("\nforge_version → %s (forge.yaml updated)\n", target)
	return nil
}

// isPreV01Baseline reports whether v sits below v0.1.0 — the line forge has
// not crossed yet, and therefore the line before which no backwards-
// compatibility promise exists. Purely an ordering question, answered by
// SemVer precedence, so every shape a real pin takes lands correctly:
//
//	""                                    unpinned          → pre-v0.1
//	"0.0.0"                               unpinned sentinel → pre-v0.1
//	"v0.0.3"                              released patch    → pre-v0.1
//	"v0.0.4-0.20260724212501-dfb…+dirty"  dev build         → pre-v0.1
//	"dev"                                 unorderable       → pre-v0.1
//	"v0.1.0"                              first compat tag  → NOT pre-v0.1
//
// A dev build's pseudo-version is a PRE-RELEASE of the next patch, so it
// orders below that patch and far below v0.1.0 without any special casing:
// nothing here has to know a version came from a local checkout.
func isPreV01Baseline(v string) bool {
	key := semverKey(v)
	if key == "" {
		// Unorderable (the "dev" sentinel, a malformed pin). Forge has
		// never released v0.1.0, so anything we cannot place is below it.
		return true
	}
	return semver.Compare(key, "v0.1.0") < 0
}

// nextMinor returns the next minor version after v (e.g. "0.1" → "0.2",
// "v1.4.3" → "1.5"). When v can't be parsed cleanly we fall back to
// the input string — the caller's error message is still informative
// even with the fallback. Used by the minor-hop guard's error message.
func nextMinor(v string) string {
	maj, minor, ok := splitMinor(v)
	if !ok {
		return v
	}
	return fmt.Sprintf("%d.%d", maj, minor+1)
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
