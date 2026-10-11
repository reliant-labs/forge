package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/goexec"
)

// The "generate hooks (forge.yaml)" step: the project's own commands, run
// after forge's code generation and before the go mod tidy + go build that
// validate it (see config.GenerateHook for the contract).

// hasGenerateHooks gates the step on the project declaring any hook.
func hasGenerateHooks(ctx *pipelineContext) bool {
	return ctx.Cfg != nil && len(ctx.Cfg.Generate.Hooks) > 0
}

// writesGenerateHooks declares every hook's `writes:` paths, so the pipeline
// journals them before the step runs and a failed generate restores them.
func writesGenerateHooks(ctx *pipelineContext) []string {
	if ctx.Cfg == nil {
		return nil
	}
	var out []string
	for _, h := range ctx.Cfg.Generate.Hooks {
		for _, w := range h.Writes {
			if p := cleanProjectRel(w); p != "" {
				out = append(out, p)
			}
		}
	}
	return dedupeSorted(out)
}

func stepGenerateHooks(ctx *pipelineContext) error {
	return runGenerateHooks(ctx.ProjectDir, ctx.Cfg.Generate.Hooks, os.Stdout, os.Stderr)
}

// runGenerateHooks runs hooks in declaration order and stops at the first
// failure. A failed hook fails the generate: its output is now stale or
// partial, and the validation build that follows would report the symptom
// somewhere far from the cause.
func runGenerateHooks(projectDir string, hooks []config.GenerateHook, stdout, stderr io.Writer) error {
	for _, h := range hooks {
		dir := projectDir
		if h.Dir != "" {
			dir = filepath.Join(projectDir, filepath.FromSlash(h.Dir))
		}
		argv := strings.Join(h.Command, " ")
		fmt.Fprintf(stdout, "🪝 generate hook %q: %s\n", h.Name, argv)
		cmd := exec.Command(h.Command[0], h.Command[1:]...)
		cmd.Dir = dir
		cmd.Env = goexec.Env()
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return fmt.Errorf("generate hook %q: %q was not found on PATH — install it, or remove the hook from forge.yaml generate.hooks", h.Name, h.Command[0])
			}
			return fmt.Errorf("generate hook %q (%s) failed: %w", h.Name, argv, err)
		}
	}
	return nil
}
