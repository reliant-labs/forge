package deploytarget

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// expandVars substitutes ${KEY} (and $KEY) tokens in a command-string
// template against the provided map. Unknown keys are left empty —
// matches os.Expand's default behaviour and keeps the surprise floor
// low (a typo in the template surfaces as a missing flag rather than
// a leaked `${IMAGE}` literal landing on the remote shell).
//
// Intended for the user-supplied DeployCmd / RollbackCmd / HealthCmd
// strings on External, where the kcl/schema.k contract advertises
// ${IMAGE} / ${TAG} / ${LAST_TAG} / ${SERVICE} / ${ENV} / ${ENV_FILE}
// / ${PROJECT_DIR}.
func expandVars(template string, vars map[string]string) string {
	return os.Expand(template, func(key string) string {
		return vars[key]
	})
}

// commandRunner is the indirection point the providers use to run
// external commands (sh, docker, docker-compose, user-supplied CLIs).
// Tests swap in a fake that records calls and returns canned output;
// the production implementation shells out via os/exec.
//
// Run streams stdout/stderr to the parent process. Output captures
// combined output (used for the health-check checks that need to
// inspect the result).
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the production commandRunner. Run pipes through to
// the parent stdout/stderr so users see deploy / docker progress in
// real time; Output captures combined stdout+stderr for inspection.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return buf.Bytes(), nil
}

// defaultRunner is the package-level commandRunner used by the
// providers when their Runner field is nil. Tests construct providers
// with their own Runner.
var defaultRunner commandRunner = execRunner{}
