package migrationlint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/linter/finding"
)

// Severity and Finding now live in the shared internal/linter/finding
// package. SeverityWarn is kept as a package-local alias for source
// compatibility, but it now resolves to the canonical "warning"
// spelling (previously this package spelled it "warn" — the only
// migration-visible change is that FormatText now renders "warning").
type (
	// Severity is the shared finding severity vocabulary, re-exported
	// under this package's historical spelling.
	Severity = finding.Severity
	// Finding is the shared linter finding shape, re-exported under this
	// package's historical spelling.
	Finding = finding.Finding
)

// Severity enum values (aliases onto the canonical single-spelling set).
const (
	SeverityError = finding.SeverityError
	SeverityWarn  = finding.SeverityWarning
)

// Result aggregates every Finding from a migration-dir lint pass.
// Distinct type (not an alias) so migrationlint keeps its own FormatText.
//
// Findings alone cannot carry a verdict: a pass that read zero files
// produces zero findings, and a report built from findings alone renders
// that identically to a clean pass over fifty migrations. `forge ci
// migration-safety` shipped exactly that message — `✅ No migration
// safety warnings!` over an empty directory, and (because the directory
// path is resolved relative to the process cwd) over a nonexistent one
// whenever the command ran from a subdirectory. Scanned and Skipped are
// the evidence that makes the green falsifiable.
type Result struct {
	Findings []Finding
	// Dir is the directory the pass walked, exactly as the caller named
	// it. Printed on every verdict so a wrong path is visible instead of
	// masquerading as a clean tree.
	Dir string
	// Scanned lists every *.up.sql file actually read. A green with an
	// empty Scanned means nothing was checked, and FormatText says so.
	Scanned []string
	// Skipped, when non-empty, is the reason the pass examined nothing —
	// rule set disabled, directory absent, or directory empty. Phrased as
	// a clause that completes "nothing was checked: ...".
	Skipped string
}

// HasErrors reports whether any Finding is of severity Error.
func (r Result) HasErrors() bool {
	for _, finding := range r.Findings {
		if finding.Severity == SeverityError {
			return true
		}
	}
	return false
}

// FormatText renders the result as a human-readable text report.
//
// The verdict always names how many migration files were read and where
// they were read from, so "clean" and "checked nothing" can never render
// the same way. A pass that examined nothing gets ⏭️, not ✅ — a check
// that looked at no files has not earned a green.
func (r Result) FormatText() string {
	if r.Skipped != "" {
		return fmt.Sprintf("⏭️  migration safety: nothing was checked — %s\n", r.Skipped)
	}

	var b strings.Builder
	for _, finding := range r.Findings {
		fmt.Fprintf(&b, "%s:%d: %s [%s] %s\n", finding.File, finding.Line, finding.Severity, finding.Rule, finding.Message)
	}
	if len(r.Findings) == 0 {
		fmt.Fprintf(&b, "✅ migration safety: %s clean, no warnings\n", plural(len(r.Scanned), "migration file"))
		return b.String()
	}
	fmt.Fprintf(&b, "%s across %s\n", plural(len(r.Findings), "finding"), plural(len(r.Scanned), "migration file"))
	return b.String()
}

// plural renders "1 thing" / "3 things" so counts read naturally in the
// verdict lines without every call site repeating the ternary.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// RuleConfig is the per-rule severity configuration consumed by
// LintMigrationsDir. Mirrors config.MigrationSafetyConfig but is
// pre-resolved (no nil enabled, severities as strings).
type RuleConfig struct {
	Enabled            bool
	UnsafeAddColumn    string
	DestructiveChange  string
	VolatileDefault    string
	AllowedDestructive []string
}

// ConfigFromProject lifts a config.MigrationSafetyConfig into a
// migrationlint.RuleConfig by resolving defaults.
func ConfigFromProject(cfg config.MigrationSafetyConfig) RuleConfig {
	return RuleConfig{
		Enabled:            cfg.IsEnabled(),
		UnsafeAddColumn:    cfg.EffectiveUnsafeAddColumn(),
		DestructiveChange:  cfg.EffectiveDestructiveChange(),
		VolatileDefault:    cfg.EffectiveVolatileDefault(),
		AllowedDestructive: cfg.AllowedDestructive,
	}
}

// DefaultConfig returns the migration-lint defaults: all three rules
// at error/error/warn, enabled.
func DefaultConfig() RuleConfig {
	return RuleConfig{
		Enabled:           true,
		UnsafeAddColumn:   "error",
		DestructiveChange: "error",
		VolatileDefault:   "warn",
	}
}

