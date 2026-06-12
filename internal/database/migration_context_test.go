package database

import (
	"context"
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
		PreviousMigration: &PreviousMigrationInfo{
			Filename: "001_create_users.up.sql",
			Content:  "CREATE TABLE users (id UUID PRIMARY KEY);",
		},
		MigrationHistory: []string{"001_create_users.up.sql"},
	}

	comment := GenerateContextComment(ctx)

	checks := []string{
		"-- Migration: add_preferences",
		"-- Created: 2025-01-15T10:30:00Z",
		"-- === CURRENT SCHEMA (from existing migrations) ===",
		"-- TABLE users (",
		"--   id UUID",
		"--   email TEXT",
		"-- === PREVIOUS MIGRATION (001_create_users.up.sql) ===",
		"-- CREATE TABLE users",
		"-- === MIGRATION HISTORY ===",
		"-- 001_create_users.up.sql",
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

	// Create a pre-existing migration.
	os.MkdirAll(migDir, 0755)
	os.WriteFile(filepath.Join(migDir, "001_create_users.up.sql"),
		[]byte("CREATE TABLE users (id UUID PRIMARY KEY, email TEXT NOT NULL);"), 0644)
	os.WriteFile(filepath.Join(migDir, "001_create_users.down.sql"),
		[]byte("DROP TABLE users;"), 0644)

	if err := CreateMigration(context.Background(), "add_preferences", migDir, &MigrationOptions{}); err != nil {
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
	if !strings.Contains(upContent, "PREVIOUS MIGRATION") {
		t.Error("expected PREVIOUS MIGRATION section")
	}
}
