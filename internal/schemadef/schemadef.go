// Package schemadef projects a project's APPLIED database schema —
// db/migrations/*.up.sql executed in order against a REAL ephemeral
// postgres — into a typed model that code generation consumes.
//
// SQL is the schema language in forge: migrations are the single source
// of truth for what tables and columns exist. Everything else (entity
// structs, the ORM, CRUD wiring, frontend gating) is a projection of
// the schema this package reports.
//
// # Shadow strategy
//
// forge is postgres-pinned. Migrations are applied verbatim — byte for
// byte, no rewriting — to a real ephemeral postgres (pkg/pgtest:
// embedded-postgres by default, an already-running server when
// FORGE_TEST_POSTGRES_URL is set), then the resulting schema is read back
// through postgres's own catalog (information_schema / pg_catalog).
//
// This is a hard improvement over the previous in-memory SQLite shadow,
// which approximated postgres by exploiting SQLite's permissive type
// affinity and required a normalization pass (DEFAULT (now()) wrapping,
// '::type' cast stripping, multi-ADD splitting) to coax idiomatic
// postgres DDL through SQLite's parser. That approximation broke the
// moment a project used a construct SQLite couldn't parse — most
// notably schema-qualified DDL (CREATE TABLE controlplane.foo), which
// froze cp-forge's ORM. Real postgres needs none of that: it IS the
// target, so the schema the generator sees is exactly the schema
// production runs.
//
// Statements that fail to apply are still skipped when they cannot
// affect the table/column model (DML data movement, CREATE FUNCTION /
// TRIGGER / EXTENSION / VIEW, COMMENT, SET ...) — postgres rejects some
// of these in the bare ephemeral DB (e.g. an extension that isn't
// installed) and that must not abort introspection. A failing CREATE
// TABLE / ALTER TABLE / DROP TABLE / CREATE INDEX is a hard error: those
// define the schema being projected, so silently skipping one would
// generate an ORM that lies.
package schemadef

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/pgtest"
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
	// Managed timestamps are type-gated like deleted_at: the pair counts
	// only when the generator can actually STAMP both columns — time
	// columns (stamped as time.Time) or legacy TEXT columns (stamped as
	// RFC3339Nano strings; kalshi fr-3fba9166ba). An exotic type (epoch
	// INTEGER, arrays) opts the table out of managed timestamps; the
	// columns stay plain schema instead of driving stamping code the
	// emitter can't express.
	stampable := func(name string) bool {
		col, ok := byName[name]
		return ok && !col.IsArray && (col.Type == TypeTime || col.Type == TypeString)
	}
	c.Timestamps = stampable(ColCreatedAt) && stampable(ColUpdatedAt)
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
// order) to a fresh real-postgres shadow database and returns the
// resulting schema. A missing or empty migrations directory returns
// (nil, nil): no schema, no entities, nothing to project.
//
// The shadow is a freshly-created database on the process-shared
// ephemeral postgres (pkg/pgtest). It is dropped before returning, so
// every call sees a clean schema. Booting the shared server the first
// time downloads/caches the postgres binary (embedded-postgres) unless
// FORGE_TEST_POSTGRES_URL points at a running server.
func ApplyAndIntrospect(migDir string) ([]Table, error) {
	ups, err := upMigrations(migDir)
	if err != nil || len(ups) == 0 {
		return nil, err
	}

	db, cleanup, err := pgtest.New()
	if err != nil {
		return nil, fmt.Errorf("open postgres shadow db: %w", err)
	}
	defer cleanup()

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

// applyMigration executes one migration file statement by statement
// against the real-postgres shadow. Schema-defining statements that fail
// are hard errors; statements that cannot affect the table/column model
// are skipped on failure (postgres DML data movement, or auxiliary DDL
// the bare ephemeral DB can't satisfy — an extension that isn't
// installed, a role that doesn't exist).
//
// Migrations apply VERBATIM: postgres is the target, so there is no
// normalization pass. Every construct in the migration — schema-qualified
// names, postgres-only types, '::type' casts, multi-ADD ALTERs,
// generated/identity columns — runs exactly as written.
func applyMigration(db *sql.DB, sqlText string) error {
	for _, stmt := range SplitStatements(sqlText) {
		if !isSchemaDefining(stmt) {
			// Non-schema statement (data movement, functions, triggers,
			// comments, extensions): failure cannot change the
			// table/column model — best-effort apply, skip on error.
			_, _ = db.Exec(stmt)
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("%w\nstatement:\n%s", err, strings.TrimSpace(stmt))
		}
	}
	return nil
}

// isSchemaDefining reports whether a statement defines the table/column
// model the ORM is projected from. Leading SQL comments are stripped
// first — migration files routinely carry banner comments, and a
// comment-prefixed CREATE TABLE that silently fell into the skip path
// would produce a partial schema with no error (an ORM that lies).
func isSchemaDefining(stmt string) bool {
	head := strings.ToUpper(strings.Join(strings.Fields(stripLeadingSQLComments(stmt)), " "))
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

// stripLeadingSQLComments removes leading `--` line comments, `/* */`
// block comments and blank lines from a statement.
func stripLeadingSQLComments(stmt string) string {
	s := stmt
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = s[i+2:]
			} else {
				return ""
			}
		default:
			return s
		}
	}
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
			// BEGIN ... END; bodies inside a CREATE TRIGGER statement must
			// not be split on the inner `;`. A bare transaction BEGIN;
			// never lands here because the buffer wouldn't contain
			// CREATE TRIGGER.
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

