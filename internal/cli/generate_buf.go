package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
// Defaults to `local:` plugins (resolved from $PATH) so anonymous users
// can `forge generate` without `buf registry login`. Users who want
// BSR-hosted plugins can edit the file to `remote:` (see file comments).
func writeDefaultBufGenYaml(projectDir string) error {
	config := `version: v2
# Default: local plugins resolved from $PATH. Anonymous users can
# ` + "`forge generate`" + ` without touching the BSR. Run ` + "`forge tools install`" + `
# (or ` + "`go install`" + ` the two binaries listed below) to put them on PATH.
#
# To opt back into BSR-hosted plugins, replace ` + "`local:`" + ` with ` + "`remote:`" + `
# and the binary name with the BSR plugin path, e.g.
#   - remote: buf.build/protocolbuffers/go
#   - remote: buf.build/connectrpc/go
#
# OpenAPI specs: set ` + "`api.openapi: true`" + ` in forge.yaml. Forge invokes
# protoc-gen-connect-openapi via a synthesized template; you do NOT
# need to add it here. Install with:
#   ` + openAPIPluginInstallCmd + `
plugins:
  - local: protoc-gen-go
    out: gen
    opt:
      - paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt:
      - paths=source_relative
`
	return os.WriteFile(filepath.Join(projectDir, "buf.gen.yaml"), []byte(config), 0644)
}

