package database

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var migrationNameSanitizer = regexp.MustCompile(`[^a-z0-9_]+`)

// CreateMigration creates a new blank SQL migration pair using golang-migrate's
// timestamped filename convention.
func CreateMigration(name, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create migrations directory: %w", err)
	}

	sanitizedName := sanitizeMigrationName(name)
	if sanitizedName == "" {
		return fmt.Errorf("migration name %q produced an empty filename; use letters or numbers", name)
	}

	timestamp := time.Now().UTC().Format("20060102150405")
	baseName := fmt.Sprintf("%s_%s", timestamp, sanitizedName)
	upPath := filepath.Join(dir, baseName+".up.sql")
	downPath := filepath.Join(dir, baseName+".down.sql")

	upContents := fmt.Sprintf("-- Migration: %s\n-- Write forward SQL here.\n\n", sanitizedName)
	downContents := fmt.Sprintf("-- Rollback: %s\n-- Write rollback SQL here.\n\n", sanitizedName)

	if err := writeNewFile(upPath, upContents); err != nil {
		return err
	}
	if err := writeNewFile(downPath, downContents); err != nil {
		return err
	}

	fmt.Printf("✅ Migration '%s' created:\n", sanitizedName)
	fmt.Printf("   %s\n", upPath)
	fmt.Printf("   %s\n", downPath)
	return nil
}

func sanitizeMigrationName(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, " ", "_")
	normalized = migrationNameSanitizer.ReplaceAllString(normalized, "_")
	normalized = strings.Trim(normalized, "_")
	for strings.Contains(normalized, "__") {
		normalized = strings.ReplaceAll(normalized, "__", "_")
	}
	return normalized
}

func writeNewFile(path, contents string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("migration file already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}