// introspect reads the full table model out of the real-postgres shadow
// through the postgres catalog (information_schema / pg_catalog). It
// enumerates BASE TABLEs across every user schema — not just `public` —
// so schema-qualified migrations (CREATE TABLE controlplane.foo) are
// introspected like any other. Tables are keyed by their bare name in
// the returned model (the projection layer matches entities by table
// name); pg's own catalog rejects same-named tables in different schemas
// only at query time, which forge projects do not do.
func introspect(db *sql.DB) ([]Table, error) {
	ctx := context.Background()

	// schema, table for every user BASE TABLE. Exclude the system and
	// migration-bookkeeping schemas; keep everything a project defined,
	// including non-public schemas.
	rows, err := db.QueryContext(ctx, `
		SELECT table_schema, table_name
		FROM information_schema.tables
		WHERE table_type = 'BASE TABLE'
		  AND table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND table_name <> 'schema_migrations'
		ORDER BY table_schema, table_name`)
	if err != nil {
		return nil, fmt.Errorf("list shadow tables: %w", err)
	}
	type ref struct{ schema, name string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.schema, &r.name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	cerr := rows.Err()
	_ = rows.Close()
	if cerr != nil {
		return nil, cerr
	}

	var tables []Table
	for _, r := range refs {
		t, err := introspectTable(ctx, db, r.schema, r.name)
		if err != nil {
			return nil, fmt.Errorf("introspect table %s.%s: %w", r.schema, r.name, err)
		}
		tables = append(tables, t)
	}
	return tables, nil
}

func introspectTable(ctx context.Context, db *sql.DB, schema, name string) (Table, error) {
	t := Table{Name: name}

	// Columns in ordinal order. udt_name carries the precise postgres
	// type (int8, timestamptz, jsonb, _text for a text[] …) which
	// MapDeclaredType already understands; data_type = 'ARRAY' marks
	// array columns (udt_name then has the `_elem` form).
	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, udt_name, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, name)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var (
			col        Column
			dataType   string
			udtName    string
			isNullable string
			deflt      sql.NullString
		)
		if err := rows.Scan(&col.Name, &dataType, &udtName, &isNullable, &deflt); err != nil {
			_ = rows.Close()
			return t, err
		}
		col.DeclType = pgDeclType(dataType, udtName)
		col.Type, col.IsArray = MapDeclaredType(col.DeclType)
		col.NotNull = isNullable == "NO"
		if deflt.Valid {
			col.Default = deflt.String
		}
		t.Columns = append(t.Columns, col)
	}
	cerr := rows.Err()
	_ = rows.Close()
	if cerr != nil {
		return t, cerr
	}

	pks, err := introspectPrimaryKey(ctx, db, schema, name)
	if err != nil {
		return t, err
	}
	t.PKCols = pks
	pkSet := make(map[string]bool, len(pks))
	for _, p := range pks {
		pkSet[p] = true
	}
	for i := range t.Columns {
		if pkSet[t.Columns[i].Name] {
			t.Columns[i].IsPK = true
			// PK columns are NOT NULL by definition even when the
			// catalog reports nullable (it never does, but be explicit).
			t.Columns[i].NotNull = true
		}
	}

	idx, err := introspectIndexes(ctx, db, schema, name)
	if err != nil {
		return t, err
	}
	t.Indexes = idx

	fks, err := introspectForeignKeys(ctx, db, schema, name)
	if err != nil {
		return t, err
	}
	t.ForeignKeys = fks

	return t, nil
}

