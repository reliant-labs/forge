package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
