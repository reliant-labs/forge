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
// template against the provided map. ONLY keys present in vars are
// substituted — the documented token set plus the keys the user
// declared in `env`/`build_env`. Every other `$X` / `${X}` is left
// byte-for-byte intact for the shell that runs the command.
//
// It used to be os.Expand, which substitutes EVERY reference and maps
// unknown ones to "". That silently emptied the script's own shell
// variables: `W=$(mktemp -d); … "$W/root"` reached `sh -c` as
// `W=$(mktemp -d); … "/root"`, and `$HOME` / `$PATH` vanished the same
// way. A template is a shell program with a few forge tokens in it, so
// the shell must see everything forge does not own.
//
// A present key with an empty value (e.g. ${LAST_TAG} on a first
// deploy) still substitutes to "" — the token is forge's, and empty is
// its value.
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
	var out strings.Builder
	out.Grow(len(template))
	for i := 0; i < len(template); {
		if template[i] != '$' || i+1 >= len(template) {
			out.WriteByte(template[i])
			i++
			continue
		}
		// ${NAME}
		if template[i+1] == '{' {
			end := strings.IndexByte(template[i+2:], '}')
			if end < 0 {
				out.WriteString(template[i:])
				break
			}
			name := template[i+2 : i+2+end]
			ref := template[i : i+3+end]
			if value, ok := vars[name]; ok {
				out.WriteString(value)
			} else {
				out.WriteString(ref)
			}
			i += len(ref)
			continue
		}
		// $NAME — the longest shell identifier, as sh itself reads it, so
		// `$IMAGE_NAME` is the script's variable and never ${IMAGE}+"_NAME".
		j := i + 1
		for j < len(template) && isShellIdentByte(template[j], j == i+1) {
			j++
		}
		if j == i+1 {
			// `$$`, `$1`, `$@`, `$(…)`: shell syntax forge never owns.
			out.WriteByte('$')
			i++
			continue
		}
		name := template[i+1 : j]
		if value, ok := vars[name]; ok {
			out.WriteString(value)
		} else {
			out.WriteString(template[i:j])
		}
		i = j
	}
	return out.String()
}

// isShellIdentByte reports whether c can appear in a shell variable name
// at this position (a leading digit is a positional parameter, not a name).
func isShellIdentByte(c byte, first bool) bool {
	switch {
	case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return !first
	}
	return false
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
// result); OutputWithEnv does both.
//
// OutputWithEnv exists because a compose subcommand that INSPECTS
// (`compose ps`) re-reads and re-interpolates the compose file exactly
// like one that ACTS (`compose up`) — so it needs the same overlay, or it
// resolves a different config than the one just deployed.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	OutputWithEnv(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error)
}

// outputWithEnv runs an output-capturing command with an env overlay,
// falling through to the plain Output when there is nothing to overlay so
// the common (no-env) call keeps its existing shape in test recordings.
func outputWithEnv(ctx context.Context, runner commandRunner, env map[string]string, name string, args ...string) ([]byte, error) {
	if len(env) == 0 {
		return runner.Output(ctx, name, args...)
	}
	return runner.OutputWithEnv(ctx, env, name, args...)
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
	return execRunner{}.OutputWithEnv(ctx, nil, name, args...)
}

func (execRunner) OutputWithEnv(ctx context.Context, env map[string]string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if len(env) > 0 {
		cmd.Env = envutil.MergeExtraWins(os.Environ(), env)
	}
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return buf.Bytes(), nil
}

// defaultRunner is the package-level commandRunner used by the
// providers when their Runner field is nil. Tests construct providers
// with their own Runner.
var defaultRunner commandRunner = execRunner{}
