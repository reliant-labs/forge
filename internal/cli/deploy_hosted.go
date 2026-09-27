package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// hostedDeployClient builds the control-plane client a hosted deploy talks
// through. A var so the CLI e2e test can point it at an httptest server while
// every other step runs for real.
var hostedDeployClient = func(ep cloud.Endpoint, cred cloud.Credential) deploytarget.HostedCaller {
	return cloud.NewClient(ep, cred)
}

// hostedPollInterval is the readiness poll cadence; tests shorten it.
var hostedPollInterval = 3 * time.Second

// dispatchHostedDeploy routes a HOSTED env (its Bundle declares
// control_plane) to runHostedDeploy and reports that it did. A hosted env
// takes a different path from here on: no tag resolution, no kubectl, no
// cluster. Decided from the env's own render, before anything cluster-shaped
// runs. hosted=false means the caller continues down the cluster path.
func dispatchHostedDeploy(ctx context.Context, projectDir, envName string, opts deployOptions) (hosted bool, err error) {
	entities, rerr := RenderKCL(ctx, projectDir, envName)
	if rerr != nil || entities == nil || entities.ControlPlane == nil {
		return false, nil
	}
	if isLocalControlPlaneEnv(entities) {
		// A LOCAL env is not hosted: its control plane is only its secret
		// store, and its workloads run here. `forge env up`'s deploy phase
		// (renderToLaunch) continues down the ordinary path — compose,
		// external, local clusters; a direct `forge env deploy` of it is
		// refused BEFORE any RPC, because the platform never deploys to it.
		if opts.purpose == renderToLaunch {
			return false, nil
		}
		return true, refuseLocalEnvDeploy(envName)
	}
	if opts.frontendsOnly {
		return true, fmt.Errorf("--frontends-only is not supported on hosted env %q", envName)
	}
	return true, runHostedDeploy(ctx, envName, entities, opts)
}

// runHostedDeploy is `forge env deploy <env>` for an env whose Bundle declares
// control_plane.
//
// IT TOUCHES NO CLUSTER. There is no ClusterProvider.Ensure, no kubectl
// context guard, no local image build, no preflight against a kubeconfig and
// no manifest apply: the control plane owns the cluster, and forge's job ends
// at publishing admissible specs and confirming they converged. Every one of
// the skipped steps would either fail (there is no context to address) or,
// worse, succeed against whatever cluster happened to be current.
//
// The order, and why it is this order, is documented on the provider
// (internal/deploytarget/hosted.go). This function only resolves the inputs:
// the endpoint and credential from the env's own declaration, and the release
// binding from the env's ledger — which, for a hosted env, is that same
// control plane.
func runHostedDeploy(ctx context.Context, envName string, entities *KCLEntities, opts deployOptions) error {
	report := opts.report
	if len(opts.targets) > 0 {
		// A partial publish of a hosted env would leave the platform running
		// a mix of two releases under one binding — the state the release
		// model exists to rule out.
		return fmt.Errorf("--target is not supported on hosted env %q: a hosted deploy publishes the whole env at its bound release", envName)
	}

	groups, err := buildDeployGroups(envName, entities, "")
	if err != nil {
		return err
	}
	decl := declarationFromEntities(entities)
	ep, err := cloud.ResolveEndpoint(envName, decl)
	if err != nil {
		return err
	}
	cred, err := cloud.ResolveCredential("", ep)
	if err != nil {
		return fmt.Errorf("env %q deploys to the control plane at %s: %w", envName, ep.URL, err)
	}
	client := hostedDeployClient(ep, cred)
	ref := hostedEnvRefFor(envName, entities)

	// The guard, stated for a hosted destination: the declared "context" is
	// the endpoint. A consumer that keys its confirmation on where the bytes
	// land (the reliant console refuses a deploy whose declared context
	// changed between preview and confirm) gets the same guarantee.
	report.setGuard(deployJSONGuard{
		DeclaredContext: ep.URL,
		Verdict:         deployGuardVerdictAllow,
		Reason:          deployGuardReasonControlPlaneDeclared,
	})
	report.clearKubeContexts()
	envID := ""
	if id, lerr := deploytarget.LookupHostedEnvironment(ctx, client, ref.Project, envName); lerr == nil {
		envID = id
	} else if !errors.Is(lerr, deploytarget.ErrHostedEnvironmentNotFound) {
		return lerr
	}
	report.setHostedTarget(ep.URL, envID)

	var (
		release    string
		digests    map[string]string
		registries map[string]string
	)
	ledger := hostedLedger(client, ep.URL, ref.Project, ref.Kind)
	binding, bound, berr := ledger.Bindings.Current(ctx, envName)
	if berr != nil {
		return fmt.Errorf("read the promotion ledger for %q (%s): %w", envName, ep.URL, berr)
	}
	if bound {
		release, digests = binding.Release, binding.Resolved
		// The promotion froze digests; WHERE each was pushed lives on the
		// (immutable) release. A backend this project builds is declared
		// registry-less and is pinned under that recorded registry.
		rel, rerr := ledger.Releases.Get(ctx, release)
		if rerr != nil {
			return fmt.Errorf("read release %s from %s: %w", release, ep.URL, rerr)
		}
		registries = releaseRegistries(rel)
	}
	report.setTags("", "release "+emptyAs(release, "(none)")+" (promoted; "+ep.URL+")", release, false)

	fmt.Printf("Deploying env %s to the control plane at %s\n", envName, ep.URL)
	if release != "" {
		fmt.Printf("  Release:     %s  (pinning its digests)\n", release)
	}
	fmt.Printf("  Dry run:     %v\n\n", opts.dryRun)

	if len(groups) == 0 {
		fmt.Println("Nothing to deploy — the env declares no deploy tiers.")
		return nil
	}
	for i := range groups {
		groups[i].DryRun = opts.dryRun
		groups[i].Hosted = &deploytarget.HostedTarget{Endpoint: ep.URL, Project: ref.Project, Release: release, Digests: digests, Registries: registries}
	}

	registry := &deploytarget.Registry{}
	registry.Register(deploytarget.HostedProvider{
		Client:        client,
		Rollout:       opts.rollout,
		PollInterval:  hostedPollInterval,
		OnRollout:     report.rolloutObserver(),
		OnEnvironment: func(id string) { report.setHostedTarget(ep.URL, id) },
	})
	start := time.Now()
	// A failed hosted publish is NOT reverted. The ledger still names the
	// release the operator promoted; recovery is roll forward.
	if err := dispatchDeployGroups(ctx, registry, groups); err != nil {
		return err
	}
	if !opts.dryRun {
		fmt.Printf("\nDeploy completed in %s.\n", time.Since(start).Truncate(time.Millisecond))
	}
	return nil
}

// releaseRegistries is a release's OCI artifact → registry map: the URI each
// image was pushed to, which the hosted pin uses to locate a backend this
// project built. A nil release (never cut) or an artifact with no URI (built
// without --push) contributes nothing, and the pin refuses such a backend with
// the fix.
func releaseRegistries(rel *release.Release) map[string]string {
	if rel == nil {
		return nil
	}
	out := map[string]string{}
	for name, art := range rel.Artifacts {
		if art.Kind == release.KindOCI && art.URI != "" {
			out[name] = art.URI
		}
	}
	return out
}
