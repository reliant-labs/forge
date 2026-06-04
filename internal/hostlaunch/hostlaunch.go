// Package hostlaunch composes exec.Cmds for host-mode services and
// frontends, plus the small env-file helpers both call sites need.
//
// Two CLI surfaces target the same dispatch matrix:
//
//   - `forge run <svc>` — single-service host runner (foreground or
//     background; backed by a per-service PID file).
//   - `forge up` host phase — N-service loop that hangs the cmds off
//     a process registry for cascade teardown.
//
// Both pick a runner (go-run / air / binary / delve), default the env
// file to `.env.<env>`, and layer the env-file values onto the child
// process. Before this package existed, the dispatch was duplicated
// across `internal/cli/run.go` (buildRunHostCmd / runHostService) and
// `internal/cli/up.go` (buildHostServiceCmd / upHostServices) — same
// runner table, two implementations.
//
// The package intentionally does NOT own the process lifecycle:
//
//   - foreground stream-prefix + signal handling lives in the single-
//     service `forge run` path because it has different semantics
//     (one process, persistent PID file, `stop` subcommand);
//   - the N-process registry that `forge up` uses for cascade
//     teardown stays in `internal/cli/up.go` for the same reason.
//
// What's shared here is the pure command-construction matrix plus the
// minimal env-file parser. Anything that tracks PIDs / streams output
// / handles signals stays at the call site.
package hostlaunch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Defaults pinned by the runner dispatch. Exported so tests and CLI
// help text can reference them without re-deriving the magic numbers.
const (
	// DefaultDelvePort is the dlv --listen=:<port> default when KCL
	// doesn't pin one explicitly. Matches the historical
	// `forge run --debug` shape.
	DefaultDelvePort = 2345

	// DefaultAirConfig is the `air -c <path>` default when KCL doesn't
	// pin one explicitly. Mirrors the `air` tool's own default —
	// `.air.toml` at the project root.
	DefaultAirConfig = ".air.toml"
)

// RunnerSpec is the dispatch input. The three Runner/AirConfig/DelvePort
// fields mirror the KCL HostDeploy block; env composition (env_vars
// from KCL + an optional gitignored secrets dotenv) is layered by the
// caller via [LoadSecretsFile] and [MergeEnv] so this package stays
// vendor-neutral about how config gets sourced.
//
// An empty Runner falls through to the legacy go-run shape so projects
// that haven't migrated to the deploy module yet keep working.
type RunnerSpec struct {
	Runner    string // "" | "go-run" | "air" | "binary" | "delve"
	AirConfig string // path relative to project root; default DefaultAirConfig
	DelvePort int    // dlv --listen=:<port>; default DefaultDelvePort
}

// BuildCmd composes the *exec.Cmd for a host-mode service.
//
// Runner dispatch:
//   - air:    `air -c <spec.AirConfig|.air.toml>`
//   - binary: `./bin/<name>`
//   - delve:  `dlv exec --headless --listen=:<port> ... ./bin/<name>`
//   - default ("" / "go-run" / unknown): `go run ./cmd server <name>`
//
// Unknown runners fall through to go-run rather than erroring — this
// preserves the `forge run` behaviour and prevents a typo in KCL from
// hard-failing the host phase. Callers that want strict matching
// (the up orchestrator does) should check `IsKnownRunner(spec.Runner)`
// first and report their own error.
func BuildCmd(ctx context.Context, name string, spec RunnerSpec) *exec.Cmd {
	runner := strings.TrimSpace(spec.Runner)
	switch runner {
	case "air":
		cfg := spec.AirConfig
		if cfg == "" {
			cfg = DefaultAirConfig
		}
		return exec.CommandContext(ctx, "air", "-c", cfg)
	case "binary":
		return exec.CommandContext(ctx, "./bin/"+name)
	case "delve":
		port := spec.DelvePort
		if port <= 0 {
			port = DefaultDelvePort
		}
		return exec.CommandContext(ctx, "dlv", "exec", "--headless",
			fmt.Sprintf("--listen=:%d", port),
			"--api-version=2", "--accept-multiclient",
			"--continue", "./bin/"+name)
	default:
		// "go-run" or "" or unknown — the legacy shape.
		return exec.CommandContext(ctx, "go", "run", "./cmd", "server", name)
	}
}

