// File: internal/cli/lint_column_markers.go
//
// column-markers — `forge lint --column-markers`.
//
// A `forge:*` column-comment marker (COMMENT ON COLUMN / COMMENT ON
// CONSTRAINT) that is misspelled or simply not a marker forge recognizes
// does NOTHING today — postgres stores the comment verbatim, forge reads
// it looking for an exact known marker, and a typo like `forge:immutible`
// silently fails to opt the column into anything. That is the same
// silent-failure shape `forge:ref` shipped with before this registry
// existed: undiscoverable until the seeder (or, for forge:immutable, an
// unwanted UPDATE clobber) surprised someone.
//
// This check flags any COMMENT ON COLUMN / COMMENT ON CONSTRAINT whose
// text contains `forge:` but matches none of schemadef.KnownColumnMarkers
// — the SAME registry `forge project annotations --kind column` dumps, so
// the marker vocabulary this check enforces and the vocabulary the docs
// advertise cannot drift apart.
//
// Severity is warning, never gating: an unrecognized marker might be a
// future forge version's, or simply prose that happens to start with
// "forge:" in an unrelated comment. The finding is a nudge to check the
// spelling, not a hard failure.

package lint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/pkg/schemadef"
)

// commentOnColumnRe matches `COMMENT ON COLUMN <table>.<column> IS '...'`.
// The captured object is the dotted table.column form as authored; the
// captured text is the comment body (quotes doubled per postgres escaping
// are left as-is — marker names never contain a quote, so this is fine for
// marker detection).
var commentOnColumnRe = regexp.MustCompile(`(?is)\bcomment\s+on\s+column\s+("?[\w.]+"?)\s+is\s+'((?:[^']|'')*)'`)

// commentOnConstraintRe matches
// `COMMENT ON CONSTRAINT <name> ON <table> IS '...'`.
var commentOnConstraintRe = regexp.MustCompile(`(?is)\bcomment\s+on\s+constraint\s+("?[\w]+"?)\s+on\s+("?[\w]+"?)\s+is\s+'((?:[^']|'')*)'`)

// forgeTokenRe extracts the whole `forge:...` token (up to the next
// whitespace) out of a comment body, for reporting the exact offending
// text rather than the whole comment.
var forgeTokenRe = regexp.MustCompile(`forge:\S*`)

// columnMarkerFinding is one COMMENT ON COLUMN/CONSTRAINT whose text
// carries an unrecognized `forge:` marker. File is projectDir-relative.
type columnMarkerFinding struct {
	File   string
	Line   int
	Object string // "orders.patient_id" or "constraint orders_patient_id_fkey on orders"
	Marker string // the offending forge:* token, verbatim
}

// columnMarkerFixHint renders the canonical remediation, listing the
// known markers straight from schemadef.KnownColumnMarkers so the hint can
// never advertise a marker the registry doesn't have (or omit one it does).
func columnMarkerFixHint(f columnMarkerFinding) string {
	return fmt.Sprintf(
		"%q is not a marker forge recognizes on %s — known column markers: %s (see `forge project annotations --kind column`)",
		f.Marker, f.Object, strings.Join(schemadef.KnownColumnMarkers, ", "))
}

// runColumnMarkersLint is the text-mode entry point.
func runColumnMarkersLint(cfg *config.ProjectConfig) error {
	fmt.Println("Running column-markers lint...")
	findings, err := collectColumnMarkerFindings(migrationsDirFor(cfg))
	if err != nil {
		return err
	}
	formatColumnMarkers(os.Stdout, findings)
	return nil
}

// migrationsDirFor resolves the migrations directory the same way
// runMigrationSafetyLint does: cfg's override, or the "db/migrations"
// default.
func migrationsDirFor(cfg *config.ProjectConfig) string {
	if cfg != nil && cfg.Database.MigrationsDir != "" {
		return cfg.Database.MigrationsDir
	}
	return filepath.Join("db", "migrations")
}