// LintMigrationsDir walks dir for *.up.sql files and returns a Result
// containing every rule violation it finds.
//
// The three "examined nothing" outcomes — rules disabled, directory
// absent, directory empty — each set Result.Skipped with the reason and
// the next action, so the caller reports a skip rather than a pass. They
// are NOT errors: a project with no migrations yet is a normal state, and
// failing CI over it would be wrong. What is wrong is calling it clean.
func LintMigrationsDir(dir string, cfg RuleConfig) (Result, error) {
	if !cfg.Enabled {
		return Result{Dir: dir, Skipped: "the rule set is off (database.migration_safety.enabled: false in forge.yaml) — remove that key to turn the checks back on"}, nil
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return Result{Dir: dir, Skipped: fmt.Sprintf("no migrations directory at %s (path is resolved from the current directory — run this from the project root, or set database.migrations_dir in forge.yaml)", dir)}, nil
	}

	var files []string
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".up.sql") {
			return nil
		}
		files = append(files, path)
		return nil
	}); err != nil {
		return Result{}, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return Result{Dir: dir, Skipped: fmt.Sprintf("%s exists but holds no *.up.sql files — create one with `forge db migration new <name>`", dir)}, nil
	}

	var findings []Finding
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return Result{}, err
		}
		findings = append(findings, lintMigrationFile(file, string(data), cfg)...)
	}
	return Result{Findings: findings, Dir: dir, Scanned: files}, nil
}