// IsKnownRunner reports whether the runner name is one of the
// explicitly-supported dispatch keys. Callers that want to refuse
// unknown runners (rather than silently fall through to go-run) check
// this before BuildCmd.
func IsKnownRunner(runner string) bool {
	switch strings.TrimSpace(runner) {
	case "", "go-run", "air", "binary", "delve":
		return true
	}
	return false
}

// LoadSecretsFile reads a gitignored secrets dotenv into a map. Returns
// (nil, nil) when path is empty so the caller can unconditionally call
// this. Missing-file is logged via the returned warn-only error wrapping
// os.ErrNotExist; permission / parse errors propagate.
//
// Distinct from the legacy "env_file" load: the secrets-file contract
// is "if present, layer first; KCL env_vars override on conflict" — see
// [LayerHostEnv] for the composition.
func LoadSecretsFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	out, err := ReadDotEnvFile(path)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LayerHostEnv composes the env for a host-mode subprocess.
//
//	final = base ⊕ projectConfig ⊕ secrets ⊕ envVars
//
// Order (each later layer overrides earlier on key conflict among the
// three map layers — base os.Environ() always wins so a developer's
// shell override beats them all):
//
//  1. base — the parent process env (typically os.Environ()). Wins last.
//  2. projectConfig — forge.yaml `environments[<env>].config` projected
//     to env-var strings. Same non-secret values cluster-mode services
//     see via the ConfigMap projection, layered here so host-mode
//     services don't drift from cluster-mode behavior. Lowest precedence
//     among the extra layers: secrets and envVars both override on
//     conflict because dev-local overrides (secrets) and KCL pins
//     (envVars) are more specific.
//  3. secrets — KEY=VALUE pairs from the gitignored secrets_file
//     (`.env.<env>`). Wins over projectConfig so a developer can
//     override forge.yaml values locally without editing committed
//     config.
//  4. envVars — KCL-declared per-env config. Wins over secrets so
//     reproducible per-env config can't drift across machines.
//
// Returns a fresh []string safe to assign to cmd.Env. A nil
// projectConfig is treated as an empty layer so the pre-extension
// callers stay terse.
func LayerHostEnv(base []string, projectConfig, secrets, envVars map[string]string) []string {
	// Build the merged extra map in precedence order: projectConfig
	// first, then secrets on top, then envVars on top of that.
	extra := make(map[string]string, len(projectConfig)+len(secrets)+len(envVars))
	for k, v := range projectConfig {
		extra[k] = v
	}
	for k, v := range secrets {
		extra[k] = v
	}
	for k, v := range envVars {
		extra[k] = v
	}
	return MergeEnv(extra, base)
}

// PIDPath returns the canonical per-service PID file path:
//
//	$HOME/.cache/forge/run/<service>.pid
//
// Canonical convention. Used by `forge run <svc>` (foreground cleanup
// + background detach + stop subcommand). `forge up` uses its own
// per-env state file under $HOME/.cache/forge/up/<env>.pids because it
// tracks N processes (services + frontends + port-forwards) and the
// per-env grouping is the unit of teardown there; the two conventions
// coexist deliberately.
func PIDPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "forge", "run", name+".pid"), nil
}

// ReadDotEnvFile parses a .env file (KEY=VALUE per line, # comments,
// trailing whitespace trimmed) into a map. Quoted values
// ("VALUE", 'VALUE') have their outer quotes stripped. Missing file
// returns os.ErrNotExist so callers can treat absence as non-fatal.
//
// Intentionally minimal — we don't expand $VARS or `${VAR:-default}`
// shell features. Projects needing those should use direnv or a wrapper
// script; this helper is just enough for the common
// "DATABASE_URL=postgres://..." case the host-mode loop needs.
func ReadDotEnvFile(path string) (map[string]string, error) {
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
		// strip an optional leading "export ".
		line = strings.TrimPrefix(line, "export ")
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		// Strip a single layer of matching quotes, if present.
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		out[k] = v
	}
	return out, nil
}

// MergeEnv layers per-env config onto a base os.Environ() slice. Keys
// already present in base are kept (so a developer's shell override
// always wins). Returns a fresh slice safe to assign to cmd.Env.
func MergeEnv(extra map[string]string, base []string) []string {
	have := map[string]struct{}{}
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			have[kv[:i]] = struct{}{}
		}
	}
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	for k, v := range extra {
		if _, exists := have[k]; exists {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}