// formatColumnMarkers writes the human report. Empty findings print a
// single success line, matching the sibling advisory lints.
func formatColumnMarkers(w io.Writer, findings []columnMarkerFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  column-markers clean — every forge:* column/constraint comment matches a known marker")
		return
	}
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "  ⚠ [forge-column-markers] %s:%d\n", f.File, f.Line)
		_, _ = fmt.Fprintf(w, "      → %s\n", columnMarkerFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d unrecognized column marker(s).\n", len(findings))
	_, _ = fmt.Fprintln(w, "(warnings only — not failing the build)")
}

// collectColumnMarkerFindings is the shared engine behind text mode and
// `forge lint --json`. It walks migrationsDir for *.up.sql files and flags
// every COMMENT ON COLUMN / COMMENT ON CONSTRAINT whose text contains
// `forge:` but matches none of schemadef.KnownColumnMarkers. Findings come
// back sorted by (file, line) so output is deterministic.
//
// A missing or empty migrations directory is not an error — many linter
// invocations run from a repo root before any migration exists — it just
// yields no findings.
func collectColumnMarkerFindings(migrationsDir string) ([]columnMarkerFinding, error) {
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return nil, nil
	}

	var files []string
	if err := filepath.WalkDir(migrationsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".up.sql") {
			return nil
		}
		files = append(files, path)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk %s: %w", migrationsDir, err)
	}
	sort.Strings(files)

	var findings []columnMarkerFinding
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		findings = append(findings, findUnknownColumnMarkers(file, string(data))...)
	}
	return findings, nil
}

// findUnknownColumnMarkers scans one migration file's content for
// COMMENT ON COLUMN / COMMENT ON CONSTRAINT statements carrying an
// unrecognized forge:* marker.
func findUnknownColumnMarkers(file, content string) []columnMarkerFinding {
	var findings []columnMarkerFinding

	for _, m := range commentOnColumnRe.FindAllStringSubmatchIndex(content, -1) {
		object := content[m[2]:m[3]]
		text := content[m[4]:m[5]]
		if f, ok := unknownMarkerFinding(file, content, m[0], object, text); ok {
			findings = append(findings, f)
		}
	}
	for _, m := range commentOnConstraintRe.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		table := content[m[4]:m[5]]
		text := content[m[6]:m[7]]
		object := fmt.Sprintf("constraint %s on %s", name, table)
		if f, ok := unknownMarkerFinding(file, content, m[0], object, text); ok {
			findings = append(findings, f)
		}
	}

	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Line < findings[j].Line })
	return findings
}

// unknownMarkerFinding builds a finding when text carries a `forge:` token
// that matches none of schemadef.KnownColumnMarkers. The token is the
// whole-word span after `forge:` up to the next whitespace/quote, compared
// for EXACT equality against the registry — a substring/prefix check would
// let `forge:refactor` silently pass as `forge:ref` (with an argument) once
// a marker that takes arguments (like forge:ref) exists. matchStart is the
// byte offset of the whole COMMENT ON ... statement, used to compute the
// 1-indexed line number.
func unknownMarkerFinding(file, content string, matchStart int, object, text string) (columnMarkerFinding, bool) {
	token := forgeTokenRe.FindString(text)
	if token == "" {
		return columnMarkerFinding{}, false
	}
	// A marker whose argument is attached with `=` (forge:fill=handler) arrives
	// as one token, so compare the part before the `=` as well. The split is
	// on the FIRST `=` only, and the registry entry must still match in full:
	// that keeps the exact-equality guarantee this function exists for —
	// `forge:refactor` cannot pass as `forge:ref`, because neither the whole
	// token nor its pre-`=` head equals a registry entry.
	//
	// forge:ref takes its argument separated by a space, so it never reached
	// this case and the gap only appeared once a `=`-style marker existed.
	head := token
	if i := strings.IndexByte(head, '='); i >= 0 {
		head = head[:i]
	}
	for _, marker := range schemadef.KnownColumnMarkers {
		if token == marker || head == marker {
			return columnMarkerFinding{}, false
		}
	}
	return columnMarkerFinding{
		File:   file,
		Line:   strings.Count(content[:matchStart], "\n") + 1,
		Object: object,
		Marker: token,
	}, true
}
