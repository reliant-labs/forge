package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
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

// loadProjectConfigEnvMap renders config_gen.appConfigEnvMap(app_config)
// from deploy/kcl/<env>/config.k — the SAME projection cluster mode renders
// into every workload's env — and returns the ENV_VAR-keyed source map. Reading
// this one source keeps host-run injection, the parity report, and the seed
// gate from drifting off what a cluster deploy would inject.
//
// It evaluates a throwaway probe module (written into the env dir, removed on
// return) as a SINGLE file, so only the probe's import graph —
// config_gen + config.k + the forge module — is pulled;
// the env's main.k (and the components it imports) is not needed. Any
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
	// emits it. `import config_gen` resolves from the deploy/kcl package
	// root; `import .config` is the env's own AppConfig instance.
	//
	// With PER-BINARY configs there is no single app_config to project, so
	// the probe merges every binary's env map instead. That is the honest
	// host-mode answer: `forge run` starts every binary on one host, so the
	// host env is the union of what those processes read — while each
	// binary's CLUSTER Deployment still carries only its own vars, which is
	// where the isolation matters. A name set by two binaries resolves to
	// one host value (map-merge, last wins); they are separate values only
	// once they are separate processes with separate environments.
	probe, perr := configProbeSource(projectDir, envDir)
	if perr != nil {
		return nil, perr
	}
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

// configProbeSource builds the throwaway KCL module that renders an env's
// config projection, shaped to whatever the project declares.
//
// The shape is read off the env's own config.k — the instances it declares
// ARE the configs this env has values for. A `<name>: config_gen.<Schema>`
// binding names both the variable to project and the schema whose lambda
// projects it, so the probe needs no second source of truth. A project with
// the classic single `app_config: config_gen.AppConfig` gets exactly the
// probe it always did.
func configProbeSource(projectDir, envDir string) (string, error) {
	src, err := os.ReadFile(filepath.Join(envDir, "config.k"))
	if err != nil {
		return "", fmt.Errorf("read config.k: %w", err)
	}

	instances := configKInstances(string(src))
	if len(instances) == 0 {
		return "", fmt.Errorf("no config instances found in %s/config.k", envDir)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "import %s\n", codegen.ConfigSchemaModule)
	b.WriteString("import .config as appcfg\n\n")
	b.WriteString("forge_config_env = ")
	for i, inst := range instances {
		_, lambda := codegen.KCLConfigName(inst.schema)
		if i > 0 {
			b.WriteString(" | ")
		}
		fmt.Fprintf(&b, "%s.%s(appcfg.%s)", codegen.ConfigSchemaModule, lambda, inst.varName)
	}
	b.WriteString("\n")
	return b.String(), nil
}

// configInstance is one `<var>: config_gen.<Schema> = {` binding in an env's
// config.k: the variable holding the values and the schema naming its
// projection lambda.
type configInstance struct {
	varName string
	schema  string
}

// configKInstances finds the typed config instances an env's config.k
// declares. It matches the binding form the scaffolder emits (and that a
// hand-edited file keeps), ignoring comments and any other statement.
func configKInstances(src string) []configInstance {
	prefix := codegen.ConfigSchemaModule + "."
	var out []configInstance
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, prefix) {
			continue
		}
		schema := strings.TrimPrefix(rest, prefix)
		// Keep only the schema identifier: `AdminConfig = {` -> `AdminConfig`.
		schema = strings.TrimSpace(strings.Split(strings.Split(schema, "=")[0], " ")[0])
		if name == "" || schema == "" {
			continue
		}
		out = append(out, configInstance{varName: name, schema: schema})
	}
	return out
}
