package migrationlint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

type Severity string

const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warn"
)

type Finding struct {
	File     string
	Line     int
	Rule     string
	Severity Severity
	Message  string
}

type Result struct {
	Findings []Finding
}

func (r Result) HasErrors() bool {
	for _, finding := range r.Findings {
		if finding.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (r Result) FormatText() string {
	if len(r.Findings) == 0 {
		return "✅ No migration safety warnings!\n"
	}

	var b strings.Builder
	for _, finding := range r.Findings {
		fmt.Fprintf(&b, "%s:%d: %s [%s] %s\n", finding.File, finding.Line, finding.Severity, finding.Rule, finding.Message)
	}
	return b.String()
}

type RuleConfig struct {
	Enabled            bool
	UnsafeAddColumn    string
	DestructiveChange  string
	VolatileDefault    string
	AllowedDestructive []string
}

func ConfigFromProject(cfg config.MigrationSafetyConfig) RuleConfig {
	return RuleConfig{
		Enabled:            cfg.IsEnabled(),
		UnsafeAddColumn:    cfg.EffectiveUnsafeAddColumn(),
		DestructiveChange:  cfg.EffectiveDestructiveChange(),
		VolatileDefault:    cfg.EffectiveVolatileDefault(),
		AllowedDestructive: cfg.AllowedDestructive,
	}
}

func DefaultConfig() RuleConfig {
	return RuleConfig{
		Enabled:           true,
		UnsafeAddColumn:   "error",
		DestructiveChange: "error",
		VolatileDefault:   "warn",
	}
}

func LintMigrationsDir(dir string, cfg RuleConfig) (Result, error) {
	if !cfg.Enabled {
		return Result{}, nil
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return Result{}, nil
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

	var findings []Finding
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return Result{}, err
		}
		findings = append(findings, lintMigrationFile(file, string(data), cfg)...)
	}
	return Result{Findings: findings}, nil
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
	clean := stripSQLComments(content)
	statements := splitStatements(clean)
	backfilledColumns := map[string]bool{}
	var findings []Finding

	for _, stmt := range statements {
		text := strings.TrimSpace(stmt.Text)
		if text == "" {
			continue
		}

		if severity := severityFor(cfg.DestructiveChange); severity != "" && destructiveRe.MatchString(text) && !isAllowedDestructive(file, cfg.AllowedDestructive) {
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
	switch strings.ToLower(value) {
	case "error":
		return SeverityError
	case "warn", "warning":
		return SeverityWarn
	default:
		return ""
	}
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
