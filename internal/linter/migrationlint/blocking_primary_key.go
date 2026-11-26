package migrationlint

import (
	"fmt"
	"regexp"
	"strings"
)

// RuleBlockingPrimaryKeyBuild flags ALTER TABLE ADD PRIMARY KEY when it has
// to build the backing index and the table may already contain rows.
const RuleBlockingPrimaryKeyBuild = "blocking-primary-key-build"

const blockingPrimaryKeyBuildRemediation = "build the backing index first with CREATE UNIQUE INDEX CONCURRENTLY in a migration file of its own. Make sure the key columns are already NOT NULL to avoid a table scan at attachment. In a later migration, use ALTER TABLE ... ADD PRIMARY KEY USING INDEX ...; if replacing a key, drop the old constraint and attach the new one in the same transaction. The ALTER still takes an ACCESS EXCLUSIVE lock, but does not build the index under it"

var addPrimaryKeyRe = regexp.MustCompile(`(?is)^\s*add\s+(?:constraint\s+` + indexIdentifierPattern + `\s+)?primary\s+key\s*\(`)

func blockingPrimaryKeyFindings(file string, stmt statement, created map[indexRelation]tableProof) []Finding {
	m := alterTableNameRe.FindStringSubmatchIndex(stmt.Text)
	if len(m) < 4 {
		return nil
	}
	table := stmt.Text[m[2]:m[3]]
	if created[relationKey(table)] == createdEmpty {
		return nil // no rows to scan or sort, as for a plain CREATE INDEX
	}

	var findings []Finding
	line := stmt.Line + strings.Count(stmt.Text[:m[1]], "\n")
	for _, clause := range splitAlterClauses(stmt.Text[m[1]:]) {
		if action := strings.TrimSpace(clause); addPrimaryKeyRe.MatchString(action) {
			start := strings.Index(clause, action)
			findings = append(findings, Finding{
				File:     file,
				Line:     line + strings.Count(clause[:start], "\n"),
				Rule:     RuleBlockingPrimaryKeyBuild,
				Severity: SeverityWarn,
				Message:  fmt.Sprintf("ADD PRIMARY KEY on %s builds an index under an ACCESS EXCLUSIVE lock, blocking reads and writes on a populated table for the whole build; build a unique index CONCURRENTLY and attach it with ADD PRIMARY KEY USING INDEX", table),
			})
		}
		line += strings.Count(clause, "\n")
	}
	return findings
}
