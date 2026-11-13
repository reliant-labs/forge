package dbanalyze

import (
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/sqllex"
)

// Statement is one top-level statement of a SQL file, split the way postgres
// reads it (comments, string constants, dollar-quoted bodies and BEGIN ATOMIC
// bodies never end a statement).
type Statement struct {
	// Index is the statement's 0-based position in its file.
	Index int
	// Line is the 1-based line of its first token.
	Line int
	// SQL is its text, without the terminating `;`.
	SQL   string
	words []string
}

// SplitStatements splits a SQL file into its statements.
func SplitStatements(src string) []Statement {
	lexed := sqllex.Split(src)
	out := make([]Statement, 0, len(lexed))
	for i, l := range lexed {
		out = append(out, Statement{
			Index: i,
			Line:  l.Line,
			SQL:   strings.TrimSpace(src[l.Start:l.End]),
			words: l.Words,
		})
	}
	return out
}

// NonTransactional names the statement when postgres refuses it inside a
// transaction block (CREATE INDEX CONCURRENTLY, VACUUM, …), else "". Such a
// statement cannot run in an analysis session.
func (st Statement) NonTransactional() string { return sqllex.NonTransactional(st.words) }

// NonConcurrently returns a CREATE [UNIQUE] INDEX CONCURRENTLY statement
// without CONCURRENTLY, which can run inside a transaction: a pass that needs
// the index to exist (a query plan that should use it) builds it this way in
// the rolled-back transaction. False for any other statement.
func (st Statement) NonConcurrently() (string, bool) {
	loc := concurrentIndexRE.FindStringSubmatchIndex(st.SQL)
	if loc == nil {
		return "", false
	}
	return st.SQL[:loc[3]] + st.SQL[loc[1]:], true
}

var concurrentIndexRE = regexp.MustCompile(`(?is)^(\s*create\s+(?:unique\s+)?index\s+)concurrently\s+`)

// first is the statement's leading keyword, lowercased.
func (st Statement) first() string {
	if len(st.words) == 0 {
		return ""
	}
	return st.words[0]
}

// has reports whether the statement holds every one of words.
func (st Statement) has(words ...string) bool {
	for _, w := range words {
		found := false
		for _, sw := range st.words {
			if sw == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// staticLocks is the lock a statement that was not executed is known to
// take, from its kind alone: the concurrent index statements take SHARE
// UPDATE EXCLUSIVE, which blocks DDL and VACUUM but neither reads nor
// writes. Other non-transactional statements report none.
func (st Statement) staticLocks() []Lock {
	const mode = "ShareUpdateExclusiveLock"
	switch {
	case st.first() == "create" && st.has("index", "concurrently"):
		if m := indexTableRE.FindStringSubmatch(st.SQL); m != nil {
			return []Lock{{Relation: unquoteName(m[1]), Mode: mode, Static: true}}
		}
	case st.first() == "drop" && st.has("index", "concurrently"),
		st.first() == "reindex" && st.has("concurrently"):
		if m := lastNameRE.FindStringSubmatch(st.SQL); m != nil {
			return []Lock{{Relation: unquoteName(m[1]), Mode: mode, Static: true}}
		}
	}
	return nil
}

const sqlName = `(?:"[^"]+"|[\w$]+)(?:\.(?:"[^"]+"|[\w$]+))?`

var (
	indexTableRE = regexp.MustCompile(`(?is)\bon\s+(?:only\s+)?(` + sqlName + `)`)
	lastNameRE   = regexp.MustCompile(`(?is)(` + sqlName + `)\s*$`)
)

// unquoteName renders a SQL name the way the document names relations:
// unquoted parts folded to lower case as postgres folds them, quotes
// removed, public's schema dropped.
func unquoteName(name string) string {
	parts := sqlNamePartRE.FindAllString(name, -1)
	for i, p := range parts {
		if strings.HasPrefix(p, `"`) {
			parts[i] = strings.Trim(p, `"`)
		} else {
			parts[i] = strings.ToLower(p)
		}
	}
	if len(parts) == 2 && parts[0] == "public" {
		parts = parts[1:]
	}
	return strings.Join(parts, ".")
}

var sqlNamePartRE = regexp.MustCompile(`"[^"]+"|[\w$]+`)
