package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/migrationver"
)

var migrationNameSanitizer = regexp.MustCompile(`[^a-z0-9_]+`)

// CreateMigration creates a new forward-only SQL migration, versioned with a
// UTC timestamp (YYYYMMDDHHMMSS_name.up.sql). See internal/migrationver for
// why the version is a timestamp rather than max+1: a sequential allocator
// hands the same number to every branch cut from the same commit, which is
// how ten version numbers in control-plane each ended up claimed by two
// different migrations. Existing sequential files are never renamed — a
// 14-digit timestamp sorts after any 5-digit number. When opts is non-nil,
// it gathers schema context and writes a rich comment block into the
// .up.sql file.
func CreateMigration(ctx context.Context, name, dir string, opts *MigrationOptions) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create migrations directory: %w", err)
	}

	sanitizedName := sanitizeMigrationName(name)
	if sanitizedName == "" {
		return fmt.Errorf("migration name %q produced an empty filename; use letters or numbers", name)
	}

	version, err := migrationver.Next(dir)
	if err != nil {
		return err
	}
	baseName := fmt.Sprintf("%s_%s", version, sanitizedName)
	// Up only. Forge rolls forward: a bad migration is repaired by the next
	// migration, never reversed by a down script written before the release
	// ran (see migrationlint's no-down-migration rule).
	upPath := filepath.Join(dir, baseName+".up.sql")

	// Build up contents with context if opts provided.
	var upContents string
	if opts == nil {
		opts = &MigrationOptions{}
	}

	migCtx, err := GatherMigrationContext(ctx, sanitizedName, dir, *opts)
	if err != nil {
		// Non-fatal — fall back to minimal header.
		upContents = fmt.Sprintf("-- Migration: %s\n-- Write forward SQL here.\n\n", sanitizedName)
	} else {
		upContents = GenerateContextComment(migCtx)
	}

	if err := writeNewFile(upPath, upContents); err != nil {
		return err
	}

	fmt.Printf("✅ Migration '%s' created:\n", sanitizedName)
	fmt.Printf("   %s\n", upPath)
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
