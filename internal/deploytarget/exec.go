package deploytarget

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/reliant-labs/forge/internal/envutil"
)

// ExpandVars substitutes ${KEY} (and $KEY) tokens in a command-string
// template against the provided map. Unknown keys are left empty —
// matches os.Expand's default behaviour and keeps the surprise floor
// low (a typo in the template surfaces as a missing flag rather than
// a leaked `${IMAGE}` literal landing on the remote shell).
//
// Intended for any user-supplied shell-command template forge runs
// via `sh -c` after substituting a documented set of tokens:
//
//   - External deploy: DeployCmd / RollbackCmd / HealthCmd, where the
//     kcl/schema.k contract advertises ${IMAGE} / ${TAG} /
//     ${CODE_VERSION} / ${PIPELINE} / ${LAST_TAG} / ${SERVICE} / ${ENV}
//     / ${ENV_FILE} / ${PROJECT_DIR}.
//   - Service.build_cmd: the build-side escape hatch, where the
//     contract advertises ${IMAGE} / ${TAG} / ${SERVICE} / ${TARGETARCH}
//     / ${REGISTRY} / ${PROJECT_DIR} / ${BUILD_CWD} + keys from
//     `build_env`. See internal/buildtarget for the build-side
//     consumer.
//
// Exported so the build-side runner can use the same substitution
// semantics the deploy-side External provider uses — one mental model
// across both the build and deploy escape hatches.
func ExpandVars(template string, vars map[string]string) string {
	return os.Expand(template, func(key string) string {
		return vars[key]
	})
}

// expandVars is the unexported alias kept for callers inside this
// package (external.go, compose.go) so the existing call sites stay
// untouched while public consumers get the canonical [ExpandVars]
// name.
func expandVars(template string, vars map[string]string) string {
	return ExpandVars(template, vars)
}

// commandRunner is the indirection point the providers use to run
// external commands (sh, docker, docker-compose, user-supplied CLIs).
// Tests swap in a fake that records calls and returns canned output;
// the production implementation shells out via os/exec.
//
// Run streams stdout/stderr to the parent process. RunWithEnv layers
// the supplied key=value map onto os.Environ() before exec (used to
// thread `env_file` contents into the child). Output captures combined
// output (used for the health-check checks that need to inspect the
// result).
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the production commandRunner. Run pipes through to
// the parent stdout/stderr so users see deploy / docker progress in
// real time; Output captures combined stdout+stderr for inspection.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	return execRunner{}.RunWithEnv(ctx, nil, name, args...)
}

func (execRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		cmd.Env = envutil.MergeExtraWins(os.Environ(), env)
	}
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
