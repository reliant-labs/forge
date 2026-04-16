package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMigrationsForSchema(t *testing.T) {
	dir := t.TempDir()

	// Write two migration files.
	migration1 := `CREATE TABLE users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`
	migration2 := `ALTER TABLE users ADD COLUMN display_name TEXT DEFAULT '';
CREATE TABLE posts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

	os.WriteFile(filepath.Join(dir, "001_create_users.up.sql"), []byte(migration1), 0644)
	os.WriteFile(filepath.Join(dir, "002_add_posts.up.sql"), []byte(migration2), 0644)

	tables, err := ParseMigrationsForSchema(dir)
	if err != nil {
		t.Fatalf("ParseMigrationsForSchema() error = %v", err)
	}

	if len(tables) != 2 {
		t.Fatalf("expected 2 tables, got %d", len(tables))
	}

	// Tables should be sorted by name.
	if tables[0].Name != "posts" {
		t.Fatalf("expected first table to be 'posts', got %q", tables[0].Name)
	}
	if tables[1].Name != "users" {
		t.Fatalf("expected second table to be 'users', got %q", tables[1].Name)
	}

	// Users should have 4 columns (3 from CREATE + 1 from ALTER).
	if len(tables[1].Columns) != 4 {
		t.Fatalf("expected 4 columns for users, got %d: %+v", len(tables[1].Columns), tables[1].Columns)
	}

	// Check that display_name was added by ALTER TABLE.
	found := false
	for _, c := range tables[1].Columns {
		if c.Name == "display_name" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected display_name column from ALTER TABLE")
	}
}

func TestParseMigrationsForSchema_DropTable(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "001_create.up.sql"), []byte("CREATE TABLE temp (id UUID PRIMARY KEY);"), 0644)
	os.WriteFile(filepath.Join(dir, "002_drop.up.sql"), []byte("DROP TABLE temp;"), 0644)

	tables, err := ParseMigrationsForSchema(dir)
	if err != nil {
		t.Fatalf("ParseMigrationsForSchema() error = %v", err)
	}

	if len(tables) != 0 {
		t.Fatalf("expected 0 tables after DROP, got %d", len(tables))
	}
}

func TestParseMigrationsForSchema_NonexistentDir(t *testing.T) {
	tables, err := ParseMigrationsForSchema("/nonexistent/path")
	if err != nil {
		t.Fatalf("expected no error for nonexistent dir, got %v", err)
	}
	if tables != nil {
		t.Fatalf("expected nil tables for nonexistent dir, got %v", tables)
	}
}

func TestScanProtoModels(t *testing.T) {
	dir := t.TempDir()

	protoContent := `syntax = "proto3";

package example.v1;

message UserPreference {
  string user_id = 1;
  string key = 2;
  string value = 3;
}

message AuditLog {
  string id = 1;
  string action = 2;
  google.protobuf.Timestamp created_at = 3;
}
`
	os.WriteFile(filepath.Join(dir, "models.proto"), []byte(protoContent), 0644)

	models, err := ScanProtoModels(dir)
	if err != nil {
		t.Fatalf("ScanProtoModels() error = %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	if models[0].Name != "UserPreference" {
		t.Fatalf("expected first model to be 'UserPreference', got %q", models[0].Name)
	}
	if len(models[0].Fields) != 3 {
		t.Fatalf("expected 3 fields in UserPreference, got %d", len(models[0].Fields))
	}

	if models[1].Name != "AuditLog" {
		t.Fatalf("expected second model to be 'AuditLog', got %q", models[1].Name)
	}
}

func TestScanProtoModels_NonexistentDir(t *testing.T) {
	models, err := ScanProtoModels("/nonexistent/path")
	if err != nil {
		t.Fatalf("expected no error for nonexistent dir, got %v", err)
	}
	if models != nil {
		t.Fatalf("expected nil models for nonexistent dir, got %v", models)
	}
}

func TestComputeSchemaDiff(t *testing.T) {
	tables := []ParsedTable{
		{
			Name: "users",
			Columns: []ParsedColumn{
				{Name: "id", Type: "UUID"},
				{Name: "email", Type: "TEXT"},
			},
		},
	}

	models := []ProtoModel{
		{
			Name: "User",
			Fields: []ProtoModelField{
				{Name: "id", ProtoType: "string", Number: 1},
				{Name: "email", ProtoType: "string", Number: 2},
				{Name: "display_name", ProtoType: "string", Number: 3},
			},
		},
		{
			Name: "UserPreference",
			Fields: []ProtoModelField{
				{Name: "user_id", ProtoType: "string", Number: 1},
				{Name: "key", ProtoType: "string", Number: 2},
			},
		},
	}

	diffs := ComputeSchemaDiff(tables, models)

	if len(diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %d: %+v", len(diffs), diffs)
	}

	// First diff: display_name is in proto but not in schema.
	if diffs[0].Kind != "NEW_COLUMN" {
		t.Fatalf("expected NEW_COLUMN, got %q", diffs[0].Kind)
	}
	if !strings.Contains(diffs[0].Message, "display_name") {
		t.Fatalf("expected diff to mention display_name, got %q", diffs[0].Message)
	}

	// Second diff: UserPreference has no table.
	if diffs[1].Kind != "NEW_TABLE" {
		t.Fatalf("expected NEW_TABLE, got %q", diffs[1].Kind)
	}
	if !strings.Contains(diffs[1].Message, "UserPreference") {
		t.Fatalf("expected diff to mention UserPreference, got %q", diffs[1].Message)
	}
}

func TestProtoToCreateTable(t *testing.T) {
	model := ProtoModel{
		Name: "UserPreference",
		Fields: []ProtoModelField{
			{Name: "user_id", ProtoType: "string", Number: 1},
			{Name: "key", ProtoType: "string", Number: 2},
			{Name: "value", ProtoType: "string", Number: 3},
		},
	}

	sql := ProtoToCreateTable(model)

	if !strings.Contains(sql, "CREATE TABLE user_preferences") {
		t.Fatalf("expected table name user_preferences, got:\n%s", sql)
	}
	if !strings.Contains(sql, "id UUID PRIMARY KEY DEFAULT gen_random_uuid()") {
		t.Fatalf("expected id column, got:\n%s", sql)
	}
	if !strings.Contains(sql, "user_id TEXT NOT NULL") {
		t.Fatalf("expected user_id TEXT NOT NULL, got:\n%s", sql)
	}
	if !strings.Contains(sql, "created_at TIMESTAMPTZ NOT NULL DEFAULT now()") {
		t.Fatalf("expected created_at, got:\n%s", sql)
	}
	if !strings.Contains(sql, "updated_at TIMESTAMPTZ NOT NULL DEFAULT now()") {
		t.Fatalf("expected updated_at, got:\n%s", sql)
	}
}

func TestProtoToCreateTable_TypeMapping(t *testing.T) {
	model := ProtoModel{
		Name: "Widget",
		Fields: []ProtoModelField{
			{Name: "count", ProtoType: "int32", Number: 1},
			{Name: "big_count", ProtoType: "int64", Number: 2},
			{Name: "active", ProtoType: "bool", Number: 3},
			{Name: "score", ProtoType: "double", Number: 4},
			{Name: "event_time", ProtoType: "google.protobuf.Timestamp", Number: 5},
			{Name: "data", ProtoType: "bytes", Number: 6},
		},
	}

	sql := ProtoToCreateTable(model)

	expectations := map[string]string{
		"count":      "INTEGER",
		"big_count":  "BIGINT",
		"active":     "BOOLEAN",
		"score":      "DOUBLE PRECISION",
		"event_time": "TIMESTAMPTZ",
		"data":       "BYTEA",
	}

	for col, expectedType := range expectations {
		if !strings.Contains(sql, col+" "+expectedType) {
			t.Errorf("expected %s %s in SQL, got:\n%s", col, expectedType, sql)
		}
	}
}

func TestProtoToCreateTable_SkipsManagedColumns(t *testing.T) {
	model := ProtoModel{
		Name: "Thing",
		Fields: []ProtoModelField{
			{Name: "id", ProtoType: "string", Number: 1},
			{Name: "name", ProtoType: "string", Number: 2},
			{Name: "created_at", ProtoType: "google.protobuf.Timestamp", Number: 3},
			{Name: "updated_at", ProtoType: "google.protobuf.Timestamp", Number: 4},
		},
	}

	sql := ProtoToCreateTable(model)

	// Count occurrences of "id" as a column def — should be exactly 1 (the managed one).
	if strings.Count(sql, "id UUID PRIMARY KEY") != 1 {
		t.Fatalf("expected exactly one id column definition, got:\n%s", sql)
	}
	// name should be present.
	if !strings.Contains(sql, "name TEXT NOT NULL") {
		t.Fatalf("expected name column, got:\n%s", sql)
	}
}

func TestGenerateContextComment(t *testing.T) {
	ctx := &MigrationContext{
		MigrationName: "add_preferences",
		CreatedAt:     time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC),
		ParsedTables: []ParsedTable{
			{
				Name: "users",
				Columns: []ParsedColumn{
					{Name: "id", Type: "UUID", Constraint: "PRIMARY KEY DEFAULT gen_random_uuid()"},
					{Name: "email", Type: "TEXT", Constraint: "NOT NULL UNIQUE"},
				},
			},
		},
		ProtoModels: []ProtoModel{
			{
				Name: "UserPreference",
				Fields: []ProtoModelField{
					{Name: "user_id", ProtoType: "string", Number: 1},
					{Name: "key", ProtoType: "string", Number: 2},
				},
			},
		},
		PreviousMigration: &PreviousMigrationInfo{
			Filename: "001_create_users.up.sql",
			Content:  "CREATE TABLE users (id UUID PRIMARY KEY);",
		},
		MigrationHistory: []string{"001_create_users.up.sql"},
		SchemaDiffs: []SchemaDiffEntry{
			{Kind: "NEW_TABLE", Message: "UserPreference has no corresponding table"},
		},
	}

	comment := GenerateContextComment(ctx)

	checks := []string{
		"-- Migration: add_preferences",
		"-- Created: 2025-01-15T10:30:00Z",
		"-- === CURRENT SCHEMA (from existing migrations) ===",
		"-- TABLE users (",
		"--   id UUID",
		"--   email TEXT",
		"-- === PROTO MODELS (potential new tables/columns) ===",
		"-- message UserPreference {",
		"--   string user_id = 1;",
		"-- === PREVIOUS MIGRATION (001_create_users.up.sql) ===",
		"-- CREATE TABLE users",
		"-- === MIGRATION HISTORY ===",
		"-- 001_create_users.up.sql",
		"-- === DIFF (proto vs schema) ===",
		"-- NEW_TABLE: UserPreference",
		"-- Write your migration SQL below:",
	}

	for _, check := range checks {
		if !strings.Contains(comment, check) {
			t.Errorf("expected context comment to contain %q.\nGot:\n%s", check, comment)
		}
	}
}

func TestGetPreviousMigration(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "001_first.up.sql"), []byte("CREATE TABLE a (id INT);"), 0644)
	os.WriteFile(filepath.Join(dir, "002_second.up.sql"), []byte("ALTER TABLE a ADD COLUMN name TEXT;"), 0644)

	prev, err := GetPreviousMigration(dir)
	if err != nil {
		t.Fatalf("GetPreviousMigration() error = %v", err)
	}
	if prev == nil {
		t.Fatal("expected non-nil previous migration")
	}
	if prev.Filename != "002_second.up.sql" {
		t.Fatalf("expected latest migration, got %q", prev.Filename)
	}
	if !strings.Contains(prev.Content, "ALTER TABLE") {
		t.Fatalf("expected ALTER TABLE content, got %q", prev.Content)
	}
}

func TestGetMigrationHistory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "001_first.up.sql"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "001_first.down.sql"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "002_second.up.sql"), []byte(""), 0644)

	history, err := GetMigrationHistory(dir)
	if err != nil {
		t.Fatalf("GetMigrationHistory() error = %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(history))
	}
}

func TestCreateMigrationWithContext(t *testing.T) {
	dir := t.TempDir()
	migDir := filepath.Join(dir, "db/migrations")
	protoDir := filepath.Join(dir, "proto")

	// Create a pre-existing migration.
	os.MkdirAll(migDir, 0755)
	os.WriteFile(filepath.Join(migDir, "001_create_users.up.sql"),
		[]byte("CREATE TABLE users (id UUID PRIMARY KEY, email TEXT NOT NULL);"), 0644)
	os.WriteFile(filepath.Join(migDir, "001_create_users.down.sql"),
		[]byte("DROP TABLE users;"), 0644)

	// Create a proto file.
	os.MkdirAll(protoDir, 0755)
	os.WriteFile(filepath.Join(protoDir, "models.proto"), []byte(`syntax = "proto3";
message UserPreference {
  string user_id = 1;
  string key = 2;
  string value = 3;
}
`), 0644)

	opts := &MigrationOptions{
		ProtoDir: protoDir,
	}

	if err := CreateMigration("add_preferences", migDir, opts); err != nil {
		t.Fatalf("CreateMigration() error = %v", err)
	}

	// Find the generated .up.sql file.
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}

	var upContent string
	for _, e := range entries {
		if strings.Contains(e.Name(), "add_preferences") && strings.HasSuffix(e.Name(), ".up.sql") {
			content, err := os.ReadFile(filepath.Join(migDir, e.Name()))
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			upContent = string(content)
			break
		}
	}

	if upContent == "" {
		t.Fatal("could not find generated add_preferences.up.sql")
	}

	// Verify it contains schema context.
	if !strings.Contains(upContent, "CURRENT SCHEMA") {
		t.Error("expected CURRENT SCHEMA section in migration")
	}
	if !strings.Contains(upContent, "TABLE users") {
		t.Error("expected TABLE users in schema section")
	}
	if !strings.Contains(upContent, "PROTO MODELS") {
		t.Error("expected PROTO MODELS section")
	}
	if !strings.Contains(upContent, "UserPreference") {
		t.Error("expected UserPreference in proto section")
	}
	if !strings.Contains(upContent, "PREVIOUS MIGRATION") {
		t.Error("expected PREVIOUS MIGRATION section")
	}
	if !strings.Contains(upContent, "DIFF") {
		t.Error("expected DIFF section")
	}
}

func TestCreateMigrationWithFromProto(t *testing.T) {
	dir := t.TempDir()
	migDir := filepath.Join(dir, "db/migrations")
	protoDir := filepath.Join(dir, "proto")

	os.MkdirAll(migDir, 0755)
	os.MkdirAll(protoDir, 0755)

	os.WriteFile(filepath.Join(protoDir, "models.proto"), []byte(`syntax = "proto3";
message UserPreference {
  string user_id = 1;
  string key = 2;
  string value = 3;
}
`), 0644)

	opts := &MigrationOptions{
		ProtoDir:  protoDir,
		FromProto: true,
	}

	if err := CreateMigration("add_preferences", migDir, opts); err != nil {
		t.Fatalf("CreateMigration() error = %v", err)
	}

	// Find the generated .up.sql file.
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}

	var upContent string
	for _, e := range entries {
		if strings.Contains(e.Name(), "add_preferences") && strings.HasSuffix(e.Name(), ".up.sql") {
			content, err := os.ReadFile(filepath.Join(migDir, e.Name()))
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			upContent = string(content)
			break
		}
	}

	if upContent == "" {
		t.Fatal("could not find generated add_preferences.up.sql")
	}

	// Verify it contains CREATE TABLE SQL from proto.
	if !strings.Contains(upContent, "CREATE TABLE user_preferences") {
		t.Errorf("expected CREATE TABLE user_preferences, got:\n%s", upContent)
	}
	if !strings.Contains(upContent, "user_id TEXT NOT NULL") {
		t.Errorf("expected user_id column, got:\n%s", upContent)
	}
}

func TestProtoMessageToTableName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"User", "users"},
		{"UserPreference", "user_preferences"},
		{"AuditLog", "audit_logs"},
		{"Address", "address"}, // already ends in 's'
	}

	for _, tt := range tests {
		got := protoMessageToTableName(tt.input)
		if got != tt.want {
			t.Errorf("protoMessageToTableName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestProtoToSQL(t *testing.T) {
	tests := []struct {
		protoType string
		want      string
	}{
		{"string", "TEXT"},
		{"int32", "INTEGER"},
		{"int64", "BIGINT"},
		{"bool", "BOOLEAN"},
		{"float", "REAL"},
		{"double", "DOUBLE PRECISION"},
		{"bytes", "BYTEA"},
		{"google.protobuf.Timestamp", "TIMESTAMPTZ"},
		{"SomeUnknownType", "TEXT"},
	}

	for _, tt := range tests {
		got := ProtoToSQL(tt.protoType)
		if got != tt.want {
			t.Errorf("ProtoToSQL(%q) = %q, want %q", tt.protoType, got, tt.want)
		}
	}
}