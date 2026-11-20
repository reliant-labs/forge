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
	// addNotNullCheckRe is a WHOLE clause: ADD CONSTRAINT <name> CHECK
	// (<col> IS NOT NULL), the expression optionally parenthesized once,
	// then an optional NOT VALID and nothing else. Anchored at both ends, so
	// an expression with another predicate (`(c IS NOT NULL) OR true`,
	// which admits NULLs) or a modifier this does not know proves nothing.
	addNotNullCheckRe    = regexp.MustCompile(`(?is)^\s*add\s+constraint\s+("[^"]+"|[a-zA-Z_][\w$]*)\s+check\s*\(\s*(?:\(\s*("[^"]+"|[a-zA-Z_][\w$]*)\s+is\s+not\s+null\s*\)|("[^"]+"|[a-zA-Z_][\w$]*)\s+is\s+not\s+null)\s*\)\s*(not\s+valid)?\s*$`)
	validateConstraintRe = regexp.MustCompile(`(?is)^\s*validate\s+constraint\s+("[^"]+"|[a-zA-Z_][\w$]*)\s*$`)
	dropConstraintRe     = regexp.MustCompile(`(?is)^\s*drop\s+constraint\s+(?:if\s+exists\s+)?("[^"]+"|[a-zA-Z_][\w$]*)`)
	setNotNullClauseRe   = regexp.MustCompile(`(?is)^\s*alter\s+(?:column\s+)?(?:"[^"]+"|[a-zA-Z_][\w$]*)\s+set\s+not\s+null\s*$`)
	// tableReplacedRe is an ALTER TABLE clause after which the table's
	// proofs name the wrong table: a rename or a move to another schema.
	tableReplacedRe = regexp.MustCompile(`(?is)^\s*(?:rename\s+to|set\s+schema)\b`)
	leadingWordsRe  = regexp.MustCompile(`(?is)^\s*([a-z]+)(?:\s+([a-z]+))?`)
)

// notNullCheck is one CHECK (<column> IS NOT NULL) constraint on a table.
type notNullCheck struct {
	column string
	valid  bool
}

// notNullProofs is the CHECK (<column> IS NOT NULL) constraints the
// migrations read so far leave in place, keyed by table, then constraint
// name (both normalized). It forgets a proof whenever a statement could have
// removed or replaced what it proves and is not one it understands: a proof
// it keeps wrongly silences a finding, one it drops wrongly only keeps it.
type notNullProofs map[string]map[string]*notNullCheck

func dropCheckProvenSetNotNull(files []compatFile, findings []Finding) []Finding {
	checks := notNullProofs{}
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
				checks.forgetStatement(stmt.Text)
				continue
			}
			table := normalizeSQLName(m[1])
			// Clauses of one ALTER TABLE apply in order, so a SET NOT NULL
			// in the same statement as the ADD CONSTRAINT (without NOT
			// VALID) is proven too: postgres validates the CHECK first.
			for _, clause := range splitAlterClauses(stmt.Text[len(m[0]):]) {
				switch {
				case addNotNullCheckRe.MatchString(clause):
					a := addNotNullCheckRe.FindStringSubmatch(clause)
					if checks[table] == nil {
						checks[table] = map[string]*notNullCheck{}
					}
					checks[table][normalizeSQLName(a[1])] = &notNullCheck{column: normalizeSQLName(firstNonEmpty(a[2], a[3])), valid: a[4] == ""}
				case validateConstraintRe.MatchString(clause):
					if c := checks[table][normalizeSQLName(validateConstraintRe.FindStringSubmatch(clause)[1])]; c != nil {
						c.valid = true
					}
				case dropConstraintRe.MatchString(clause):
					delete(checks[table], normalizeSQLName(dropConstraintRe.FindStringSubmatch(clause)[1]))
				case setNotNullClauseRe.MatchString(clause):
					for _, column := range setNotNullColumns("alter table t " + clause) {
						col := normalizeSQLName(column)
						for _, c := range checks[table] {
							if c.valid && c.column == col {
								proven[key{f.path, stmt.Line, strings.ToLower(col)}] = true
							}
						}
					}
				case tableReplacedRe.MatchString(clause):
					checks.forgetTable(table)
				default:
					// DROP COLUMN, ALTER COLUMN … TYPE, RENAME COLUMN, a
					// CHECK this does not read: any clause that names a
					// proven column may have removed or replaced it.
					checks.forgetColumnsNamedIn(table, clause)
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

// forgetStatement drops the proofs of every table a statement that is not an
// ALTER TABLE names — DROP TABLE, CREATE TABLE after one, DROP SCHEMA,
// ALTER … RENAME of another object onto it — except the statements that
// cannot remove or replace a table or a column: DML, comments, grants,
// maintenance, and index builds.
func (p notNullProofs) forgetStatement(text string) {
	if len(p) == 0 {
		return
	}
	w := leadingWordsRe.FindStringSubmatch(text)
	if w == nil {
		return
	}
	first, second := strings.ToLower(w[1]), strings.ToLower(w[2])
	switch first {
	case "insert", "update", "delete", "select", "with", "comment", "grant", "revoke", "analyze", "vacuum", "set", "do":
		return
	case "create":
		if second == "index" || second == "unique" {
			return
		}
	}
	for table := range p {
		if namesIdent(text, unqualified(table)) {
			delete(p, table)
		}
	}
}

// forgetTable drops every proof on table, and on any table of the same name
// in another schema: a qualified and an unqualified spelling may be one table.
func (p notNullProofs) forgetTable(table string) {
	for t := range p {
		if unqualified(t) == unqualified(table) {
			delete(p, t)
		}
	}
}

// forgetColumnsNamedIn drops the proofs on table whose column clause names.
func (p notNullProofs) forgetColumnsNamedIn(table, clause string) {
	for t, byName := range p {
		if unqualified(t) != unqualified(table) {
			continue
		}
		for name, c := range byName {
			if namesIdent(clause, c.column) {
				delete(byName, name)
			}
		}
	}
}

// unqualified is a normalized table name without its schema.
func unqualified(table string) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[i+1:]
	}
	return table
}

// namesIdent reports whether text contains ident as a whole identifier,
// quoted or not, case-insensitively.
func namesIdent(text, ident string) bool {
	re := regexp.MustCompile(`(?i)(?:^|[^\w$])"?` + regexp.QuoteMeta(ident) + `"?(?:$|[^\w$])`)
	return re.MatchString(text)
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
