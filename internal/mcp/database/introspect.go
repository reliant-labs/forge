package database

import (
	"database/sql"
	"fmt"
	"strings"
)

// TableInfo represents database table metadata
type TableInfo struct {
	Name    string
	Schema  string
	Columns []*ColumnInfo
	Indexes []*IndexInfo
}

// ColumnInfo represents a table column
type ColumnInfo struct {
	Name         string
	DataType     string
	IsNullable   bool
	DefaultValue *string
	IsPrimaryKey bool
}

// IndexInfo represents a table index
type IndexInfo struct {
	Name     string
	Columns  []string
	IsUnique bool
}

// SchemaIntrospector inspects database schema
type SchemaIntrospector struct {
	db *sql.DB
}

// NewSchemaIntrospector creates a new schema introspector
func NewSchemaIntrospector(db *sql.DB) *SchemaIntrospector {
	return &SchemaIntrospector{db: db}
}

// IntrospectTable gets complete information about a table
func (si *SchemaIntrospector) IntrospectTable(tableName string) (*TableInfo, error) {
	table := &TableInfo{
		Name:   tableName,
		Schema: "public",
	}

	// Get columns
	columns, err := si.getColumns(tableName)
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}
	table.Columns = columns

	// Get indexes
	indexes, err := si.getIndexes(tableName)
	if err != nil {
		return nil, fmt.Errorf("failed to get indexes: %w", err)
	}
	table.Indexes = indexes

	return table, nil
}

// getColumns retrieves column information for a table
func (si *SchemaIntrospector) getColumns(tableName string) ([]*ColumnInfo, error) {
	query := `
		SELECT
			c.column_name,
			c.data_type,
			c.is_nullable,
			c.column_default,
			CASE WHEN pk.column_name IS NOT NULL THEN true ELSE false END as is_primary_key
		FROM information_schema.columns c
		LEFT JOIN (
			SELECT ku.column_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage ku
				ON tc.constraint_name = ku.constraint_name
				AND tc.table_schema = ku.table_schema
			WHERE tc.constraint_type = 'PRIMARY KEY'
				AND tc.table_name = $1
		) pk ON c.column_name = pk.column_name
		WHERE c.table_name = $1
		ORDER BY c.ordinal_position
	`

	rows, err := si.db.Query(query, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []*ColumnInfo
	for rows.Next() {
		var col ColumnInfo
		var isNullable string
		var defaultValue sql.NullString

		if err := rows.Scan(&col.Name, &col.DataType, &isNullable, &defaultValue, &col.IsPrimaryKey); err != nil {
			return nil, err
		}

		col.IsNullable = isNullable == "YES"
		if defaultValue.Valid {
			col.DefaultValue = &defaultValue.String
		}

		columns = append(columns, &col)
	}

	return columns, rows.Err()
}

// getIndexes retrieves index information for a table
func (si *SchemaIntrospector) getIndexes(tableName string) ([]*IndexInfo, error) {
	query := `
		SELECT
			i.relname as index_name,
			ix.indisunique as is_unique,
			array_agg(a.attname ORDER BY array_position(ix.indkey, a.attnum)) as columns
		FROM pg_class t
		JOIN pg_index ix ON t.oid = ix.indrelid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(ix.indkey)
		WHERE t.relname = $1
			AND t.relkind = 'r'
		GROUP BY i.relname, ix.indisunique
		ORDER BY i.relname
	`

	rows, err := si.db.Query(query, tableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var indexes []*IndexInfo
	for rows.Next() {
		var idx IndexInfo
		var colsArray string

		if err := rows.Scan(&idx.Name, &idx.IsUnique, &colsArray); err != nil {
			return nil, err
		}

		// Parse PostgreSQL array format: {col1,col2,col3}
		colsArray = strings.Trim(colsArray, "{}")
		if colsArray != "" {
			idx.Columns = strings.Split(colsArray, ",")
		}

		indexes = append(indexes, &idx)
	}

	return indexes, rows.Err()
}

// ListTables returns all tables in the database
func (si *SchemaIntrospector) ListTables() ([]string, error) {
	query := `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public'
			AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`

	rows, err := si.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			return nil, err
		}
		tables = append(tables, tableName)
	}

	return tables, rows.Err()
}

// FormatTableInfo formats table information as a readable string
func FormatTableInfo(table *TableInfo) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Table: %s.%s\n", table.Schema, table.Name))
	sb.WriteString("\nColumns:\n")

	for _, col := range table.Columns {
		nullable := ""
		if !col.IsNullable {
			nullable = " NOT NULL"
		}

		pk := ""
		if col.IsPrimaryKey {
			pk = " PRIMARY KEY"
		}

		defaultVal := ""
		if col.DefaultValue != nil {
			defaultVal = fmt.Sprintf(" DEFAULT %s", *col.DefaultValue)
		}

		sb.WriteString(fmt.Sprintf("  - %s %s%s%s%s\n",
			col.Name, col.DataType, nullable, defaultVal, pk))
	}

	if len(table.Indexes) > 0 {
		sb.WriteString("\nIndexes:\n")
		for _, idx := range table.Indexes {
			unique := ""
			if idx.IsUnique {
				unique = " UNIQUE"
			}
			sb.WriteString(fmt.Sprintf("  - %s%s (%s)\n",
				idx.Name, unique, strings.Join(idx.Columns, ", ")))
		}
	}

	return sb.String()
}

// CompareWithProto compares database schema with proto definitions
func (si *SchemaIntrospector) CompareWithProto(tableName, protoFile string) (string, error) {
	table, err := si.IntrospectTable(tableName)
	if err != nil {
		return "", err
	}

	result := FormatTableInfo(table)
	result += "\n\nProto comparison: Not yet implemented\n"
	result += "To implement: Parse proto file and compare field definitions with columns\n"

	return result, nil
}
