// Package schemadef projects a project's APPLIED database schema —
// db/migrations/*.up.sql executed in order against an in-memory shadow
// database — into a typed model that code generation consumes.
//
// SQL is the schema language in forge: migrations are the single source
// of truth for what tables and columns exist. Everything else (entity
// structs, the ORM, CRUD wiring, frontend gating) is a projection of
// the schema this package reports.
//
// # Shadow strategy
//
// Migrations are applied to an in-memory SQLite database (modernc.org/
// sqlite — pure Go, no cgo, no network, no container). This works for
// the postgres dialect too because SQLite's parser accepts arbitrary
// declared column types verbatim (TEXT[], TIMESTAMPTZ, JSONB, DOUBLE
// PRECISION, BIGSERIAL, UUID all parse) and PRAGMA table_info reports
// the declared type exactly as written — so the postgres types written
// in the migration survive introspection untouched. This is the same
// contract every generated project's own tests already rely on:
// pkg/testkit applies the project's migrations verbatim to in-memory
// SQLite.
//
// The portable-subset rules this imposes on migrations (all of which
// `forge add entity` emits by construction):
//
//   - function defaults must be parenthesized: DEFAULT (now()), not
//     DEFAULT now()
//   - no '::type' cast syntax — write DEFAULT '{}' rather than
//     DEFAULT '{}'::jsonb (postgres implicitly casts the literal)
//
// Statements that fail on the shadow are skipped when they cannot
// affect the table/column model (DML data movement, CREATE FUNCTION /
// TRIGGER / EXTENSION / VIEW, COMMENT, SET ...). A failing CREATE
// TABLE / ALTER TABLE / DROP TABLE / CREATE INDEX is a hard error —
// those define the schema being projected, so silently skipping one
// would generate an ORM that lies.
package schemadef

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver for the shadow DB
)

// Table is one introspected table of the applied schema.
type Table struct {
	Name    string
	Columns []Column
	// PKCols lists primary-key column names in key order.
	PKCols []string
	// Indexes lists non-PK indexes (unique and plain).
	Indexes []Index
	// ForeignKeys lists declared REFERENCES constraints.
	ForeignKeys []ForeignKey
}

// Column is one introspected column.
type Column struct {
	Name string
	// DeclType is the declared SQL type verbatim from the migration
	// (e.g. "TIMESTAMPTZ", "TEXT[]", "DOUBLE PRECISION").
	DeclType string
	// Type is the canonical forge type the declared type maps to.
	Type CanonicalType
	// IsArray is true for declared array types ("TEXT[]"); Type then
	// holds the ELEMENT type.
	IsArray bool
	NotNull bool
	// Default is the raw default expression, "" when none.
	Default string
	IsPK    bool
}

// Index is a non-PK index on a table.
type Index struct {
	Name    string
	Columns []string
	Unique  bool
}

// ForeignKey is a declared REFERENCES constraint.
type ForeignKey struct {
	Column    string
	RefTable  string
	RefColumn string
}

// CanonicalType is the dialect-neutral type vocabulary the generators
// consume. The mapping from declared SQL types is documented on
// MapDeclaredType.
type CanonicalType string

const (
	TypeString CanonicalType = "string"
	TypeInt    CanonicalType = "int64"
	TypeFloat  CanonicalType = "float64"
	TypeBool   CanonicalType = "bool"
	TypeTime   CanonicalType = "time"
	TypeJSON   CanonicalType = "json"
	TypeBytes  CanonicalType = "bytes"
)

// Conventions are the behavior-by-convention signals derived from real
// columns. No annotations: the columns ARE the declaration.
//
//	deleted_at (time)            ⇒ soft delete (UPDATE-not-DELETE,
//	                               reads filter IS NULL, ListAll* unfiltered)
//	created_at + updated_at      ⇒ managed timestamps
//	tenant_id (string, NOT NULL) ⇒ tenant-scoped rows
//	text columns                 ⇒ searchable by the generated list filter
type Conventions struct {
	SoftDelete bool
	Timestamps bool
	HasTenant  bool
	// TenantColumn is "tenant_id" when HasTenant.
	TenantColumn string
	// SearchColumns are the text columns (excluding the PK, tenant and
	// managed columns) the generated search filter matches against.
	SearchColumns []string
}

// managed column names recognized by convention.
const (
	ColCreatedAt = "created_at"
	ColUpdatedAt = "updated_at"
	ColDeletedAt = "deleted_at"
	ColTenantID  = "tenant_id"
)

