package deploytarget

// The HOSTED provider: ship forge's deploy tiers to a control plane.
//
// An env whose Bundle declares `control_plane` does not apply anything to a
// cluster forge can see. Its tiers (SimpleBackend, ManagedDatabase) are
// PUBLISHED to the control plane as forge.dev/v1alpha1 specs, and the platform
// renders and runs them. So this provider never shells out to kubectl and
// never ensures a cluster: every step is a Connect call.
//
// THE ORDER IS THE CONTRACT, and it is fixed so that nothing is written until
// everything is known to be admissible:
//
//  1. pin every backend spec to the digest the env's bound release froze
//  2. Validate + CheckShapeBand EVERY spec — a refusal here costs zero RPCs
//  3. EnsureEnvironment (by name) → EnsureDeployment (by name, per workload)
//  4. PublishDeploymentConfig per deployment
//  5. a bounded readiness wait on GetStatus{environmentId}; timing out is
//     reported as timed out, never as success
//
// forge does not import control-plane. The request and response shapes below
// are the proto3-JSON subset forge reads, declared here, and carried by an
// injected Caller (cloud.Client in production).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/pkg/deploy"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// HostedProviderID is the registry id of the hosted provider.
const HostedProviderID = "hosted"

// HostedCaller is the one method the provider needs from a control-plane
// client. Declared here, at its consumer; cloud.Client satisfies it.
type HostedCaller interface {
	Call(ctx context.Context, procedure string, req, out any) error
}

// HostedTier is the tier a hosted workload is published as.
type HostedTier string

// The three hosted tiers, one per forge.dev tier CRD.
const (
	HostedTierBackend  HostedTier = "backend"
	HostedTierDatabase HostedTier = "database"
	HostedTierStatic   HostedTier = "static"
)

// wireTier is the controlplane.v1.DeployTier value name for a tier.
func (t HostedTier) wireTier() string {
	switch t {
	case HostedTierBackend:
		return "DEPLOY_TIER_BACKEND"
	case HostedTierDatabase:
		return "DEPLOY_TIER_DATABASE"
	case HostedTierStatic:
		return "DEPLOY_TIER_STATIC"
	default:
		return ""
	}
}

// HostedWorkload is one tier declaration bound for the control plane. Exactly
// one of Backend / Database / Static is set, matching Tier.
type HostedWorkload struct {
	Tier HostedTier
	// Artifact is the release-ledger artifact name this workload's digest is
	// bound under. Empty means HostedArtifactName(Backend.Image) for a
	// backend and the workload's own name for a static site (the frontend
	// name `forge build` records its site release under).
	Artifact string
	Backend  *v1alpha1.SimpleBackendSpec
	Database *v1alpha1.ManagedDatabaseSpec
	// Static is the deployed half of a StaticSite: everything but the
	// digest, which planHosted pins from the bound release as liveDigest.
	Static *v1alpha1.StaticSiteSpec
}

// HostedTarget is the env-level half of a hosted group.
type HostedTarget struct {
	// Endpoint is the control plane's normalized base URL. Display only —
	// the Caller already addresses it.
	Endpoint string
	// Release is the version the env is promoted to. Empty means unbound,
	// which a deploy refuses: a hosted deploy ships only promoted digests.
	Release string
	// Digests is the bound release's artifact → digest map (the ledger's
	// Resolved set).
	Digests map[string]string
}

// ─── Wire (controlplane.v1, proto3 JSON) ─────────────────────────────────────

const (
	procEnsureEnvironment = "controlplane.v1.DeployService/EnsureEnvironment"
	procEnsureDeployment  = "controlplane.v1.DeployService/EnsureDeployment"
	procPublishConfig     = "controlplane.v1.DeployService/PublishDeploymentConfig"
	procGetStatus         = "controlplane.v1.DeployService/GetStatus"
	procListEnvironments  = "controlplane.v1.DeployService/ListEnvironments"
	procListPromotions    = "controlplane.v1.DeployService/ListPromotions"
	procRollback          = "controlplane.v1.DeployService/Rollback"
)

type wireEnvironment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// ImagePushBase is `<registry_base>/<org>`: the one registry subtree the
	// control plane admits this org's images from. Empty means it admits
	// none (no registry base is configured), so every backend publish fails.
	ImagePushBase string `json:"imagePushBase,omitempty"`
}

