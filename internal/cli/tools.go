package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// frontendTSPluginPackage is the npm package providing the local TS codegen
// binary that buf invokes via `local: ./<frontend>/node_modules/.bin/protoc-gen-es`.
// Pin the major to keep parity with @bufbuild/protobuf in scaffolded
// package.json templates (currently ^2.5.0).
const frontendTSPluginPackage = "@bufbuild/protoc-gen-es"

// requiredProtoTools lists the proto codegen plugins forge expects on PATH
// when buf.gen.yaml uses `local:` plugins (the default since the BSR-auth
// fix). Both packages produce binaries whose names match their `cmd/` dir.
//
// Keep this list aligned with:
//   - internal/templates/project/buf.gen.yaml (template default)
//   - internal/cli/generate_buf.go writeDefaultBufGenYaml (runtime fallback)
//   - internal/templates/project/Taskfile.yml.tmpl (preflight checks)
//   - scripts/bootstrap.sh (devcontainer bootstrap)
var requiredProtoTools = []protoTool{
	{
		Binary: "protoc-gen-go",
		Module: "google.golang.org/protobuf/cmd/protoc-gen-go",
	},
	{
		Binary: "protoc-gen-connect-go",
		Module: "connectrpc.com/connect/cmd/protoc-gen-connect-go",
	},
}

type protoTool struct {
	Binary string
	Module string
}

func newToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Manage developer tooling forge depends on (proto plugins, etc.)",
		Long: `Manage developer tooling that forge expects on PATH but does not ship.

Subcommands:
  install   Install required proto codegen plugins (protoc-gen-go,
            protoc-gen-connect-go) via 'go install'.

Forge scaffolds buf.gen.yaml with 'local:' plugins by default so that
'forge generate' works without any BSR (buf.build) authentication.
Those local plugins must be on PATH; this command installs them.`,
	}

	cmd.AddCommand(newToolsInstallCmd())

	return cmd
}

func newToolsInstallCmd() *cobra.Command {
	var (
		version string
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install proto codegen plugins (protoc-gen-go, protoc-gen-connect-go)",
		Long: `Install the proto codegen plugins forge needs on PATH for the default
local:-plugin buf.gen.yaml workflow.

By default, plugins already present on PATH are skipped. Use --force to
re-install at the requested version (default: @latest).

Examples:
  forge tools install
  forge tools install --version v1.34.2
  forge tools install --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runToolsInstall(version, force); err != nil {
				return err
			}
			// Also install the local TS plugin in any scaffolded frontend
			// dirs we can find under cwd. Best-effort — never fatal because
			// many forge projects (cli/library kinds) have no frontends.
			installFrontendTSPlugin(".", force)
			return nil
		},
	}

	cmd.Flags().StringVar(&version, "version", "latest", "Version suffix passed to 'go install' (e.g. latest, v1.34.2)")
	cmd.Flags().BoolVar(&force, "force", false, "Reinstall even when the binary is already on PATH")

	return cmd
}

// installFrontendTSPlugin walks <projectDir>/frontends/* and runs
// `npm install --save-dev @bufbuild/protoc-gen-es` in each frontend that
// has a package.json but no node_modules/.bin/protoc-gen-es. Skips silently
// when npm is not on PATH (with a one-line message) so this doesn't block
// users on no-frontend projects.
func installFrontendTSPlugin(projectDir string, force bool) {
	frontendsDir := filepath.Join(projectDir, "frontends")
	entries, err := os.ReadDir(frontendsDir)
	if err != nil {
		// No frontends/ dir → nothing to do, no message needed.
		return
	}

	// At least one frontend dir present — check npm before iterating.
	if _, err := exec.LookPath("npm"); err != nil {
		fmt.Println("ℹ️  Skipping frontend TS plugin install — `npm` not on PATH.")
		fmt.Println("    Install Node.js + npm, then run `npm install` in each frontends/<name>/.")
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		feDir := filepath.Join(frontendsDir, entry.Name())
		pkgJSON := filepath.Join(feDir, "package.json")
		if _, err := os.Stat(pkgJSON); err != nil {
			continue
		}
		pluginBin := filepath.Join(feDir, "node_modules", ".bin", "protoc-gen-es")
		if !force {
			if _, err := os.Stat(pluginBin); err == nil {
				fmt.Printf("✅ %-26s already installed in frontends/%s/ (use --force to reinstall)\n", "protoc-gen-es", entry.Name())
				continue
			}
		}
		fmt.Printf("📦 Installing %-26s in frontends/%s/ (npm install --save-dev %s)\n", "protoc-gen-es", entry.Name(), frontendTSPluginPackage)
		cmd := exec.Command("npm", "install", "--save-dev", "--no-audit", "--no-fund", frontendTSPluginPackage)
		cmd.Dir = feDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  npm install in frontends/%s/ failed: %v\n", entry.Name(), err)
			continue
		}
		if _, err := os.Stat(pluginBin); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  Installed %s but %s not found.\n", frontendTSPluginPackage, pluginBin)
			continue
		}
		fmt.Printf("  ✅ protoc-gen-es installed in frontends/%s/\n", entry.Name())
	}
}

// runToolsInstall installs the required proto plugins. Returns the first
// install error (if any) but always tries every tool so users see the
// full picture.
func runToolsInstall(version string, force bool) error {
	if version == "" {
		version = "latest"
	}

	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("'go' not found on PATH — install Go before running '%s tools install'", CLIName())
	}

	var firstErr error
	for _, t := range requiredProtoTools {
		if !force {
			if _, err := exec.LookPath(t.Binary); err == nil {
				fmt.Printf("✅ %-26s already installed (use --force to reinstall)\n", t.Binary)
				continue
			}
		}

		spec := t.Module + "@" + version
		fmt.Printf("📦 Installing %-26s (go install %s)\n", t.Binary, spec)
		out, err := exec.Command("go", "install", spec).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ❌ go install %s failed: %v\n", spec, err)
			if len(out) > 0 {
				fmt.Fprintln(os.Stderr, string(out))
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("install %s: %w", t.Binary, err)
			}
			continue
		}
		// Verify it landed on PATH (catches GOBIN/GOPATH-not-on-PATH).
		if _, err := exec.LookPath(t.Binary); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠️  Installed %s but it is not on PATH. Add $(go env GOBIN) (or $(go env GOPATH)/bin if GOBIN is unset) to PATH.\n", t.Binary)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s installed but not on PATH", t.Binary)
			}
			continue
		}
		fmt.Printf("  ✅ %s installed\n", t.Binary)
	}

	if firstErr != nil {
		return firstErr
	}
	fmt.Println()
	fmt.Println("✅ All required proto plugins installed.")
	return nil
}