// DetectConventions reads the behavior conventions off a table's columns.
func DetectConventions(t Table) Conventions {
	var c Conventions
	byName := map[string]Column{}
	for _, col := range t.Columns {
		byName[col.Name] = col
	}
	if col, ok := byName[ColDeletedAt]; ok && col.Type == TypeTime {
		c.SoftDelete = true
	}
	_, hasCreated := byName[ColCreatedAt]
	_, hasUpdated := byName[ColUpdatedAt]
	c.Timestamps = hasCreated && hasUpdated
	if col, ok := byName[ColTenantID]; ok && col.Type == TypeString {
		c.HasTenant = true
		c.TenantColumn = ColTenantID
	}
	for _, col := range t.Columns {
		if col.Type != TypeString || col.IsArray || col.IsPK {
			continue
		}
		switch col.Name {
		case ColTenantID:
			continue
		}
		c.SearchColumns = append(c.SearchColumns, col.Name)
	}
	return c
}

// ApplyAndIntrospect applies every *.up.sql under migDir (in lexical
// order) to a fresh in-memory shadow database and returns the resulting
// schema. A missing or empty migrations directory returns (nil, nil):
// no schema, no entities, nothing to project.
func ApplyAndIntrospect(migDir string) ([]Table, error) {
	ups, err := upMigrations(migDir)
	if err != nil || len(ups) == 0 {
		return nil, err
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open shadow db: %w", err)
	}
	defer func() { _ = db.Close() }()
	// The :memory: DSN gives every pooled connection its OWN database;
	// pin to a single connection so all migrations land in one schema.
	db.SetMaxOpenConns(1)

	for _, name := range ups {
		raw, rerr := os.ReadFile(filepath.Join(migDir, name))
		if rerr != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, rerr)
		}
		if aerr := applyMigration(db, string(raw)); aerr != nil {
			return nil, fmt.Errorf("apply migration %s to shadow schema: %w", name, aerr)
		}
	}

	return introspect(db)
}

func upMigrations(migDir string) ([]string, error) {
	entries, err := os.ReadDir(migDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	sort.Strings(ups)
	return ups, nil
}

// applyMigration executes one migration file statement by statement.
// Schema-defining statements that fail are hard errors; statements that
// cannot affect the table/column model are skipped on failure (postgres-
// only DML or auxiliary DDL the shadow can't execute).
func applyMigration(db *sql.DB, sqlText string) error {
	for _, stmt := range SplitStatements(sqlText) {
		if _, err := db.Exec(stmt); err != nil {
			if isSchemaDefining(stmt) {
				return fmt.Errorf("%w\nstatement:\n%s\n\nhint: the shadow schema runs on SQLite; keep table-defining DDL in the portable subset (parenthesized function defaults like DEFAULT (now()), no '::type' casts)", err, stmt)
			}
			// Non-schema statement (data movement, functions, triggers,
			// comments, extensions): the table/column model is unaffected.
			continue
		}
	}
	return nil
}

// isSchemaDefining reports whether a statement defines the table/column
// model the ORM is projected from.
func isSchemaDefining(stmt string) bool {
	head := strings.ToUpper(strings.Join(strings.Fields(stmt), " "))
	for _, p := range []string{
		"CREATE TABLE", "ALTER TABLE", "DROP TABLE",
		"CREATE INDEX", "CREATE UNIQUE INDEX", "DROP INDEX",
	} {
		if strings.HasPrefix(head, p) {
			return true
		}
	}
	return false
}

// SplitStatements splits SQL text into individual statements, honoring
// single/double quotes, line and block comments, postgres dollar-quoted
// strings ($tag$ ... $tag$), and trigger bodies (BEGIN ... END;).
func SplitStatements(sqlText string) []string {
	var stmts []string
	var b strings.Builder
	s := sqlText
	i := 0
	beginDepth := 0
	flush := func() {
		stmt := strings.TrimSpace(b.String())
		b.Reset()
		if stmt != "" && !isOnlyComments(stmt) {
			stmts = append(stmts, stmt)
		}
	}
	for i < len(s) {
		c := s[i]
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-': // line comment
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				j = len(s) - i
			}
			b.WriteString(s[i : i+j])
			i += j
		case c == '/' && i+1 < len(s) && s[i+1] == '*': // block comment
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				j = len(s) - i - 2
			}
			b.WriteString(s[i : i+j+4])
			i += j + 4
		case c == '\'' || c == '"': // quoted string / identifier
			q := c
			j := i + 1
			for j < len(s) {
				if s[j] == q {
					if j+1 < len(s) && s[j+1] == q { // doubled quote escape
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			b.WriteString(s[i:j])
			i = j
		case c == '$': // possible dollar-quoted string
			if end := dollarQuoteEnd(s, i); end > i {
				b.WriteString(s[i:end])
				i = end
			} else {
				b.WriteByte(c)
				i++
			}
		case c == ';':
			b.WriteByte(c)
			i++
			if beginDepth == 0 {
				flush()
			}
		default:
			// BEGIN ... END; bodies only exist inside CREATE TRIGGER
			// statements (SQLite trigger syntax). A bare transaction
			// BEGIN; never lands here because the buffer wouldn't
			// contain CREATE TRIGGER.
			if isWordBoundary(s, i) {
				inTrigger := beginDepth > 0 ||
					strings.Contains(strings.ToUpper(b.String()), "CREATE TRIGGER")
				switch {
				case inTrigger && (hasWordAt(s, i, "BEGIN") || hasWordAt(s, i, "CASE")):
					beginDepth++
				case inTrigger && hasWordAt(s, i, "END"):
					if beginDepth > 0 {
						beginDepth--
					}
				}
			}
			b.WriteByte(c)
			i++
		}
	}
	flush()
	return stmts
}

func isOnlyComments(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") || t == ";" {
			continue
		}
		return false
	}
	return true
}

func isWordBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	p := s[i-1]
	return !(p >= 'a' && p <= 'z' || p >= 'A' && p <= 'Z' || p >= '0' && p <= '9' || p == '_')
}

func hasWordAt(s string, i int, word string) bool {
	if i+len(word) > len(s) {
		return false
	}
	if !strings.EqualFold(s[i:i+len(word)], word) {
		return false
	}
	j := i + len(word)
	if j == len(s) {
		return true
	}
	n := s[j]
	return !(n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z' || n >= '0' && n <= '9' || n == '_')
}

// dollarQuoteEnd returns the index just past a postgres dollar-quoted
// string starting at i, or i when s[i:] is not a dollar quote.
func dollarQuoteEnd(s string, i int) int {
	j := i + 1
	for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return i
	}
	tag := s[i : j+1] // "$tag$" or "$$"
	end := strings.Index(s[j+1:], tag)
	if end < 0 {
		return i
	}
	return j + 1 + end + len(tag)
}

// introspect reads the full table model out of the shadow database.
func introspect(db *sql.DB) ([]Table, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list shadow tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var tables []Table
	for _, name := range names {
		t, err := introspectTable(db, name)
		if err != nil {
			return nil, fmt.Errorf("introspect table %s: %w", name, err)
		}
		tables = append(tables, t)
	}
	return tables, nil
}

