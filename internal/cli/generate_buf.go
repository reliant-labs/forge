package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/config"
)

// runBufGenerateGo runs buf generate using the project's buf.gen.yaml for Go stubs.
func runBufGenerateGo(projectDir string) error {
	// Create a default buf.gen.yaml only if one doesn't exist
	if _, err := os.Stat(filepath.Join(projectDir, "buf.gen.yaml")); os.IsNotExist(err) {
		if err := writeDefaultBufGenYaml(projectDir); err != nil {
			return fmt.Errorf("failed to create buf.gen.yaml: %w", err)
		}
		fmt.Println("📝 Generated default buf.gen.yaml")
	}

	fmt.Println("🔨 Running buf generate (Go stubs)...")
	cmd := exec.Command("buf", "generate")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("buf generate failed: %w", err)
	}

	fmt.Println("  ✅ Go protobuf + Connect stubs generated")
	return nil
}

// writeDefaultBufGenYaml writes a standard buf.gen.yaml with Go plugins.
func writeDefaultBufGenYaml(projectDir string) error {
	config := `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt:
      - paths=source_relative
  - remote: buf.build/connectrpc/go
    out: gen
    opt:
      - paths=source_relative
`
	return os.WriteFile(filepath.Join(projectDir, "buf.gen.yaml"), []byte(config), 0644)
}

// runBufGenerateTypeScript runs buf generate for TypeScript stubs in a Next.js frontend.
// It runs buf from the project root to avoid picking up node_modules proto files,
// using --path flags to scope generation and --template to point at the frontend's buf.gen.yaml.
func runBufGenerateTypeScript(fe config.FrontendConfig, cfg *config.ProjectConfig, projectDir string) error {
	feDir := fe.Path
	if feDir == "" {
		feDir = filepath.Join("frontends", fe.Name)
	}

	absFeDir := filepath.Join(projectDir, feDir)
	if !dirExists(absFeDir) {
		return fmt.Errorf("frontend directory %s not found", feDir)
	}

	fmt.Printf("🔨 Generating TypeScript stubs for %s...\n", fe.Name)

	// Ensure the frontend has a buf.gen.yaml with out: relative to project root
	feBufGen := filepath.Join(absFeDir, "buf.gen.yaml")
	if _, err := os.Stat(feBufGen); os.IsNotExist(err) {
		// include_imports must be a plugin-level field in buf.gen.yaml v2,
		// not an `opt:` entry — bufbuild/es rejects unknown opts. Mirror the
		// structure used by the frontend template (buf.gen.yaml.tmpl) so
		// regenerated and manually-written configs stay consistent.
		tsConfig := fmt.Sprintf(`version: v2
plugins:
  - remote: buf.build/bufbuild/es
    out: %s/src/gen
    include_imports: true
    opt:
      - target=ts
      - import_extension=.js
`, filepath.ToSlash(feDir))
		if err := os.WriteFile(feBufGen, []byte(tsConfig), 0644); err != nil {
			return fmt.Errorf("failed to write TypeScript buf config: %w", err)
		}
	}

	// Build command: run from project root, use --template with relative path to frontend's buf.gen.yaml
	relativeTemplate := filepath.Join(feDir, "buf.gen.yaml")
	args := []string{"generate", "--template", relativeTemplate, "--path", "proto/services"}

	// Include proto/api if it exists
	if dirExists(filepath.Join(projectDir, "proto/api")) {
		args = append(args, "--path", "proto/api")
	}

	cmd := exec.Command("buf", args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("TypeScript generation failed for %s: %w", fe.Name, err)
	}

	fmt.Printf("  ✅ TypeScript stubs generated for %s\n", fe.Name)
	return nil
}