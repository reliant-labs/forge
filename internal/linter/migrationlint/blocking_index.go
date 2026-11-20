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
// could put a row in it. Only recognized harmless DDL may come between;
// unknown statements, including CREATE forms that can execute SQL or expose
// existing rows, end the proof and the index is reported. lint.rules raises the rule to an error
// for a project that wants it to gate.

// RuleBlockingIndexBuild is the rule ID for a non-concurrent CREATE INDEX on a
// table this migration did not create.
const RuleBlockingIndexBuild = "blocking-index-build"

// BlockingIndexBuildRemediation is the fix text for a blocking-index-build
// finding.
const BlockingIndexBuildRemediation = "build the index with CREATE INDEX CONCURRENTLY, alone in a migration file of its own (`forge db migration new <name>`; postgres refuses CONCURRENTLY inside a transaction block, and a lone statement runs outside one). If the table is empty in every environment, the plain build is fine: `forge db analyze` measures it against a real postgres"

var (
	createIndexRe = regexp.MustCompile(`(?is)^\s*create\s+(?:unique\s+)?index\s+(concurrently\s+)?(?:if\s+not\s+exists\s+)?(?:(?:"[^"]+"|[a-zA-Z_][\w$]*)\s+)?on\s+(?:only\s+)?((?:(?:"[^"]+"|[a-zA-Z_][\w$]*)\.)?(?:"[^"]+"|[a-zA-Z_][\w$]*))`)
	// The first token after the table name must open a column list. This
	// excludes PARTITION OF, OF <type> and column-list-free CTAS. The AS
	// check also excludes CTAS with column names before AS <query>.
	createTableRe = regexp.MustCompile(`(?is)^\s*create\s+(?:(?:global\s+|local\s+)?(?:temporary|temp|unlogged)\s+)?table\s+((?:(?:"[^"]+"|[a-zA-Z_][\w$]*)\.)?(?:"[^"]+"|[a-zA-Z_][\w$]*))\s*\(`)
	ctasRe        = regexp.MustCompile(`(?is)\bas\s*\(?\s*(?:select|with|values|table|execute)\b`)
	// ALTER TABLE may attach a populated partition, or make rows visible
	// under a new name. Only a simple ADD COLUMN preserves the proof.
	addOneColumnRe = regexp.MustCompile(`(?is)^\s*alter\s+table\s+(?:only\s+)?(?:(?:"[^"]+"|[a-zA-Z_][\w$]*)\.)?(?:"[^"]+"|[a-zA-Z_][\w$]*)\s+add\s+column\b[^,]*$`)
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
	if len(m) < 2 || ctasRe.MatchString(stmt) {
		return "", false
	}
	return relationKey(m[1]), true
}

// preservesEmptyTables recognizes the small set of statements that leave a
// fresh-table proof intact. Unknown DDL is not harmless by default: for
// example CREATE EXTENSION runs SQL and ATTACH PARTITION can expose existing
// rows. An index on a relation not already known empty may evaluate arbitrary
// index expressions, so it does not preserve proofs for other tables either.
func preservesEmptyTables(stmt string, emptyTables map[string]bool) bool {
	head := strings.TrimSpace(stmt)
	if end := strings.IndexAny(head, " \t\r\n"); end >= 0 {
		head = head[:end]
	}
	switch strings.ToLower(head) {
	case "comment", "grant", "revoke":
		return true
	case "alter":
		return addOneColumnRe.MatchString(stmt)
	case "create":
		m := createIndexRe.FindStringSubmatch(stmt)
		return len(m) >= 3 && emptyTables[relationKey(m[2])]
	default:
		return false
	}
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
