package deploytarget

import (
	"context"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// F1 sends the release binding on EnsureDeployment: the `artifact` key whose
// digest a deployment runs, and the `promotionId` whose pins its spec was
// rendered from.
//
// THE CLIENT SENDS THE ARTIFACT RATHER THAN THE SERVER INFERRING IT, so the
// two cannot pair a workload with a digest differently. That makes the exact
// value per tier a wire contract, not an implementation detail — hence a test
// per tier.

// ensureBodyFor returns the EnsureDeployment request body for one workload.
func ensureBodyFor(t *testing.T, cp *fakeCP, name string) map[string]any {
	t.Helper()
	for _, c := range cp.calls {
		if !hasSuffix(c.Proc, "EnsureDeployment") {
			continue
		}
		if c.Body["name"] == name {
			return c.Body
		}
	}
	t.Fatalf("no EnsureDeployment for %q", name)
	return nil
}

func hasSuffix(proc, short string) bool {
	return len(proc) >= len(short) && proc[len(proc)-len(short):] == short
}

// TestHostedArtifactOf_PerTier is the unit half: the artifact key each tier
// is bound under.
//
// THE DATABASE ROW IS THE POINT. hostedArtifactOf used to fall through to
// `return name` for every non-Workload tier, so a ManagedDatabase got the
// deployment's own NAME as its artifact. That was latent while nothing sent
// the value; the moment F1 sends it, the control plane refuses it twice over
// (InvalidArgument in EnsureDeployment, and the schema's
// ck_cp_deployments_artifact_release_bound_tier behind that), so every env
// with a hosted database would fail to deploy.
func TestHostedArtifactOf_PerTier(t *testing.T) {
	t.Parallel()

	// A MANAGED DATABASE IS NEVER RELEASE-BOUND: a promotion moves a
	// Workload's digest or a StaticSite's release and nothing else.
	db := &HostedWorkload{Tier: HostedTierDatabase, Database: &v1alpha1.ManagedDatabaseSpec{}}
	if got := hostedArtifactOf("orders", db); got != "" {
		t.Errorf("a database's artifact = %q, want empty (it is never release-bound)", got)
	}
	// Even when a declaration NAMES one. Honouring it would only move the
	// platform's refusal to the far side of the wire, where the message is
	// worse.
	declared := &HostedWorkload{Tier: HostedTierDatabase, Artifact: "orders-db",
		Database: &v1alpha1.ManagedDatabaseSpec{}}
	if got := hostedArtifactOf("orders", declared); got != "" {
		t.Errorf("a database with a declared artifact = %q, want empty", got)
	}

	// A WORKLOAD is bound under its image repository.
	workload := &HostedWorkload{Tier: HostedTierWorkload,
		Workload: &v1alpha1.WorkloadSpec{Image: "ghcr.io/acme/api:v1"}}
	if got, want := hostedArtifactOf("api", workload), HostedArtifactName("ghcr.io/acme/api:v1"); got != want {
		t.Errorf("a workload's artifact = %q, want the image repo %q", got, want)
	}
	// A declared artifact wins for a release-bound tier: the ledger may
	// key the bytes under a name the image does not derive.
	pinned := &HostedWorkload{Tier: HostedTierWorkload, Artifact: "hounders",
		Workload: &v1alpha1.WorkloadSpec{Image: "ghcr.io/acme/api:v1"}}
	if got := hostedArtifactOf("api", pinned); got != "hounders" {
		t.Errorf("a declared workload artifact = %q, want %q", got, "hounders")
	}

	// A STATIC SITE is bound under its declared artifact — the repository
	// `forge build` recorded the site release under.
	static := &HostedWorkload{Tier: HostedTierStatic, Artifact: "ghcr.io/acme/web/site",
		Static: &v1alpha1.StaticSiteSpec{}}
	if got := hostedArtifactOf("web", static); got != "ghcr.io/acme/web/site" {
		t.Errorf("a static site's artifact = %q", got)
	}
	// With none declared it falls back to the frontend NAME, which is the
	// key `forge build` records a site release under. Unlike the database
	// case this fallback is correct: a static site IS release-bound.
	bare := &HostedWorkload{Tier: HostedTierStatic, Static: &v1alpha1.StaticSiteSpec{}}
	if got := hostedArtifactOf("web", bare); got != "web" {
		t.Errorf("a static site with no declared artifact = %q, want the name %q", got, "web")
	}
}

// TestHostedEnsureDeployment_SendsArtifactPerTier is the wire half, and the
// one that would have caught the bug: a WORKLOAD carries its artifact, and a
// DATABASE carries none at all.
//
// `artifact` must be ABSENT rather than present-and-empty on a database.
// Empty is not a value the platform accepts — the schema stores NULL for
// "not release-bound", one spelling, so no reader has to treat two as equal.
func TestHostedEnsureDeployment_SendsArtifactPerTier(t *testing.T) {
	cp := &fakeCP{status: readyStatus(digestA)}
	p := HostedProvider{Client: cp, PollInterval: time.Millisecond}
	group := hostedGroup("v1", map[string]string{"api": digestA}, v1alpha1.Resources{})
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// The workload: the artifact key the plan looked its digest up under.
	api := ensureBodyFor(t, cp, "api")
	if api["tier"] != "DEPLOY_TIER_BACKEND" {
		t.Fatalf("api tier = %v", api["tier"])
	}
	if got := api["artifact"]; got != "api" {
		t.Errorf("api artifact = %v, want %q (the key its digest was pinned under)", got, "api")
	}

	// The database: no artifact at all.
	orders := ensureBodyFor(t, cp, "orders")
	if orders["tier"] != "DEPLOY_TIER_DATABASE" {
		t.Fatalf("orders tier = %v", orders["tier"])
	}
	if raw, present := orders["artifact"]; present {
		t.Errorf("a database must carry NO artifact field; got %q.\n"+
			"  The control plane refuses one (InvalidArgument, plus "+
			"ck_cp_deployments_artifact_release_bound_tier), so sending it "+
			"breaks every env with a hosted database.", raw)
	}
}

// TestHostedEnsureDeployment_SendsStaticArtifact covers the third tier
// against the static fixture, whose artifact is a repository rather than a
// bare name.
func TestHostedEnsureDeployment_SendsStaticArtifact(t *testing.T) {
	cp := &fakeCP{status: staticReadyStatus(digestB)}
	p := HostedProvider{Client: cp, PollInterval: time.Millisecond}
	if err := p.Deploy(context.Background(), staticGroup("v2", map[string]string{"web": digestB})); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	web := ensureBodyFor(t, cp, "web")
	if web["tier"] != "DEPLOY_TIER_STATIC" {
		t.Fatalf("web tier = %v", web["tier"])
	}
	// staticGroup declares Artifact: "web", and the digest was looked up
	// under that same key — which is the invariant: what forge SENDS is
	// what forge looked the digest up under.
	if got := web["artifact"]; got != "web" {
		t.Errorf("static artifact = %v, want %q", got, "web")
	}
}

// TestHostedEnsureDeployment_SendsPromotionID: applied_promotion_id is the
// discriminator between "a promotion is waiting to be applied" and "this row
// has drifted" — two states that need opposite answers under the OBSERVE
// policy and that a digest comparison cannot tell apart, since both read as
// "the row does not match the pin".
//
// It goes on EVERY deployment including the database: the promotion is a
// fact about which declaration this row carries, not about release binding,
// so a database row pinned by a promotion is perfectly meaningful.
func TestHostedEnsureDeployment_SendsPromotionID(t *testing.T) {
	cp := &fakeCP{status: readyStatus(digestA)}
	p := HostedProvider{Client: cp, PollInterval: time.Millisecond}
	group := hostedGroup("v1", map[string]string{"api": digestA}, v1alpha1.Resources{})
	group.Hosted.PromotionID = "pr_42"
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	for _, name := range []string{"api", "orders"} {
		if got := ensureBodyFor(t, cp, name)["promotionId"]; got != "pr_42" {
			t.Errorf("%s promotionId = %v, want %q", name, got, "pr_42")
		}
	}
}

// TestHostedEnsureDeployment_OmitsAnAbsentPromotionID: absent and empty mean
// DIFFERENT things server-side. An omitted promotion leaves the stored one
// untouched — the correct reading of a hand-run deploy, which is drift and
// not a promotion — while an empty value would be a claim that this row is
// pinned from no promotion, clearing a real one.
func TestHostedEnsureDeployment_OmitsAnAbsentPromotionID(t *testing.T) {
	cp := &fakeCP{status: readyStatus(digestA)}
	p := HostedProvider{Client: cp, PollInterval: time.Millisecond}
	// No PromotionID set: a control plane that predates promotion ids, or
	// a deploy forge could not attribute to one.
	group := hostedGroup("v1", map[string]string{"api": digestA}, v1alpha1.Resources{})
	if err := p.Deploy(context.Background(), group); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	for _, name := range []string{"api", "orders"} {
		if raw, present := ensureBodyFor(t, cp, name)["promotionId"]; present {
			t.Errorf("%s sent promotionId=%q; an unattributable deploy must OMIT it, "+
				"because an empty value would clear the stored promotion", name, raw)
		}
	}
}
