package orm

import (
	"context"
	"errors"
	"testing"

	_ "github.com/mattn/go-sqlite3" // SQLite driver for testing
)

// setupSQLiteClient creates an in-memory SQLite database for testing
// Note: Depends on SQLite dialect being registered (done in migration_test.go)
func setupSQLiteClient(t *testing.T) *Client {
	t.Helper()

	// Use NewClient which will use the registered SQLite dialect
	client, err := NewClient("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to create ORM client: %v", err)
	}

	return client
}

// TestIntrospectTable_SQLite tests introspecting a SQLite table with various column types
func TestIntrospectTable_SQLite(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Create a test table with various column types
	createTableSQL := `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL,
			email TEXT UNIQUE,
			age INTEGER,
			is_active INTEGER,
			created_at DATETIME,
			metadata TEXT,
			avatar BLOB
		)
	`

	_, err := client.Exec(ctx, createTableSQL)
	if err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	// Introspect the table
	schema, err := IntrospectTable(ctx, client, client.Dialect(), "users")
	if err != nil {
		t.Fatalf("IntrospectTable failed: %v", err)
	}

	// Validate basic schema properties
	if schema.Name != "users" {
		t.Errorf("Expected table name 'users', got '%s'", schema.Name)
	}

	if len(schema.Fields) != 8 {
		t.Errorf("Expected 8 fields, got %d", len(schema.Fields))
	}

	// Create a map for easier field lookup
	fieldMap := make(map[string]FieldSchema)
	for _, field := range schema.Fields {
		fieldMap[field.Name] = field
	}

	// Test individual fields
	tests := []struct {
		name       string
		expectType FieldType
		expectPK   bool
		expectNN   bool
		expectUniq bool
	}{
		{"id", TypeInteger, true, true, false}, // Primary keys are implicitly NOT NULL in SQLite
		{"username", TypeText, false, true, false},
		{"email", TypeText, false, false, false},
		{"age", TypeInteger, false, false, false},
		{"is_active", TypeInteger, false, false, false},
		{"created_at", TypeTimestampTZ, false, false, false},
		{"metadata", TypeText, false, false, false},
		{"avatar", TypeBytea, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field, exists := fieldMap[tt.name]
			if !exists {
				t.Fatalf("Field '%s' not found in schema", tt.name)
			}

			if field.Type != tt.expectType {
				t.Errorf("Field '%s': expected type %s, got %s", tt.name, tt.expectType, field.Type)
			}

			if field.PrimaryKey != tt.expectPK {
				t.Errorf("Field '%s': expected PrimaryKey=%v, got %v", tt.name, tt.expectPK, field.PrimaryKey)
			}

			if field.NotNull != tt.expectNN {
				t.Errorf("Field '%s': expected NotNull=%v, got %v", tt.name, tt.expectNN, field.NotNull)
			}

			if field.Unique != tt.expectUniq {
				t.Errorf("Field '%s': expected Unique=%v, got %v", tt.name, tt.expectUniq, field.Unique)
			}
		})
	}
}

// TestIntrospectTable_NotFound tests error when table doesn't exist
func TestIntrospectTable_NotFound(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Try to introspect a non-existent table
	_, err := IntrospectTable(ctx, client, client.Dialect(), "nonexistent_table")
	if err == nil {
		t.Fatal("Expected error when introspecting non-existent table, got nil")
	}

	// Verify it's a SchemaError
	var schemaErr *SchemaError
	if !errors.Is(err, &SchemaError{}) && !asSchemaError(err, &schemaErr) {
		t.Errorf("Expected SchemaError, got %T: %v", err, err)
	}
}

// asSchemaError is a helper to check if error is a SchemaError
func asSchemaError(err error, target **SchemaError) bool {
	if err == nil {
		return false
	}
	se, ok := err.(*SchemaError)
	if ok {
		*target = se
		return true
	}
	return false
}

// TestIntrospectTable_NilContext tests error handling for nil context
func TestIntrospectTable_NilContext(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	// Try to introspect with nil context
	_, err := IntrospectTable(nil, client, client.Dialect(), "users")
	if err == nil {
		t.Fatal("Expected error when using nil context, got nil")
	}

	// Should return ErrNilContext
	if !errors.Is(err, ErrNilContext) {
		t.Errorf("Expected ErrNilContext, got: %v", err)
	}
}

