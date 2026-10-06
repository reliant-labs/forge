// File: internal/cli/lint/lint_migration_sql.go
//
// Shared text-level readers over db/migrations and embedded SQL, used by the
// lanes that must work on a half-migrated project where no postgres is
// available (read-only-fields, time-bucketing). Purely textual: comments are
// blanked, CREATE/ALTER statements are replayed in the order the migrator
// applies them.

package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// matchParen returns the offset of the ')' closing the '(' at open, ignoring
// parens inside string literals.
func matchParen(s string, open int) (int, bool) {
	depth := 0
	inString := false
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case inString:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// normIdent strips quoting and schema qualification from an identifier and
// folds it to lower case, matching how postgres resolves the unquoted names
// forge generates.
func normIdent(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"`)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = strings.Trim(s[i+1:], `"`)
	}
	return strings.ToLower(s)
}

// blankSQLComments replaces SQL comments with spaces, preserving both the
// total length and every newline so byte offsets and line numbers still
// address the original text.
//
// This is load-bearing rather than tidy: forge's own birth migration writes
// the foreign key it CANNOT yet apply as a commented-out suggestion for the
// author to uncomment later. Reading comments would make the check treat
// every such suggestion as an applied constraint and flag fixtures against a
// schema that does not exist.
func blankSQLComments(s string) string {
	out := []byte(s)
	inString := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inString:
			if c == '\'' {
				if i+1 < len(out) && out[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '-' && i+1 < len(out) && out[i+1] == '-':
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			for i < len(out) {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i] = ' '
					out[i+1] = ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		}
	}
	return string(out)
}

// ── Foreign keys out of migration text ────────────────────────────────────

// migrationFK is one declared foreign key, with the migration that declared
// it so a finding can attribute the constraint to the change that added it.
type migrationFK struct {
	Table      string
	Column     string
	RefTable   string
	RefColumn  string
	Constraint string
	DeclaredIn string
}

var (
	alterAddFKRe = regexp.MustCompile(
		`(?is)\bALTER\s+TABLE\s+(?:ONLY\s+)?("?[\w.]+"?)\s+ADD\s+CONSTRAINT\s+("?[\w]+"?)\s+FOREIGN\s+KEY\s*\(\s*("?[\w]+"?)\s*\)\s*REFERENCES\s+("?[\w.]+"?)\s*(?:\(\s*("?[\w]+"?)\s*\))?`)
	alterDropConstraintRe = regexp.MustCompile(
		`(?is)\bALTER\s+TABLE\s+(?:ONLY\s+)?("?[\w.]+"?)\s+DROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?("?[\w]+"?)`)
	dropTableRe = regexp.MustCompile(
		`(?is)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?("?[\w.]+"?)`)
	createTableRe = regexp.MustCompile(
		`(?is)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?("?[\w.]+"?)\s*\(`)
	tableFKRe = regexp.MustCompile(
		`(?is)^\s*(?:CONSTRAINT\s+("?[\w]+"?)\s+)?FOREIGN\s+KEY\s*\(\s*("?[\w]+"?)\s*\)\s*REFERENCES\s+("?[\w.]+"?)\s*(?:\(\s*("?[\w]+"?)\s*\))?`)
	columnFKRe = regexp.MustCompile(
		`(?is)^\s*("?[\w]+"?)\s+.*?\bREFERENCES\s+("?[\w.]+"?)\s*(?:\(\s*("?[\w]+"?)\s*\))?`)
)

// foreignKeysFromMigrations reads the declared foreign keys out of the
// project's migrations, indexed as fks[table][column].
//
// Migrations are replayed in lexical order — the order the migrator applies
// them — and DROP CONSTRAINT / DROP TABLE remove what earlier files added, so
// the result describes the schema as it stands after the last migration
// rather than every constraint the history ever mentioned.
func foreignKeysFromMigrations(root, migrationsDir string) (map[string]map[string]migrationFK, error) {
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return nil, nil
	}
	var files []string
	if err := filepath.WalkDir(migrationsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".up.sql") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk %s: %w", migrationsDir, err)
	}
	sort.Strings(files)

	// Keyed by constraint name so DROP CONSTRAINT can retract exactly what
	// ADD CONSTRAINT introduced.
	live := map[string]migrationFK{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		applyMigrationFKs(live, blankSQLComments(string(data)), filepath.ToSlash(rel))
	}

	out := map[string]map[string]migrationFK{}
	for _, fk := range live {
		if out[fk.Table] == nil {
			out[fk.Table] = map[string]migrationFK{}
		}
		out[fk.Table][fk.Column] = fk
	}
	return out, nil
}

// applyMigrationFKs folds one migration's foreign-key additions and removals
// into the running set. content must already have its comments blanked.
func applyMigrationFKs(live map[string]migrationFK, content, relPath string) {
	for _, m := range createTableRe.FindAllStringSubmatchIndex(content, -1) {
		table := normIdent(content[m[2]:m[3]])
		open := m[1] - 1 // the '(' the pattern ends on
		end, ok := matchParen(content, open)
		if !ok {
			continue
		}
		for _, fk := range createTableFKs(table, content[open+1:end]) {
			fk.DeclaredIn = relPath
			live[fk.Constraint] = fk
		}
	}

	for _, m := range alterAddFKRe.FindAllStringSubmatch(content, -1) {
		fk := migrationFK{
			Table:      normIdent(m[1]),
			Constraint: normIdent(m[2]),
			Column:     normIdent(m[3]),
			RefTable:   normIdent(m[4]),
			RefColumn:  normIdent(m[5]),
			DeclaredIn: relPath,
		}
		if fk.RefColumn == "" {
			fk.RefColumn = "id"
		}
		live[fk.Constraint] = fk
	}

	for _, m := range alterDropConstraintRe.FindAllStringSubmatch(content, -1) {
		delete(live, normIdent(m[2]))
	}
	for _, m := range dropTableRe.FindAllStringSubmatch(content, -1) {
		dropped := normIdent(m[1])
		for name, fk := range live {
			if fk.Table == dropped {
				delete(live, name)
			}
		}
	}
}

// createTableFKs extracts the foreign keys declared inside a CREATE TABLE
// body, both as table constraints (`FOREIGN KEY (x) REFERENCES ...`) and
// inline on the column (`x TEXT REFERENCES ...`).
//
// An unnamed constraint is given the name postgres would derive for it,
// `<table>_<column>_fkey`, so a later DROP CONSTRAINT naming it retracts the
// right entry.
func createTableFKs(table, body string) []migrationFK {
	var out []migrationFK
	for _, part := range splitTopLevel(body) {
		var fk migrationFK
		switch m := tableFKRe.FindStringSubmatch(part); {
		case m != nil:
			fk = migrationFK{
				Table:      table,
				Constraint: normIdent(m[1]),
				Column:     normIdent(m[2]),
				RefTable:   normIdent(m[3]),
				RefColumn:  normIdent(m[4]),
			}
		default:
			cm := columnFKRe.FindStringSubmatch(part)
			if cm == nil {
				continue
			}
			// A table-level constraint clause is not a column definition;
			// anything whose first token is a constraint keyword is skipped
			// so `PRIMARY KEY (a)` is never read as a column named PRIMARY.
			if isConstraintKeyword(normIdent(cm[1])) {
				continue
			}
			fk = migrationFK{
				Table:     table,
				Column:    normIdent(cm[1]),
				RefTable:  normIdent(cm[2]),
				RefColumn: normIdent(cm[3]),
			}
		}
		if fk.RefColumn == "" {
			fk.RefColumn = "id"
		}
		if fk.Constraint == "" {
			fk.Constraint = fmt.Sprintf("%s_%s_fkey", fk.Table, fk.Column)
		}
		out = append(out, fk)
	}
	return out
}

// isConstraintKeyword reports whether tok opens a table-level constraint
// clause rather than naming a column.
func isConstraintKeyword(tok string) bool {
	switch tok {
	case "primary", "unique", "check", "foreign", "constraint", "exclude", "like":
		return true
	}
	return false
}

// splitTopLevel splits a CREATE TABLE body on commas at paren depth zero,
// keeping each column definition and table constraint whole.
func splitTopLevel(body string) []string {
	var out []string
	depth := 0
	inString := false
	start := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inString:
			if c == '\'' {
				if i+1 < len(body) && body[i+1] == '\'' {
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	return append(out, body[start:])
}

// eachMigration walks the project's .up.sql files in lexical order — the
// order the migrator applies them — and hands each one's root-relative path
// and comment-blanked content to fn.
//
// Comments are blanked because forge's own birth migration writes the
// constraints it CANNOT yet apply as commented-out suggestions. Reading
// those would build a schema that does not exist and report fixtures against
// constraints nobody has applied.
func eachMigration(root, migrationsDir string, fn func(relPath, content string)) error {
	if _, err := os.Stat(migrationsDir); os.IsNotExist(err) {
		return nil
	}
	var files []string
	if err := filepath.WalkDir(migrationsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".up.sql") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("walk %s: %w", migrationsDir, err)
	}
	sort.Strings(files)

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		fn(filepath.ToSlash(rel), blankSQLComments(string(data)))
	}
	return nil
}
