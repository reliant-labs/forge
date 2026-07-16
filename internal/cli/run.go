package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// newRunCmd restores `forge run` as the single-command dev runner: bring
// the project's host services + frontends up against the current working
// directory, skipping the cluster build/deploy. It is a thin alias over
// the SAME runner `forge up --host-only` uses (runUp with hostOnly=true) —
// no duplicated launch logic — so behaviour (KCL render, port-conflict
// guard, non-TTY detach, per-service logs) is identical to that path.
//
// The one thing `run` adds over `up --host-only` is dev-server passthrough:
// tokens after `--` are forwarded to each frontend's dev server
// (`npm run dev -- <flags>`). This is what the reliant one-shot workflow
// relies on — `reliant forge run -- --host 0.0.0.0` starts the scaffolded
// Vite frontend bound to 0.0.0.0 so the workspace proxy can reach it and
// hand the user a preview URL.
//
// No positional target: like the old orchestrator-shaped `forge run`, it
// brings up EVERYTHING host-mode in the env (the scaffold's single service
// + frontend), so the workflow needs no target to name. Env defaults to dev
// (the env `forge new` scaffolds and the one-shot builds against).
func newRunCmd() *cobra.Command {
	var env string
	cmd := &cobra.Command{
		Use:   "run [-- <dev-server flags>]",
		Short: "Run the project's dev servers (host services + frontends) against the current dir, skipping cluster build/deploy",
		Long: `Run the project's dev loop against the current working directory.

Brings up every host-mode service and frontend declared in
deploy/kcl/<env>/ (default env: dev), skipping the cluster build + deploy
phases — the inner loop for iterating on a scaffolded project. This is an
alias for ` + "`forge up --host-only`" + `; see that command for the full
lifecycle (non-TTY runs start everything and return, leaving the processes
running; stop them with ` + "`forge up stop --env=<env>`" + `).

Tokens after ` + "`--`" + ` are forwarded to each frontend's dev server
(` + "`npm run dev -- <flags>`" + `), so a Vite/Next dev server can be told
to bind a specific host/port.

Examples:
  forge run                        # host services + frontends, env=dev
  forge run --env=staging          # against the staging env's KCL
  forge run -- --host 0.0.0.0      # forward --host 0.0.0.0 to the dev server`,
		// Runtime failures (a port already bound, a child dying) are not
		// usage errors — dumping the flag table after them buries the
		// actionable message. Mirrors the removed run command's shape.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			frontendArgs, err := runPassthroughArgs(args, cmd.ArgsLenAtDash())
			if err != nil {
				return err
			}
			return runUp(cmd.Context(), upOptions{
				env:          env,
				hostOnly:     true,
				frontendArgs: frontendArgs,
			})
		},
	}
	cmd.Flags().StringVar(&env, "env", "dev", "Deploy environment whose deploy/kcl/<env>/ to run (default: dev)")
	return cmd
}

// runPassthroughArgs splits `forge run`'s positional args at the cobra
// `--` terminator: everything AFTER `--` is dev-server passthrough
// (forwarded to each frontend), and there must be nothing BEFORE it —
// `forge run` takes no positional target (it brings up everything
// host-mode). dashPos is cmd.ArgsLenAtDash(): the count of args before the
// `--`, or -1 when no `--` was given. Extracted from the RunE so the
// split/validation is unit-testable without a real project.
func runPassthroughArgs(args []string, dashPos int) ([]string, error) {
	const noPositional = "forge run takes no positional arguments; pass dev-server flags after `--` (e.g. forge run -- --host 0.0.0.0)"
	if dashPos < 0 {
		// No `--` terminator. Any bare positional is a usage mistake.
		if len(args) > 0 {
			return nil, fmt.Errorf(noPositional)
		}
		return nil, nil
	}
	if dashPos > 0 {
		// Positional args appeared before the `--`.
		return nil, fmt.Errorf(noPositional)
	}
	return args[dashPos:], nil
}

// This file holds the env-composition helpers shared by the host-mode
// phase of `forge up` (up.go) and the dev/prod parity check
// (doctor_parity.go). The standalone `forge run` command — both the
// docker-compose orchestrator and the single host-mode service runner —
// was removed: the compose orchestrator is now a KCL deploy target
// consumed by `forge up`/`forge deploy`, and the single-service runner
// is `forge up --target <service> --host-only`. These helpers stayed
// because non-run code still depends on them.

// managedProcess tracks a running child process started by the `forge up`
// orchestrator (up.go). name/cmd identify the child; pid is the PID
// captured at Start time, which survives cmd.Process.Release() on the
// `--background` detach path (Release resets cmd.Process.Pid to -1, so
// reading it afterwards — for the persisted state file `forge up stop`
// reads — would record -1). Zero when unset; the foreground path reads
// cmd.Process.Pid directly.
type managedProcess struct {
	name string
	cmd  *exec.Cmd
	pid  int
}

