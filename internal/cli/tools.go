package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
)

// frontendTSPluginPackage is the npm package providing the local TS codegen
// binary that buf invokes via `local: ./<frontend>/node_modules/.bin/protoc-gen-es`.
const frontendTSPluginPackage = "@bufbuild/protoc-gen-es"

// frontendTSPluginRange is the devDependency range every scaffolded
// frontend's package.json declares for frontendTSPluginPackage — the major
// kept in step with @bufbuild/protobuf. It is what the undeclared-plugin
// runbook tells a user to add, so a test pins it to the templates.
const frontendTSPluginRange = "^2.5.0"

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
		Binary:        "protoc-gen-go",
		Module:        "google.golang.org/protobuf/cmd/protoc-gen-go",
		VersionModule: "google.golang.org/protobuf",
	},
	{
		Binary:        "protoc-gen-connect-go",
		Module:        "connectrpc.com/connect/cmd/protoc-gen-connect-go",
		VersionModule: "connectrpc.com/connect",
	},
	// goimports formats every Go file forge generates. It is not a buf
	// plugin, but it shapes committed output the same way: `forge generate`
	// without it skips the pass, so a regenerate on a machine that lacks it
	// (a CI runner) produces different bytes than one that has it.
	{
		Binary:        "goimports",
		Module:        "golang.org/x/tools/cmd/goimports",
		VersionModule: "golang.org/x/tools",
	},
}

type protoTool struct {
	Binary string
	Module string
	// VersionModule is the module whose version go.mod resolves for this
	// tool. protoc-gen-go's output names its own version in every file it
	// writes, so installing the version the project's runtime library is
	// pinned at is what makes a regenerate reproduce the committed bytes.
	VersionModule string
}

// resolveToolVersion picks the version to `go install` for a tool: an
// explicit --version wins; otherwise the version projectDir's go.mod
// resolves for the tool's module (the one the committed code was generated
// against), and `latest` only when the module graph does not contain it.
func resolveToolVersion(ctx context.Context, projectDir string, t protoTool, override string) string {
	if override != "" {
		return override
	}
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Version}}", t.VersionModule)
	cmd.Dir = projectDir
	// The module graph CI sees: a developer's go.work may bridge a local
	// checkout, which is not what the committed stubs were built against.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if v := strings.TrimSpace(string(out)); err == nil && v != "" {
		return v
	}
	return "latest"
}

func newToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Manage developer tooling forge depends on (proto plugins, etc.)",
		Long: `Manage developer tooling that forge expects on PATH but does not ship.

Subcommands:
  install   Install the codegen tools forge runs (protoc-gen-go,
            protoc-gen-connect-go, goimports) via 'go install', at the
            versions this project's go.mod resolves, and check that every
            frontend declares its TypeScript plugin.

Forge scaffolds buf.gen.yaml with 'local:' plugins by default so that
'forge generate' works without any BSR (buf.build) authentication.
Those local plugins must be on PATH; this command installs them.`,
	}

	cmd.AddCommand(newToolsInstallCmd())

	return cmdutil.StrictGroup(cmd)
}

