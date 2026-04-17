package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/database"
)

// runOrmGenerate runs buf generate with the protoc-gen-forge-orm plugin for proto/db/ entities.
func runOrmGenerate(projectDir string) error {
	hasProtoFiles, err := hasProtoFilesInDir(filepath.Join(projectDir, "proto", "db"))
	if err != nil {
		return fmt.Errorf("scan proto/db for ORM protos: %w", err)
	}
	if !hasProtoFiles {
		fmt.Println("  ℹ️  No proto files found in proto/db - skipping ORM code generation")
		return nil
	}

	forgeCmd, err := forgeExecCommand()
	if err != nil {
		return fmt.Errorf("resolve forge binary: %w", err)
	}

	fmt.Println("🔨 Running protoc-gen-forge-orm for entity protos...")

	// Build the buf plugin command: ["<forge-bin>", ..., "protoc-gen-forge-orm"]
	pluginArgs := append(forgeCmd, "protoc-gen-forge-orm")
	quoted := make([]string, len(pluginArgs))
	for i, a := range pluginArgs {
		quoted[i] = fmt.Sprintf(`"%s"`, a)
	}
	pluginCmd := "[" + strings.Join(quoted, ", ") + "]"

	ormConfig := fmt.Sprintf(`version: v2
plugins:
  - local: %s
    out: gen
    opt:
      - paths=source_relative
`, pluginCmd)
	tmpFile := filepath.Join(projectDir, "buf.gen.orm.yaml")
	if err := os.WriteFile(tmpFile, []byte(ormConfig), 0644); err != nil {
		return fmt.Errorf("failed to write ORM buf config: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cmd := exec.Command("buf", "generate", "--template", "buf.gen.orm.yaml", "--path", "proto/db")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ORM generation failed: %w", err)
	}

	fmt.Println("  ✅ ORM code generated from proto/db/")
	return nil
}

func hasProtoFilesInDir(root string) (bool, error) {
	if !dirExists(root) {
		return false, nil
	}

	hasProtoFiles := false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".proto" {
			hasProtoFiles = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return false, err
	}
	return hasProtoFiles, nil
}

// hasSQLMigrations returns true if db/migrations/ contains at least one .sql file.
func hasSQLMigrations(projectDir string) bool {
	migDir := filepath.Join(projectDir, "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			return true
		}
	}
	return false
}

// maybeGenerateInitialMigration auto-generates an initial migration from proto/db entities
// when db/migrations/ is empty and proto/db has .proto files.
func maybeGenerateInitialMigration(projectDir string) error {
	hasProtos, err := hasProtoFilesInDir(filepath.Join(projectDir, "proto", "db"))
	if err != nil || !hasProtos {
		return nil
	}

	migDir := filepath.Join(projectDir, "db", "migrations")
	fmt.Println("🔧 Auto-generating initial migration from proto/db entities...")
	opts := &database.MigrationOptions{
		FromProto: true,
	}
	if err := database.CreateMigration("init", migDir, opts); err != nil {
		return fmt.Errorf("auto-generate initial migration: %w", err)
	}
	return nil
}