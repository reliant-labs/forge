// `forge fmt` runs goimports across the project with `-local` set to
// the project's module path so project-local imports group separately
// from stdlib and third-party. Without `-local` the canonical Go three-
// group layout collapses to two — project-local imports get sorted
// alongside third-party, and the import block stops being a
// hand-skimmable boundary between "third-party code I should pin" and
// "first-party code I own."
//
// FRICTION 2026-06-02: cp-forge polish pass. The codegen pipeline
// already runs goimports with `-local` via stepGoimports, but
// hand-written files (anything outside the codegen targets) only get
// formatted when the user remembers the right command. Surfacing the
// behavior under `forge fmt` makes the canonical formatting one
// invocation.
//
// The command resolves `-local` lazily:
//
//  1. forge.yaml's module_path field if set (project-config-first).
//  2. go.mod's `module` directive (the codegen.GetModulePath fallback).
//  3. Empty → goimports runs without `-local` (a warning is printed).
//
// Targets default to the conventional forge directories (`cmd/`,
// `pkg/`, `gen/`, `handlers/`, `internal/`) but the user can pass
// explicit paths to scope a quick re-format (`forge fmt internal/foo`).

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/codegen"
)

// newFmtCmd registers the `forge fmt` command. The command is a thin
// wrapper over goimports — it resolves the project's module path and
// invokes the tool with the right `-local` and `-w` flags. Failure
// modes (goimports missing, module-path unresolvable) are surfaced as
// warnings, not errors, so the command is safe to wire into pre-commit
// hooks.
func newFmtCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fmt [paths...]",
		Short: "Run goimports with the project's module path as `-local`",
		Long: "Run goimports across the project, with -local set to the project\n" +
			"module path so first-party imports group separately from stdlib and\n" +
			"third-party. With no arguments, formats the conventional forge\n" +
			"directories (cmd, pkg, gen, handlers, internal). Pass paths to scope.\n" +
			"\n" +
			"The module path is derived from forge.yaml's module_path field if set,\n" +
			"falling back to the `module` directive in go.mod.\n" +
			"\n" +
			"Examples:\n" +
			"  forge fmt                       # format conventional forge dirs\n" +
			"  forge fmt internal/foo          # format a single directory\n" +
			"  forge fmt internal/foo bar.go   # format a directory + a file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFmt(cmd.Context(), args)
		},
	}
}

// runFmt is the command body. It is exported as a package-private
// helper so the unit test can drive it without going through cobra's
// argument-parsing layer.
func runFmt(ctx context.Context, paths []string) error {
	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	modulePath := resolveFmtModulePath(projectDir)

	if len(paths) == 0 {
		paths = defaultFmtTargets(projectDir)
		if len(paths) == 0 {
			fmt.Println("forge fmt: no default targets found (cmd/, pkg/, gen/, handlers/, internal/); pass explicit paths to format")
			return nil
		}
	}

	return runGoimportsFmt(ctx, projectDir, modulePath, paths)
}

// resolveFmtModulePath returns the `-local` value derived first from
// forge.yaml's module_path, then from go.mod. Returns "" when neither
// is resolvable — the caller passes "" through to runGoimportsFmt
// which logs a warning and runs without `-local`.
func resolveFmtModulePath(projectDir string) string {
	// forge.yaml first — it's the project-authoritative module path. A
	// missing forge.yaml (CLI projects with no .yaml) is fine; the
	// go.mod fallback covers that case.
	cfg, err := loadProjectConfig()
	if err == nil && cfg != nil && cfg.ModulePath != "" {
		return cfg.ModulePath
	}
	if err != nil && !errors.Is(err, ErrProjectConfigNotFound) {
		// Surface the config-load error as a warning so the user knows
		// the fallback path was taken, but don't abort — goimports
		// without -local still produces useful output.
		fmt.Fprintf(os.Stderr, "warning: forge.yaml load failed (%v); falling back to go.mod\n", err)
	}

	modPath, err := codegen.GetModulePath(projectDir)
	if err != nil {
		return ""
	}
	return modPath
}

// defaultFmtTargets returns the subset of conventional forge directories
// that exist in projectDir. Skipping non-existent paths keeps goimports
// from erroring on a library project that has no `handlers/`, etc.
func defaultFmtTargets(projectDir string) []string {
	candidates := []string{"cmd", "pkg", "gen", "handlers", "internal"}
	var out []string
	for _, c := range candidates {
		if dirExists(filepath.Join(projectDir, c)) {
			out = append(out, c)
		}
	}
	return out
}

// runGoimportsFmt invokes goimports with the resolved module path and
// targets. Returns nil with a stderr warning when goimports isn't on
// PATH; pre-commit hooks shouldn't fail on a missing tool — the user
// gets the install hint instead.
func runGoimportsFmt(ctx context.Context, projectDir, modulePath string, targets []string) error {
	goimportsPath, err := exec.LookPath("goimports")
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: goimports not found on PATH — skipping format")
		fmt.Fprintln(os.Stderr, "        install with: go install golang.org/x/tools/cmd/goimports@latest")
		return nil
	}

	args := []string{"-w"}
	if modulePath != "" {
		args = append([]string{"-local", modulePath}, args...)
	} else {
		fmt.Fprintln(os.Stderr, "warning: could not derive module path (no forge.yaml module_path, no go.mod module directive); running goimports without -local")
	}
	args = append(args, targets...)

	cmd := exec.CommandContext(ctx, goimportsPath, args...)
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("goimports failed: %w", err)
	}
	return nil
}
