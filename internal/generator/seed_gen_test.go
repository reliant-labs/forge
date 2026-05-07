package generator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeterministicUUID(t *testing.T) {
	// Same input must always produce the same output.
	a := deterministicUUID("users.0")
	b := deterministicUUID("users.0")
	if a != b {
		t.Fatalf("expected deterministic output, got %q and %q", a, b)
	}

	// Different inputs must produce different outputs.
	c := deterministicUUID("users.1")
	if a == c {
		t.Fatalf("expected different UUIDs for different inputs, both got %q", a)
	}

	// Must look like a UUID (8-4-4-4-12).
	parts := strings.Split(a, "-")
	if len(parts) != 5 {
		t.Fatalf("expected UUID format, got %q", a)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("UUID segment lengths wrong: %q", a)
	}

	// Version nibble must be 5.
	if parts[2][0] != '5' {
		t.Fatalf("expected version 5 UUID, got version nibble %c in %q", parts[2][0], a)
	}
}

func TestGenerateEntitySeeds_EmptyEntities(t *testing.T) {
	dir := t.TempDir()
	if err := generateEntitySeeds(nil, dir); err != nil {
		t.Fatalf("unexpected error for nil entities: %v", err)
	}
	// No files should be created.
	if _, err := os.Stat(filepath.Join(dir, "db", "seeds")); !os.IsNotExist(err) {
		t.Fatalf("expected no seeds dir for empty entities")
	}
}

func TestGenerateEntitySeeds_SQLOutput(t *testing.T) {
	dir := t.TempDir()
	entities := []SeedEntity{{
		TableName:  "users",
		SoftDelete: true,
		Timestamps: true,
		Fields: []SeedField{
			{ColumnName: "id", FieldType: SeedFieldText, IsPK: true},
			{ColumnName: "name", FieldType: SeedFieldText, NotNull: true},
			{ColumnName: "email", FieldType: SeedFieldText, NotNull: true},
			{ColumnName: "active", FieldType: SeedFieldBoolean},
			{ColumnName: "age", FieldType: SeedFieldInteger},
			{ColumnName: "created_at", FieldType: SeedFieldTimestamp, IsTimestamp: true},
			{ColumnName: "updated_at", FieldType: SeedFieldTimestamp, IsTimestamp: true},
			{ColumnName: "deleted_at", FieldType: SeedFieldTimestamp, IsTimestamp: true},
		},
	}}

	if err := generateEntitySeeds(entities, dir); err != nil {
		t.Fatalf("generateEntitySeeds: %v", err)
	}

	sqlPath := filepath.Join(dir, "db", "seeds", "0002_users.sql")
	data, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read SQL seed: %v", err)
	}
	sql := string(data)

	// Must have INSERT INTO.
	if !strings.Contains(sql, "INSERT INTO users (") {
		t.Fatalf("missing INSERT INTO users, got:\n%s", sql)
	}

	// Must have ON CONFLICT.
	if !strings.Contains(sql, "ON CONFLICT (id) DO NOTHING;") {
		t.Fatalf("missing ON CONFLICT, got:\n%s", sql)
	}

	// Must NOT contain deleted_at column (soft-delete entities skip it).
	if strings.Contains(sql, "deleted_at") {
		t.Fatalf("deleted_at should be omitted, got:\n%s", sql)
	}

	// Booleans should be bare (not quoted).
	if strings.Contains(sql, "'true'") || strings.Contains(sql, "'false'") {
		t.Fatalf("boolean values should not be quoted, got:\n%s", sql)
	}

	// Integers should not be quoted.
	// The age column should contain bare numbers like 20, 21, ...
	if strings.Contains(sql, "'20'") {
		t.Fatalf("integer values should not be quoted, got:\n%s", sql)
	}

	// Should have 10 value rows.
	rowCount := strings.Count(sql, "    (")
	if rowCount != 10 {
		t.Fatalf("expected 10 rows, got %d", rowCount)
	}
}