func introspectTable(db *sql.DB, name string) (Table, error) {
	t := Table{Name: name}

	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(` + quoteSQLString(name) + `) ORDER BY cid`)
	if err != nil {
		return t, err
	}
	defer func() { _ = rows.Close() }()
	type pkPos struct {
		name string
		pos  int
	}
	var pks []pkPos
	for rows.Next() {
		var (
			col      Column
			notNull  int
			deflt    sql.NullString
			pk       int
			declType string
		)
		if err := rows.Scan(&col.Name, &declType, &notNull, &deflt, &pk); err != nil {
			return t, err
		}
		col.DeclType = declType
		col.Type, col.IsArray = MapDeclaredType(declType)
		col.NotNull = notNull != 0
		if deflt.Valid {
			col.Default = deflt.String
		}
		if pk > 0 {
			col.IsPK = true
			// PK columns are NOT NULL by definition even when the DDL
			// didn't say so explicitly.
			col.NotNull = true
			pks = append(pks, pkPos{col.Name, pk})
		}
		t.Columns = append(t.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return t, err
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
	for _, p := range pks {
		t.PKCols = append(t.PKCols, p.name)
	}

	idx, err := introspectIndexes(db, name)
	if err != nil {
		return t, err
	}
	t.Indexes = idx

	fks, err := introspectForeignKeys(db, name)
	if err != nil {
		return t, err
	}
	t.ForeignKeys = fks

	return t, nil
}

func introspectIndexes(db *sql.DB, table string) ([]Index, error) {
	rows, err := db.Query(`SELECT name, "unique", origin FROM pragma_index_list(` + quoteSQLString(table) + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var idxs []Index
	for rows.Next() {
		var (
			ix     Index
			unique int
			origin string
		)
		if err := rows.Scan(&ix.Name, &unique, &origin); err != nil {
			return nil, err
		}
		if origin == "pk" {
			continue
		}
		ix.Unique = unique != 0
		idxs = append(idxs, ix)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range idxs {
		crows, err := db.Query(`SELECT name FROM pragma_index_info(` + quoteSQLString(idxs[i].Name) + `) ORDER BY seqno`)
		if err != nil {
			return nil, err
		}
		for crows.Next() {
			var cn sql.NullString
			if err := crows.Scan(&cn); err != nil {
				_ = crows.Close()
				return nil, err
			}
			if cn.Valid {
				idxs[i].Columns = append(idxs[i].Columns, cn.String)
			}
		}
		cerr := crows.Err()
		_ = crows.Close()
		if cerr != nil {
			return nil, cerr
		}
	}
	return idxs, nil
}

func introspectForeignKeys(db *sql.DB, table string) ([]ForeignKey, error) {
	rows, err := db.Query(`SELECT "table", "from", "to" FROM pragma_foreign_key_list(` + quoteSQLString(table) + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var fks []ForeignKey
	for rows.Next() {
		var (
			fk ForeignKey
			to sql.NullString
		)
		if err := rows.Scan(&fk.RefTable, &fk.Column, &to); err != nil {
			return nil, err
		}
		fk.RefColumn = "id"
		if to.Valid && to.String != "" {
			fk.RefColumn = to.String
		}
		fks = append(fks, fk)
	}
	return fks, rows.Err()
}

// quoteSQLString single-quotes a value for inline use in the pragma
// table-valued functions (the modernc driver does not bind parameters
// inside pragma_* calls).
func quoteSQLString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// MapDeclaredType maps a declared SQL column type to the canonical
// forge type. The second return is true for array types ("TEXT[]"),
// in which case the canonical type describes the ELEMENT.
//
// Mapping table (case-insensitive, length/precision suffixes ignored):
//
//	TEXT, VARCHAR, CHAR, CITEXT, UUID            → string
//	BIGINT, INTEGER, INT, SMALLINT, *SERIAL      → int64
//	DOUBLE PRECISION, REAL, FLOAT, NUMERIC, DECIMAL → float64
//	BOOLEAN, BOOL                                → bool
//	TIMESTAMPTZ, TIMESTAMP[ WITH(OUT) TIME ZONE], DATE, DATETIME → time
//	JSONB, JSON                                  → json
//	BYTEA, BLOB                                  → bytes
//	anything else (incl. SQLite's untyped "")    → string
func MapDeclaredType(decl string) (CanonicalType, bool) {
	d := strings.ToUpper(strings.TrimSpace(decl))
	isArray := false
	if strings.HasSuffix(d, "[]") {
		isArray = true
		d = strings.TrimSpace(strings.TrimSuffix(d, "[]"))
	}
	// Strip length/precision: VARCHAR(255), NUMERIC(10,2).
	if p := strings.IndexByte(d, '('); p >= 0 {
		d = strings.TrimSpace(d[:p])
	}
	switch d {
	case "TEXT", "VARCHAR", "CHARACTER VARYING", "CHAR", "CHARACTER", "CITEXT", "UUID":
		return TypeString, isArray
	case "BIGINT", "INTEGER", "INT", "INT2", "INT4", "INT8", "SMALLINT", "MEDIUMINT", "TINYINT", "BIGSERIAL", "SERIAL", "SMALLSERIAL":
		return TypeInt, isArray
	case "DOUBLE PRECISION", "DOUBLE", "REAL", "FLOAT", "FLOAT4", "FLOAT8", "NUMERIC", "DECIMAL":
		return TypeFloat, isArray
	case "BOOLEAN", "BOOL":
		return TypeBool, isArray
	case "TIMESTAMPTZ", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITHOUT TIME ZONE", "DATE", "DATETIME":
		return TypeTime, isArray
	case "JSONB", "JSON":
		return TypeJSON, isArray
	case "BYTEA", "BLOB":
		return TypeBytes, isArray
	default:
		return TypeString, isArray
	}
}
