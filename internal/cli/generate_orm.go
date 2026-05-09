package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/database"
)

// runOrmGenerate runs buf generate with the protoc-gen-forge plugin (mode=orm) for proto/db/ entities.
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

	fmt.Println("🔨 Running protoc-gen-forge (mode=orm) for entity protos...")

	// Build the buf plugin command: ["<forge-bin>", ..., "protoc-gen-forge"]
	pluginArgs := append(forgeCmd, "protoc-gen-forge")
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
      - mode=orm
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

// touchORMOutputs updates the mtime of every *.pb.orm.go file under root to now.
// Runs as a late pipeline step so post-orm passes (goimports, rehash) don't
// leave the *.pb.go siblings looking newer; the proto-orm-out-of-sync lint
// uses mtime comparison and protogen skips writes for byte-identical content,
// so without this nudge a no-op regen tickles a false stale-warning.
func touchORMOutputs(root string) error {
	if !dirExists(root) {
		return nil
	}
	now := time.Now()
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".pb.orm.go") {
			return nil
		}
		// Best-effort; ignore individual errors so one stat-only file doesn't
		// abort the whole pass.
		_ = os.Chtimes(path, now, now)
		return nil
	})
}

// runDescriptorGenerate runs buf generate with the protoc-gen-forge plugin (mode=descriptor)
// to extract service, entity, and config data from all proto files into forge_descriptor.json.
func runDescriptorGenerate(projectDir string) error {
	// Collect every proto/<sub>/ that contains .proto files. This includes
	// the canonical {services,api,db,config} dirs plus any pack-emitted
	// proto trees (e.g. proto/audit/ from the audit-log pack). Without
	// the broader walk, pack services are invisible to the descriptor
	// and downstream codegen (frontend hooks, mocks, etc).
	protoPaths := discoverProtoSubdirs(projectDir)

	if len(protoPaths) == 0 {
		return nil // Nothing to extract
	}

	// Remove stale descriptor + any leftover staging fragments so the
	// merge step in MergeDescriptorFragments only sees fragments from
	// this invocation.
	_ = os.Remove(filepath.Join(projectDir, "gen", "forge_descriptor.json"))
	_ = os.RemoveAll(filepath.Join(projectDir, "gen", descriptorStageDir))

	forgeCmd, err := forgeExecCommand()
	if err != nil {
		return fmt.Errorf("resolve forge binary: %w", err)
	}

	fmt.Println("🔨 Running protoc-gen-forge (mode=descriptor) to extract proto metadata...")

	pluginArgs := append(forgeCmd, "protoc-gen-forge")
	quoted := make([]string, len(pluginArgs))
	for i, a := range pluginArgs {
		quoted[i] = fmt.Sprintf(`"%s"`, a)
	}
	pluginCmd := "[" + strings.Join(quoted, ", ") + "]"

	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("resolve project dir: %w", err)
	}
	genDir := filepath.Join(absProjectDir, "gen")
	descConfig := fmt.Sprintf(`version: v2
plugins:
  - local: %s
    out: gen
    opt:
      - mode=descriptor
      - descriptor_out=%s
`, pluginCmd, genDir)
	tmpFile := filepath.Join(projectDir, "buf.gen.descriptor.yaml")
	if err := os.WriteFile(tmpFile, []byte(descConfig), 0644); err != nil {
		return fmt.Errorf("failed to write descriptor buf config: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	args := []string{"generate", "--template", "buf.gen.descriptor.yaml"}
	for _, p := range protoPaths {
		args = append(args, "--path", p)
	}

	cmd := exec.Command("buf", args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("descriptor generation failed: %w", err)
	}

	// Each plugin process wrote a per-invocation fragment under
	// gen/.descriptor.d/; merge them into gen/forge_descriptor.json now
	// that buf has finished. This step is what makes parallel buf-plugin
	// invocations safe — the merge happens in a single parent process.
	if err := MergeDescriptorFragments(filepath.Join(projectDir, "gen")); err != nil {
		return fmt.Errorf("merge descriptor fragments: %w", err)
	}

	fmt.Println("  ✅ forge_descriptor.json generated")
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

// isBoilerplateMigration returns true if the existing migrations only contain
// scaffold-generated init tables ("items" boilerplate or single-entity default).
func isBoilerplateMigration(projectDir string) bool {
	migDir := filepath.Join(projectDir, "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		return false
	}

	// Collect non-boilerplate up files. Scaffold generates two patterns:
	// - 0001_init.up.sql (example items table)
	// - 00001_init.up.sql (entity-aware single table)
	// If all up files are one of these patterns, it's boilerplate.
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}

		content, err := os.ReadFile(filepath.Join(migDir, e.Name()))
		if err != nil {
			return false
		}
		s := string(content)

		// Legacy boilerplate: scaffold "items" table.
		if strings.Contains(s, "CREATE TABLE IF NOT EXISTS items") {
			continue
		}

		// Scaffold init: single CREATE TABLE in an init migration.
		isInit := strings.HasPrefix(e.Name(), "00001_init") || strings.HasPrefix(e.Name(), "0001_init")
		if isInit && strings.Count(s, "CREATE TABLE") == 1 {
			continue
		}

		// Found a non-boilerplate migration — user has authored migrations.
		return false
	}

	return true
}

// removeBoilerplateMigrations removes all scaffold-generated migration files.
func removeBoilerplateMigrations(migDir string) {
	entries, err := os.ReadDir(migDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// Remove both naming conventions: 0001_init and 00001_init.
		if strings.HasPrefix(e.Name(), "00001_init") || strings.HasPrefix(e.Name(), "0001_init") {
			os.Remove(filepath.Join(migDir, e.Name()))
		}
	}
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