// TestIntrospectTable_EmptyTableName tests error handling for empty table name
func TestIntrospectTable_EmptyTableName(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Try to introspect with empty table name
	_, err := IntrospectTable(ctx, client, client.Dialect(), "")
	if err == nil {
		t.Fatal("Expected error when using empty table name, got nil")
	}
}

// TestIntrospectAllTables_SQLite tests introspecting all tables
func TestIntrospectAllTables_SQLite(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Create multiple test tables
	tables := []string{
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL
		)`,
		`CREATE TABLE posts (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			user_id INTEGER
		)`,
		`CREATE TABLE comments (
			id INTEGER PRIMARY KEY,
			content TEXT,
			post_id INTEGER
		)`,
	}

	for _, createSQL := range tables {
		_, err := client.Exec(ctx, createSQL)
		if err != nil {
			t.Fatalf("Failed to create test table: %v", err)
		}
	}

	// Introspect all tables
	schemas, err := IntrospectAllTables(ctx, client, client.Dialect())
	if err != nil {
		t.Fatalf("IntrospectAllTables failed: %v", err)
	}

	// Should have exactly 3 tables
	if len(schemas) != 3 {
		t.Errorf("Expected 3 tables, got %d", len(schemas))
	}

	// Check that all expected tables are present
	tableNames := make(map[string]bool)
	for _, schema := range schemas {
		tableNames[schema.Name] = true
	}

	expectedTables := []string{"users", "posts", "comments"}
	for _, name := range expectedTables {
		if !tableNames[name] {
			t.Errorf("Expected table '%s' not found in results", name)
		}
	}

	// Verify each table has correct structure
	for _, schema := range schemas {
		switch schema.Name {
		case "users":
			if len(schema.Fields) != 2 {
				t.Errorf("Table 'users': expected 2 fields, got %d", len(schema.Fields))
			}
		case "posts":
			if len(schema.Fields) != 3 {
				t.Errorf("Table 'posts': expected 3 fields, got %d", len(schema.Fields))
			}
		case "comments":
			if len(schema.Fields) != 3 {
				t.Errorf("Table 'comments': expected 3 fields, got %d", len(schema.Fields))
			}
		}
	}
}

// TestIntrospectAllTables_EmptyDatabase tests introspecting an empty database
func TestIntrospectAllTables_EmptyDatabase(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Introspect all tables in empty database
	schemas, err := IntrospectAllTables(ctx, client, client.Dialect())
	if err != nil {
		t.Fatalf("IntrospectAllTables failed: %v", err)
	}

	// Should return empty slice, not nil
	if schemas == nil {
		t.Error("Expected empty slice, got nil")
	}

	if len(schemas) != 0 {
		t.Errorf("Expected 0 tables, got %d", len(schemas))
	}
}

// TestIntrospectAllTables_NilContext tests error handling for nil context
func TestIntrospectAllTables_NilContext(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	// Try to introspect with nil context
	_, err := IntrospectAllTables(nil, client, client.Dialect())
	if err == nil {
		t.Fatal("Expected error when using nil context, got nil")
	}

	// Should return ErrNilContext
	if !errors.Is(err, ErrNilContext) {
		t.Errorf("Expected ErrNilContext, got: %v", err)
	}
}

// TestIntrospectTable_PrimaryKeys tests that primary keys are correctly identified
func TestIntrospectTable_PrimaryKeys(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	testCases := []struct {
		name       string
		createSQL  string
		tableName  string
		pkFields   []string
	}{
		{
			name: "Single primary key",
			createSQL: `CREATE TABLE single_pk (
				id INTEGER PRIMARY KEY,
				name TEXT
			)`,
			tableName: "single_pk",
			pkFields:  []string{"id"},
		},
		{
			name: "Composite primary key",
			createSQL: `CREATE TABLE composite_pk (
				user_id INTEGER,
				role_id INTEGER,
				name TEXT,
				PRIMARY KEY (user_id, role_id)
			)`,
			tableName: "composite_pk",
			pkFields:  []string{"user_id", "role_id"},
		},
		{
			name: "No primary key",
			createSQL: `CREATE TABLE no_pk (
				id INTEGER,
				name TEXT
			)`,
			tableName: "no_pk",
			pkFields:  []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create table
			_, err := client.Exec(ctx, tc.createSQL)
			if err != nil {
				t.Fatalf("Failed to create test table: %v", err)
			}

			// Introspect
			schema, err := IntrospectTable(ctx, client, client.Dialect(), tc.tableName)
			if err != nil {
				t.Fatalf("IntrospectTable failed: %v", err)
			}

			// Count primary key fields
			pkCount := 0
			pkFieldsFound := make([]string, 0)
			for _, field := range schema.Fields {
				if field.PrimaryKey {
					pkCount++
					pkFieldsFound = append(pkFieldsFound, field.Name)
				}
			}

			// Verify count
			if pkCount != len(tc.pkFields) {
				t.Errorf("Expected %d primary key fields, got %d", len(tc.pkFields), pkCount)
			}

			// Verify specific fields
			for _, expectedPK := range tc.pkFields {
				found := false
				for _, field := range schema.Fields {
					if field.Name == expectedPK && field.PrimaryKey {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected field '%s' to be primary key, but it wasn't", expectedPK)
				}
			}
		})
	}
}

// TestIntrospectTable_Indexes tests that indexes are correctly introspected
func TestIntrospectTable_Indexes(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Create a table with various indexes
	createTableSQL := `
		CREATE TABLE indexed_table (
			id INTEGER PRIMARY KEY,
			email TEXT,
			username TEXT,
			created_at DATETIME,
			status TEXT
		)
	`

	_, err := client.Exec(ctx, createTableSQL)
	if err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	// Create indexes
	indexes := []string{
		"CREATE UNIQUE INDEX idx_email ON indexed_table(email)",
		"CREATE INDEX idx_username ON indexed_table(username)",
		"CREATE INDEX idx_status_created ON indexed_table(status, created_at)",
	}

	for _, indexSQL := range indexes {
		_, err := client.Exec(ctx, indexSQL)
		if err != nil {
			t.Fatalf("Failed to create index: %v", err)
		}
	}

	// Introspect the table
	schema, err := IntrospectTable(ctx, client, client.Dialect(), "indexed_table")
	if err != nil {
		t.Fatalf("IntrospectTable failed: %v", err)
	}

	// Should have 3 indexes (excluding primary key index)
	if len(schema.Indexes) != 3 {
		t.Errorf("Expected 3 indexes, got %d", len(schema.Indexes))
		for i, idx := range schema.Indexes {
			t.Logf("Index %d: %s (unique=%v, fields=%v)", i, idx.Name, idx.Unique, idx.Fields)
		}
	}

	// Create index map for easier lookup
	indexMap := make(map[string]IndexSchema)
	for _, idx := range schema.Indexes {
		indexMap[idx.Name] = idx
	}

	// Test idx_email - unique, single column
	if idx, exists := indexMap["idx_email"]; exists {
		if !idx.Unique {
			t.Error("Index 'idx_email' should be unique")
		}
		if len(idx.Fields) != 1 || idx.Fields[0] != "email" {
			t.Errorf("Index 'idx_email': expected fields [email], got %v", idx.Fields)
		}
	} else {
		t.Error("Index 'idx_email' not found")
	}

	// Test idx_username - non-unique, single column
	if idx, exists := indexMap["idx_username"]; exists {
		if idx.Unique {
			t.Error("Index 'idx_username' should not be unique")
		}
		if len(idx.Fields) != 1 || idx.Fields[0] != "username" {
			t.Errorf("Index 'idx_username': expected fields [username], got %v", idx.Fields)
		}
	} else {
		t.Error("Index 'idx_username' not found")
	}

	// Test idx_status_created - composite index
	if idx, exists := indexMap["idx_status_created"]; exists {
		if idx.Unique {
			t.Error("Index 'idx_status_created' should not be unique")
		}
		if len(idx.Fields) != 2 {
			t.Errorf("Index 'idx_status_created': expected 2 fields, got %d", len(idx.Fields))
		}
		// Check field order
		expectedFields := []string{"status", "created_at"}
		for i, expectedField := range expectedFields {
			if i >= len(idx.Fields) || idx.Fields[i] != expectedField {
				t.Errorf("Index 'idx_status_created': expected field %d to be '%s', got '%s'",
					i, expectedField, idx.Fields[i])
			}
		}
	} else {
		t.Error("Index 'idx_status_created' not found")
	}
}

// TestIntrospectTable_NoIndexes tests a table with no indexes
func TestIntrospectTable_NoIndexes(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Create a table without any indexes (except implicit primary key)
	createTableSQL := `
		CREATE TABLE no_indexes (
			id INTEGER PRIMARY KEY,
			name TEXT
		)
	`

	_, err := client.Exec(ctx, createTableSQL)
	if err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	// Introspect the table
	schema, err := IntrospectTable(ctx, client, client.Dialect(), "no_indexes")
	if err != nil {
		t.Fatalf("IntrospectTable failed: %v", err)
	}

	// Should have no indexes (primary key index is excluded)
	if len(schema.Indexes) != 0 {
		t.Errorf("Expected 0 indexes, got %d", len(schema.Indexes))
	}
}

// TestIntrospectTable_Types tests that different column types are parsed correctly
func TestIntrospectTable_Types(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Create a table with various SQLite column types and type affinities
	createTableSQL := `
		CREATE TABLE type_test (
			col_integer INTEGER,
			col_int INT,
			col_tinyint TINYINT,
			col_smallint SMALLINT,
			col_mediumint MEDIUMINT,
			col_bigint BIGINT,
			col_text TEXT,
			col_varchar VARCHAR(255),
			col_char CHAR(100),
			col_clob CLOB,
			col_blob BLOB,
			col_real REAL,
			col_double DOUBLE,
			col_float FLOAT,
			col_numeric NUMERIC,
			col_decimal DECIMAL(10,2),
			col_boolean BOOLEAN,
			col_datetime DATETIME,
			col_timestamp TIMESTAMP,
			col_empty_type
		)
	`

	_, err := client.Exec(ctx, createTableSQL)
	if err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	// Introspect the table
	schema, err := IntrospectTable(ctx, client, client.Dialect(), "type_test")
	if err != nil {
		t.Fatalf("IntrospectTable failed: %v", err)
	}

	// Expected type mappings based on SQLite type declarations
	// Note: SQLite preserves the original type names, so VARCHAR -> TypeVarchar, BIGINT -> TypeBigInt
	expectedTypes := map[string]FieldType{
		// INTEGER affinity
		"col_integer":   TypeInteger,
		"col_int":       TypeInteger,
		"col_tinyint":   TypeInteger,
		"col_smallint":  TypeInteger,
		"col_mediumint": TypeInteger,
		"col_bigint":    TypeBigInt, // SQLite preserves BIGINT type name
		// TEXT affinity
		"col_text":    TypeText,
		"col_varchar": TypeVarchar, // SQLite preserves VARCHAR type name
		"col_char":    TypeText,
		"col_clob":    TypeText,
		// BLOB affinity
		"col_blob": TypeBytea,
		// REAL affinity (mapped to TEXT for safety)
		"col_real":   TypeText,
		"col_double": TypeText,
		"col_float":  TypeText,
		// NUMERIC affinity (mapped to TEXT for safety)
		"col_numeric": TypeText,
		"col_decimal": TypeText,
		// Special types
		"col_boolean":   TypeBoolean,
		"col_datetime":  TypeTimestampTZ,
		"col_timestamp": TypeTimestampTZ,
		// Empty type defaults to TEXT
		"col_empty_type": TypeText,
	}

	// Create field map
	fieldMap := make(map[string]FieldSchema)
	for _, field := range schema.Fields {
		fieldMap[field.Name] = field
	}

	// Test each column type
	for colName, expectedType := range expectedTypes {
		t.Run(colName, func(t *testing.T) {
			field, exists := fieldMap[colName]
			if !exists {
				t.Fatalf("Field '%s' not found in schema", colName)
			}

			if field.Type != expectedType {
				t.Errorf("Field '%s': expected type %s, got %s", colName, expectedType, field.Type)
			}
		})
	}

	// Verify we got all expected columns
	if len(schema.Fields) != len(expectedTypes) {
		t.Errorf("Expected %d fields, got %d", len(expectedTypes), len(schema.Fields))
	}
}

// TestIntrospectTable_DefaultValues tests that default values are captured
func TestIntrospectTable_DefaultValues(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	createTableSQL := `
		CREATE TABLE defaults_test (
			id INTEGER PRIMARY KEY,
			status TEXT DEFAULT 'active',
			count INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			enabled INTEGER DEFAULT 1
		)
	`

	_, err := client.Exec(ctx, createTableSQL)
	if err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	// Introspect the table
	schema, err := IntrospectTable(ctx, client, client.Dialect(), "defaults_test")
	if err != nil {
		t.Fatalf("IntrospectTable failed: %v", err)
	}

	// Create field map
	fieldMap := make(map[string]FieldSchema)
	for _, field := range schema.Fields {
		fieldMap[field.Name] = field
	}

	// Test that fields with defaults have DefaultValue set
	fieldsWithDefaults := []string{"status", "count", "created_at", "enabled"}
	for _, fieldName := range fieldsWithDefaults {
		t.Run(fieldName, func(t *testing.T) {
			field, exists := fieldMap[fieldName]
			if !exists {
				t.Fatalf("Field '%s' not found in schema", fieldName)
			}

			if field.DefaultValue == "" {
				t.Errorf("Field '%s' should have a default value, but DefaultValue is empty", fieldName)
			}
		})
	}

	// Test that id does not have a default value
	if field, exists := fieldMap["id"]; exists {
		if field.DefaultValue != "" {
			t.Errorf("Field 'id' should not have a default value, got: %s", field.DefaultValue)
		}
	}
}

// TestMapDatabaseTypeToFieldType tests the type mapping function
func TestMapDatabaseTypeToFieldType(t *testing.T) {
	tests := []struct {
		dbType      string
		expectType  FieldType
		expectError bool
	}{
		{"text", TypeText, false},
		{"TEXT", TypeText, false},
		{"varchar", TypeVarchar, false},
		{"character varying", TypeVarchar, false},
		{"integer", TypeInteger, false},
		{"int", TypeInteger, false},
		{"int4", TypeInteger, false},
		{"bigint", TypeBigInt, false},
		{"int8", TypeBigInt, false},
		{"boolean", TypeBoolean, false},
		{"bool", TypeBoolean, false},
		{"timestamptz", TypeTimestampTZ, false},
		{"timestamp with time zone", TypeTimestampTZ, false},
		{"jsonb", TypeJSONB, false},
		{"bytea", TypeBytea, false},
		{"serial", TypeSerial, false},
		{"bigserial", TypeBigSerial, false},
		{"varchar(255)", TypeVarchar, false}, // Test with type modifier
		{"", TypeText, false},                // Empty type defaults to TEXT (SQLite behavior)
		{"unknown_type", "", true},           // Unknown type should error
	}

	for _, tt := range tests {
		t.Run(tt.dbType, func(t *testing.T) {
			fieldType, err := mapDatabaseTypeToFieldType(tt.dbType)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error for type '%s', got nil", tt.dbType)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error for type '%s': %v", tt.dbType, err)
				}
				if fieldType != tt.expectType {
					t.Errorf("Type '%s': expected %s, got %s", tt.dbType, tt.expectType, fieldType)
				}
			}
		})
	}
}

// TestTableExists tests the tableExists helper function
func TestTableExists(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Create a test table
	_, err := client.Exec(ctx, "CREATE TABLE test_exists (id INTEGER PRIMARY KEY)")
	if err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	// Test existing table
	exists, err := tableExists(ctx, client, client.Dialect(), "test_exists")
	if err != nil {
		t.Fatalf("tableExists failed: %v", err)
	}
	if !exists {
		t.Error("Expected table 'test_exists' to exist")
	}

	// Test non-existing table
	exists, err = tableExists(ctx, client, client.Dialect(), "nonexistent")
	if err != nil {
		t.Fatalf("tableExists failed: %v", err)
	}
	if exists {
		t.Error("Expected table 'nonexistent' to not exist")
	}
}

// TestGetUserTables tests the getUserTables helper function
func TestGetUserTables(t *testing.T) {
	client := setupSQLiteClient(t)
	defer client.Close()

	ctx := context.Background()

	// Create test tables
	tableNames := []string{"table_a", "table_b", "table_c"}
	for _, name := range tableNames {
		_, err := client.Exec(ctx, "CREATE TABLE "+name+" (id INTEGER PRIMARY KEY)")
		if err != nil {
			t.Fatalf("Failed to create table %s: %v", name, err)
		}
	}

	// Get user tables
	tables, err := getUserTables(ctx, client, client.Dialect())
	if err != nil {
		t.Fatalf("getUserTables failed: %v", err)
	}

	// Should return tables in alphabetical order
	if len(tables) != 3 {
		t.Errorf("Expected 3 tables, got %d", len(tables))
	}

	// Verify table names
	for i, expected := range tableNames {
		if i >= len(tables) || tables[i] != expected {
			t.Errorf("Expected table[%d] to be '%s', got '%s'", i, expected, tables[i])
		}
	}
}