// runBufGenerateTypeScript runs buf generate for TypeScript stubs in a Next.js frontend.
// It runs buf from the project root to avoid picking up node_modules proto files,
// using --path flags to scope generation and --template to point at the frontend's buf.gen.yaml.
//
// When the project opted into the pnpm-workspace layout
// (frontend.workspaces: true), TS stubs go to the shared
// packages/api/src/gen/ once via runBufGenerateTypeScriptWorkspace and
// the per-frontend buf step is short-circuited. The per-frontend code
// path below is preserved unchanged for the default layout so existing
// snapshot tests stay stable.
func runBufGenerateTypeScript(fe config.FrontendConfig, cfg *config.ProjectConfig, projectDir string) error {
	if cfg != nil && cfg.IsFrontendWorkspacesEnabled() {
		return runBufGenerateTypeScriptWorkspace(cfg, projectDir)
	}
	feDir := fe.Path
	if feDir == "" {
		feDir = filepath.Join("frontends", fe.Name)
	}

	absFeDir := filepath.Join(projectDir, feDir)
	if !dirExists(absFeDir) {
		return fmt.Errorf("frontend directory %s not found", feDir)
	}

	fmt.Printf("🔨 Generating TypeScript stubs for %s...\n", fe.Name)

	// Ensure the frontend has a buf.gen.yaml with out: relative to project root.
	// Default to the local TypeScript plugin (./<feDir>/node_modules/.bin/protoc-gen-es)
	// so anonymous users can `forge generate` without `buf registry login`. The
	// path is relative to where `buf generate` runs (project root via --template),
	// not relative to this YAML file. Mirrors the template at
	// internal/templates/frontend/{nextjs,react-native}/buf.gen.yaml.tmpl.
	feBufGen := filepath.Join(absFeDir, "buf.gen.yaml")
	if _, err := os.Stat(feBufGen); os.IsNotExist(err) {
		// include_imports must be a plugin-level field in buf.gen.yaml v2,
		// not an `opt:` entry — protoc-gen-es rejects unknown opts.
		feSlash := filepath.ToSlash(feDir)
		tsConfig := fmt.Sprintf(`version: v2
# Local TypeScript plugin (no BSR auth needed). Run 'npm install' in
# %s/ before 'forge generate' so node_modules/.bin/protoc-gen-es exists.
# To switch to BSR-hosted plugin, replace 'local:' line with:
#   - remote: buf.build/bufbuild/es
plugins:
  - local: ./%s/node_modules/.bin/protoc-gen-es
    out: %s/src/gen
    include_imports: true
    opt:
      - target=ts
      - import_extension=.js
`, feSlash, feSlash, feSlash)
		if err := os.WriteFile(feBufGen, []byte(tsConfig), 0644); err != nil {
			return fmt.Errorf("failed to write TypeScript buf config: %w", err)
		}
	}

	// Verify the local TS plugin is on disk before invoking buf — otherwise
	// buf emits a confusing "fork/exec: no such file" error. If absent, surface
	// a clear remediation message and skip cleanly.
	if usesLocalTSPlugin(feBufGen) {
		pluginPath := filepath.Join(absFeDir, "node_modules", ".bin", "protoc-gen-es")
		if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
			fmt.Printf("  ⚠️  %s: @bufbuild/protoc-gen-es not installed yet — run `npm install` in %s before `forge generate`.\n", fe.Name, feDir)
			return nil
		}
	}

	// Build command: run from project root, use --template with relative path to frontend's buf.gen.yaml
	relativeTemplate := filepath.Join(feDir, "buf.gen.yaml")
	args := []string{"generate", "--template", relativeTemplate}

	// Include every proto/<sub>/ with .proto files so pack-emitted services
	// (e.g. proto/audit/ from audit-log) participate in TypeScript codegen,
	// not just the canonical proto/services + proto/api pair.
	for _, p := range discoverProtoSubdirs(projectDir) {
		args = append(args, "--path", p)
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

// runBufGenerateTypeScriptWorkspace runs buf generate for TypeScript
// stubs in pnpm-workspaces mode. Output goes to packages/api/src/gen
// once; the per-frontend buf step is skipped because each frontend now
// consumes the shared workspace package via `"@<scope>/api": "workspace:*"`.
//
// The TS plugin is sourced from the first frontend's node_modules/.bin/
// to avoid making packages/api/ itself a Node project with its own
// installed plugin. That keeps the workspace package free of build-time
// dependencies — pnpm install once at the workspace root populates
// every node_modules/.bin/ in lockstep.
func runBufGenerateTypeScriptWorkspace(cfg *config.ProjectConfig, projectDir string) error {
	// Pick the first nextjs / react-native / vite-spa frontend to source
	// the protoc-gen-es plugin from. We prefer nextjs > vite-spa > react-
	// native because the web-target frontends are likeliest to already
	// have node_modules populated in dev.
	var pluginFrontend *config.FrontendConfig
	priority := map[string]int{"nextjs": 1, "vite-spa": 2, "react-native": 3}
	bestRank := 99
	for i := range cfg.Frontends {
		fe := cfg.Frontends[i]
		rank, ok := priority[strings.ToLower(fe.Type)]
		if !ok {
			continue
		}
		if rank < bestRank {
			bestRank = rank
			pluginFrontend = &cfg.Frontends[i]
		}
	}
	if pluginFrontend == nil {
		// No TS-capable frontend yet — nothing to generate. forge add
		// frontend will rerun this on a future generate cycle.
		return nil
	}

	feDir := pluginFrontend.Path
	if feDir == "" {
		feDir = filepath.Join("frontends", pluginFrontend.Name)
	}
	absFeDir := filepath.Join(projectDir, feDir)

	// Ensure the workspace buf.gen.yaml exists at packages/api/buf.gen.yaml.
	// We write it relative to project root so `out: packages/api/src/gen`
	// resolves correctly when buf runs from projectDir.
	bufGenPath := filepath.Join(projectDir, "packages", "api", "buf.gen.yaml")
	if _, err := os.Stat(bufGenPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(bufGenPath), 0o755); err != nil {
			return fmt.Errorf("create packages/api dir: %w", err)
		}
		feSlash := filepath.ToSlash(feDir)
		body := fmt.Sprintf(`version: v2
# Local TypeScript plugin (no BSR auth needed). Sourced from
# %s/node_modules/.bin/ — the workspace-layout convention picks one
# frontend to install protoc-gen-es; all frontends consume the
# generated output via the @<scope>/api workspace package.
#
# To switch to BSR-hosted plugin, replace 'local:' with
#   - remote: buf.build/bufbuild/es
plugins:
  - local: ./%s/node_modules/.bin/protoc-gen-es
    out: packages/api/src/gen
    include_imports: true
    opt:
      - target=ts
      - import_extension=.js
`, feSlash, feSlash)
		if err := os.WriteFile(bufGenPath, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write packages/api/buf.gen.yaml: %w", err)
		}
	}

	// Verify the local TS plugin exists before invoking buf — same
	// pre-flight as the per-frontend path.
	if usesLocalTSPlugin(bufGenPath) {
		pluginPath := filepath.Join(absFeDir, "node_modules", ".bin", "protoc-gen-es")
		if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
			fmt.Printf("  ⚠️  workspace TS gen: @bufbuild/protoc-gen-es not installed yet — run `pnpm install` at the project root before `forge generate`.\n")
			return nil
		}
	}

	fmt.Println("🔨 Generating TypeScript stubs into packages/api/src/gen...")
	relativeTemplate := filepath.Join("packages", "api", "buf.gen.yaml")
	args := []string{"generate", "--template", relativeTemplate}
	for _, p := range discoverProtoSubdirs(projectDir) {
		args = append(args, "--path", p)
	}

	cmd := exec.Command("buf", args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("workspace TypeScript generation failed: %w", err)
	}
	fmt.Println("  ✅ TypeScript stubs generated into packages/api/src/gen")
	return nil
}

// usesLocalTSPlugin reports whether the frontend buf.gen.yaml at path uses
// a `local:` plugin entry that points at protoc-gen-es (the default since
// the BSR removal). Best-effort — if the file is unreadable we assume yes
// to err on the side of running the existence check.
func usesLocalTSPlugin(bufGenPath string) bool {
	data, err := os.ReadFile(bufGenPath)
	if err != nil {
		return true
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Match e.g. "- local: ./frontends/web/node_modules/.bin/protoc-gen-es"
		if strings.HasPrefix(trimmed, "- local:") && strings.Contains(trimmed, "protoc-gen-es") {
			return true
		}
		if strings.HasPrefix(trimmed, "- remote:") {
			return false
		}
	}
	return false
}
