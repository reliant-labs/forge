package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/migrationver"
)

func newMigrateImportCmd() *cobra.Command {
	var (
		from    string
		srcDir  string
		destDir string
		dryRun  bool
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import migrations from another format (e.g. goose) into golang-migrate shape",
		Long: `Import SQL migrations from another tool's format into forge's
forward-only golang-migrate shape (one .up.sql per migration).

Currently supports:
  --from goose    One-file goose migrations with -- +goose Up / -- +goose Down

For each *.sql file in --src-dir, the importer:
  1. Keeps the -- +goose Up section and DROPS the -- +goose Down section.
     Forge rolls forward only and never runs down SQL, so the importer
     writes no .down.sql; each dropped Down section is listed so you can
     see what was discarded.
  2. Drops -- +goose StatementBegin / -- +goose StatementEnd markers.
  3. Carries -- +goose NO TRANSACTION over to a golang-migrate x-no-tx-wrap
     header.
  4. Renumbers starting from the next-available index in --dest-dir, so
     pack-installed migrations (00001-0000N) keep their slots.

Files with no goose markers are skipped.

Examples:
  forge project migrate import --from goose --src-dir ../old-project/migrations
  forge project migrate import --from goose --src-dir ./legacy --dry-run
  forge project migrate import --from goose --src-dir ./legacy --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrateImport(migrateImportOptions{
				From:    from,
				SrcDir:  srcDir,
				DestDir: destDir,
				DryRun:  dryRun,
				Force:   force,
				Stdout:  cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "Source format (currently only 'goose' supported)")
	cmd.Flags().StringVar(&srcDir, "src-dir", "", "Directory containing source migration files")
	cmd.Flags().StringVar(&destDir, "dest-dir", "", "Destination migrations directory (default: project's migrations dir)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print planned writes without touching disk")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing target files (default refuses)")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("src-dir")

	return cmd
}

type migrateImportOptions struct {
	From    string
	SrcDir  string
	DestDir string
	DryRun  bool
	Force   bool
	Stdout  interface{ Write(p []byte) (int, error) }
}

func runMigrateImport(opts migrateImportOptions) error {
	if opts.From != "goose" {
		return fmt.Errorf("--from %q is not supported (only 'goose' is supported today)", opts.From)
	}

	srcAbs, err := filepath.Abs(opts.SrcDir)
	if err != nil {
		return fmt.Errorf("resolve --src-dir: %w", err)
	}
	if info, err := os.Stat(srcAbs); err != nil {
		return fmt.Errorf("--src-dir %q: %w", opts.SrcDir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("--src-dir %q is not a directory", opts.SrcDir)
	}

	destDir := opts.DestDir
	if destDir == "" {
		destDir = resolveMigrationsDir("")
	}

	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve --dest-dir: %w", err)
	}

	srcFiles, err := listGooseFiles(srcAbs)
	if err != nil {
		return err
	}
	if len(srcFiles) == 0 {
		_, _ = fmt.Fprintf(opts.Stdout, "No .sql files found in %s\n", srcAbs)
		return nil
	}

	// Allocate one version per source file up front. Some are skipped
	// during planning (a goose file forge cannot convert), so a few
	// allocated versions may go unused — harmless, since versions are
	// sparse by design and nothing assumes version+1 exists.
	versions, err := migrationver.NextN(destAbs, len(srcFiles))
	if err != nil {
		return err
	}

	plans, skipped, err := planGooseImport(srcFiles, versions)
	if err != nil {
		return err
	}

	if !opts.Force && !opts.DryRun {
		existingStems, err := existingMigrationStems(destAbs)
		if err != nil {
			return err
		}
		for _, p := range plans {
			if _, err := os.Stat(p.UpPath(destAbs)); err == nil {
				return fmt.Errorf("target file already exists: %s (pass --force to overwrite)", p.UpPath(destAbs))
			}
			if _, ok := existingStems[p.Stem]; ok {
				return fmt.Errorf("a migration with stem %q already exists in %s (pass --force to overwrite, or rename the source file)", p.Stem, destAbs)
			}
		}
	}

	if opts.Force && !opts.DryRun {
		stemToPaths, err := existingMigrationFilesByStem(destAbs)
		if err != nil {
			return err
		}
		for _, p := range plans {
			for _, oldPath := range stemToPaths[p.Stem] {
				if oldPath == p.UpPath(destAbs) {
					continue
				}
				if err := os.Remove(oldPath); err != nil {
					return fmt.Errorf("--force: remove old %s: %w", oldPath, err)
				}
			}
		}
	}

	if opts.DryRun {
		_, _ = fmt.Fprintf(opts.Stdout, "Dry run: would write %d migration(s) to %s\n\n", len(plans), destAbs)
		for _, p := range plans {
			_, _ = fmt.Fprintf(opts.Stdout, "  %s\n", p.UpPath(destAbs))
		}
		printDroppedDowns(opts.Stdout, plans)
		printImportSkips(opts.Stdout, skipped)
		printFKWarnings(opts.Stdout, plans)
		return nil
	}

	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return fmt.Errorf("create --dest-dir %q: %w", destDir, err)
	}

	for _, p := range plans {
		if err := os.WriteFile(p.UpPath(destAbs), []byte(p.UpBody), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p.UpPath(destAbs), err)
		}
		_, _ = fmt.Fprintf(opts.Stdout, "  Wrote %s\n", p.UpPath(destAbs))
	}

	printDroppedDowns(opts.Stdout, plans)
	printImportSkips(opts.Stdout, skipped)
	printFKWarnings(opts.Stdout, plans)

	_, _ = fmt.Fprintf(opts.Stdout, "\nImported %d migration(s).\n", len(plans))
	return nil
}

type importPlan struct {
	// Version is the allocated timestamp version prefix. The importer
	// allocates the whole batch up front (migrationver.NextN), one distinct
	// second each, so an import of forty goose files cannot collide with
	// itself.
	Version string
	Stem    string
	UpBody  string
	// DroppedDown reports that the source carried a goose Down section
	// with SQL in it, which the import discarded (forge rolls forward
	// only). Surfaced so the drop is visible rather than silent.
	DroppedDown bool
	SrcPath     string
}

func (p importPlan) UpPath(destDir string) string {
	return filepath.Join(destDir, fmt.Sprintf("%s_%s.up.sql", p.Version, p.Stem))
}

type importSkip struct {
	Path   string
	Reason string
}

func listGooseFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read --src-dir %q: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

var migrationFilePattern = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)
var gooseFilePattern = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

func existingMigrationStems(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("read --dest-dir %q: %w", dir, err)
	}
	out := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		out[m[2]] = struct{}{}
	}
	return out, nil
}

func existingMigrationFilesByStem(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]string{}, nil
		}
		return nil, fmt.Errorf("read --dest-dir %q: %w", dir, err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		stem := m[2]
		out[stem] = append(out[stem], filepath.Join(dir, e.Name()))
	}
	return out, nil
}

func planGooseImport(srcFiles []string, versions []string) ([]importPlan, []importSkip, error) {
	var plans []importPlan
	var skips []importSkip
	next := 0
	for _, src := range srcFiles {
		raw, err := os.ReadFile(src)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", src, err)
		}
		converted, ok, reason := convertGooseFile(string(raw))
		if !ok {
			skips = append(skips, importSkip{Path: src, Reason: reason})
			continue
		}
		if next >= len(versions) {
			return nil, nil, fmt.Errorf("internal: allocated %d migration versions but the import needs more", len(versions))
		}
		plans = append(plans, importPlan{
			Version:     versions[next],
			Stem:        gooseSlug(src),
			UpBody:      converted.Up,
			DroppedDown: converted.HadDown,
			SrcPath:     src,
		})
		next++
	}
	return plans, skips, nil
}

func gooseSlug(srcPath string) string {
	base := filepath.Base(srcPath)
	if m := gooseFilePattern.FindStringSubmatch(base); m != nil {
		return m[2]
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

type convertedGoose struct {
	Up string
	// HadDown reports a Down section carrying SQL — discarded, not written.
	HadDown bool
}

var (
	gooseUpMarker       = regexp.MustCompile(`^\s*--\s*\+goose\s+Up\b`)
	gooseDownMarker     = regexp.MustCompile(`^\s*--\s*\+goose\s+Down\b`)
	gooseStatementBegin = regexp.MustCompile(`^\s*--\s*\+goose\s+StatementBegin\b`)
	gooseStatementEnd   = regexp.MustCompile(`^\s*--\s*\+goose\s+StatementEnd\b`)
	gooseNoTransaction  = regexp.MustCompile(`^\s*--\s*\+goose\s+NO\s+TRANSACTION\b`)
	noTxWrapHeader      = "-- golang-migrate: no transaction wrap\n-- x-no-tx-wrap: true\n"
)

func convertGooseFile(content string) (convertedGoose, bool, string) {
	if !containsGooseMarker(content) {
		return convertedGoose{}, false, "no goose markers found, skipping"
	}

	noTx := containsGooseNoTransaction(content)

	upLines, downLines := splitAtGooseDown(content)

	upBody := strings.TrimSpace(stripGooseMarkers(upLines)) + "\n"
	hadDown := strings.TrimSpace(stripSQLLineComments(stripGooseMarkers(downLines))) != ""

	if noTx {
		upBody = noTxWrapHeader + upBody
	}

	return convertedGoose{Up: upBody, HadDown: hadDown}, true, ""
}

func containsGooseMarker(content string) bool {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if gooseUpMarker.MatchString(line) || gooseDownMarker.MatchString(line) {
			return true
		}
	}
	return false
}

func containsGooseNoTransaction(content string) bool {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if gooseNoTransaction.MatchString(scanner.Text()) {
			return true
		}
	}
	return false
}

func splitAtGooseDown(content string) (upLines, downLines []string) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	inDown := false
	for scanner.Scan() {
		line := scanner.Text()
		if gooseDownMarker.MatchString(line) {
			inDown = true
			continue
		}
		// Drop +goose Up / +goose NO TRANSACTION markers — they're
		// goose-specific directives, not migration content. Lines before
		// any marker are treated as up-section by default.
		if gooseUpMarker.MatchString(line) || gooseNoTransaction.MatchString(line) {
			continue
		}
		if inDown {
			downLines = append(downLines, line)
		} else {
			upLines = append(upLines, line)
		}
	}
	return upLines, downLines
}

func stripGooseMarkers(lines []string) string {
	var out strings.Builder
	for _, line := range lines {
		if gooseStatementBegin.MatchString(line) || gooseStatementEnd.MatchString(line) {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

var fkReferencePattern = regexp.MustCompile(`(?i)\breferences\b`)

func printFKWarnings(w interface{ Write(p []byte) (int, error) }, plans []importPlan) {
	var fkFiles []string
	for _, p := range plans {
		if fkReferencePattern.MatchString(p.UpBody) {
			fkFiles = append(fkFiles, filepath.Base(p.UpPath("")))
		}
	}
	if len(fkFiles) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nForeign-key check: the following imported files reference other tables.")
	_, _ = fmt.Fprintln(w, "Verify the referenced tables exist in earlier (lower-numbered) migrations:")
	for _, f := range fkFiles {
		_, _ = fmt.Fprintf(w, "  - %s\n", f)
	}
}

// stripSQLLineComments drops `--` comment lines, so a Down section holding
// only commentary does not count as down SQL.
func stripSQLLineComments(body string) string {
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// printDroppedDowns lists the source files whose goose Down section was
// discarded. Dropping it is the policy — forge never runs down SQL — but a
// silent drop would read as data loss, so it is named.
func printDroppedDowns(w interface{ Write(p []byte) (int, error) }, plans []importPlan) {
	var dropped []string
	for _, p := range plans {
		if p.DroppedDown {
			dropped = append(dropped, filepath.Base(p.SrcPath))
		}
	}
	if len(dropped) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nDropped -- +goose Down sections (forge rolls forward only and never runs down SQL;")
	_, _ = fmt.Fprintln(w, "recover from a bad migration with a new forward migration):")
	for _, f := range dropped {
		_, _ = fmt.Fprintf(w, "  - %s\n", f)
	}
}

func printImportSkips(w interface{ Write(p []byte) (int, error) }, skips []importSkip) {
	if len(skips) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nSkipped:")
	for _, s := range skips {
		_, _ = fmt.Fprintf(w, "  - %s: %s\n", filepath.Base(s.Path), s.Reason)
	}
}