var (
	statementSplitRe  = regexp.MustCompile(`;`)
	addColumnRe       = regexp.MustCompile(`(?is)\balter\s+table\b[^;]*\badd\s+column\s+(?:if\s+not\s+exists\s+)?(?:"[^"]+"|[a-zA-Z_][\w$]*)\s+[^;]*`)
	setNotNullRe      = regexp.MustCompile(`(?is)\balter\s+table\b[^;]*\balter\s+column\s+(?:"([^"]+)"|([a-zA-Z_][\w$]*))\s+set\s+not\s+null\b`)
	updateRe          = regexp.MustCompile(`(?is)^\s*update\s+(?:"[^"]+"|[a-zA-Z_][\w$.]*)\s+set\s+([^;]+)\bwhere\b[^;]+`)
	columnSetRe       = regexp.MustCompile(`(?is)(?:"([^"]+)"|([a-zA-Z_][\w$]*))\s*=`)
	destructiveRe     = regexp.MustCompile(`(?is)\b(drop\s+table|drop\s+schema|drop\s+column|truncate\s+table|alter\s+column\s+[^;]+\s+type\b|rename\s+column|rename\s+to)\b`)
	volatileDefaultRe = regexp.MustCompile(`(?is)\bdefault\s+(now\s*\(|current_timestamp\b|clock_timestamp\s*\(|statement_timestamp\s*\(|transaction_timestamp\s*\(|gen_random_uuid\s*\(|uuid_generate_v4\s*\(|random\s*\()`)
	lineCommentRe     = regexp.MustCompile(`(?m)--.*$`)
	blockCommentRe    = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

func lintMigrationFile(file, content string, cfg RuleConfig) []Finding {
	// Honor in-file pragmas before stripping comments. The destructive-
	// change rule accepts two opt-out forms anywhere in the file:
	//   -- forge:allow-destructive
	//   -- forge-safety: allow-destructive
	// Both mean "I, the author of this migration, accept the destructive
	// op; please don't make me round-trip through forge.yaml's
	// AllowedDestructive globs." Useful for one-off destructive moves
	// (replace-this-table migrations etc.) where editing the project
	// config is out-of-scope for the lane. See
	// migrationlint-no-per-file-destructive-pragma in FORGE_BACKLOG.
	allowDestructive := hasAllowDestructivePragma(content)

	clean := stripSQLComments(content)
	statements := splitStatements(clean)
	backfilledColumns := map[string]bool{}
	var findings []Finding

	for _, stmt := range statements {
		text := strings.TrimSpace(stmt.Text)
		if text == "" {
			continue
		}

		if severity := severityFor(cfg.DestructiveChange); severity != "" && destructiveRe.MatchString(text) && !allowDestructive && !isAllowedDestructive(file, cfg.AllowedDestructive) {
			findings = append(findings, Finding{
				File:     file,
				Line:     stmt.Line,
				Rule:     "destructive-change",
				Severity: severity,
				Message:  "destructive migration operation detected; split data-preserving rollout steps or allowlist this file intentionally",
			})
		}

		for _, column := range updatedColumns(text) {
			backfilledColumns[strings.ToLower(column)] = true
		}

		if severity := severityFor(cfg.UnsafeAddColumn); severity != "" && addColumnRe.MatchString(text) && hasNotNull(text) && !hasDefault(text) {
			findings = append(findings, Finding{
				File:     file,
				Line:     stmt.Line,
				Rule:     "unsafe-add-not-null-column",
				Severity: severity,
				Message:  "ADD COLUMN ... NOT NULL without a DEFAULT fails on populated tables; add nullable column, backfill, then SET NOT NULL",
			})
		}

		if severity := severityFor(cfg.VolatileDefault); severity != "" && addColumnRe.MatchString(text) && volatileDefaultRe.MatchString(text) {
			findings = append(findings, Finding{
				File:     file,
				Line:     stmt.Line,
				Rule:     "volatile-default",
				Severity: severity,
				Message:  "volatile DEFAULT in ADD COLUMN can rewrite/lock populated tables; prefer nullable column plus explicit backfill",
			})
		}

		if severity := severityFor(cfg.UnsafeAddColumn); severity != "" {
			for _, column := range setNotNullColumns(text) {
				if !backfilledColumns[strings.ToLower(column)] {
					findings = append(findings, Finding{
						File:     file,
						Line:     stmt.Line,
						Rule:     "set-not-null-without-backfill",
						Severity: severity,
						Message:  fmt.Sprintf("SET NOT NULL on column %q without an earlier UPDATE backfill in this migration can fail on populated tables", column),
					})
				}
			}
		}
	}
	return findings
}

type statement struct {
	Text string
	Line int
}

func splitStatements(content string) []statement {
	parts := statementSplitRe.Split(content, -1)
	statements := make([]statement, 0, len(parts))
	line := 1
	for _, part := range parts {
		statements = append(statements, statement{Text: part, Line: line})
		line += strings.Count(part, "\n")
	}
	return statements
}

func stripSQLComments(content string) string {
	content = blockCommentRe.ReplaceAllStringFunc(content, func(match string) string {
		return strings.Repeat("\n", strings.Count(match, "\n"))
	})
	return lineCommentRe.ReplaceAllString(content, "")
}

func hasNotNull(statement string) bool {
	return regexp.MustCompile(`(?is)\bnot\s+null\b`).MatchString(statement)
}

func hasDefault(statement string) bool {
	return regexp.MustCompile(`(?is)\bdefault\b`).MatchString(statement)
}

func setNotNullColumns(statement string) []string {
	matches := setNotNullRe.FindAllStringSubmatch(statement, -1)
	columns := make([]string, 0, len(matches))
	for _, match := range matches {
		columns = append(columns, firstNonEmpty(match[1], match[2]))
	}
	return columns
}

func updatedColumns(statement string) []string {
	match := updateRe.FindStringSubmatch(statement)
	if match == nil {
		return nil
	}
	setClause := match[1]
	matches := columnSetRe.FindAllStringSubmatch(setClause, -1)
	columns := make([]string, 0, len(matches))
	for _, m := range matches {
		columns = append(columns, firstNonEmpty(m[1], m[2]))
	}
	return columns
}

func severityFor(value string) Severity {
	// Empty Severity ("") means "rule disabled" — ParseSeverity returns
	// ("", false) for unrecognized values, which collapses to the same
	// disabled sentinel the callers already check for.
	sev, _ := finding.ParseSeverity(value)
	return sev
}

// DestructiveChangeRemediation is the actionable fix text for a
// destructive-change finding. It names the two suppression mechanisms
// the linter actually honors, in the order we recommend:
//
//  1. The in-file pragma `-- forge:allow-destructive` (see
//     allowDestructivePragmaRe below) — per-migration intent, no
//     forge.yaml round-trip, consistent with forge's other markers.
//  2. The config allowlist of file globs — which lives under the
//     `database.migration_safety.allowed_destructive` key, NOT a
//     top-level `migration_safety` key (the strict loader rejects that).
//
// This const is colocated with the pragma regex so the syntax we tell
// users can't drift from the syntax the linter matches.
const DestructiveChangeRemediation = `either rewrite the destructive migration as a non-destructive sequence, or — if the change is intentional — mark the migration file with a "-- forge:allow-destructive" comment (or glob-allowlist it under database.migration_safety.allowed_destructive in forge.yaml)`

// allowDestructivePragmaRe matches either of the supported in-file opt-out
// forms. Whitespace between tokens is permissive; the leading `--` must be
// a SQL line comment. Examples that match:
//
//	-- forge:allow-destructive
//	-- forge-safety: allow-destructive
//	--   FORGE:ALLOW-DESTRUCTIVE
var allowDestructivePragmaRe = regexp.MustCompile(`(?im)^\s*--\s*(?:forge:allow-destructive\b|forge-safety:\s*allow-destructive\b)`)

func hasAllowDestructivePragma(content string) bool {
	return allowDestructivePragmaRe.MatchString(content)
}

func isAllowedDestructive(file string, patterns []string) bool {
	for _, pattern := range patterns {
		if ok, _ := filepath.Match(pattern, filepath.Base(file)); ok {
			return true
		}
		if ok, _ := filepath.Match(pattern, file); ok {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
