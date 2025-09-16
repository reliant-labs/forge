package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// SQLiteTestDialect is a minimal SQLite dialect for testing.
// This duplicates pkg/dialects/sqlite intentionally to avoid an import cycle:
// pkg/orm tests cannot import pkg/dialects/sqlite because that package imports pkg/orm.
type SQLiteTestDialect struct{}

func (d *SQLiteTestDialect) Name() string                 { return "sqlite" }
func (d *SQLiteTestDialect) DriverName() string           { return "sqlite3" }
func (d *SQLiteTestDialect) Placeholder(index int) string { return "?" }
func (d *SQLiteTestDialect) QuoteIdentifier(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
func (d *SQLiteTestDialect) SupportsReturning() bool { return false }

func (d *SQLiteTestDialect) MapFieldType(ft FieldType) string {
	switch ft {
	case TypeText, TypeVarchar:
		return "TEXT"
	case TypeInteger:
		return "INTEGER"
	case TypeBigInt:
		return "INTEGER"
	case TypeBoolean:
		return "INTEGER"
	case TypeTimestampTZ:
		return "DATETIME"
	case TypeJSONB:
		return "TEXT"
	case TypeBytea:
		return "BLOB"
	case TypeSerial, TypeBigSerial:
		return "INTEGER"
	default:
		return "TEXT"
	}
}

func (d *SQLiteTestDialect) OnConflictClause(conflictColumn string, updateColumns []string) string {
	sets := make([]string, len(updateColumns))
	for i, col := range updateColumns {
		sets[i] = fmt.Sprintf("%s = excluded.%s", col, col)
	}
	var updateSet string
	if len(sets) > 0 {
		updateSet = " DO UPDATE SET " + strings.Join(sets, ", ")
	} else {
		updateSet = " DO NOTHING"
	}
	return fmt.Sprintf("ON CONFLICT (%s)%s", conflictColumn, updateSet)
}

func (d *SQLiteTestDialect) TableExistsQuery(tableName string) string {
	return fmt.Sprintf(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='%s'`, tableName)
}

func (d *SQLiteTestDialect) ListTablesQuery() string {
	return `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`
}

func (d *SQLiteTestDialect) IntrospectColumnsQuery(tableName string) string {
	return fmt.Sprintf(`PRAGMA table_info(%s)`, tableName)
}

func (d *SQLiteTestDialect) IntrospectIndexesQuery(tableName string) string {
	return fmt.Sprintf(`PRAGMA index_list(%s)`, tableName)
}

func (d *SQLiteTestDialect) ParseColumnType(dbType string) (FieldType, error) {
	dbType = strings.ToUpper(strings.TrimSpace(dbType))
	if strings.Contains(dbType, "INT") {
		return TypeInteger, nil
	}
	if strings.Contains(dbType, "TEXT") || strings.Contains(dbType, "CHAR") || strings.Contains(dbType, "CLOB") {
		return TypeText, nil
	}
	if strings.Contains(dbType, "BLOB") {
		return TypeBytea, nil
	}
	if strings.Contains(dbType, "DATETIME") || strings.Contains(dbType, "TIMESTAMP") {
		return TypeTimestampTZ, nil
	}
	if strings.Contains(dbType, "BOOL") {
		return TypeBoolean, nil
	}
	if dbType == "" {
		return TypeText, nil
	}
	return "", fmt.Errorf("unknown column type: %s", dbType)
}

func (d *SQLiteTestDialect) ScanColumn(rows *sql.Rows) (IntrospectedColumn, error) {
	var cid int
	var name, dbType string
	var notNull, pk int
	var dfltValue sql.NullString

	err := rows.Scan(&cid, &name, &dbType, &notNull, &dfltValue, &pk)
	if err != nil {
		return IntrospectedColumn{}, err
	}

	fieldType, err := d.ParseColumnType(dbType)
	if err != nil {
		return IntrospectedColumn{}, err
	}

	var defaultValue *string
	if dfltValue.Valid {
		defaultValue = &dfltValue.String
	}

	// In SQLite, primary key columns are implicitly NOT NULL even if the notnull flag is 0
	isPrimaryKey := pk > 0
	isNullable := notNull == 0 && !isPrimaryKey

	return IntrospectedColumn{
		Name:         name,
		Type:         fieldType,
		Nullable:     isNullable,
		DefaultValue: defaultValue,
		IsPrimaryKey: isPrimaryKey,
		IsUnique:     false,
	}, nil
}

func (d *SQLiteTestDialect) ScanIndex(rows *sql.Rows) (indexName, columnName string, isUnique bool, err error) {
	var seq, unique, partial int
	var name, origin string

	err = rows.Scan(&seq, &name, &unique, &origin, &partial)
	if err != nil {
		return "", "", false, err
	}

	return name, "", unique == 1, nil
}

// init registers the test SQLite dialect.
func init() {
	// Only register if not already registered
	if _, err := GetDialect("sqlite"); err != nil {
		RegisterDialect(&SQLiteTestDialect{})
	}
}

// setupTestDB creates an in-memory SQLite database for testing
func setupTestDB(t *testing.T) *Client {
	t.Helper()

	// Create a temporary file database
	// We use a file database instead of :memory: because the migration code
	// calls m.client.Exec() from within transactions, which causes locking issues
	// with in-memory databases in shared cache mode
	tmpfile := fmt.Sprintf("/tmp/test_migration_%d.db", time.Now().UnixNano())

	db, err := sql.Open("sqlite3", tmpfile)
	if err != nil {
		t.Fatalf("failed to open SQLite database: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
		os.Remove(tmpfile)
	})

	client, err := NewClientWithDB(db, "sqlite")
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	return client
}

// TestPairedMigration_SchemaOnly tests paired migration with only schema changes
func TestPairedMigration_SchemaOnly(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Define schema for a users table
	usersSchema := TableSchema{
		Name: "users",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "name", Type: TypeText, NotNull: true},
			{Name: "email", Type: TypeText, NotNull: true},
		},
	}

	// Register paired migration with schema changes only
	paired := &PairedMigration{
		Version:       "20240101_001",
		Description:   "Create users table",
		SchemaChanges: []TableSchema{usersSchema},
	}

	err := mgr.RegisterPairedMigration(paired)
	if err != nil {
		t.Fatalf("failed to register paired migration: %v", err)
	}

	// Run migration
	err = mgr.Migrate(ctx)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Verify table was created
	var count int
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='users'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check table existence: %v", err)
	}
	if count != 1 {
		t.Errorf("expected users table to exist, got count: %d", count)
	}

	// Verify migration was recorded
	var version string
	err = client.QueryRow(ctx,
		"SELECT version FROM schema_migrations WHERE version = ?",
		"20240101_001",
	).Scan(&version)
	if err != nil {
		t.Fatalf("migration not recorded: %v", err)
	}
	if version != "20240101_001" {
		t.Errorf("expected version 20240101_001, got: %s", version)
	}

	// Verify columns exist
	rows, err := client.Query(ctx, "PRAGMA table_info(users)")
	if err != nil {
		t.Fatalf("failed to get table info: %v", err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("failed to scan column: %v", err)
		}
		columns[name] = true
	}

	expectedColumns := []string{"id", "name", "email"}
	for _, col := range expectedColumns {
		if !columns[col] {
			t.Errorf("expected column %s to exist", col)
		}
	}
}

// TestPairedMigration_SchemaAndData tests paired migration with both schema and data changes
func TestPairedMigration_SchemaAndData(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Define schema for a products table
	productsSchema := TableSchema{
		Name: "products",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "name", Type: TypeText, NotNull: true},
			{Name: "price", Type: TypeInteger, NotNull: true},
		},
	}

	// Create paired migration with schema and data changes
	paired := &PairedMigration{
		Version:       "20240101_002",
		Description:   "Create products table and seed data",
		SchemaChanges: []TableSchema{productsSchema},
		DataMigration: &Migration{
			Up: func(ctx context.Context, db Context) error {
				// Insert seed data
				_, err := db.Exec(ctx, "INSERT INTO products (id, name, price) VALUES (1, 'Widget', 100)")
				if err != nil {
					return err
				}
				_, err = db.Exec(ctx, "INSERT INTO products (id, name, price) VALUES (2, 'Gadget', 200)")
				return err
			},
		},
	}

	err := mgr.RegisterPairedMigration(paired)
	if err != nil {
		t.Fatalf("failed to register paired migration: %v", err)
	}

	// Run migration
	err = mgr.Migrate(ctx)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Verify table was created
	var count int
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM products",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query products: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 products, got: %d", count)
	}

	// Verify data was inserted correctly
	var name string
	var price int
	err = client.QueryRow(ctx,
		"SELECT name, price FROM products WHERE id = 1",
	).Scan(&name, &price)
	if err != nil {
		t.Fatalf("failed to query product: %v", err)
	}
	if name != "Widget" || price != 100 {
		t.Errorf("expected Widget with price 100, got: %s with price %d", name, price)
	}
}

// TestPairedMigration_ExecutionOrder verifies schema runs before data
func TestPairedMigration_ExecutionOrder(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Track execution order
	var executionLog []string

	// Define schema
	ordersSchema := TableSchema{
		Name: "orders",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "total", Type: TypeInteger, NotNull: true},
		},
	}

	// Create paired migration that verifies table exists during data migration
	paired := &PairedMigration{
		Version:       "20240101_003",
		Description:   "Create orders table with execution order validation",
		SchemaChanges: []TableSchema{ordersSchema},
		DataMigration: &Migration{
			Up: func(ctx context.Context, db Context) error {
				// This should only succeed if schema was already created
				_, err := db.Exec(ctx, "INSERT INTO orders (id, total) VALUES (1, 500)")
				if err != nil {
					return fmt.Errorf("data migration failed (schema likely not created first): %w", err)
				}
				executionLog = append(executionLog, "data")
				return nil
			},
		},
	}

	err := mgr.RegisterPairedMigration(paired)
	if err != nil {
		t.Fatalf("failed to register paired migration: %v", err)
	}

	// Run migration
	err = mgr.Migrate(ctx)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Verify execution order
	if len(executionLog) != 1 || executionLog[0] != "data" {
		t.Errorf("expected data migration to execute after schema, got log: %v", executionLog)
	}

	// Verify data was inserted (proves schema was created first)
	var total int
	err = client.QueryRow(ctx, "SELECT total FROM orders WHERE id = 1").Scan(&total)
	if err != nil {
		t.Fatalf("failed to query order: %v", err)
	}
	if total != 500 {
		t.Errorf("expected total 500, got: %d", total)
	}
}

// TestGenerateMigration tests migration generation from schemas
func TestGenerateMigration(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Create initial table
	_, err := client.Exec(ctx, `
		CREATE TABLE customers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("failed to create initial table: %v", err)
	}

	// Define updated schema with new column
	updatedSchema := TableSchema{
		Name: "customers",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "name", Type: TypeText, NotNull: true},
			{Name: "email", Type: TypeText, NotNull: false}, // New column
		},
	}

	// Generate migration
	migration, err := mgr.GenerateMigration(ctx, []TableSchema{updatedSchema})
	if err != nil {
		t.Fatalf("failed to generate migration: %v", err)
	}

	// Verify migration was generated
	if migration == nil {
		t.Fatal("expected migration to be generated")
	}
	if migration.Version == "" {
		t.Error("expected migration to have a version")
	}
	if !strings.HasPrefix(migration.Version, "auto_") {
		t.Errorf("expected auto-generated version, got: %s", migration.Version)
	}

	// Register and run the generated migration
	err = mgr.Register(migration)
	if err != nil {
		t.Fatalf("failed to register generated migration: %v", err)
	}

	err = mgr.Migrate(ctx)
	if err != nil {
		t.Fatalf("failed to run generated migration: %v", err)
	}

	// Verify the new column exists
	rows, err := client.Query(ctx, "PRAGMA table_info(customers)")
	if err != nil {
		t.Fatalf("failed to get table info: %v", err)
	}
	defer rows.Close()

	hasEmail := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("failed to scan column: %v", err)
		}
		if name == "email" {
			hasEmail = true
		}
	}

	if !hasEmail {
		t.Error("expected email column to be added")
	}
}

