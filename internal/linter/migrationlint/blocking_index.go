package migrationlint

import (
	"fmt"
	"regexp"
	"strings"
)

// A PLAIN CREATE INDEX ON A TABLE THAT ALREADY EXISTS BLOCKS ITS WRITES.
//
// CREATE INDEX without CONCURRENTLY holds a SHARE lock on the table for the
// whole build: reads go on, but every INSERT, UPDATE and DELETE waits until
// the index is built, which on a populated table is the length of a full
// scan and sort. `forge db analyze` measures it (db-analyze-blocking-index-
// build) against a real postgres; this rule is its static twin, so `forge
// lint --migration-safety` names the shape before anything runs (NS FG-35).
//
// Static analysis cannot know a table's size, so the finding is a WARNING at
// any pin: an index on a table that is still empty in every environment is
// fine as written. The one emptiness it can prove is local: a table this
// file creates with a plain CREATE TABLE, indexed before any statement that
// could put a row in it. Only DDL that writes no rows may come between
// (writesNoRows); an INSERT, a COPY, a function call, CREATE TABLE … AS or
// CREATE TABLE IF NOT EXISTS (a no-op over a table that may be full) ends the
// proof, and the index is reported. lint.rules raises the rule to an error
// for a project that wants it to gate.

// RuleBlockingIndexBuild is the rule ID for a non-concurrent CREATE INDEX on a
// table this migration did not create.
const RuleBlockingIndexBuild = "blocking-index-build"

// BlockingIndexBuildRemediation is the fix text for a blocking-index-build
// finding.
const BlockingIndexBuildRemediation = "build the index with CREATE INDEX CONCURRENTLY, alone in a migration file of its own (`forge db migration new <name>`; postgres refuses CONCURRENTLY inside a transaction block, and a lone statement runs outside one). If the table is empty in every environment, the plain build is fine: `forge db analyze` measures it against a real postgres"

var (
	createIndexRe = regexp.MustCompile(`(?is)^\s*create\s+(?:unique\s+)?index\s+(concurrently\s+)?(?:if\s+not\s+exists\s+)?(?:(?:"[^"]+"|[a-zA-Z_][\w$]*)\s+)?on\s+(?:only\s+)?((?:(?:"[^"]+"|[a-zA-Z_][\w$]*)\.)?(?:"[^"]+"|[a-zA-Z_][\w$]*))`)
	createTableRe = regexp.MustCompile(`(?is)^\s*create\s+(?:(?:global\s+|local\s+)?(?:temporary|temp|unlogged)\s+)?table\s+(if\s+not\s+exists\s+)?((?:(?:"[^"]+"|[a-zA-Z_][\w$]*)\.)?(?:"[^"]+"|[a-zA-Z_][\w$]*))`)
	// ctasRe matches CREATE TABLE … AS <query> (and SELECT … INTO's twin):
	// "as" followed by a query, never a column's GENERATED … AS (expr).
	ctasRe         = regexp.MustCompile(`(?is)^\s*create\b[^;]*?\btable\b[^;]*?\bas\s*\(?\s*(?:select|with|values|table|execute)\b`)
	materializedRe = regexp.MustCompile(`(?is)^\s*create\s+(?:or\s+replace\s+)?materialized\s+view\b`)
)

// relationKey normalises a possibly schema-qualified, possibly quoted
// relation name for comparison within one file: unquoted identifiers fold
// to lower case, as postgres folds them, and the schema is dropped (a file
// that creates public.t and indexes t means one table).
func relationKey(name string) string {
	inQuote, dot := false, -1
	for i, r := range name {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '.' && !inQuote:
			dot = i
		}
	}
	name = name[dot+1:]
	if len(name) >= 2 && strings.HasPrefix(name, `"`) && strings.HasSuffix(name, `"`) {
		return name[1 : len(name)-1]
	}
	return strings.ToLower(name)
}

// createdTable reports the table a statement creates EMPTY: a plain CREATE
// TABLE. Not CREATE TABLE IF NOT EXISTS (a no-op over a table that may hold
// rows), and not CREATE TABLE … AS (it creates the table full).
func createdTable(stmt string) (string, bool) {
	m := createTableRe.FindStringSubmatch(stmt)
	if len(m) < 3 || m[1] != "" || ctasRe.MatchString(stmt) {
		return "", false
	}
	return relationKey(m[2]), true
}

// writesNoRows reports whether a statement is DDL that cannot put a row in
// any table: CREATE (but not CREATE TABLE … AS or a MATERIALIZED VIEW, which
// run a query), ALTER, DROP, COMMENT, GRANT, REVOKE. Anything else — an
// INSERT, COPY, UPDATE, a SELECT or DO block that calls a function, a CALL —
// may write rows, directly or through a trigger, so a table this file created
// is no longer known to be empty after it.
func writesNoRows(stmt string) bool {
	head := strings.Fields(strings.ToLower(strings.TrimSpace(stmt)))
	if len(head) == 0 {
		return true
	}
	switch head[0] {
	case "alter", "drop", "comment", "grant", "revoke":
		return true
	case "create":
		return !ctasRe.MatchString(stmt) && !materializedRe.MatchString(stmt)
	}
	return false
}

// blockingIndexFinding reports a plain CREATE INDEX on a table not in
// created.
func blockingIndexFinding(file string, stmt statement, created map[string]bool) (Finding, bool) {
	m := createIndexRe.FindStringSubmatch(stmt.Text)
	if len(m) < 3 || m[1] != "" {
		return Finding{}, false
	}
	table := relationKey(m[2])
	if created[table] {
		return Finding{}, false
	}
	return Finding{
		File:     file,
		Line:     stmt.Line,
		Rule:     RuleBlockingIndexBuild,
		Severity: SeverityWarn,
		Message: fmt.Sprintf("CREATE INDEX on %s, which may already hold rows (it is not a table this file created and has written nothing to), blocks every write to it for the whole build on a populated table; build it CONCURRENTLY in a migration file of its own",
			strings.TrimSpace(m[2])),
	}, true
}