func newToolsInstallCmd() *cobra.Command {
	var (
		version string
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install codegen tools (protoc-gen-go, protoc-gen-connect-go, goimports)",
		Long: `Install the codegen tools forge needs on PATH for the default
local:-plugin buf.gen.yaml workflow, plus goimports, which formats every Go
file forge generates.

Each tool is installed at the version this project's go.mod resolves for it
(google.golang.org/protobuf, connectrpc.com/connect, golang.org/x/tools) —
the version the committed generated code was produced with — falling back to
@latest when go.mod does not contain the module. --version overrides that for
every tool.

By default, tools already present on PATH are skipped. Use --force to
re-install them.

The TypeScript plugin, @bufbuild/protoc-gen-es, is not installed here. It
is an ordinary devDependency of each frontend, so the frontend's own
'npm ci' installs it at the version its lockfile pins. This command only
checks that every frontend whose buf.gen.yaml runs it declares it, and
fails with the edit to make when one does not. It never runs npm and never
writes a frontend's package.json or package-lock.json — with or without
--force.

Examples:
  forge tools install
  forge tools install --version v1.34.2
  forge tools install --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			goErr := runToolsInstall(cmd.Context(), version, force)
			return errors.Join(goErr, checkFrontendTSPlugins())
		},
	}

	cmd.Flags().StringVar(&version, "version", "", "Version passed to 'go install' for every tool (e.g. latest, v1.34.2); default: the version go.mod resolves, else latest")
	cmd.Flags().BoolVar(&force, "force", false, "Reinstall the Go tools even when already on PATH (never touches a frontend's npm dependencies)")

	return cmd
}

// checkFrontendTSPlugins checks the TypeScript plugin of every frontend in
// the enclosing forge project. Outside a project there is nothing to check.
func checkFrontendTSPlugins() error {
	configPath, err := findProjectConfigFile()
	if errors.Is(err, ErrProjectConfigNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	store, err := loadProjectStoreFrom(configPath)
	if err != nil {
		return err
	}
	return checkFrontendTSPluginsIn(filepath.Dir(configPath), store.Config().Frontends)
}

// checkFrontendTSPluginsIn verifies that each frontend whose buf.gen.yaml
// runs the local protoc-gen-es DECLARES it, and installs nothing.
//
// It used to run `npm install --save-dev @bufbuild/protoc-gen-es` — in every
// frontend under --force, which the scaffolded verify-generated job passes.
// npm re-resolves the whole tree when it saves, so on a Linux runner it
// rewrote a lockfile a macOS npm had written (dropping its "libc" entries),
// and verify-generated reported the user's package-lock.json as generated
// code drift. Committing either OS's lockfile only moved the failure to the
// other OS. A frontend's manifest and lockfile are the user's: a declared
// plugin is installed by the frontend's own `npm ci`, and an undeclared one
// is an edit for the user to make and commit, not one forge makes for them.
func checkFrontendTSPluginsIn(root string, frontends []config.FrontendConfig) error {
	var undeclared []string
	for _, feDir := range localTSPluginFrontendDirs(root, frontends) {
		manifest := feDir + "/package.json"
		declared, err := declaresTSPlugin(filepath.Join(root, manifest))
		if err != nil {
			return fmt.Errorf("read %s: %w", manifest, err)
		}
		if !declared {
			undeclared = append(undeclared, manifest)
			continue
		}
		// Both npm layouts count: under forge's dev workspace bridge npm
		// hoists the plugin to <project>/node_modules and creates no
		// frontend-local node_modules at all. resolveLocalTSPluginRel is
		// the resolver the buf pass uses, so the two cannot disagree.
		if _, ok := resolveLocalTSPluginRel(root, feDir); ok {
			fmt.Printf("✅ %-26s declared and installed for %s/\n", "protoc-gen-es", feDir)
		} else {
			fmt.Printf("ℹ️  %-26s declared in %s, not installed yet — `npm ci` in %s/ installs it\n", "protoc-gen-es", manifest, feDir)
		}
	}
	if len(undeclared) == 0 {
		return nil
	}
	verb := "does not declare"
	if len(undeclared) > 1 {
		verb = "do not declare"
	}
	return cliutil.UserErr("forge tools install",
		fmt.Sprintf("%s %s %s, the plugin its buf.gen.yaml runs to generate TypeScript stubs "+
			"(forge never edits a frontend's package.json or lockfile, so it will not add it)",
			strings.Join(undeclared, ", "), verb, frontendTSPluginPackage),
		"",
		fmt.Sprintf("add %q: %q to devDependencies in %s, run `npm install` beside it, "+
			"and commit package.json together with package-lock.json",
			frontendTSPluginPackage, frontendTSPluginRange, strings.Join(undeclared, " and ")))
}

// declaresTSPlugin reports whether the package.json at path lists the
// protoc-gen-es package as a dependency of either kind — both are installed
// by `npm ci`. A missing package.json declares nothing.
func declaresTSPlugin(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var manifest struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, err
	}
	_, dev := manifest.DevDependencies[frontendTSPluginPackage]
	_, prod := manifest.Dependencies[frontendTSPluginPackage]
	return dev || prod, nil
}

// runToolsInstall installs the required proto plugins. Returns the first
// install error (if any) but always tries every tool so users see the
// full picture.
func runToolsInstall(ctx context.Context, version string, force bool) error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("'go' not found on PATH — install Go before running '%s tools install'", Name())
	}

	var firstErr error
	for _, t := range requiredProtoTools {
		if !force {
			if _, err := exec.LookPath(t.Binary); err == nil {
				fmt.Printf("✅ %-26s already installed (use --force to reinstall)\n", t.Binary)
				continue
			}
		}

		spec := t.Module + "@" + resolveToolVersion(ctx, ".", t, version)
		fmt.Printf("📦 Installing %-26s (go install %s)\n", t.Binary, spec)
		out, err := exec.CommandContext(ctx, "go", "install", spec).CombinedOutput()
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
	fmt.Println("✅ All required codegen tools installed.")
	return nil
}
