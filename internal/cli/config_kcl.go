package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/kclrender"
)

// kclEnvSource mirrors the two channels of forge.EnvSource that the config
// projection produces: an inline `value`, or a `from_secret` {name,key} ref.
// (Value is a pointer so an explicit empty-string value is distinguishable
// from an absent value channel.)
type kclEnvSource struct {
	Value      *string           `json:"value"`
	FromSecret map[string]string `json:"from_secret"`
}

// loadProjectConfigEnvMap renders config_projection.appConfigEnvMap(app_config)
// from deploy/kcl/<env>/config.k — the SAME projection cluster mode renders
// into every workload's env — and returns the ENV_VAR-keyed source map. Reading
// this one source keeps host-run injection, the parity report, and the seed
// gate from drifting off what a cluster deploy would inject.
//
// It evaluates a throwaway probe module (written into the env dir, removed on
// return) as a SINGLE file, so only the probe's import graph —
// config_projection + config_schema + config.k + the forge module — is pulled;
// the env's main.k (and its components_gen.json read) is not needed. Any
// failure is returned to the caller, which treats config as absent (non-fatal),
// matching how a missing per-env config always behaved.
func loadProjectConfigEnvMap(projectDir, env string) (map[string]kclEnvSource, error) {
	if env == "" {
		return nil, fmt.Errorf("config env: env required")
	}
	envDir := filepath.Join(projectDir, "deploy", "kcl", env)
	if _, err := os.Stat(filepath.Join(envDir, "config.k")); err != nil {
		return nil, err
	}

	// The probe outputs the projected env map under a public top-level so KCL
	// emits it. `import config_projection` resolves from the deploy/kcl package
	// root; `import .config` is the env's own AppConfig instance.
	probe := "import config_projection\n" +
		"import .config as appcfg\n\n" +
		"forge_config_env = config_projection.appConfigEnvMap(appcfg.app_config)\n"
	probePath := filepath.Join(envDir, fmt.Sprintf("zz_forge_config_probe_%d.k", os.Getpid()))
	if err := os.WriteFile(probePath, []byte(probe), 0o644); err != nil {
		return nil, fmt.Errorf("write config probe: %w", err)
	}
	defer func() { _ = os.Remove(probePath) }()

	out, err := kclrender.Run(projectDir, probePath, []string{"env=" + env})
	if err != nil {
		return nil, fmt.Errorf("render config projection for %s: %w", env, err)
	}
	var doc struct {
		Env map[string]kclEnvSource `json:"forge_config_env"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parse config projection: %w", err)
	}
	return doc.Env, nil
}
