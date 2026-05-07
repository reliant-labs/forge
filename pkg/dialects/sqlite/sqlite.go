package sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/mattn/go-sqlite3" // SQLite driver

	"github.com/reliant-labs/forge/pkg/orm"
)

// Dialect implements the Dialect interface for SQLite
type Dialect struct{}

func (d *Dialect) Name() string {
	return "sqlite"
}

func (d *Dialect) DriverName() string {
	return "sqlite3"
}

func (d *Dialect) Placeholder(index int) string {
	// SQLite uses ? for all placeholders
	return "?"
}

func (d *Dialect) QuoteIdentifier(identifier string) string {
	// SQLite uses double quotes for identifiers
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func (d *Dialect) MapFieldType(fieldType orm.FieldType) string {
	// Map ORM field types to SQLite types
	switch fieldType {
	case orm.TypeText, orm.TypeVarchar:
		return "TEXT"
	case orm.TypeInteger:
		return "INTEGER"
	case orm.TypeBigInt:
		return "INTEGER" // SQLite doesn't distinguish between INTEGER and BIGINT
	case orm.TypeBoolean:
		return "INTEGER" // SQLite uses INTEGER for booleans (0 or 1)
	case orm.TypeTimestampTZ:
		return "DATETIME" // SQLite uses DATETIME for timestamps
	case orm.TypeJSONB:
		return "TEXT" // SQLite stores JSON as TEXT
	case orm.TypeBytea:
		return "BLOB"
	case orm.TypeSerial:
		return "INTEGER" // SQLite uses INTEGER for auto-increment
	case orm.TypeBigSerial:
		return "INTEGER" // SQLite uses INTEGER for auto-increment
	default:
		return "TEXT"
	}
}

func (d *Dialect) SupportsReturning() bool {
	// SQLite 3.35.0+ supports RETURNING, but for compatibility, we return false
	return false
}

func (d *Dialect) OnConflictClause(conflictColumn string, updateColumns []string) string {
	sets := make([]string, len(updateColumns))
	for i, col := range updateColumns {
		sets[i] = fmt.Sprintf("%s = excluded.%s", col, col)
	}

	var updateSet string
	if len(sets) > 0 {
		updateSet = " DO UPDATE SET "
		for i, set := range sets {
			if i > 0 {
				updateSet += ", "
			}
			updateSet += set
		}
	} else {
		updateSet = " DO NOTHING"
	}

	return fmt.Sprintf("ON CONFLICT (%s)%s", conflictColumn, updateSet)
}

// TableExistsQuery returns a SQL query to check if a table exists in SQLite
func (d *Dialect) TableExistsQuery(tableName string) string {
	if err := orm.ValidateIdentifier(tableName); err != nil {
		panic(err)
	}
	// SQLite uses sqlite_master to check for table existence
	return fmt.Sprintf(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='%s'`, tableName)
}

// ListTablesQuery returns a SQL query to list all user tables in SQLite
func (d *Dialect) ListTablesQuery() string {
	// Query sqlite_master, excluding SQLite system tables (those starting with sqlite_)
	return `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`
}

// IntrospectColumnsQuery returns a SQL query to introspect columns of a table in SQLite
func (d *Dialect) IntrospectColumnsQuery(tableName string) string {
	if err := orm.ValidateIdentifier(tableName); err != nil {
		panic(err)
	}
	// SQLite uses PRAGMA table_info() for column introspection
	// Returns: cid, name, type, notnull, dflt_value, pk
	return fmt.Sprintf(`PRAGMA table_info(%s)`, tableName)
}

// IntrospectIndexesQuery returns a SQL query to introspect indexes of a table in SQLite
func (d *Dialect) IntrospectIndexesQuery(tableName string) string {
	if err := orm.ValidateIdentifier(tableName); err != nil {
		panic(err)
	}
	// SQLite requires PRAGMA index_list() to get index names
	// Then PRAGMA index_info() for each index to get columns
	// This returns the index list query; the implementation will need to follow up
	// with index_info() for each index
	return fmt.Sprintf(`PRAGMA index_list(%s)`, tableName)
}

// ParseColumnType converts a SQLite type string to ORM FieldType
func (d *Dialect) ParseColumnType(dbType string) (orm.FieldType, error) {
	// SQLite has type affinity - normalize to uppercase for comparison
	dbType = strings.ToUpper(strings.TrimSpace(dbType))

	// Handle common type variations with affinity rules
	// SQLite type affinity: TEXT, NUMERIC, INTEGER, REAL, BLOB

	// INTEGER affinity
	if strings.Contains(dbType, "INT") {
		return orm.TypeInteger, nil
	}

	// TEXT affinity
	if strings.Contains(dbType, "CHAR") || strings.Contains(dbType, "CLOB") ||
		strings.Contains(dbType, "TEXT") || dbType == "VARCHAR" {
		return orm.TypeText, nil
	}

	// BLOB affinity
	if strings.Contains(dbType, "BLOB") {
		return orm.TypeBytea, nil
	}

	// REAL affinity
	if strings.Contains(dbType, "REAL") || strings.Contains(dbType, "FLOA") ||
		strings.Contains(dbType, "DOUB") {
		return orm.TypeText, nil // Map floating point to TEXT for safety
	}

	// Special types
	if strings.Contains(dbType, "DATETIME") || strings.Contains(dbType, "TIMESTAMP") {
		return orm.TypeTimestampTZ, nil
	}

	if strings.Contains(dbType, "BOOL") {
		return orm.TypeBoolean, nil
	}

	// NUMERIC affinity - default to TEXT for safety
	if strings.Contains(dbType, "NUMERIC") || strings.Contains(dbType, "DECIMAL") {
		return orm.TypeText, nil
	}

	// If empty or unknown, default to TEXT (SQLite's default affinity)
	if dbType == "" {
		return orm.TypeText, nil
	}

	// Unknown type - return error with context
	return "", fmt.Errorf("sqlite: unknown column type '%s' - cannot map to FieldType", dbType)
}

// ScanColumn scans a row from PRAGMA table_info() into an IntrospectedColumn
func (d *Dialect) ScanColumn(rows *sql.Rows) (orm.IntrospectedColumn, error) {
	// PRAGMA table_info() format: cid, name, type, notnull, dflt_value, pk
	var cid int
	var name string
	var dbType string
	var notNull int // SQLite uses integer: 0 = nullable, 1 = not null
	var dfltValue sql.NullString
	var pk int // SQLite uses integer: 0 = not primary key, >0 = primary key position

	err := rows.Scan(&cid, &name, &dbType, &notNull, &dfltValue, &pk)
	if err != nil {
		return orm.IntrospectedColumn{}, fmt.Errorf("sqlite: failed to scan PRAGMA table_info row: %w", err)
	}

	// Parse the database type to ORM FieldType
	fieldType, err := d.ParseColumnType(dbType)
	if err != nil {
		return orm.IntrospectedColumn{}, fmt.Errorf("sqlite: error parsing column '%s': %w", name, err)
	}

	// Convert default value
	var defaultValue *string
	if dfltValue.Valid {
		defaultValue = &dfltValue.String
	}

	// In SQLite, primary key columns are implicitly NOT NULL even if the notnull flag is 0
	// See: https://www.sqlite.org/lang_createtable.html#primkeyconst
	isPrimaryKey := pk > 0
	isNullable := notNull == 0 && !isPrimaryKey

	column := orm.IntrospectedColumn{
		Name:         name,
		Type:         fieldType,
		Nullable:     isNullable,
		DefaultValue: defaultValue,
		IsPrimaryKey: isPrimaryKey,
		IsUnique:     false, // Will be determined from index introspection
	}

	return column, nil
}

// ScanIndex scans a row from PRAGMA index_list() into index information
func (d *Dialect) ScanIndex(rows *sql.Rows) (indexName, columnName string, isUnique bool, err error) {
	// PRAGMA index_list() format: seq, name, unique, origin, partial
	// Note: This only gives us the index name, not the columns
	// We need to call PRAGMA index_info(index_name) separately to get columns
	var seq int
	var name string
	var unique int // 0 = not unique, 1 = unique
	var origin string
	var partial int

	err = rows.Scan(&seq, &name, &unique, &origin, &partial)
	if err != nil {
		return "", "", false, fmt.Errorf("sqlite: failed to scan PRAGMA index_list row: %w", err)
	}

	// For SQLite, we return the index name but no column name yet
	// The caller will need to execute PRAGMA index_info(name) to get the columns
	// We use a sentinel empty string for columnName to indicate this
	return name, "", unique == 1, nil
}

// init registers the SQLite dialect
func init() {
	orm.RegisterDialect(&Dialect{})
}
