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
	"os"
	"path/filepath"
	"strings"
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
	// PGO is the subset of Hosted that builds a Go binary `forge pgo pull
	// --env <env>` writes a profile for (ciEnvBuildsPGOBinary): the envs
	// pgo-refresh.yml may pull (generator.CIInputs.PGOEnvs).
	PGO []string
	// Unrendered are the envs that failed to render, in ListEnvs' order.
	// Discovery cannot classify them, so they are in neither list above —
	// which a caller must not report as "not hosted".
	Unrendered []string
}

// discoverCIHostedEnvs returns the hosted and mixed envs. An env that fails
// to render is skipped, exactly as frontend discovery skips it: one env
// mid-edit must not blank every workflow.
//
// Memoized per project directory for the same reason discoverCIFrontends is:
// several builders may ask in one `forge generate`, and the answer costs a
// render of every env. A caller that asks OUTSIDE a generate run, and then
// runs one, must forgetCIDiscovery in between (see rescaffold.go).
func discoverCIHostedEnvs(projectDir string) ciHostedTopology {
	ciHostedMu.Lock()
	defer ciHostedMu.Unlock()
	if cached, ok := ciHostedCache[projectDir]; ok {
		return cached
	}
	var out ciHostedTopology
	envs, _ := ListCommittedEnvs(projectDir)
	for _, r := range renderEnvsPure(context.Background(), projectDir, envs) {
		env, entities, err := r.env, r.entities, r.err
		if err != nil || entities == nil {
			out.Unrendered = append(out.Unrendered, env)
			continue
		}
		if !ciEnvIsHosted(entities) {
			continue
		}
		out.Hosted = append(out.Hosted, env)
		// Mixed is what needs the kubeconfig step: an OnExternal part
		// brings its own tooling's credentials, so it alone is not mixed.
		if forgeAppliesItselfLocally(entities) {
			out.Mixed = append(out.Mixed, env)
		}
		if ciEnvBuildsPGOBinary(projectDir, entities) {
			out.PGO = append(out.PGO, env)
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

// ciEnvBuildsPGOBinary reports whether `forge pgo pull --env <env>` has a
// Go binary to write a profile for: ResolvePGOBinaries — the very list pull
// resolves — holds one with PGO on whose main package is real. A
// project-relative package (./cmd/<bin>) must be a directory of this
// checkout, because an image built from the project Dockerfile reads as
// running ./cmd/<project> whether or not the image is Go (an adopted app).
// Its presence is the test, not a main.go in it: `forge project new` writes
// cmd/<bin>/main.go last, after the CI files. A package outside the checkout (a ShellBuild's sibling,
// an import path) is one the env's pgo names on purpose, so it counts.
func ciEnvBuildsPGOBinary(projectDir string, entities *KCLEntities) bool {
	bins, err := ResolvePGOBinaries(entities)
	if err != nil {
		return false // the render already refused such an env
	}
	for _, b := range bins {
		if !b.PGO.Enabled {
			continue
		}
		rel, local := strings.CutPrefix(b.Package, "./")
		if !local {
			return true
		}
		if fi, err := os.Stat(filepath.Join(projectDir, filepath.FromSlash(rel))); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

var (
	ciHostedMu    sync.Mutex
	ciHostedCache = map[string]ciHostedTopology{}
)

// forgetCIDiscovery drops every memoized CI discovery answer for projectDir,
// so the next question renders the envs as they are on disk now.
//
// The memo's lifetime is one `forge generate` run: inside it, the CI step is
// the only asker and runs after every step that writes what an env imports.
// A caller that asks before a run and then starts one would otherwise hand
// the run's CI step an answer rendered from a tree the run has since
// changed.
func forgetCIDiscovery(projectDir string) {
	ciHostedMu.Lock()
	delete(ciHostedCache, projectDir)
	ciHostedMu.Unlock()
	ciFrontendsMu.Lock()
	delete(ciFrontendsCache, projectDir)
	ciFrontendsMu.Unlock()
}