// TestPlanMigration tests dry-run functionality
func TestPlanMigration(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Create initial table
	_, err := client.Exec(ctx, `
		CREATE TABLE inventory (
			id INTEGER PRIMARY KEY,
			item TEXT NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("failed to create initial table: %v", err)
	}

	// Define updated schema
	updatedSchema := TableSchema{
		Name: "inventory",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "item", Type: TypeText, NotNull: true},
			{Name: "quantity", Type: TypeInteger, NotNull: true},
		},
	}

	// Plan migration (dry-run)
	plan, err := mgr.PlanMigration(ctx, []TableSchema{updatedSchema})
	if err != nil {
		t.Fatalf("failed to plan migration: %v", err)
	}

	// Verify plan contains SQL
	if plan == "" {
		t.Fatal("expected plan to contain SQL")
	}
	if !strings.Contains(plan, "Migration Plan") {
		t.Error("expected plan to contain header")
	}
	if !strings.Contains(plan, "ADD COLUMN") || !strings.Contains(plan, "quantity") {
		t.Error("expected plan to contain ADD COLUMN statement for quantity")
	}

	// Verify table was NOT modified (dry-run should not change anything)
	rows, err := client.Query(ctx, "PRAGMA table_info(inventory)")
	if err != nil {
		t.Fatalf("failed to get table info: %v", err)
	}
	defer rows.Close()

	columnCount := 0
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("failed to scan column: %v", err)
		}
		columnCount++
	}

	if columnCount != 2 {
		t.Errorf("expected table to remain unchanged with 2 columns, got: %d", columnCount)
	}

	// Test plan with no changes
	planNoChanges, err := mgr.PlanMigration(ctx, []TableSchema{
		{
			Name: "inventory",
			Fields: []FieldSchema{
				{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
				{Name: "item", Type: TypeText, NotNull: true},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to plan migration with no changes: %v", err)
	}

	if !strings.Contains(planNoChanges, "No schema changes detected") {
		t.Error("expected plan to indicate no changes")
	}
}

// TestAutoMigrate tests automatic migration
func TestAutoMigrate(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Define schema for new table
	categoriesSchema := TableSchema{
		Name: "categories",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "name", Type: TypeText, NotNull: true},
		},
	}

	// Run auto-migrate
	err := mgr.AutoMigrate(ctx, []TableSchema{categoriesSchema})
	if err != nil {
		t.Fatalf("auto-migrate failed: %v", err)
	}

	// Verify table was created
	var count int
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='categories'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check table existence: %v", err)
	}
	if count != 1 {
		t.Errorf("expected categories table to exist, got count: %d", count)
	}

	// Verify migration was recorded
	var version string
	err = client.QueryRow(ctx,
		"SELECT version FROM schema_migrations WHERE version LIKE 'auto_%'",
	).Scan(&version)
	if err != nil {
		t.Fatalf("auto-migration not recorded: %v", err)
	}
	if !strings.HasPrefix(version, "auto_") {
		t.Errorf("expected auto-generated version, got: %s", version)
	}

	// Run auto-migrate again with no changes
	err = mgr.AutoMigrate(ctx, []TableSchema{categoriesSchema})
	if err != nil {
		t.Fatalf("auto-migrate with no changes failed: %v", err)
	}

	// Add a column using auto-migrate
	updatedSchema := TableSchema{
		Name: "categories",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "name", Type: TypeText, NotNull: true},
			{Name: "description", Type: TypeText, NotNull: false},
		},
	}

	err = mgr.AutoMigrate(ctx, []TableSchema{updatedSchema})
	if err != nil {
		t.Fatalf("auto-migrate with new column failed: %v", err)
	}

	// Verify new column exists
	rows, err := client.Query(ctx, "PRAGMA table_info(categories)")
	if err != nil {
		t.Fatalf("failed to get table info: %v", err)
	}
	defer rows.Close()

	hasDescription := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("failed to scan column: %v", err)
		}
		if name == "description" {
			hasDescription = true
		}
	}

	if !hasDescription {
		t.Error("expected description column to be added by auto-migrate")
	}
}

// TestRegisterPairedMigration_ConflictingVersions tests error on duplicate versions
func TestRegisterPairedMigration_ConflictingVersions(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	mgr := NewMigrationManager(client)

	// Register first migration
	schema := TableSchema{
		Name: "test_table",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
		},
	}

	paired1 := &PairedMigration{
		Version:       "20240101_001",
		Description:   "First migration",
		SchemaChanges: []TableSchema{schema},
	}

	err := mgr.RegisterPairedMigration(paired1)
	if err != nil {
		t.Fatalf("failed to register first paired migration: %v", err)
	}

	// Try to register second migration with same version
	paired2 := &PairedMigration{
		Version:       "20240101_001",
		Description:   "Second migration",
		SchemaChanges: []TableSchema{schema},
	}

	err = mgr.RegisterPairedMigration(paired2)
	if err == nil {
		t.Fatal("expected error when registering duplicate version")
	}

	expectedError := "migration version 20240101_001 already registered"
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("expected error to contain '%s', got: %v", expectedError, err)
	}

	// Test conflict with regular migration
	regularMigration := &Migration{
		Version:     "20240101_002",
		Description: "Regular migration",
		Up: func(ctx context.Context, db Context) error {
			return nil
		},
	}

	err = mgr.Register(regularMigration)
	if err != nil {
		t.Fatalf("failed to register regular migration: %v", err)
	}

	// Try to register paired migration with same version as regular migration
	paired3 := &PairedMigration{
		Version:       "20240101_002",
		Description:   "Conflicting paired migration",
		SchemaChanges: []TableSchema{schema},
	}

	err = mgr.RegisterPairedMigration(paired3)
	if err == nil {
		t.Fatal("expected error when paired migration conflicts with regular migration")
	}

	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("expected error about conflicting version, got: %v", err)
	}
}

// TestMigrationManager_Transaction tests that migrations run in transactions and rollback on failure
func TestMigrationManager_Transaction(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Register a migration that will fail
	failingMigration := &Migration{
		Version:     "20240101_fail",
		Description: "Failing migration",
		Up: func(ctx context.Context, db Context) error {
			// Create a table
			_, err := db.Exec(ctx, "CREATE TABLE temp_table (id INTEGER PRIMARY KEY)")
			if err != nil {
				return err
			}
			// Then fail
			return errors.New("intentional failure")
		},
	}

	err := mgr.Register(failingMigration)
	if err != nil {
		t.Fatalf("failed to register migration: %v", err)
	}

	// Run migration (should fail)
	err = mgr.Migrate(ctx)
	if err == nil {
		t.Fatal("expected migration to fail")
	}

	if !strings.Contains(err.Error(), "intentional failure") {
		t.Errorf("expected error message to contain 'intentional failure', got: %v", err)
	}

	// Verify table was NOT created (transaction rolled back)
	var count int
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='temp_table'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check table existence: %v", err)
	}
	if count != 0 {
		t.Error("expected temp_table to not exist after rollback")
	}

	// Verify migration was NOT recorded
	err = client.QueryRow(ctx,
		"SELECT version FROM schema_migrations WHERE version = ?",
		"20240101_fail",
	).Scan(&count)
	if err != sql.ErrNoRows {
		t.Error("expected migration to not be recorded after failure")
	}

	// Test paired migration rollback
	mgr2 := NewMigrationManager(client)

	schema := TableSchema{
		Name: "rollback_test",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "value", Type: TypeText, NotNull: true},
		},
	}

	pairedFailing := &PairedMigration{
		Version:       "20240101_paired_fail",
		Description:   "Paired migration that fails in data step",
		SchemaChanges: []TableSchema{schema},
		DataMigration: &Migration{
			Up: func(ctx context.Context, db Context) error {
				// Insert data
				_, err := db.Exec(ctx, "INSERT INTO rollback_test (id, value) VALUES (1, 'test')")
				if err != nil {
					return err
				}
				// Then fail
				return errors.New("data migration failure")
			},
		},
	}

	err = mgr2.RegisterPairedMigration(pairedFailing)
	if err != nil {
		t.Fatalf("failed to register paired migration: %v", err)
	}

	err = mgr2.Migrate(ctx)
	if err == nil {
		t.Fatal("expected paired migration to fail")
	}

	// Verify table was NOT created (entire transaction rolled back)
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='rollback_test'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check table existence: %v", err)
	}
	if count != 0 {
		t.Error("expected rollback_test table to not exist after rollback")
	}

	// Verify migration was NOT recorded
	err = client.QueryRow(ctx,
		"SELECT version FROM schema_migrations WHERE version = ?",
		"20240101_paired_fail",
	).Scan(&count)
	if err != sql.ErrNoRows {
		t.Error("expected paired migration to not be recorded after failure")
	}
}

// TestPairedMigration_ValidationErrors tests validation of paired migrations
func TestPairedMigration_ValidationErrors(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	mgr := NewMigrationManager(client)

	tests := []struct {
		name        string
		migration   *PairedMigration
		expectedErr string
	}{
		{
			name:        "nil migration",
			migration:   nil,
			expectedErr: "paired migration cannot be nil",
		},
		{
			name: "empty version",
			migration: &PairedMigration{
				Version:     "",
				Description: "Test",
				SchemaChanges: []TableSchema{
					{Name: "test", Fields: []FieldSchema{{Name: "id", Type: TypeInteger}}},
				},
			},
			expectedErr: "version cannot be empty",
		},
		{
			name: "no schema or data",
			migration: &PairedMigration{
				Version:     "20240101_001",
				Description: "Test",
			},
			expectedErr: "must have either schema changes or data migration",
		},
		{
			name: "conflicting data migration version",
			migration: &PairedMigration{
				Version:     "20240101_001",
				Description: "Test",
				SchemaChanges: []TableSchema{
					{Name: "test", Fields: []FieldSchema{{Name: "id", Type: TypeInteger}}},
				},
				DataMigration: &Migration{
					Version: "20240101_002", // Different version
					Up:      func(ctx context.Context, db Context) error { return nil },
				},
			},
			expectedErr: "conflicts with data migration version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mgr.RegisterPairedMigration(tt.migration)
			if err == nil {
				t.Fatal("expected error but got nil")
			}
			if !strings.Contains(err.Error(), tt.expectedErr) {
				t.Errorf("expected error containing '%s', got: %v", tt.expectedErr, err)
			}
		})
	}
}

// TestMigrationStatus tests the Status method
func TestMigrationStatus(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Register multiple migrations
	migrations := []*Migration{
		{
			Version:     "20240101_001",
			Description: "First migration",
			Up:          func(ctx context.Context, db Context) error { return nil },
		},
		{
			Version:     "20240101_002",
			Description: "Second migration",
			Up:          func(ctx context.Context, db Context) error { return nil },
		},
		{
			Version:     "20240101_003",
			Description: "Third migration",
			Up:          func(ctx context.Context, db Context) error { return nil },
		},
	}

	for _, m := range migrations {
		if err := mgr.Register(m); err != nil {
			t.Fatalf("failed to register migration: %v", err)
		}
	}

	// Run only first two migrations
	if err := mgr.MigrateTo(ctx, "20240101_002"); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Get status
	statuses, err := mgr.Status(ctx)
	if err != nil {
		t.Fatalf("failed to get status: %v", err)
	}

	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got: %d", len(statuses))
	}

	// Verify first two are applied
	for i := 0; i < 2; i++ {
		if !statuses[i].Applied {
			t.Errorf("expected migration %s to be applied", statuses[i].Version)
		}
		if statuses[i].AppliedAt.IsZero() {
			t.Errorf("expected migration %s to have applied time", statuses[i].Version)
		}
	}

	// Verify third is not applied
	if statuses[2].Applied {
		t.Errorf("expected migration %s to not be applied", statuses[2].Version)
	}
	if !statuses[2].AppliedAt.IsZero() {
		t.Errorf("expected migration %s to have zero applied time", statuses[2].Version)
	}
}

// TestMigrationRollback tests the Rollback functionality
func TestMigrationRollback(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Register migrations with down functions
	migration1 := &Migration{
		Version:     "20240101_001",
		Description: "Create test1 table",
		Up: func(ctx context.Context, db Context) error {
			_, err := db.Exec(ctx, "CREATE TABLE test1 (id INTEGER PRIMARY KEY)")
			return err
		},
		Down: func(ctx context.Context, db Context) error {
			_, err := db.Exec(ctx, "DROP TABLE test1")
			return err
		},
	}

	migration2 := &Migration{
		Version:     "20240101_002",
		Description: "Create test2 table",
		Up: func(ctx context.Context, db Context) error {
			_, err := db.Exec(ctx, "CREATE TABLE test2 (id INTEGER PRIMARY KEY)")
			return err
		},
		Down: func(ctx context.Context, db Context) error {
			_, err := db.Exec(ctx, "DROP TABLE test2")
			return err
		},
	}

	if err := mgr.Register(migration1); err != nil {
		t.Fatalf("failed to register migration1: %v", err)
	}
	if err := mgr.Register(migration2); err != nil {
		t.Fatalf("failed to register migration2: %v", err)
	}

	// Run migrations
	if err := mgr.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// Verify both tables exist
	var count int
	err := client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND (name='test1' OR name='test2')",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check tables: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 tables, got: %d", count)
	}

	// Rollback one migration
	if err := mgr.Rollback(ctx, 1); err != nil {
		t.Fatalf("failed to rollback: %v", err)
	}

	// Verify test2 was dropped but test1 still exists
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='test1'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check test1: %v", err)
	}
	if count != 1 {
		t.Error("expected test1 to still exist")
	}

	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='test2'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check test2: %v", err)
	}
	if count != 0 {
		t.Error("expected test2 to be dropped")
	}

	// Verify migration record was removed
	var version string
	err = client.QueryRow(ctx,
		"SELECT version FROM schema_migrations WHERE version = ?",
		"20240101_002",
	).Scan(&version)
	if err != sql.ErrNoRows {
		t.Error("expected migration record to be removed")
	}
}

// TestGenerateMigration_NoChanges tests that GenerateMigration returns error when no changes
func TestGenerateMigration_NoChanges(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Create a table
	schema := TableSchema{
		Name: "existing_table",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
			{Name: "name", Type: TypeText, NotNull: true},
		},
	}

	_, err := client.Exec(ctx, GenerateCreateTableSQL(schema))
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	// Try to generate migration with same schema
	migration, err := mgr.GenerateMigration(ctx, []TableSchema{schema})
	if err == nil {
		t.Fatal("expected error when generating migration with no changes")
	}

	if migration != nil {
		t.Error("expected nil migration when no changes detected")
	}

	expectedErr := "no schema changes detected"
	if !strings.Contains(err.Error(), expectedErr) {
		t.Errorf("expected error containing '%s', got: %v", expectedErr, err)
	}
}

// TestAutoMigrate_EmptySchemas tests AutoMigrate with empty schema list
func TestAutoMigrate_EmptySchemas(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	err := mgr.AutoMigrate(ctx, []TableSchema{})
	if err == nil {
		t.Fatal("expected error when auto-migrating with empty schemas")
	}

	expectedErr := "no schemas provided"
	if !strings.Contains(err.Error(), expectedErr) {
		t.Errorf("expected error containing '%s', got: %v", expectedErr, err)
	}
}

// TestPairedMigration_DataMigrationVersionOverride tests that data migration version is overridden
func TestPairedMigration_DataMigrationVersionOverride(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	schema := TableSchema{
		Name: "version_test",
		Fields: []FieldSchema{
			{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
		},
	}

	// Create paired migration where data migration has empty version
	paired := &PairedMigration{
		Version:       "20240101_100",
		Description:   "Test version override",
		SchemaChanges: []TableSchema{schema},
		DataMigration: &Migration{
			Version: "", // Should be overridden
			Up:      func(ctx context.Context, db Context) error { return nil },
		},
	}

	err := mgr.RegisterPairedMigration(paired)
	if err != nil {
		t.Fatalf("failed to register paired migration: %v", err)
	}

	// Run migration
	err = mgr.Migrate(ctx)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Verify only one migration record with the paired version
	var count int
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?",
		"20240101_100",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 migration record, got: %d", count)
	}
}

// TestMigrationManager_CustomTableName tests using a custom migration table name
func TestMigrationManager_CustomTableName(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)
	mgr.SetTableName("custom_migrations")

	migration := &Migration{
		Version:     "20240101_001",
		Description: "Test migration",
		Up:          func(ctx context.Context, db Context) error { return nil },
	}

	err := mgr.Register(migration)
	if err != nil {
		t.Fatalf("failed to register migration: %v", err)
	}

	err = mgr.Migrate(ctx)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Verify custom table was created
	var count int
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='custom_migrations'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to check custom table: %v", err)
	}
	if count != 1 {
		t.Error("expected custom_migrations table to exist")
	}

	// Verify migration was recorded in custom table
	var version string
	err = client.QueryRow(ctx,
		"SELECT version FROM custom_migrations WHERE version = ?",
		"20240101_001",
	).Scan(&version)
	if err != nil {
		t.Fatalf("migration not recorded in custom table: %v", err)
	}
	if version != "20240101_001" {
		t.Errorf("expected version 20240101_001, got: %s", version)
	}
}

// TestPairedMigration_SchemaOnly_MultipleVersions tests multiple paired migrations
func TestPairedMigration_MultipleVersions(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := context.Background()
	mgr := NewMigrationManager(client)

	// Register multiple paired migrations
	for i := 1; i <= 3; i++ {
		schema := TableSchema{
			Name: fmt.Sprintf("table_%d", i),
			Fields: []FieldSchema{
				{Name: "id", Type: TypeInteger, PrimaryKey: true, NotNull: true},
				{Name: "value", Type: TypeText, NotNull: true},
			},
		}

		paired := &PairedMigration{
			Version:       fmt.Sprintf("2024010100%d", i),
			Description:   fmt.Sprintf("Create table_%d", i),
			SchemaChanges: []TableSchema{schema},
		}

		err := mgr.RegisterPairedMigration(paired)
		if err != nil {
			t.Fatalf("failed to register paired migration %d: %v", i, err)
		}
	}

	// Run all migrations
	err := mgr.Migrate(ctx)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Verify all tables were created
	for i := 1; i <= 3; i++ {
		var count int
		tableName := fmt.Sprintf("table_%d", i)
		err := client.QueryRow(ctx,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?",
			tableName,
		).Scan(&count)
		if err != nil {
			t.Fatalf("failed to check table %s: %v", tableName, err)
		}
		if count != 1 {
			t.Errorf("expected table %s to exist", tableName)
		}
	}

	// Verify all migrations were recorded
	var migrationCount int
	err = client.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE version LIKE '2024010100%'",
	).Scan(&migrationCount)
	if err != nil {
		t.Fatalf("failed to count migrations: %v", err)
	}
	if migrationCount != 3 {
		t.Errorf("expected 3 migrations to be recorded, got: %d", migrationCount)
	}
}