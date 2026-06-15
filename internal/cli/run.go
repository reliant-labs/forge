package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

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

// declaredServiceNames returns the names of every service in forge.yaml,
// used by error paths that point users at the right spelling when they
// typo a service name.
func declaredServiceNames(cfg *config.ProjectConfig) []string {
	out := make([]string, 0, len(cfg.Components))
	for _, s := range cfg.Components {
		out = append(out, s.Name)
	}
	return out
}