func TestGenerateEntitySeeds_JSONFixture(t *testing.T) {
	dir := t.TempDir()
	entities := []SeedEntity{{
		TableName: "products",
		Fields: []SeedField{
			{ColumnName: "id", FieldType: SeedFieldText, IsPK: true},
			{ColumnName: "title", FieldType: SeedFieldText, NotNull: true},
			{ColumnName: "price", FieldType: SeedFieldInteger},
			{ColumnName: "active", FieldType: SeedFieldBoolean},
		},
	}}

	if err := generateEntitySeeds(entities, dir); err != nil {
		t.Fatalf("generateEntitySeeds: %v", err)
	}

	jsonPath := filepath.Join(dir, "db", "fixtures", "products.json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read JSON fixture: %v", err)
	}

	var fixture struct {
		Name        string                              `json:"name"`
		Description string                              `json:"description"`
		Tables      map[string][]map[string]interface{} `json:"tables"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	if fixture.Name != "products" {
		t.Fatalf("expected name=products, got %q", fixture.Name)
	}
	if fixture.Description != "Auto-generated seed data" {
		t.Fatalf("expected auto-generated description, got %q", fixture.Description)
	}

	rows, ok := fixture.Tables["products"]
	if !ok {
		t.Fatalf("missing products table in fixture")
	}
	if len(rows) != 10 {
		t.Fatalf("expected 10 rows, got %d", len(rows))
	}

	// Check typed values: price should be a number, active should be boolean.
	first := rows[0]
	if _, ok := first["price"].(float64); !ok {
		t.Fatalf("expected price to be numeric, got %T: %v", first["price"], first["price"])
	}
	if _, ok := first["active"].(bool); !ok {
		t.Fatalf("expected active to be boolean, got %T: %v", first["active"], first["active"])
	}
}

func TestGenerateEntitySeeds_AutoIncrementPKSkipped(t *testing.T) {
	dir := t.TempDir()
	entities := []SeedEntity{{
		TableName: "counters",
		Fields: []SeedField{
			{ColumnName: "id", FieldType: SeedFieldInteger, IsPK: true, AutoIncrement: true},
			{ColumnName: "name", FieldType: SeedFieldText},
		},
	}}

	if err := generateEntitySeeds(entities, dir); err != nil {
		t.Fatalf("generateEntitySeeds: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "db", "seeds", "0002_counters.sql"))
	if err != nil {
		t.Fatalf("read SQL: %v", err)
	}
	sql := string(data)

	// The id column should not appear (auto-increment PK skipped).
	if strings.Contains(sql, "INSERT INTO counters (id,") {
		t.Fatalf("auto-increment id should be skipped, got:\n%s", sql)
	}
	if !strings.Contains(sql, "INSERT INTO counters (name)") {
		t.Fatalf("expected only name column, got:\n%s", sql)
	}
}

func TestGenerateEntitySeeds_ForeignKeyReferences(t *testing.T) {
	dir := t.TempDir()
	entities := []SeedEntity{{
		TableName: "posts",
		Fields: []SeedField{
			{ColumnName: "id", FieldType: SeedFieldText, IsPK: true},
			{ColumnName: "user_id", FieldType: SeedFieldText, References: "users.id"},
			{ColumnName: "title", FieldType: SeedFieldText},
		},
	}}

	if err := generateEntitySeeds(entities, dir); err != nil {
		t.Fatalf("generateEntitySeeds: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "db", "seeds", "0002_posts.sql"))
	if err != nil {
		t.Fatalf("read SQL: %v", err)
	}
	sql := string(data)

	// user_id values should be UUIDs matching the users table's deterministic UUIDs.
	expectedFK := deterministicUUID("users.0")
	if !strings.Contains(sql, expectedFK) {
		t.Fatalf("expected FK UUID %q in SQL, got:\n%s", expectedFK, sql)
	}
}

func TestGenerateEntitySeeds_MultipleEntities(t *testing.T) {
	dir := t.TempDir()
	entities := []SeedEntity{
		{
			TableName: "orgs",
			Fields: []SeedField{
				{ColumnName: "id", FieldType: SeedFieldText, IsPK: true},
				{ColumnName: "name", FieldType: SeedFieldText},
			},
		},
		{
			TableName: "teams",
			Fields: []SeedField{
				{ColumnName: "id", FieldType: SeedFieldText, IsPK: true},
				{ColumnName: "org_id", FieldType: SeedFieldText, References: "orgs.id"},
				{ColumnName: "name", FieldType: SeedFieldText},
			},
		},
	}

	if err := generateEntitySeeds(entities, dir); err != nil {
		t.Fatalf("generateEntitySeeds: %v", err)
	}

	// orgs → 0002, teams → 0003
	assertPathExists(t, filepath.Join(dir, "db", "seeds", "0002_orgs.sql"))
	assertPathExists(t, filepath.Join(dir, "db", "seeds", "0003_teams.sql"))
	assertPathExists(t, filepath.Join(dir, "db", "fixtures", "orgs.json"))
	assertPathExists(t, filepath.Join(dir, "db", "fixtures", "teams.json"))
}

func TestGenerateEntitySeeds_Deterministic(t *testing.T) {
	entities := []SeedEntity{{
		TableName: "items",
		Fields: []SeedField{
			{ColumnName: "id", FieldType: SeedFieldText, IsPK: true},
			{ColumnName: "name", FieldType: SeedFieldText},
			{ColumnName: "status", FieldType: SeedFieldText},
		},
	}}

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	if err := generateEntitySeeds(entities, dir1); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := generateEntitySeeds(entities, dir2); err != nil {
		t.Fatalf("second run: %v", err)
	}

	sql1, _ := os.ReadFile(filepath.Join(dir1, "db", "seeds", "0002_items.sql"))
	sql2, _ := os.ReadFile(filepath.Join(dir2, "db", "seeds", "0002_items.sql"))
	if string(sql1) != string(sql2) {
		t.Fatalf("output is not deterministic:\nRun 1:\n%s\nRun 2:\n%s", sql1, sql2)
	}

	json1, _ := os.ReadFile(filepath.Join(dir1, "db", "fixtures", "items.json"))
	json2, _ := os.ReadFile(filepath.Join(dir2, "db", "fixtures", "items.json"))
	if string(json1) != string(json2) {
		t.Fatalf("JSON output is not deterministic")
	}
}

func TestGenerateStringValue_Patterns(t *testing.T) {
	tests := []struct {
		col      string
		contains string
	}{
		{"email", "@example.com"},
		{"phone", "+1555"},
		{"username", "user_"},
		{"slug", "item-"},
		{"code", "CODE-"},
		{"url", "https://example.com/"},
		{"zip_code", "1000"},
	}
	for _, tt := range tests {
		v := generateStringValue(tt.col, 0)
		if !strings.Contains(v, tt.contains) {
			t.Errorf("generateStringValue(%q, 0) = %q, expected to contain %q", tt.col, v, tt.contains)
		}
	}
}

func TestGenerateTimestamp(t *testing.T) {
	created := generateTimestamp("created_at", 0)
	updated := generateTimestamp("updated_at", 0)

	if !strings.Contains(created, "2024-01-01T08:00:00Z") {
		t.Fatalf("expected created_at to be morning, got %q", created)
	}
	if !strings.Contains(updated, "2024-01-01T12:00:00Z") {
		t.Fatalf("expected updated_at to be noon, got %q", updated)
	}
}

func TestSqlQuote(t *testing.T) {
	fields := []SeedField{
		{ColumnName: "name", FieldType: SeedFieldText},
		{ColumnName: "count", FieldType: SeedFieldInteger},
		{ColumnName: "active", FieldType: SeedFieldBoolean},
		{ColumnName: "score", FieldType: SeedFieldFloat},
	}

	if got := sqlQuote(fields, "name", "O'Brien"); got != "'O''Brien'" {
		t.Fatalf("expected escaped quote, got %q", got)
	}
	if got := sqlQuote(fields, "count", "42"); got != "42" {
		t.Fatalf("expected bare integer, got %q", got)
	}
	if got := sqlQuote(fields, "active", "true"); got != "true" {
		t.Fatalf("expected bare boolean, got %q", got)
	}
	if got := sqlQuote(fields, "score", "10.50"); got != "10.50" {
		t.Fatalf("expected bare float, got %q", got)
	}
}