type wireObserved struct {
	State       string     `json:"state,omitempty"`
	Replicas    int        `json:"replicas,omitempty"`
	ImageDigest string     `json:"imageDigest,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	URL         string     `json:"url,omitempty"`
	StableSince *time.Time `json:"stableSince,omitempty"`
}

type wireDeployment struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Tier     string        `json:"tier,omitempty"`
	Observed *wireObserved `json:"observed,omitempty"`
	RunState string        `json:"runState,omitempty"`
}

type wireDeploymentStatus struct {
	Deployment    wireDeployment `json:"deployment"`
	Verdict       string         `json:"verdict,omitempty"`
	VerdictReason string         `json:"verdictReason,omitempty"`
	DesiredDigest string         `json:"desiredDigest,omitempty"`
	Drifted       bool           `json:"drifted,omitempty"`
	ObservedAt    *time.Time     `json:"observedAt,omitempty"`
}

type wireStatusResponse struct {
	Deployments        []wireDeploymentStatus `json:"deployments"`
	EnvironmentVerdict string                 `json:"environmentVerdict,omitempty"`
	ReconcilePolicy    string                 `json:"reconcilePolicy,omitempty"`
}

type wirePromotion struct {
	ReleaseVersion    string            `json:"releaseVersion"`
	ResolvedArtifacts map[string]string `json:"resolvedArtifacts,omitempty"`
}

// The verdict and observed-state value names this file branches on.
const (
	wireVerdictPrefix     = "DEPLOY_VERDICT_"
	wireVerdictConverged  = "DEPLOY_VERDICT_CONVERGED"
	wireObservedPrefix    = "DEPLOY_OBSERVED_STATE_"
	wireObservedReady     = "DEPLOY_OBSERVED_STATE_READY"
	wireObservedPending   = "DEPLOY_OBSERVED_STATE_PENDING"
	wireObservedProgress  = "DEPLOY_OBSERVED_STATE_PROGRESSING"
	wireObservedDegraded  = "DEPLOY_OBSERVED_STATE_DEGRADED"
	wireObservedSuspended = "DEPLOY_OBSERVED_STATE_SUSPENDED"
	wireObservedDeleted   = "DEPLOY_OBSERVED_STATE_DELETED"
)

// ─── Environment lookup (shared with the CLI's hosted ledger and secrets) ────

// ErrHostedEnvironmentNotFound is LookupHostedEnvironment's "no environment of
// that name" answer.
var ErrHostedEnvironmentNotFound = errors.New("the control plane has no environment")

// LookupHostedEnvironment resolves an environment NAME to the control plane's
// id, by listing the caller's environments and matching the name EXACTLY.
// `search` only narrows server-side: it is a free-text match, and "prod" must
// not resolve to "prod-eu". Two exact matches is a contract violation and is
// refused rather than guessed at — a guess would write into the wrong env.
func LookupHostedEnvironment(ctx context.Context, c HostedCaller, envName string) (string, error) {
	env, err := lookupHostedEnvironment(ctx, c, envName)
	return env.ID, err
}

func lookupHostedEnvironment(ctx context.Context, c HostedCaller, envName string) (wireEnvironment, error) {
	var resp struct {
		Environments []wireEnvironment `json:"environments"`
	}
	if err := c.Call(ctx, procListEnvironments, map[string]any{"search": envName}, &resp); err != nil {
		return wireEnvironment{}, fmt.Errorf("resolve hosted environment %q: %w", envName, err)
	}
	var matches []wireEnvironment
	for _, e := range resp.Environments {
		if e.Name == envName {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return wireEnvironment{}, fmt.Errorf("%w named %q\nfix: `forge env deploy %s` creates it on first deploy",
			ErrHostedEnvironmentNotFound, envName, envName)
	case 1:
		if strings.TrimSpace(matches[0].ID) == "" {
			return wireEnvironment{}, fmt.Errorf("the control plane returned environment %q with no id", envName)
		}
		return matches[0], nil
	default:
		return wireEnvironment{}, fmt.Errorf("the control plane returned %d environments named %q; refusing to guess", len(matches), envName)
	}
}

// EnsureHostedEnvironment makes an environment of this NAME exist in the
// caller's org and returns its id and whether this call created it. It is the
// one write every hosted path that needs an environment goes through, so a
// hosted env is DECLARED in KCL and never provisioned by a manual step —
// whichever of promote / deploy runs first creates it.
func EnsureHostedEnvironment(ctx context.Context, c HostedCaller, envName string) (string, bool, error) {
	env, created, err := ensureHostedEnvironment(ctx, c, envName)
	return env.ID, created, err
}

func ensureHostedEnvironment(ctx context.Context, c HostedCaller, envName string) (wireEnvironment, bool, error) {
	var ensured struct {
		Environment wireEnvironment `json:"environment"`
		Created     bool            `json:"created"`
	}
	if err := c.Call(ctx, procEnsureEnvironment, map[string]any{
		"spec": map[string]any{"name": envName, "kind": "DEPLOY_ENVIRONMENT_KIND_PERSISTENT"},
	}, &ensured); err != nil {
		return wireEnvironment{}, false, fmt.Errorf("ensure environment %q: %w", envName, err)
	}
	if ensured.Environment.ID == "" {
		return wireEnvironment{}, false, fmt.Errorf("ensure environment %q: the control plane returned no environment id", envName)
	}
	return ensured.Environment, ensured.Created, nil
}

// checkImagePushBase refuses every backend in the plan whose pinned image is
// not under the org's image push base — the subtree the control plane's
// publish-time boundary admits. It runs after the environment is known (the
// base comes back on it) and BEFORE any EnsureDeployment or publish, so an
// image the platform will refuse costs no deployment write and no ledger
// entry. Every offending backend is reported together.
//
// An EMPTY base means the control plane has no registry configured and admits
// no image at all; that is refused too, unless the plan carries no backend.
//
// This is deliberately stricter than the server's own Contains, which lets an
// image OUTSIDE the platform registry through to the tier's registry
// allowlist: the hosted path ships only images forge can prove the platform
// will run, and the allowlist is not exposed.
func checkImagePushBase(envName, base string, plan []hostedPlanItem) error {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	var errs []error
	for _, item := range plan {
		if item.Tier != HostedTierBackend {
			continue
		}
		spec, ok := item.Spec.(v1alpha1.SimpleBackendSpec)
		if !ok {
			continue
		}
		if base == "" {
			return fmt.Errorf("hosted env %q: the control plane reports no image push base, so it admits no registry "+
				"and would refuse every backend image (first: %s for %s).\n"+
				"  fix: the control plane must be configured with a registry base (ImageBuildConfig.registry_base); "+
				"nothing a deploy can change will make it publish", envName, spec.Image, item.Name)
		}
		repo := HostedImageRepository(spec.Image)
		if !strings.HasPrefix(repo, base+"/") {
			errs = append(errs, fmt.Errorf("%s: image %s is not under this org's image push base %s, and the control plane "+
				"refuses to publish it.\n"+
				"  fix: push the image to %s/%s, re-cut the release (forge release cut <version> --env %s), "+
				"then re-promote it (forge env promote <version> --to %s)",
				item.Name, spec.Image, base, base, HostedArtifactName(spec.Image), envName, envName))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("hosted env %q: refusing to publish anything — %d image(s) are outside the registry the platform admits:\n  %w",
			envName, len(errs), errors.Join(errs...))
	}
	return nil
}

// ─── Artifact naming and digest pinning ──────────────────────────────────────

// HostedImageRepository is an image reference with its digest or tag removed.
func HostedImageRepository(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		image = image[:i]
	}
	if slash, colon := strings.LastIndex(image, "/"), strings.LastIndex(image, ":"); colon > slash {
		image = image[:colon]
	}
	return image
}

// HostedArtifactName is the release-ledger artifact name a hosted backend's
// image is recorded and bound under: the repository's LAST path segment
// ("ghcr.io/acme/api:v1" → "api"). That is the same key forge's own builds use
// (a registry-less image name, with the registry held separately as the
// artifact's URI), so a backend whose image forge builds and one whose image
// CI pushes resolve through one rule.
func HostedArtifactName(image string) string {
	repo := HostedImageRepository(image)
	return repo[strings.LastIndex(repo, "/")+1:]
}

// HostedImageDigest is the digest an image reference is pinned by, or "".
func HostedImageDigest(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[i+1:]
	}
	return ""
}

// ─── Planning (pure: no RPCs) ────────────────────────────────────────────────

// hostedPlanItem is one workload ready to publish: its name, tier and the
// exact spec document the control plane will store.
type hostedPlanItem struct {
	Name          string
	Tier          HostedTier
	Spec          any
	DesiredDigest string
}

// planHosted pins, validates and shape-checks every workload in a hosted
// group, returning the specs to publish. It makes no calls, so a refusal here
// provably wrote nothing. EVERY workload is checked and every problem is
// reported together: a deploy refused one error at a time is a deploy that
// takes N attempts to learn N things.
func planHosted(group ServiceGroup) ([]hostedPlanItem, error) {
	return planHostedWith(group, groupDigests(group))
}

func groupDigests(group ServiceGroup) map[string]string {
	if group.Hosted == nil {
		return nil
	}
	return group.Hosted.Digests
}

func planHostedWith(group ServiceGroup, digests map[string]string) ([]hostedPlanItem, error) {
	if group.Hosted == nil {
		return nil, errors.New("hosted deploy: the group carries no hosted target")
	}
	if group.Hosted.Release == "" && hostedGroupHasPinnedArtifact(group) {
		return nil, fmt.Errorf("hosted env %q has no promoted release, so there is no digest to deploy.\n"+
			"A hosted deploy ships only the digests a promotion froze — never a tag, never a local build.\n"+
			"fix: forge release cut <version> --env %s && forge env promote <version> --to %s",
			group.Env, group.Env, group.Env)
	}
	var (
		out  []hostedPlanItem
		errs []error
		seen = map[string]string{}
	)
	for _, svc := range group.Services {
		w := svc.Hosted
		if w == nil {
			errs = append(errs, fmt.Errorf("%s: not a hosted tier workload", svc.Name))
			continue
		}
		switch w.Tier {
		case HostedTierBackend:
			if w.Backend == nil {
				errs = append(errs, fmt.Errorf("%s: backend workload has no spec", svc.Name))
				continue
			}
			spec := *w.Backend
			artifact := w.Artifact
			if artifact == "" {
				artifact = HostedArtifactName(spec.Image)
			}
			if other, dup := seen[artifact]; dup && HostedImageRepository(spec.Image) != other {
				errs = append(errs, fmt.Errorf("%s: image %q and %q share the release artifact name %q, so one binding cannot pin both; rename one repository",
					svc.Name, spec.Image, other, artifact))
				continue
			}
			seen[artifact] = HostedImageRepository(spec.Image)
			digest, ok := digests[artifact]
			if !ok || digest == "" {
				errs = append(errs, fmt.Errorf("%s: release %s pins no artifact %q (the backend's image %s).\n"+
					"  fix: re-cut the release so it covers this backend (forge release cut <version> --env %s), then promote it",
					svc.Name, group.Hosted.Release, artifact, spec.Image, group.Env))
				continue
			}
			spec.Image = HostedImageRepository(spec.Image) + "@" + digest
			if err := spec.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", svc.Name, err))
				continue
			}
			if err := deploy.CheckShapeBand(spec.Resources); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", svc.Name, err))
				continue
			}
			out = append(out, hostedPlanItem{Name: svc.Name, Tier: w.Tier, Spec: spec, DesiredDigest: digest})
		case HostedTierStatic:
			if w.Static == nil {
				errs = append(errs, fmt.Errorf("%s: static site workload has no spec", svc.Name))
				continue
			}
			spec := *w.Static
			artifact := w.Artifact
			if artifact == "" {
				artifact = svc.Name
			}
			digest, ok := digests[artifact]
			if !ok || digest == "" {
				errs = append(errs, fmt.Errorf("%s: release %s pins no static site artifact %q.\n"+
					"  fix: build and push the site (forge build %s --push <image push base>), re-cut the release "+
					"(forge release cut <version> --env %s), then promote it",
					svc.Name, group.Hosted.Release, artifact, group.Env, group.Env))
				continue
			}
			// The release IS the digest. Nothing else in the spec says
			// where bytes come from: the operator derives the repository
			// from the org the CR belongs to, never from user input.
			spec.LiveDigest = digest
			if err := spec.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", svc.Name, err))
				continue
			}
			out = append(out, hostedPlanItem{Name: svc.Name, Tier: w.Tier, Spec: spec, DesiredDigest: digest})
		case HostedTierDatabase:
			if w.Database == nil {
				errs = append(errs, fmt.Errorf("%s: database workload has no spec", svc.Name))
				continue
			}
			spec := w.Database.WithDefaults()
			if err := spec.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", svc.Name, err))
				continue
			}
			out = append(out, hostedPlanItem{Name: svc.Name, Tier: w.Tier, Spec: spec})
		default:
			errs = append(errs, fmt.Errorf("%s: unknown hosted tier %q", svc.Name, w.Tier))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("hosted env %q: refusing to publish anything — %d workload(s) are not admissible:\n  %w",
			group.Env, len(errs), errors.Join(errs...))
	}
	return out, nil
}

// hostedGroupHasPinnedArtifact reports whether any workload ships bytes a
// release must pin — a backend image or a static site. A database-only env
// needs no release.
func hostedGroupHasPinnedArtifact(group ServiceGroup) bool {
	for _, s := range group.Services {
		if s.Hosted != nil && (s.Hosted.Tier == HostedTierBackend || s.Hosted.Tier == HostedTierStatic) {
			return true
		}
	}
	return false
}

// ─── The provider ────────────────────────────────────────────────────────────

// HostedProvider ships a hosted group through a control plane.
//
// The zero value is registered in NewRegistry so the observation audit covers
// it; the deploy dispatcher re-registers a configured one (Client set), the
// same pattern K8sClusterProvider uses for its ApplyOptsBuilder.
type HostedProvider struct {
	Client HostedCaller
	// Rollout bounds and shapes the readiness wait: mode skip waits for
	// nothing, warn reports without failing, wait (the default) fails on a
	// workload that never becomes ready. Timeout is the WHOLE wait.
	Rollout cluster.RolloutPolicy
	// PollInterval is the GetStatus cadence. Zero means 3s.
	PollInterval time.Duration
	// OnRollout, when set, receives one outcome per published workload.
	OnRollout func(cluster.RolloutObservation)
	// OnEnvironment, when set, receives the control plane's environment id
	// as soon as it is known, so a report can carry it even if a later step
	// fails.
	OnEnvironment func(id string)
}

// Name is the provider's registry id.
func (HostedProvider) Name() string { return HostedProviderID }

func (p HostedProvider) client() (HostedCaller, error) {
	if p.Client == nil {
		return nil, errors.New("hosted provider has no control-plane client (the deploy dispatcher must register a configured one)")
	}
	return p.Client, nil
}

// Deploy runs the fixed order in the file header.
func (p HostedProvider) Deploy(ctx context.Context, group ServiceGroup) error {
	plan, err := planHosted(group)
	if err != nil {
		return err
	}
	if group.DryRun {
		printHostedPlan(group, plan)
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	env, created, err := ensureHostedEnvironment(ctx, c, group.Env)
	if err != nil {
		return err
	}
	envID := env.ID
	if p.OnEnvironment != nil {
		p.OnEnvironment(envID)
	}
	verb := "exists"
	if created {
		verb = "created"
	}
	fmt.Printf("  environment %s %s (id %s)\n", group.Env, verb, envID)
	if err := checkImagePushBase(group.Env, env.ImagePushBase, plan); err != nil {
		return err
	}
	return p.publish(ctx, c, group, envID, plan)
}

// publish ensures every deployment, then publishes each, then waits.
func (p HostedProvider) publish(ctx context.Context, c HostedCaller, group ServiceGroup, envID string, plan []hostedPlanItem) error {
	ids := make(map[string]string, len(plan))
	for _, item := range plan {
		var resp struct {
			Deployment wireDeployment `json:"deployment"`
			Created    bool           `json:"created"`
			Updated    bool           `json:"updated"`
		}
		if err := c.Call(ctx, procEnsureDeployment, map[string]any{
			"environmentId": envID,
			"name":          item.Name,
			"tier":          item.Tier.wireTier(),
			"spec":          item.Spec,
		}, &resp); err != nil {
			return fmt.Errorf("ensure deployment %s: %w", item.Name, err)
		}
		if resp.Deployment.ID == "" {
			return fmt.Errorf("ensure deployment %s: the control plane returned no deployment id", item.Name)
		}
		ids[item.Name] = resp.Deployment.ID
		state := "unchanged"
		switch {
		case resp.Created:
			state = "created"
		case resp.Updated:
			state = "updated"
		}
		fmt.Printf("  deployment %s %s (id %s)\n", item.Name, state, resp.Deployment.ID)
	}
	for _, item := range plan {
		var resp struct {
			Digest    string `json:"digest"`
			Reference string `json:"reference"`
		}
		if err := c.Call(ctx, procPublishConfig, map[string]any{"deploymentId": ids[item.Name]}, &resp); err != nil {
			return fmt.Errorf("publish deployment %s: %w", item.Name, err)
		}
		fmt.Printf("  published %s → %s\n", item.Name, resp.Reference)
	}
	return p.wait(ctx, c, group.Env, envID, plan, ids)
}

// wait polls GetStatus{environmentId} until every published workload is
// ready, or the budget expires.
//
// READY is environmentVerdict CONVERGED, or — per workload — verdict
// CONVERGED, or observed READY carrying the desired digest. The second arm
// exists because CONVERGED additionally requires a stability window: a deploy
// that just rolled out is running the right bytes and is healthy, and making
// the CLI sit out the window would make every deploy as slow as the window.
// Timing out is reported as TIMED OUT, never as success.
func (p HostedProvider) wait(ctx context.Context, c HostedCaller, envName, envID string, plan []hostedPlanItem, ids map[string]string) error {
	policy := p.Rollout.Normalize()
	if policy.Mode == cluster.RolloutSkip {
		for _, item := range plan {
			p.observe(item.Name, cluster.RolloutStateNotWaited, nil)
		}
		fmt.Println("  readiness: not waited (--rollout skip)")
		return nil
	}
	interval := p.PollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(policy.Timeout)
	var last map[string]string
	for {
		pending, reasons, err := p.pollOnce(ctx, c, envID, plan, ids)
		if err == nil && len(pending) == 0 {
			for _, item := range plan {
				p.observe(item.Name, cluster.RolloutStateReady, nil)
			}
			fmt.Printf("  readiness: all %d workload(s) ready\n", len(plan))
			return nil
		}
		if err == nil {
			last = reasons
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			var lines []string
			for _, item := range plan {
				if r, notReady := last[item.Name]; notReady || last == nil {
					p.observe(item.Name, cluster.RolloutStateTimedOut, errors.New(r))
					lines = append(lines, fmt.Sprintf("%s: %s", item.Name, emptyOr(r, "no status reported")))
				} else {
					p.observe(item.Name, cluster.RolloutStateReady, nil)
				}
			}
			sort.Strings(lines)
			werr := fmt.Errorf("hosted env %q: TIMED OUT after %s waiting for readiness (published, not confirmed running):\n  %s",
				envName, policy.Timeout, strings.Join(lines, "\n  "))
			if err != nil {
				werr = fmt.Errorf("%w\n  last status read failed: %v", werr, err)
			}
			if policy.Mode == cluster.RolloutWarn {
				fmt.Printf("  Warning: %v\n", werr)
				return nil
			}
			return werr
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// pollOnce reads status once and returns the names still not ready, with a
// reason for each.
func (p HostedProvider) pollOnce(ctx context.Context, c HostedCaller, envID string, plan []hostedPlanItem, ids map[string]string) ([]string, map[string]string, error) {
	var resp wireStatusResponse
	if err := c.Call(ctx, procGetStatus, map[string]any{"environmentId": envID}, &resp); err != nil {
		return nil, nil, err
	}
	envConverged := resp.EnvironmentVerdict == wireVerdictConverged
	byID := map[string]wireDeploymentStatus{}
	for _, d := range resp.Deployments {
		byID[d.Deployment.ID] = d
	}
	var pending []string
	reasons := map[string]string{}
	for _, item := range plan {
		st, ok := byID[ids[item.Name]]
		if !ok {
			pending = append(pending, item.Name)
			reasons[item.Name] = "not in the environment's status yet"
			continue
		}
		if envConverged || hostedDeploymentReady(st, item.DesiredDigest) {
			continue
		}
		pending = append(pending, item.Name)
		reasons[item.Name] = describeHostedStatus(st)
	}
	return pending, reasons, nil
}

func hostedDeploymentReady(st wireDeploymentStatus, desiredDigest string) bool {
	if st.Verdict == wireVerdictConverged {
		return true
	}
	obs := st.Deployment.Observed
	if obs == nil || obs.State != wireObservedReady {
		return false
	}
	want := desiredDigest
	if st.DesiredDigest != "" {
		want = st.DesiredDigest
	}
	return want == "" || obs.ImageDigest == want
}

func describeHostedStatus(st wireDeploymentStatus) string {
	parts := []string{"verdict " + VerdictName(st.Verdict)}
	if obs := st.Deployment.Observed; obs != nil {
		parts = append(parts, "observed "+observedName(obs.State))
		if obs.LastError != "" {
			parts = append(parts, "error: "+obs.LastError)
		}
	}
	if st.VerdictReason != "" {
		parts = append(parts, st.VerdictReason)
	}
	return strings.Join(parts, ", ")
}

func (p HostedProvider) observe(name string, state cluster.RolloutState, err error) {
	if p.OnRollout != nil {
		p.OnRollout(cluster.RolloutObservation{Kind: "HostedDeployment", Name: name, State: state, Err: err})
	}
}

// Rollback returns the env to an earlier release and republishes it.
//
// lastGoodTag, when set, is the release VERSION to return to; empty means the
// newest release the env ran before its current one, read from the ledger.
// The older release's pinned digests are validated BEFORE the ledger is
// written, so an old release that is no longer admissible (a shape band that
// changed since) refuses with the ledger untouched.
func (p HostedProvider) Rollback(ctx context.Context, group ServiceGroup, lastGoodTag string) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	env, err := lookupHostedEnvironment(ctx, c, group.Env)
	if err != nil {
		return err
	}
	envID := env.ID
	var hist struct {
		Promotions []wirePromotion `json:"promotions"`
	}
	if err := c.Call(ctx, procListPromotions, map[string]any{"environmentId": envID, "limit": 50}, &hist); err != nil {
		return fmt.Errorf("read the promotion history of %q: %w", group.Env, err)
	}
	if len(hist.Promotions) == 0 {
		return fmt.Errorf("hosted env %q has never been promoted; there is nothing to roll back to", group.Env)
	}
	current := hist.Promotions[0].ReleaseVersion
	var target *wirePromotion
	for i := range hist.Promotions {
		pr := hist.Promotions[i]
		if lastGoodTag != "" && pr.ReleaseVersion == lastGoodTag || lastGoodTag == "" && pr.ReleaseVersion != current {
			target = &pr
			break
		}
	}
	if target == nil {
		if lastGoodTag != "" {
			return fmt.Errorf("hosted env %q has never run release %s, so it cannot be rolled back to it", group.Env, lastGoodTag)
		}
		return fmt.Errorf("hosted env %q has only ever run release %s; there is no earlier release to roll back to", group.Env, current)
	}
	if target.ReleaseVersion == current {
		return fmt.Errorf("hosted env %q already runs release %s", group.Env, current)
	}
	rollbackGroup := group
	hosted := HostedTarget{Release: target.ReleaseVersion, Digests: target.ResolvedArtifacts}
	if group.Hosted != nil {
		hosted.Endpoint = group.Hosted.Endpoint
	}
	rollbackGroup.Hosted = &hosted
	plan, err := planHosted(rollbackGroup)
	if err != nil {
		return fmt.Errorf("roll back to %s: %w", target.ReleaseVersion, err)
	}
	if group.DryRun {
		fmt.Printf("  would roll %s back from %s to %s\n", group.Env, current, target.ReleaseVersion)
		printHostedPlan(rollbackGroup, plan)
		return nil
	}
	// Before the Rollback write: a release whose images the platform will no
	// longer publish must not move the ledger to a state it cannot run.
	if err := checkImagePushBase(group.Env, env.ImagePushBase, plan); err != nil {
		return fmt.Errorf("roll back to %s: %w", target.ReleaseVersion, err)
	}
	// A deploy-triggered rollback is recovery from a failed publish, so its
	// ledger note says so. An operator-authored note belongs on
	// `forge env promote --rollback --note`, which writes the ledger directly.
	if err := c.Call(ctx, procRollback, map[string]any{
		"environmentId": envID, "version": target.ReleaseVersion, "note": "forge env deploy --rollback",
	}, nil); err != nil {
		return fmt.Errorf("record the rollback to %s: %w", target.ReleaseVersion, err)
	}
	if p.OnEnvironment != nil {
		p.OnEnvironment(envID)
	}
	fmt.Printf("  rolled %s back from %s to %s (recorded on the ledger)\n", group.Env, current, target.ReleaseVersion)
	return p.publish(ctx, c, rollbackGroup, envID, plan)
}

func printHostedPlan(group ServiceGroup, plan []hostedPlanItem) {
	endpoint := ""
	if group.Hosted != nil {
		endpoint = group.Hosted.Endpoint
	}
	fmt.Printf("  [dry-run] would publish %d workload(s) to %s (release %s):\n", len(plan), endpoint, group.Hosted.Release)
	for _, item := range plan {
		raw, _ := json.Marshal(item.Spec)
		fmt.Printf("    %s (%s): %s\n", item.Name, item.Tier, raw)
	}
}

// ─── Status (shared by Observe and the CLI's topology/status JSON) ───────────

// HostedWorkloadStatus is one deployment as the console contract reports it.
type HostedWorkloadStatus struct {
	Name string `json:"name"`
	// Tier is backend | database | static.
	Tier string `json:"tier,omitempty"`
	// URL is the platform-allocated public URL, once one exists.
	URL string `json:"url,omitempty"`
	// Hostname is URL's host, for a badge that has no room for a scheme.
	Hostname string `json:"hostname,omitempty"`
	// Verdict is the control plane's lower-case deploystate verdict:
	// unknown | converging | converged | diverged | degraded.
	Verdict       string `json:"verdict"`
	VerdictReason string `json:"verdict_reason,omitempty"`
	// ObservedState is the observation vocabulary: pending | progressing |
	// ready | degraded | suspended | deleted | unknown.
	ObservedState  string `json:"observed_state"`
	ObservedDigest string `json:"observed_digest,omitempty"`
	DesiredDigest  string `json:"desired_digest,omitempty"`
	Drifted        bool   `json:"drifted,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// HostedEnvStatus is one environment's hosted status.
type HostedEnvStatus struct {
	EnvironmentID string
	// Verdict is the environment-level verdict, lower-case.
	Verdict         string
	ReconcilePolicy string
	Workloads       []HostedWorkloadStatus
}

// ReadHostedStatus reads an environment's status by NAME. An environment the
// control plane has never heard of returns ErrHostedEnvironmentNotFound.
func ReadHostedStatus(ctx context.Context, c HostedCaller, envName string) (HostedEnvStatus, error) {
	envID, err := LookupHostedEnvironment(ctx, c, envName)
	if err != nil {
		return HostedEnvStatus{}, err
	}
	var resp wireStatusResponse
	if err := c.Call(ctx, procGetStatus, map[string]any{"environmentId": envID}, &resp); err != nil {
		return HostedEnvStatus{EnvironmentID: envID}, err
	}
	out := HostedEnvStatus{
		EnvironmentID:   envID,
		Verdict:         VerdictName(resp.EnvironmentVerdict),
		ReconcilePolicy: strings.ToLower(strings.TrimPrefix(resp.ReconcilePolicy, "DEPLOY_RECONCILE_POLICY_")),
	}
	for _, d := range resp.Deployments {
		ws := HostedWorkloadStatus{
			Name:          d.Deployment.Name,
			Tier:          strings.ToLower(strings.TrimPrefix(d.Deployment.Tier, "DEPLOY_TIER_")),
			Verdict:       VerdictName(d.Verdict),
			VerdictReason: d.VerdictReason,
			DesiredDigest: d.DesiredDigest,
			Drifted:       d.Drifted,
			ObservedState: "unknown",
		}
		if obs := d.Deployment.Observed; obs != nil {
			ws.ObservedState = observedName(obs.State)
			ws.ObservedDigest = obs.ImageDigest
			ws.LastError = obs.LastError
			ws.URL = obs.URL
			if u, perr := url.Parse(obs.URL); perr == nil {
				ws.Hostname = u.Hostname()
			}
		}
		out.Workloads = append(out.Workloads, ws)
	}
	sort.Slice(out.Workloads, func(i, j int) bool { return out.Workloads[i].Name < out.Workloads[j].Name })
	return out, nil
}

// VerdictName renders a DeployVerdict value name in the lower-case vocabulary
// forge's deploystate uses. Unset and unrecognised values are "unknown" —
// never defaulted toward health.
func VerdictName(wire string) string {
	switch v := strings.ToLower(strings.TrimPrefix(wire, wireVerdictPrefix)); v {
	case "converging", "converged", "diverged", "degraded":
		return v
	default:
		return "unknown"
	}
}

func observedName(wire string) string {
	switch v := strings.ToLower(strings.TrimPrefix(wire, wireObservedPrefix)); v {
	case "pending", "progressing", "ready", "degraded", "suspended", "deleted":
		return v
	default:
		return "unknown"
	}
}

func emptyOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
