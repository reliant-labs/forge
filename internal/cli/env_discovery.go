package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/reliant-labs/forge/internal/config"
)

// Environment discovery walks `deploy/kcl/<env>/main.k` rather than
// reading `cfg.Envs`. This is the deploy-target-architecture
// migration's source-of-truth: per-env deploy config lives in KCL
// (a `forge.K8sCluster` ref attached to each service), and the env
// list is the set of directories under `deploy/kcl/` that contain a
// `main.k`.
//
// The legacy `cfg.Envs []EnvironmentConfig` reader survives for one
// migration cycle so existing forge.yaml files keep parsing. See the
// `environments-to-kcl` migration skill for the rewrite.

// ListEnvs returns the names of every environment declared via a
// `deploy/kcl/<env>/main.k` file. The list is sorted alphabetically
// for deterministic output. An absent `deploy/kcl/` directory yields
// an empty list (and no error) — that's the shape of a brand-new
// project, not a problem.
//
// projectDir is the project root (the directory containing
// forge.yaml). Callers can pass either projectDir or kclDir directly
// — see ListEnvsFromKCLDir for the lower-level variant.
func ListEnvs(projectDir string) ([]string, error) {
	return ListEnvsFromKCLDir(filepath.Join(projectDir, "deploy", "kcl"))
}

// ListEnvsFromKCLDir is the lower-level discovery walker. It exists
// so callers that already have the kcl root (e.g. a forge.yaml-
// configured cfg.K8s.KCLDir) can skip the projectDir join.
func ListEnvsFromKCLDir(kclDir string) ([]string, error) {
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list envs in %s: %w", kclDir, err)
	}
	var envs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mainK := filepath.Join(kclDir, e.Name(), "main.k")
		if _, err := os.Stat(mainK); err == nil {
			envs = append(envs, e.Name())
		}
	}
	sort.Strings(envs)
	return envs, nil
}

// EnvExists reports whether `deploy/kcl/<env>/main.k` exists. Returns
// false (with no error) for the absent-env / missing-kcl-dir cases —
// callers typically convert that into a friendly "env not configured"
// error themselves.
func EnvExists(projectDir, env string) (bool, error) {
	mainK := filepath.Join(projectDir, "deploy", "kcl", env, "main.k")
	if _, err := os.Stat(mainK); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, fmt.Errorf("stat %s: %w", mainK, err)
	}
}

// ListEnvsForConfig is the migration bridge: it returns the env list
// for a project regardless of whether the project still uses
// forge.yaml `environments[]` or has migrated to KCL-only.
//
// Resolution: prefer KCL-derived discovery; fall back to forge.yaml's
// cfg.Envs when no `deploy/kcl/<env>/main.k` files exist. The two
// paths overlap in projects mid-migration, so the union is also
// returned — sorted, deduped — when both are non-empty.
//
// Emits no warnings here; the deprecation warning fires once at
// loadProjectConfig time so it doesn't get logged repeatedly from
// every reader.
func ListEnvsForConfig(projectDir string, cfg *config.ProjectConfig) []string {
	kclEnvs, _ := ListEnvs(projectDir)
	yamlEnvs := envsFromConfig(cfg)
	if len(kclEnvs) == 0 {
		return yamlEnvs
	}
	if len(yamlEnvs) == 0 {
		return kclEnvs
	}
	seen := map[string]struct{}{}
	merged := make([]string, 0, len(kclEnvs)+len(yamlEnvs))
	for _, e := range kclEnvs {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		merged = append(merged, e)
	}
	for _, e := range yamlEnvs {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		merged = append(merged, e)
	}
	sort.Strings(merged)
	return merged
}

func envsFromConfig(cfg *config.ProjectConfig) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Envs))
	for _, e := range cfg.Envs {
		if e.Name == "" {
			continue
		}
		out = append(out, e.Name)
	}
	return out
}
