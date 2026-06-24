package cli

import (
	"path/filepath"

	"github.com/reliant-labs/forge/internal/secrets"
)

// secretProviderFromEntities builds a secrets.Provider from the bundle's
// declared provider, resolving a dotenv relative path against projectDir.
// Returns a noop provider when none declared.
//
// The dotenv path is resolved here (the only place that knows projectDir)
// so the secrets package stays filesystem-location-agnostic — its
// ProviderConfig.Path is "already resolved" by contract.
func secretProviderFromEntities(e *KCLEntities, projectDir string) (secrets.Provider, error) {
	if e == nil || e.SecretProvider == nil {
		return secrets.NewProvider(nil)
	}
	// A "rendered" provider declares cluster Secrets explicitly (name +
	// per-key source) — it is NOT an env-var-keyed resolver like dotenv.
	// Its cluster apply runs through applyRenderedSecretsPerGroup; for the
	// env-var-resolution consumers here (host secret-ref validation /
	// injection) it has nothing to offer, so return a noop. The dedicated
	// per-group path builds its own `.env.<env>` dotenv source for
	// from="dotenv" keys.
	if e.SecretProvider.Type == "rendered" {
		return secrets.NewProvider(nil)
	}
	cfg := &secrets.ProviderConfig{
		Type: e.SecretProvider.Type,
		Path: e.SecretProvider.Path,
	}
	if cfg.Path != "" && !filepath.IsAbs(cfg.Path) {
		cfg.Path = filepath.Join(projectDir, cfg.Path)
	}
	return secrets.NewProvider(cfg)
}

// secretRefsFromEntities walks every service's EnvVars and returns the
// declared secret references (those with a non-empty secret_ref).
// SecretKey falls back to EnvName when the KCL secret_key is empty.
// Used for validation and k8s Secret rendering.
func secretRefsFromEntities(e *KCLEntities) []secrets.SecretRef {
	if e == nil {
		return nil
	}
	var refs []secrets.SecretRef
	for i := range e.Services {
		refs = append(refs, secretRefsForService(&e.Services[i])...)
	}
	return refs
}

// secretRefsForK8sServices is like secretRefsFromEntities but ONLY for
// services whose deploy target is k8s cluster (Deploy.Type=="cluster") —
// these are the refs that need rendered Secret objects. Host/compose/
// external refs are injected as env values, not k8s Secrets.
func secretRefsForK8sServices(e *KCLEntities) []secrets.SecretRef {
	if e == nil {
		return nil
	}
	var refs []secrets.SecretRef
	for i := range e.Services {
		if e.Services[i].Deploy.Type != "cluster" {
			continue
		}
		refs = append(refs, secretRefsForService(&e.Services[i])...)
	}
	return refs
}

// secretRefsForHostServices returns the declared secret refs for host-mode
// services only — the set a host launch must be able to resolve from the
// provider (for fail-fast validation before starting the process).
func secretRefsForHostServices(e *KCLEntities) []secrets.SecretRef {
	if e == nil {
		return nil
	}
	var refs []secrets.SecretRef
	for i := range e.Services {
		if e.Services[i].Deploy.Type != "host" {
			continue
		}
		refs = append(refs, secretRefsForService(&e.Services[i])...)
	}
	return refs
}

// serviceEnvVars returns every EnvVar a service declares, across BOTH the
// top-level `env_vars` and the deploy-block `env_vars`. Projects declare
// per-env config/secret refs on the deploy block (e.g. HostDeploy.env_vars
// = _cp_host_env, K8sCluster.env_vars = …), not the top-level field — so a
// collector that read only ServiceEntity.EnvVars would miss every
// secret_ref. Compose/External carry env via env_file / an env map, not an
// EnvVar list, so they contribute no secret_ref entries here.
func serviceEnvVars(s *ServiceEntity) []KCLEnvVar {
	out := append([]KCLEnvVar(nil), s.EnvVars...)
	switch {
	case s.Deploy.Host != nil:
		out = append(out, s.Deploy.Host.EnvVars...)
	case s.Deploy.Cluster != nil:
		out = append(out, s.Deploy.Cluster.EnvVars...)
	}
	return out
}

// secretRefsForService extracts the declared secret references from one
// service's EnvVars (top-level + deploy block). A ref is "declared" when
// SecretRef is non-empty. SecretKey falls back to EnvName (matching the
// KCL _env_source lambda, which defaults secret_key to the env-var name).
func secretRefsForService(s *ServiceEntity) []secrets.SecretRef {
	var refs []secrets.SecretRef
	for _, ev := range serviceEnvVars(s) {
		if ev.SecretRef == "" {
			continue
		}
		key := ev.SecretKey
		if key == "" {
			key = ev.Name
		}
		refs = append(refs, secrets.SecretRef{
			EnvName:    ev.Name,
			SecretName: ev.SecretRef,
			SecretKey:  key,
		})
	}
	return refs
}
