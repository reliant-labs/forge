package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeMigrationName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "spaces", input: "add users table", want: "add_users_table"},
		{name: "mixed case", input: "Backfill Account Status", want: "backfill_account_status"},
		{name: "symbols", input: "add-users/table!", want: "add_users_table"},
		{name: "trim underscores", input: "__repair dirty state__", want: "repair_dirty_state"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeMigrationName(tt.input)
			if got != tt.want {
				t.Fatalf("sanitizeMigrationName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCreateMigrationCreatesUpAndDownFiles(t *testing.T) {
	dir := t.TempDir()

	if err := CreateMigration("Add Users Table", dir); err != nil {
		t.Fatalf("CreateMigration() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 migration files, got %d", len(entries))
	}

	var upFile, downFile string
	for _, entry := range entries {
		switch {
		case strings.HasSuffix(entry.Name(), ".up.sql"):
			upFile = entry.Name()
		case strings.HasSuffix(entry.Name(), ".down.sql"):
			downFile = entry.Name()
		}
	}

	if upFile == "" {
		t.Fatal("expected an .up.sql migration file")
	}
	if downFile == "" {
		t.Fatal("expected a .down.sql migration file")
	}
	if !strings.Contains(upFile, "add_users_table") {
		t.Fatalf("expected sanitized name in up migration, got %s", upFile)
	}
	if !strings.Contains(downFile, "add_users_table") {
		t.Fatalf("expected sanitized name in down migration, got %s", downFile)
	}

	upContents, err := os.ReadFile(filepath.Join(dir, upFile))
	if err != nil {
		t.Fatalf("ReadFile(up) error = %v", err)
	}
	if !strings.Contains(string(upContents), "Write forward SQL here") {
		t.Fatalf("unexpected up migration contents: %s", string(upContents))
	}

	downContents, err := os.ReadFile(filepath.Join(dir, downFile))
	if err != nil {
		t.Fatalf("ReadFile(down) error = %v", err)
	}
	if !strings.Contains(string(downContents), "Write rollback SQL here") {
		t.Fatalf("unexpected down migration contents: %s", string(downContents))
	}
}

func TestCreateMigrationRejectsEmptySanitizedName(t *testing.T) {
	dir := t.TempDir()

	err := CreateMigration("!!!", dir)
	if err == nil {
		t.Fatal("expected error for empty sanitized migration name")
	}
}
