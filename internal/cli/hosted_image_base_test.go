package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/hostedimage"
)

// ADR-0003 F1. A bare image on a hosted item resolves to the env's image push
// base; everything else is untouched.
//
// The table is the contract, row by row, because each row is a different
// REASON rather than a different input:
//
//   - bare + hosted      → the platform owns the registry, so forge supplies it
//   - host-bearing       → the author named one and meant it; verbatim
//   - bare + non-hosted  → nobody owns that registry; unchanged (and KCL still
//     refuses it at render for a cluster workload)
//   - bare + no base     → nothing to resolve under; refused, not guessed
func TestResolveHostedImageBase(t *testing.T) {
	const base = "registry.reliant.dev/org-7"
	for _, tc := range []struct {
		name, pushBase, image, want string
	}{
		{"bare hosted resolves under the push base", base, "hounders", base + "/hounders"},
		{"a multi-segment bare name keeps its path", base, "team/api", base + "/team/api"},
		{"a host-bearing image is verbatim", base, "ghcr.io/acme/api", "ghcr.io/acme/api"},
		{"an image already under the base is verbatim", base, base + "/api", base + "/api"},
		{"a localhost registry is host-bearing", base, "localhost:5051/api", "localhost:5051/api"},
		{"a port-bearing host is host-bearing", base, "127.0.0.1:5000/api", "127.0.0.1:5000/api"},
		{"no push base leaves a bare name bare", "", "hounders", "hounders"},
		{"a trailing slash on the base is not a difference", base + "/", "api", base + "/api"},
		{"an empty image stays empty", base, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveHostedImageBase(tc.pushBase, tc.image); got != tc.want {
				t.Errorf("resolveHostedImageBase(%q, %q) = %q, want %q", tc.pushBase, tc.image, got, tc.want)
			}
		})
	}
}

// The push plan is where build and deploy agree, so the resolution has to
// happen THERE and not only in the helper. A bare hosted workload becomes a
// real push destination; a bare CLUSTER workload does not, exactly as before.
func TestDeclaredImageDestinations_BareHostedResolvesBareClusterDoesNot(t *testing.T) {
	const base = "registry.reliant.dev/org-7"
	ents := &KCLEntities{Workloads: []WorkloadEntity{
		hostedWL("api", func(w *WorkloadEntity) {
			w.Image = "api"
			w.Spec.Image = "api"
			w.Build.Type = "go"
		}),
		clusterWL("worker", "c", "n", func(w *WorkloadEntity) {
			w.Image = "worker"
			w.Build.Type = "go"
		}),
	}}
	got := declaredImageDestinationsWithBase(ents, base)
	if len(got) != 1 {
		t.Fatalf("destinations = %+v, want exactly the hosted one", got)
	}
	if got[0].repository != base+"/api" {
		t.Errorf("hosted destination = %q, want %q", got[0].repository, base+"/api")
	}
	// The cluster workload is absent, and that is not an oversight: no
	// platform owns its registry, so a base borrowed from the control plane
	// would push it somewhere nobody declared.
	for _, d := range got {
		if d.workload == "worker" {
			t.Errorf("a bare CLUSTER image became a destination at %q", d.repository)
		}
	}
}

// A hosted FRONTEND's bare image resolves the same way, and the platform's
// static.v1 layout is appended to the RESULT — so the release key is the
// address the site was pushed to, which is the invariant
// hosted_static_release_ref_test.go pins on the deploy side.
func TestDeclaredImageDestinations_BareHostedFrontendGetsBaseThenStaticLayout(t *testing.T) {
	const base = "registry.reliant.dev/org-7"
	ents := &KCLEntities{Frontends: []FrontendEntity{{
		Name: "web", Image: "web", Runtime: FrontendRuntime{Type: RuntimeHosted},
	}}}
	got := declaredImageDestinationsWithBase(ents, base)
	if len(got) != 1 || got[0].repository != base+"/web/static.v1" {
		t.Fatalf("destinations = %+v, want %s/web/static.v1", got, base)
	}
}

