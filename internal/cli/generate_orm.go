package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/database"
)

// resolveOrmPluginBinary finds or installs the protoc-gen-forge-orm plugin.
// Returns the path to the binary (or empty string if unavailable).
func resolveOrmPluginBinary() (string, error) {
	// 1. Check PATH for pre-installed binary
	if path, err := exec.LookPath("protoc-gen-forge-orm"); err == nil {
		return path, nil
	}

	// 2. Check local bin directory
	localBin := filepath.Join("bin", "protoc-gen-forge-orm")
	if _, err := os.Stat(localBin); err == nil {
		return localBin, nil
	}

	// 3. Try to build from source if cmd/protoc-gen-forge-orm exists
	srcPath := filepath.Join("cmd", "protoc-gen-forge-orm", "main.go")
	if _, err := os.Stat(srcPath); err == nil {
		fmt.Println("Building protoc-gen-forge-orm from source...")
		if err := os.MkdirAll("bin", 0755); err == nil {
			buildCmd := exec.Command("go", "build", "-o", localBin, "./cmd/protoc-gen-forge-orm")
			buildCmd.Stdout = os.Stdout
			buildCmd.Stderr = os.Stderr
			if err := buildCmd.Run(); err == nil {
				fmt.Printf("Built %s\n", localBin)
				return localBin, nil
			}
			fmt.Println("Warning: failed to build from source, trying go install...")
		}
	}

	// 4. Try go install
	fmt.Println("Installing protoc-gen-forge-orm via go install...")
	installCmd := exec.Command("go", "install", "github.com/reliant-labs/forge/cmd/protoc-gen-forge-orm@latest")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err == nil {
		if path, err := exec.LookPath("protoc-gen-forge-orm"); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("protoc-gen-forge-orm not found: not on PATH, not at bin/protoc-gen-forge-orm, and go install failed")
}

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

	ormBinPath, err := resolveOrmPluginBinary()
	if err != nil {
		fmt.Println("  ⚠️  protoc-gen-forge-orm not available - skipping ORM code generation")
		fmt.Println("     Install with: go install github.com/reliant-labs/forge/cmd/protoc-gen-forge-orm@latest")
		return nil
	}

	fmt.Println("🔨 Running protoc-gen-forge-orm for entity protos...")

	// Use the resolved binary path in the buf config
	ormConfig := fmt.Sprintf(`version: v2
plugins:
  - local: %s
    out: gen
    opt:
      - paths=source_relative
`, ormBinPath)
	tmpFile := filepath.Join(projectDir, "buf.gen.orm.yaml")
	if err := os.WriteFile(tmpFile, []byte(ormConfig), 0644); err != nil {
		return fmt.Errorf("failed to write ORM buf config: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cmd := exec.Command("buf", "generate", "--template", "buf.gen.orm.yaml", "--path", "proto/db")
	cmd.Dir = projectDir
	// Add bin/ to PATH so buf can find the plugin if it's there
	cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s%c%s",
		filepath.Join(projectDir, "bin"), os.PathListSeparator, os.Getenv("PATH")))
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