// pgDeclType reconstructs a declared-type string MapDeclaredType
// understands from postgres's data_type / udt_name pair. For arrays
// (data_type == "ARRAY") udt_name is the internal "_elem" form
// (e.g. "_text"); strip the leading underscore and append "[]".
func pgDeclType(dataType, udtName string) string {
	if strings.EqualFold(dataType, "ARRAY") {
		elem := strings.TrimPrefix(udtName, "_")
		return strings.ToUpper(elem) + "[]"
	}
	return strings.ToUpper(udtName)
}

// introspectPrimaryKey returns the primary-key column names in key order.
func introspectPrimaryKey(ctx context.Context, db *sql.DB, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		  AND tc.table_schema = kcu.table_schema
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema = $1
		  AND tc.table_name = $2
		ORDER BY kcu.ordinal_position`, schema, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var pks []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		pks = append(pks, c)
	}
	return pks, rows.Err()
}

// introspectIndexes returns the non-PK indexes (unique and plain) of a
// table via pg_catalog, with columns in index order.
func introspectIndexes(ctx context.Context, db *sql.DB, schema, table string) ([]Index, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ic.relname AS index_name,
		       idx.indisunique AS is_unique,
		       a.attname AS column_name,
		       array_position(idx.indkey, a.attnum) AS ord
		FROM pg_class t
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_index idx ON idx.indrelid = t.oid
		JOIN pg_class ic ON ic.oid = idx.indexrelid
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(idx.indkey)
		WHERE n.nspname = $1
		  AND t.relname = $2
		  AND NOT idx.indisprimary
		ORDER BY ic.relname, ord`, schema, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var (
		order []string
		byName = map[string]*Index{}
	)
	for rows.Next() {
		var (
			idxName string
			unique  bool
			colName string
			ord     sql.NullInt64
		)
		if err := rows.Scan(&idxName, &unique, &colName, &ord); err != nil {
			return nil, err
		}
		ix, ok := byName[idxName]
		if !ok {
			ix = &Index{Name: idxName, Unique: unique}
			byName[idxName] = ix
			order = append(order, idxName)
		}
		ix.Columns = append(ix.Columns, colName)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	idxs := make([]Index, 0, len(order))
	for _, n := range order {
		idxs = append(idxs, *byName[n])
	}
	return idxs, nil
}

// introspectForeignKeys returns the declared REFERENCES constraints of a
// table via information_schema.
func introspectForeignKeys(ctx context.Context, db *sql.DB, schema, table string) ([]ForeignKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kcu.column_name,
		       ccu.table_name AS ref_table,
		       ccu.column_name AS ref_column
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name = kcu.constraint_name
		  AND tc.table_schema = kcu.table_schema
		JOIN information_schema.constraint_column_usage ccu
		  ON tc.constraint_name = ccu.constraint_name
		  AND tc.table_schema = ccu.table_schema
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND tc.table_schema = $1
		  AND tc.table_name = $2
		ORDER BY kcu.ordinal_position`, schema, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var fks []ForeignKey
	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(&fk.Column, &fk.RefTable, &fk.RefColumn); err != nil {
			return nil, err
		}
		if fk.RefColumn == "" {
			fk.RefColumn = "id"
		}
		fks = append(fks, fk)
	}
	return fks, rows.Err()
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
//	anything else (incl. an empty/unknown udt)   → string
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