// With no push base there is nothing to resolve a bare hosted image under.
// forge refuses rather than inventing a default, and the message is the
// pre-ADR-0003 remedy, because declaring the reference is the only thing the
// author can do from here.
func TestCheckHostedImagesResolve_NoPushBaseNamesTheFullReferenceRemedy(t *testing.T) {
	// Spec.Image is set as well as Image, because that is what the render
	// produces: the resolved reference a runtime pulls lands on the spec,
	// and for a BARE hosted image the render keeps it bare rather than
	// inventing a host (positive_hosted_image_bare_name.k). Setting only
	// Image would describe a workload no render emits.
	ents := &KCLEntities{Workloads: []WorkloadEntity{
		hostedWL("api", func(w *WorkloadEntity) {
			w.Image = "api"
			w.Spec.Image = "api"
			w.Build.Type = "go"
		}),
	}}
	err := checkHostedImagesResolve("prod", ents, "")
	if err == nil {
		t.Fatal("a bare hosted image with no push base must be refused")
	}
	// The remedy is to DECLARE THE ORG, not to write a full reference: a
	// hosted author's registry is the platform's, so telling them to
	// transcribe a host would be telling them to restate a value forge
	// already knows the shape of — the defect ADR-0003 F1 closed.
	for _, want := range []string{"names no registry host", "declares no organization", "organization = ", `"api"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q:\n%v", want, err)
		}
	}
	// With a base, the same declaration is fine.
	if err := checkHostedImagesResolve("prod", ents, "registry.reliant.dev/org-7"); err != nil {
		t.Errorf("a bare hosted image WITH a push base must be accepted, got: %v", err)
	}
	// A bare image on a cluster workload is not this check's business: KCL
	// refuses it at render, and reporting it here would give one mistake two
	// different messages.
	cluster := &KCLEntities{Workloads: []WorkloadEntity{
		clusterWL("worker", "c", "n", func(w *WorkloadEntity) {
			w.Image = "worker"
			w.Build.Type = "go"
		}),
	}}
	if err := checkHostedImagesResolve("prod", cluster, ""); err != nil {
		t.Errorf("a bare CLUSTER image is KCL's refusal, not this one, got: %v", err)
	}
}

// The push base is COMPOSED FROM THE DECLARATION, with no call and no cache.
// This is the property every offline consumer rests on: `forge env render`
// and `forge lint` compare an image against it without a credential.
func TestDeclaredPushBase(t *testing.T) {
	const org = "4f3c2b1a-0000-4000-8000-000000000001"
	restore := hostedProjectName
	hostedProjectName = func() string { return "shop" }
	t.Cleanup(func() { hostedProjectName = restore })

	// No control plane at all: no base, and that is not an error — an env
	// with nothing hosted never needs one.
	if got := declaredPushBase(&KCLEntities{}); got != "" {
		t.Errorf("an env with no control plane composed %q", got)
	}
	// A control plane with no organization: still no base. KCL refuses this
	// combination for anything hosted, which is earlier and names the field.
	if got := declaredPushBase(&KCLEntities{ControlPlane: &ControlPlaneEntity{Endpoint: "https://cp"}}); got != "" {
		t.Errorf("an env with no organization composed %q", got)
	}
	// Declared: the host defaults to Reliant's registry.
	e := &KCLEntities{ControlPlane: &ControlPlaneEntity{Endpoint: "https://cp", Organization: org}}
	if got, want := declaredPushBase(e), hostedimage.DefaultRegistryHost+"/"+org+"/shop"; got != want {
		t.Errorf("declaredPushBase = %q, want %q", got, want)
	}
	// A declared host wins over the default.
	e.ControlPlane.RegistryHost = "registry.example.com"
	if got, want := declaredPushBase(e), "registry.example.com/"+org+"/shop"; got != want {
		t.Errorf("declaredPushBase with a declared host = %q, want %q", got, want)
	}
	// The scaffolded placeholder is not an organization, so it composes
	// nothing rather than an address that 403s.
	e.ControlPlane.Organization = hostedimage.OrgPlaceholder
	if got := declaredPushBase(e); got != "" {
		t.Errorf("the scaffolded placeholder composed %q", got)
	}
	if got := declaredOrganization(e); got != "" {
		t.Errorf("declaredOrganization reported the placeholder as an org: %q", got)
	}
}

// A REFUSED push names the declared org and both of the things that can cause
// it. The registry answers 401 without naming the org it expected, so forge —
// the only party that knows what was declared — is what connects the refusal
// to the line the author can edit.
func TestDeniedPushHint(t *testing.T) {
	const org = "4f3c2b1a-0000-4000-8000-000000000001"
	const ref = "registry.reliantapi.com/" + org + "/shop/api"

	// Not a refusal: no hint, so an unrelated failure reads exactly as it
	// did before.
	if got := deniedPushHint(errors.New("connection reset by peer"), "", ref, org); got != "" {
		t.Errorf("a network error produced a realm hint: %q", got)
	}

	// A docker push carries the distribution error code on its output,
	// since a subprocess's exit status is only "failed".
	hint := deniedPushHint(errors.New("exit status 1"),
		"denied: requested access to the resource is denied", ref, org)
	for _, want := range []string{org, "organization", "registry login"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
	// It must NOT assert which cause holds: forge does not know the token's
	// org, deliberately, so a message that guessed would be wrong exactly
	// when the credential really had expired.
	if !strings.Contains(hint, "either") {
		t.Errorf("the hint asserts a single cause:\n%s", hint)
	}

	// With no org declared the cause is different and so is the fix.
	noOrg := deniedPushHint(errors.New("exit status 1"), "unauthorized", ref, "")
	if !strings.Contains(noOrg, "declares no organization") {
		t.Errorf("hint for an undeclared org:\n%s", noOrg)
	}
}
