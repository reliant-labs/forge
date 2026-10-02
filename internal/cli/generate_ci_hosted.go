package cli

// Hosted-env discovery for the CI workflow generator (hosted-deploy-primitives
// §3.6): which declared envs a control plane RUNS, so `forge generate` can
// scaffold release.yml + the forge-deploy action for them, and keep
// deploy.yml's per-env rebuild job away from them.
//
// The test is the one `forge env deploy` already uses (deploy_hosted.go):
// the env declares forge.ControlPlane AND something the platform runs
// (KCLEntities.HasHosted). A LOCAL control plane — one that is only an env's
// secret store — runs nothing, so it is not hosted for CI's purposes.

import (
	"context"
	"sync"
)

// ciHostedTopology is the hosted half of the CI generator's inputs.
type ciHostedTopology struct {
	// Hosted are the envs a control plane runs, in ListEnvs' sorted order.
	Hosted []string
	// Mixed is the subset of Hosted that also applies a part from this
	// machine (envAppliesLocally — the same test dispatchHostedDeploy uses
	// to decide a deploy is not hosted-only).
	Mixed []string
}

// discoverCIHostedEnvs returns the hosted and mixed envs. An env that fails
// to render is skipped, exactly as frontend discovery skips it: one env
// mid-edit must not blank every workflow.
//
// Memoized per project directory for the same reason discoverCIFrontends is:
// several builders may ask in one `forge generate`, and the answer costs a
// render of every env.
func discoverCIHostedEnvs(projectDir string) ciHostedTopology {
	ciHostedMu.Lock()
	defer ciHostedMu.Unlock()
	if cached, ok := ciHostedCache[projectDir]; ok {
		return cached
	}
	var out ciHostedTopology
	envs, _ := ListEnvs(projectDir)
	for _, env := range envs {
		entities, restored, err := renderKCLPure(context.Background(), projectDir, env)
		reportImpureRender(env, restored)
		if err != nil || entities == nil {
			continue
		}
		if !ciEnvIsHosted(entities) {
			continue
		}
		out.Hosted = append(out.Hosted, env)
		if envAppliesLocally(entities) {
			out.Mixed = append(out.Mixed, env)
		}
	}
	ciHostedCache[projectDir] = out
	return out
}

// ciEnvIsHosted is the render-free half of discovery, split out so the rule
// is testable from a literal entity.
func ciEnvIsHosted(entities *KCLEntities) bool {
	return entities.ControlPlane != nil && !isLocalControlPlaneEnv(entities) && entities.HasHosted()
}

var (
	ciHostedMu    sync.Mutex
	ciHostedCache = map[string]ciHostedTopology{}
)
