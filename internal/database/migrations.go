package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var migrationNameSanitizer = regexp.MustCompile(`[^a-z0-9_]+`)

// migrationVersionPattern matches the leading numeric prefix of a migration
// filename (e.g. "00019_add_users.up.sql" → "00019").
var migrationVersionPattern = regexp.MustCompile(`^(\d+)_`)

// defaultMigrationWidth is the zero-pad width used when a project has no
// existing numeric migrations to match. Mirrors the scaffold births and the
// pack allocator (00001_init → 5 digits).
const defaultMigrationWidth = 5

// CreateMigration creates a new SQL migration pair, continuing the project's
// existing sequential numbering. It scans dir for the highest numeric version
// prefix and emits max+1 in the same zero-padded style the dir already uses
// (00001_, 00002_, …). When opts is non-nil, it gathers schema context and
// writes a rich comment block into the .up.sql file.
func CreateMigration(ctx context.Context, name, dir string, opts *MigrationOptions) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create migrations directory: %w", err)
	}

	sanitizedName := sanitizeMigrationName(name)
	if sanitizedName == "" {
		return fmt.Errorf("migration name %q produced an empty filename; use letters or numbers", name)
	}

	version := nextMigrationVersion(dir)
	baseName := fmt.Sprintf("%s_%s", version, sanitizedName)
	upPath := filepath.Join(dir, baseName+".up.sql")
	downPath := filepath.Join(dir, baseName+".down.sql")

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

// nextMigrationVersion returns the next version prefix for a new migration in
// dir as a zero-padded numeric string, one greater than the highest existing
// numeric prefix. This continues the scaffold's sequential scheme (00001_,
// 00002_, …) monotonically and is inherently collision-free: every file
// written raises the max, so rapid successive calls never duplicate a version
// (unlike a wall-clock timestamp, which collides within the same second).
//
// The zero-pad width matches the existing files when they share one, else
// falls back to defaultMigrationWidth. %0*d never truncates, so a number wider
// than the pad still emits in full and stays monotonic.
func nextMigrationVersion(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing dir (fresh project) or unreadable — start the sequence.
		return fmt.Sprintf("%0*d", defaultMigrationWidth, 1)
	}

	highest := 0
	width := 0
	mixedWidth := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationVersionPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > highest {
			highest = n
		}
		switch {
		case width == 0:
			width = len(m[1])
		case width != len(m[1]):
			mixedWidth = true
		}
	}
	if width == 0 || mixedWidth {
		width = defaultMigrationWidth
	}
	return fmt.Sprintf("%0*d", width, highest+1)
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
