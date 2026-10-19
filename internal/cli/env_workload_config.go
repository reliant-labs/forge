package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/devstack"
	"github.com/reliant-labs/forge/internal/secrets"
)

// readEnvConfigEntities is deliberately declaration-only. In particular, it
// must not open a secret provider before env exec has checked placement.
func readEnvConfigEntities(ctx context.Context, projectDir, env string) (*KCLEntities, error) {
	_, restore := activateDevStack(ctx, projectDir, env, renderToLaunch, inspectBlocks)
	defer restore()
	e, err := RenderKCL(ctx, projectDir, env)
	if err != nil {
		return nil, fmt.Errorf("read deploy/kcl/%s/: %w", env, err)
	}
	if e == nil {
		return nil, fmt.Errorf("deploy/kcl/%s/ rendered no workloads", env)
	}
	return e, nil
}

// hostWorkloadConfig is shared by env up and configuration readback/exec.
// Shell policy belongs to LayerHostEnv; these are only the declared layers.
func hostWorkloadConfig(cfg *config.ProjectConfig, w WorkloadEntity, secretValues map[string]string, env string) map[string]string {
	var defaults map[string]string
	if cfg != nil {
		defaults = loadProjectConfigEnv(cfg, env)
	}
	dev, _ := seedTargetIsDev(env)
	defaults = withDevRunDefaults(defaults, dev)
	defaults = withAirRunDefaults(defaults, hostRunnerSpec(w).Runner)
	out := make(map[string]string)
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range scopeSecretsToService(secretValues, &w) {
		out[k] = v
	}
	for k, v := range withoutDerivedOTLPIfShellSet(w.HostEnv(), os.Environ()) {
		out[k] = v
	}
	return out
}

// localWorkloadConfig is the single value-resolution path for config and exec.
// Nonlocal callers must not enter here: it may read/pull LOCAL secrets.
func localWorkloadConfig(ctx context.Context, projectDir, env string, e *KCLEntities, w WorkloadEntity) (map[string]string, error) {
	if w.Runtime.Type != RuntimeHost && w.Runtime.Type != RuntimeCompose {
		return nil, fmt.Errorf("workload %q is not local", w.Name)
	}
	if err := armLocalSecretsForUp(ctx, env, e, func(string, ...any) {}); err != nil {
		return nil, err
	}
	defer disarmPulledSecrets()
	prov, err := secretProviderFromEntities(e, projectDir)
	if err != nil {
		return nil, fmt.Errorf("secret provider: %w", err)
	}
	if err := secrets.ValidateDeclaredRefs(prov, secretRefsForService(&w), secretStoreLabel(e)); err != nil {
		return nil, withEnvInSecretFix(err, env)
	}
	if w.Runtime.Type == RuntimeCompose {
		cm := w.Runtime.Compose
		if cm == nil {
			return nil, fmt.Errorf("workload %q has no Compose configuration", w.Name)
		}
		dir := projectDir
		if cm.Shared {
			dir = devstack.SharedProjectDir(projectDir)
		}
		// Pin relative env_file exactly as the Compose deploy path does for a
		// shared project, and to this project otherwise (exec retains the cwd).
		spec := &deploytarget.ComposeSpec{EnvFile: cm.EnvFile, Env: cm.Env, ProjectDirectory: dir}
		return deploytarget.ComposeProcessEnv(spec, prov.All())
	}
	var cfg *config.ProjectConfig
	if store, err := loadProjectStoreFrom(filepath.Join(projectDir, "forge.yaml")); err == nil {
		cfg = store.Config()
	} else {
		return nil, err
	}
	return hostWorkloadConfig(cfg, w, prov.All(), env), nil
}

func resolvedEnvConfigReport(ctx context.Context, projectDir, env string, e *KCLEntities, filter string) (envConfigReport, error) {
	report := envConfigReport{Env: env, Workloads: collectEnvConfigWorkloads(e)}
	if filter != "" {
		report.Workloads = filterEnvConfigWorkloads(report.Workloads, filter)
		if len(report.Workloads) == 0 {
			return report, fmt.Errorf("environment %q declares no workload named %q", env, filter)
		}
	}
	for i := range report.Workloads {
		row := &report.Workloads[i]
		for _, w := range e.Workloads {
			if row.Name != w.Name || row.Kind != w.Kind {
				continue
			}
			if w.Runtime.Type == RuntimeHost || w.Runtime.Type == RuntimeCompose {
				values, err := localWorkloadConfig(ctx, projectDir, env, e, w)
				if err != nil {
					return report, err
				}
				row.Env = values
			}
		}
	}
	overlayLaunchedEnv(&report, projectDir, env)
	return report, nil
}
