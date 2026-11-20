package migrationlint

import (
	"regexp"
	"strings"
)

// A valid CHECK (<column> IS NOT NULL) proves every row of the column
// non-null, and postgres then skips the scan a SET NOT NULL would otherwise
// make. So a SET NOT NULL over such a CHECK cannot fail on a populated table,
// and needs no UPDATE backfill. It is the path `forge db analyze`
// recommends for a large table: ADD CONSTRAINT … CHECK (c IS NOT NULL)
// NOT VALID, VALIDATE CONSTRAINT in a later migration, SET NOT NULL in a
// third. set-not-null-without-backfill reads one file at a time and could not
// see the first two, so it failed the third file of the advice it is paired
// with.
//
// dropCheckProvenSetNotNull is the cross-file pass that sees them: it walks
// the migrations in apply order, tracks which CHECK constraints are valid
// (added without NOT VALID, or validated since) and still present, and drops
// the set-not-null-without-backfill findings whose column such a constraint
// covers at that point. Like the per-file rule it reads only the SQL text,
// so the proof must sit in this directory's migrations.

var (
	alterTableNameRe = regexp.MustCompile(`(?is)^\s*alter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?((?:"[^"]+"|[a-zA-Z_][\w$]*)(?:\s*\.\s*(?:"[^"]+"|[a-zA-Z_][\w$]*))?)`)
	// addNotNullCheckRe: ADD CONSTRAINT <name> CHECK (<col> IS NOT NULL),
	// the expression optionally parenthesized, then an optional NOT VALID.
	addNotNullCheckRe    = regexp.MustCompile(`(?is)\badd\s+constraint\s+("[^"]+"|[a-zA-Z_][\w$]*)\s+check\s*\(\s*(\(\s*)?("[^"]+"|[a-zA-Z_][\w$]*)\s+is\s+not\s+null\s*(\)\s*)?\)(\s*not\s+valid\b)?`)
	validateConstraintRe = regexp.MustCompile(`(?is)\bvalidate\s+constraint\s+("[^"]+"|[a-zA-Z_][\w$]*)`)
	dropConstraintRe     = regexp.MustCompile(`(?is)\bdrop\s+constraint\s+(?:if\s+exists\s+)?("[^"]+"|[a-zA-Z_][\w$]*)`)
)

// notNullCheck is one CHECK (<column> IS NOT NULL) constraint on a table.
type notNullCheck struct {
	column string
	valid  bool
}

func dropCheckProvenSetNotNull(files []compatFile, findings []Finding) []Finding {
	// checks is keyed by table, then constraint name (both normalized).
	checks := map[string]map[string]*notNullCheck{}
	type key struct {
		file   string
		line   int
		column string
	}
	proven := map[key]bool{}
	for _, f := range files {
		for _, stmt := range splitStatements(stripSQLComments(f.content)) {
			m := alterTableNameRe.FindStringSubmatch(stmt.Text)
			if m == nil {
				continue
			}
			table := normalizeSQLName(m[1])
			// Clauses of one ALTER TABLE apply in order, so a SET NOT NULL
			// in the same statement as the ADD CONSTRAINT (without NOT
			// VALID) is proven too: postgres validates the CHECK first.
			for _, clause := range splitAlterClauses(stmt.Text[len(m[0]):]) {
				if a := addNotNullCheckRe.FindStringSubmatch(clause); a != nil {
					if checks[table] == nil {
						checks[table] = map[string]*notNullCheck{}
					}
					checks[table][normalizeSQLName(a[1])] = &notNullCheck{column: normalizeSQLName(a[3]), valid: a[5] == ""}
					continue
				}
				if v := validateConstraintRe.FindStringSubmatch(clause); v != nil {
					if c := checks[table][normalizeSQLName(v[1])]; c != nil {
						c.valid = true
					}
					continue
				}
				if d := dropConstraintRe.FindStringSubmatch(clause); d != nil {
					delete(checks[table], normalizeSQLName(d[1]))
					continue
				}
				for _, column := range setNotNullColumns("alter table t " + clause) {
					col := normalizeSQLName(column)
					for _, c := range checks[table] {
						if c.valid && c.column == col {
							proven[key{f.path, stmt.Line, strings.ToLower(col)}] = true
						}
					}
				}
			}
		}
	}
	if len(proven) == 0 {
		return findings
	}
	kept := findings[:0:0]
	for _, f := range findings {
		if f.Rule == RuleSetNotNullWithoutBackfill && proven[key{f.File, f.Line, setNotNullFindingColumn(f.Message)}] {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// setNotNullFindingColumn reads the column back out of a
// set-not-null-without-backfill message.
func setNotNullFindingColumn(message string) string {
	start := strings.Index(message, `column "`)
	if start < 0 {
		return ""
	}
	rest := message[start+len(`column "`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return strings.ToLower(rest[:end])
}

// splitAlterClauses splits an ALTER TABLE's action list on the commas
// between actions, ignoring commas inside parentheses (a CHECK expression,
// a column type's precision).
func splitAlterClauses(actions string) []string {
	var clauses []string
	depth, start := 0, 0
	for i, r := range actions {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				clauses = append(clauses, actions[start:i])
				start = i + 1
			}
		}
	}
	return append(clauses, actions[start:])
}

// normalizeSQLName folds an identifier the way postgres does: a quoted one
// keeps its case (quotes dropped), an unquoted one is lower-cased. A
// schema-qualified table keeps its schema.
func normalizeSQLName(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) && len(p) >= 2 {
			parts[i] = p[1 : len(p)-1]
		} else {
			parts[i] = strings.ToLower(p)
		}
	}
	return strings.Join(parts, ".")
}
