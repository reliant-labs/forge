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

	"github.com/reliant-labs/forge/internal/envutil"
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

// RunnerSpec is the dispatch input. The Runner/AirConfig/DelvePort
// fields mirror the KCL HostDeploy block; env composition (env_vars
// from KCL + an optional gitignored secrets dotenv) is layered by the
// caller via [LoadSecretsFile] and [LayerHostEnv] so this package stays
// vendor-neutral about how config gets sourced.
//
// An empty Runner falls through to the legacy go-run shape so projects
// that haven't migrated to the deploy module yet keep working.
//
// WorkingDir + ProjectDir control the subprocess's working directory.
// When WorkingDir is empty, the subprocess inherits the parent's cwd
// (the project root, where forge was invoked). When WorkingDir is set:
//
//   - absolute paths are used as-is;
//   - relative paths resolve against ProjectDir (which the CLI sets to
//     the forge project root).
//
// The cross-repo Air case is the load-bearing example: a forge project
// declares `WorkingDir: "../sibling-repo"` so an Air config that lives
// in the sibling repo and references build paths relative to ITS own
// repo root resolves correctly even though forge itself runs from the
// caller's project root.
type RunnerSpec struct {
	Runner    string // "" | "go-run" | "air" | "binary" | "delve"
	AirConfig string // path relative to project root; default DefaultAirConfig
	DelvePort int    // dlv --listen=:<port>; default DefaultDelvePort

	// Command, when non-empty, is run verbatim (Command[0] + args) instead
	// of any runner convention — the escape hatch for host services whose
	// entrypoint doesn't fit `go run ./cmd server <name>`. The canonical
	// case is a sibling-repo binary: pair it with WorkingDir so the
	// command's own relative paths resolve against the sibling root, e.g.
	// Command=["go","run","./cmd/reliant","server","api"] +
	// WorkingDir="../reliant". Relative paths in Command resolve against
	// the effective cmd.Dir (WorkingDir), matching shell semantics.
	Command []string

	// WorkingDir is the subprocess cwd override. Empty = inherit parent.
	// Relative paths resolve against ProjectDir; absolute paths are
	// used verbatim.
	WorkingDir string
	// ProjectDir is the forge project root used to resolve a relative
	// WorkingDir. Ignored when WorkingDir is empty or absolute. Empty
	// ProjectDir + relative WorkingDir falls through to the parent's
	// cwd interpretation (exec.Cmd default).
	ProjectDir string
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
	var cmd *exec.Cmd
	switch runner {
	case "air":
		cfg := spec.AirConfig
		if cfg == "" {
			cfg = DefaultAirConfig
		}
		cmd = exec.CommandContext(ctx, "air", "-c", cfg)
	case "binary":
		cmd = exec.CommandContext(ctx, "./bin/"+name)
	case "delve":
		port := spec.DelvePort
		if port <= 0 {
			port = DefaultDelvePort
		}
		cmd = exec.CommandContext(ctx, "dlv", "exec", "--headless",
			fmt.Sprintf("--listen=:%d", port),
			"--api-version=2", "--accept-multiclient",
			"--continue", "./bin/"+name)
	default:
		// "go-run" / "" / unknown — the default shape. An explicit
		// Command overrides JUST this convention (the escape hatch for
		// sibling-repo binaries / non-standard entrypoints); air/binary/
		// delve are deliberate alternative runners that own their command
		// shape, so a Command set alongside them is ignored (the runner
		// wins). This keeps services that declare a documentation-only
		// `command` next to `runner = air` working as before.
		if len(spec.Command) > 0 {
			cmd = exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
		} else {
			cmd = exec.CommandContext(ctx, "go", "run", "./cmd", "server", name)
		}
	}
	if dir := resolveWorkingDir(spec.WorkingDir, spec.ProjectDir); dir != "" {
		cmd.Dir = dir
	}
	return cmd
}

// resolveWorkingDir returns the effective cmd.Dir for the given spec.
// Empty workingDir = empty result (inherit parent cwd). Absolute
// workingDir = workingDir verbatim. Relative workingDir + non-empty
// projectDir = filepath.Join(projectDir, workingDir) so cross-repo
// configs (e.g. WorkingDir="../sibling") resolve against the forge
// project root rather than wherever the parent shell happens to live.
// Relative workingDir + empty projectDir falls through to workingDir
// verbatim (the exec.Cmd default — interpret against caller cwd).
func resolveWorkingDir(workingDir, projectDir string) string {
	if workingDir == "" {
		return ""
	}
	if filepath.IsAbs(workingDir) || projectDir == "" {
		return workingDir
	}
	return filepath.Join(projectDir, workingDir)
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
	out, err := envutil.ParseDotEnv(path)
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
	return envutil.MergeBaseWins(base, extra)
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