// envConfigToEnvVars projects a merged per-env config map onto a flat
// NAME→VALUE map suitable for passing to a child process.
//
// The keys of envCfg are proto field names (snake_case). We map them to
// uppercase env-var names by parsing proto/config/ for ConfigFieldOptions
// to honour any custom env_var: annotations. When the proto descriptor
// is unavailable (fresh project, no descriptor yet) we fall back to
// converting snake_case → SCREAMING_SNAKE.
//
// projectConfigPath is the path to forge.yaml; the parent dir is used
// to resolve proto/config/ for the annotation lookup. The surface
// deliberately takes the file path (not the dir) so callers can pass
// `findProjectConfigFile()`'s return value directly.
//
// Sensitive fields are skipped here — local host-mode dev shouldn't be
// plumbing secret refs through env vars. Set the secret value in your
// local env (.env / direnv) instead.
func envConfigToEnvVars(envCfg map[string]any, projectConfigPath string) map[string]string {
	out := map[string]string{}
	annotations := loadConfigAnnotations(filepath.Dir(projectConfigPath))

	for key, val := range envCfg {
		envVar := strings.ToUpper(key)
		var sensitive bool
		if ann, ok := annotations[key]; ok {
			if ann.EnvVar != "" {
				envVar = ann.EnvVar
			}
			sensitive = ann.Sensitive
		}
		if sensitive {
			continue
		}
		if s, ok := val.(string); ok {
			if _, isSecretRef := parseLooseSecretRef(s); isSecretRef {
				// Secret refs aren't resolvable at run-time. Skip and
				// expect the user to set them in their env.
				continue
			}
		}
		out[envVar] = stringifyEnvValue(val)
	}
	return out
}

// loadProjectConfigEnv loads the per-env config from the sibling
// `config.<env>.yaml` file and projects it to env-var strings via
// [envConfigToEnvVars]. Returns an empty map (not nil) on any error
// so callers can pass the result straight to [hostlaunch.LayerHostEnv]
// without guarding. Missing file / empty config is non-fatal — host-mode
// services run against whatever defaults the binary's flag/env loader
// provides when no per-env config is declared.
//
// Reuses the same loader + projector the cluster-mode ConfigMap
// projection reads, so host-mode services don't drift from their
// cluster-mode counterparts. Sensitive fields and ${SECRET_REF}
// placeholders are skipped — those belong in `.env.<env>` (the
// gitignored dotenv) or the developer shell, not in committed
// sibling-file config.
func loadProjectConfigEnv(_ *config.ProjectConfig, env string) map[string]string {
	if env == "" {
		return map[string]string{}
	}
	projectPath, perr := findProjectConfigFile()
	if perr != nil {
		return map[string]string{}
	}
	projectDir := filepath.Dir(projectPath)
	envCfg, lerr := config.LoadEnvironmentConfig(projectDir, env)
	if lerr != nil {
		return map[string]string{}
	}
	return envConfigToEnvVars(envCfg, projectPath)
}

// configAnnotation is a lightweight projection of ConfigField used to
// map proto field names to env-var names.
type configAnnotation struct {
	EnvVar    string
	Sensitive bool
}

// loadConfigAnnotations parses proto/config/ via the forge descriptor
// and returns proto-field-name → annotation. Returns an empty map on
// any error (the caller falls back to snake→SCREAMING_SNAKE).
func loadConfigAnnotations(projectDir string) map[string]configAnnotation {
	out := map[string]configAnnotation{}
	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto", "config"))
	if err != nil || len(messages) == 0 {
		return out
	}
	for _, m := range messages {
		for _, f := range m.Fields {
			out[f.Name] = configAnnotation{EnvVar: f.EnvVar, Sensitive: f.Sensitive}
		}
	}
	return out
}

// parseLooseSecretRef returns ("name", true) for "${name}" strings.
// Used to detect un-resolvable secret references in dev config that
// should be skipped (let the user's local env supply the value).
func parseLooseSecretRef(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}")
	if inner == "" {
		return "", false
	}
	return inner, true
}

// stringifyEnvValue turns a YAML-decoded scalar into its env-var string form.
func stringifyEnvValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", x)
	case int64:
		return fmt.Sprintf("%d", x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	default:
		return fmt.Sprint(v)
	}
}

// hostEnvVarsToMap projects the HostDeploy.EnvVars slice to a flat
// NAME→VALUE map for layering onto the subprocess env.
//
// Only the inline `value` channel applies on the host — KCLEnvVar's
// other channels (secret_ref, config_map_ref) are cluster-mode
// projections (Deployment.env.valueFrom.secretKeyRef etc.) with no
// meaningful host equivalent. Those projection channels stay in KCL
// for K8sCluster services; on the host, secrets come from the
// gitignored secrets_file.
//
// Returns an empty map (not nil) on a nil host, so callers can pass
// the result straight to [hostlaunch.LayerHostEnv] without guarding.
func hostEnvVarsToMap(host *HostDeploy) map[string]string {
	if host == nil || len(host.EnvVars) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(host.EnvVars))
	for _, ev := range host.EnvVars {
		if ev.Name == "" || ev.Value == "" {
			continue
		}
		out[ev.Name] = ev.Value
	}
	return out
}

// declaredServiceNames returns the names of every declared component,
// used by error paths that point users at the right spelling when they
// typo a service name. The inventory is enumerated from the REAL sources
// (proto descriptor + owned worker/operator files + cmd/ binaries), not
// the removed components.json — callers pass codegen.IntrospectComponents.
func declaredServiceNames(comps []config.ComponentConfig) []string {
	out := make([]string, 0, len(comps))
	for _, s := range comps {
		out = append(out, s.Name)
	}
	return out
}
