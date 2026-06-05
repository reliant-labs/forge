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
		cmd.Env = mergeEnv(os.Environ(), env)
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

// mergeEnv layers extra KEY=VALUE pairs onto a base os.Environ() slice.
// Extra wins on key conflict — the env_file is meant to be authoritative
// for the variables it declares, and the parent process's env is
// background context. Returns a fresh slice safe to assign to cmd.Env.
func mergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return append([]string(nil), base...)
	}
	out := make([]string, 0, len(base)+len(extra))
	seen := map[string]struct{}{}
	for k, v := range extra {
		seen[k] = struct{}{}
		out = append(out, k+"="+v)
	}
	for _, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		if _, dup := seen[kv[:eq]]; dup {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// readDotEnvFile parses a .env-style file into a map. Intentionally
// duplicates the small parser in internal/hostlaunch.ReadDotEnvFile —
// importing internal/hostlaunch from internal/deploytarget would invert
// the current dependency direction (hostlaunch is consumed by the CLI
// run path; deploytarget is consumed by the CLI deploy path; pulling
// hostlaunch in here would couple the two for a single helper). The
// dotenv shape is fixed and well-understood; the 20-line parser is
// cheaper than a new shared package would be.
//
// Supports: KEY=VALUE per line, # comments, blank lines, optional
// leading "export ", and a single layer of matching quotes around the
// value. Returns os.ErrNotExist (wrapped) when the file is missing so
// callers can warn-and-continue.
func readDotEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		out[k] = v
	}
	return out, nil
}

// defaultRunner is the package-level commandRunner used by the
// providers when their Runner field is nil. Tests construct providers
// with their own Runner.
var defaultRunner commandRunner = execRunner{}
