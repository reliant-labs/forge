package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/reliant-labs/forge/internal/exitcode"
	"github.com/reliant-labs/forge/pkg/fluxdelivery"
	"github.com/reliant-labs/forge/pkg/release"
)

const testFluxDigest = "sha256:" + "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

// fluxRelease is a release of one cluster split into the three phases, as
// the bundle records its layout.
func fluxRelease(identities bool) *fluxDeliveryRelease {
	fd := deliveryEntity("namespace")
	fd.ApplyIdentities = identities
	fd.FluxVerify = FluxDeliveryControl{Requested: "enforce", Effective: "audit"}
	tree := func(phase release.Phase, deps []string, health ...release.BundleHealthCheck) release.BundleTree {
		return release.BundleTree{
			Cluster: testCtx, Unit: release.DefaultDeliveryUnit, Phase: phase,
			Path:      release.BundleTreePath(testCtx, release.DefaultDeliveryUnit, phase),
			Documents: 1, DependsOn: deps, TimeoutSeconds: 90, RetryIntervalSeconds: 15, Health: health,
		}
	}
	pre := release.BundleTreePath(testCtx, release.DefaultDeliveryUnit, release.PhasePre)
	rollout := release.BundleTreePath(testCtx, release.DefaultDeliveryUnit, release.PhaseRollout)
	return &fluxDeliveryRelease{
		fd: fd, project: "acme", version: "v2", digest: testFluxDigest,
		pushTo: "localhost:5050/acme/bundle.v1/prod", sourceURL: "oci://host.k3d.internal:5050/acme/bundle.v1/prod",
		doc: release.BundleDoc{Trees: []release.BundleTree{
			tree(release.PhasePre, nil, release.BundleHealthCheck{Kind: "Job", Name: "migrate-abc", Namespace: "acme-prod"}),
			tree(release.PhaseRollout, []string{pre}, release.BundleHealthCheck{Kind: "Deployment", Name: "api", Namespace: "acme-prod"}),
			tree(release.PhasePost, []string{rollout}),
		}},
	}
}

// TestFluxDeliveryInputFromTheBundle: the pointer's input is one in-cluster
// applier per tree, named and chained as the bundle records them, each under
// its phase's identity, and it passes every check the writer repeats.
func TestFluxDeliveryInputFromTheBundle(t *testing.T) {
	t.Parallel()
	for _, identities := range []bool{true, false} {
		rel := fluxRelease(identities)
		in, err := rel.input("3", "promo-3")
		if err != nil {
			t.Fatal(err)
		}
		if err := checkFluxDeliveryInput(in, rel.fd); err != nil {
			t.Fatalf("identities=%t: the input fails the writer's checks: %v", identities, err)
		}
		if len(in.Appliers) != 3 {
			t.Fatalf("appliers = %d, want one per tree", len(in.Appliers))
		}
		if in.Source.Digest != testFluxDigest || in.Source.URL != rel.sourceURL || !in.Source.Insecure {
			t.Errorf("source = %+v", in.Source)
		}
		if in.Source.Verify.Mode != fluxdelivery.VerifyAudit {
			t.Errorf("verify = %q, want the capped audit", in.Source.Verify.Mode)
		}
		rollout := in.Appliers[1]
		if len(rollout.DependsOn) != 1 || rollout.DependsOn[0] != in.Appliers[0].Name {
			t.Errorf("rollout dependsOn = %v, want the pre tree %s", rollout.DependsOn, in.Appliers[0].Name)
		}
		if rollout.Timeout != "1m30s" || rollout.RetryInterval != "15s" {
			t.Errorf("rollout timeout/retry = %s/%s", rollout.Timeout, rollout.RetryInterval)
		}
		want := map[release.Phase]string{release.PhasePre: fluxdelivery.PreServiceAccount, release.PhaseRollout: fluxdelivery.ApplyServiceAccount, release.PhasePost: fluxdelivery.ApplyServiceAccount}
		for _, a := range in.Appliers {
			sa := ""
			if identities {
				sa = want[a.Phase]
			}
			if !a.InCluster || a.ServiceAccountName != sa {
				t.Errorf("identities=%t: %s applies in-cluster=%t as %q, want %q", identities, a.Name, a.InCluster, a.ServiceAccountName, sa)
			}
		}
	}
}

func TestFluxDeliveryInputRefusesABundleWithNothingForTheCluster(t *testing.T) {
	t.Parallel()
	rel := fluxRelease(true)
	rel.fd.Cluster = "some-other-context"
	if _, err := rel.input("1", "p"); err == nil || !strings.Contains(err.Error(), "carries no tree for cluster") {
		t.Fatalf("err = %v, want the no-tree refusal", err)
	}
}

func TestFluxDeliveryRegistry(t *testing.T) {
	t.Parallel()
	declared := &KCLEntities{FluxDelivery: &FluxDeliveryEntity{Registry: "ghcr.io/acme/"}}
	if got := fluxRegistryBase(declared); got != "ghcr.io/acme" {
		t.Errorf("declared registry = %q", got)
	}
	if got := fluxRegistryBase(&KCLEntities{FluxDelivery: &FluxDeliveryEntity{}}); got != "" {
		t.Errorf("no registry and no workload: %q, want none", got)
	}
	for ref, want := range map[string]string{
		"localhost:5050/acme/bundle.v1/prod": "host.k3d.internal:5050/acme/bundle.v1/prod",
		"127.0.0.1:5050/acme":                "host.k3d.internal:5050/acme",
		"ghcr.io/acme/bundle.v1/prod":        "ghcr.io/acme/bundle.v1/prod",
	} {
		if got := clusterReachableRegistry(ref); got != want {
			t.Errorf("clusterReachableRegistry(%q) = %q, want %q", ref, got, want)
		}
	}
	for ref, want := range map[string]bool{
		"oci://host.k3d.internal:5050/acme": true, "oci://k3d-reg:5000/a": true, "oci://registry.localhost:5000/a": true,
		"oci://ghcr.io/acme": false, "oci://localhost.evil.com/a": false,
	} {
		if got := insecureRegistry(ref); got != want {
			t.Errorf("insecureRegistry(%q) = %t, want %t", ref, got, want)
		}
	}
}

// fakePointer is the pointer's cluster I/O, in memory.
type fakePointer struct {
	live    *unstructured.Unstructured
	applied []map[string]any
}

func (f *fakePointer) install(t *testing.T) {
	t.Helper()
	prev := fluxDeliveryPointerIO
	t.Cleanup(func() { fluxDeliveryPointerIO = prev })
	fluxDeliveryPointerIO.read = func(context.Context, FluxDeliveryEntity) (*unstructured.Unstructured, error) {
		return f.live, nil
	}
	fluxDeliveryPointerIO.apply = func(_ context.Context, _ FluxDeliveryEntity, p map[string]any) error {
		f.applied = append(f.applied, p)
		u := (&unstructured.Unstructured{Object: p}).DeepCopy()
		u.SetResourceVersion("rv-" + u.GetAnnotations()[fluxdelivery.PointerFenceAnnotation])
		f.live = u
		return nil
	}
}

// TestWriteFluxDeliveryPointer is the writer's refusal matrix against a live
// pointer. Not parallel: it swaps the pointer seam.
func TestWriteFluxDeliveryPointer(t *testing.T) {
	rel := fluxRelease(true)
	f := &fakePointer{}
	f.install(t)

	first, _ := rel.input("4", "promo-4")
	if err := writeFluxDeliveryPointer(context.Background(), rel.fd, first); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if md := f.applied[0]["metadata"].(map[string]any); md["resourceVersion"] != nil {
		t.Errorf("a first write carries a resourceVersion precondition: %v", md["resourceVersion"])
	}

	next, _ := rel.input("5", "promo-5")
	if err := writeFluxDeliveryPointer(context.Background(), rel.fd, next); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if md := f.applied[1]["metadata"].(map[string]any); md["resourceVersion"] != "rv-4" {
		t.Errorf("the second write is not conditional on the pointer it read: resourceVersion %v", md["resourceVersion"])
	}

	stale, _ := rel.input("3", "promo-3")
	if err := writeFluxDeliveryPointer(context.Background(), rel.fd, stale); !errors.Is(err, fluxdelivery.ErrStaleFence) {
		t.Fatalf("an older fence: err = %v, want ErrStaleFence", err)
	}
	dropped, _ := rel.input("6", "promo-6")
	dropped.Appliers = dropped.Appliers[:2]
	if err := writeFluxDeliveryPointer(context.Background(), rel.fd, dropped); !errors.Is(err, fluxdelivery.ErrUnretiredApplierDropped) {
		t.Fatalf("a dropped tree: err = %v, want ErrUnretiredApplierDropped", err)
	}
	if len(f.applied) != 2 {
		t.Errorf("a refused write reached the cluster: %d writes, want 2", len(f.applied))
	}
}

// observed builds an observation of in's appliers, each in the given state.
func observed(in fluxdelivery.BindingInput, each func(a fluxdelivery.Applier) fluxObjectStatus) fluxDeliveryObservation {
	obs := fluxDeliveryObservation{
		Renderer: fluxObjectStatus{Name: "delivery.prod", Found: true, Ready: "True", Current: true},
		Source:   fluxObjectStatus{Name: in.Source.Name, Found: true, Ready: "True", Current: true},
	}
	for _, a := range in.Appliers {
		obs.Appliers = append(obs.Appliers, each(a))
	}
	return obs
}

const forbiddenMessage = `CustomResourceDefinition/gadgets.acme.dev dry-run failed (Forbidden): customresourcedefinitions.apiextensions.k8s.io "gadgets.acme.dev" is forbidden: ` +
	`User "system:serviceaccount:forge-delivery-acme-prod:forge-delivery-pre" cannot patch resource "customresourcedefinitions" in API group "apiextensions.k8s.io" at the cluster scope` + "\n" +
	`Widget/acme-prod/w dry-run failed (Forbidden): widgets.acme.dev "w" is forbidden: ` +
	`User "system:serviceaccount:forge-delivery-acme-prod:forge-delivery-pre" cannot patch resource "widgets" in API group "acme.dev" in the namespace "acme-prod"`

func TestFluxDeliveryForbidden(t *testing.T) {
	t.Parallel()
	got := fluxDeliveryForbidden(forbiddenMessage)
	rows := fluxDeliveryGrantRows(got)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Resource != "acme.dev/widgets" || rows[0].Scope != "(namespace acme-prod)" ||
		rows[1].Resource != "apiextensions.k8s.io/customresourcedefinitions" || rows[1].Scope != "(cluster)" {
		t.Errorf("rows = %+v", rows)
	}
	other := `User "system:serviceaccount:kube-system:somebody" cannot patch resource "secrets" in API group "" in the namespace "x"`
	if got := fluxDeliveryForbidden(other); len(got) != 0 {
		t.Errorf("a Forbidden for an identity forge does not grant was mapped: %v", got)
	}
}

// TestFluxDeliveryObservationVerdicts: converged, a Forbidden mapped to the
// grant refusal (exit 1), any other failure in Flux's words (exit 1), and a
// failure from the PREVIOUS revision still progressing.
func TestFluxDeliveryObservationVerdicts(t *testing.T) {
	t.Parallel()
	in, _ := fluxRelease(true).input("2", "promo-2")
	d := in.Source.Digest

	ready := observed(in, func(a fluxdelivery.Applier) fluxObjectStatus {
		return fluxObjectStatus{Name: a.Name, Found: true, Ready: "True", Current: true, LastApplied: "latest@" + d, LastAttempted: "latest@" + d}
	})
	if !ready.converged(d) || ready.failure("prod", "v2", d) != nil {
		t.Fatalf("a fully applied release is not converged: %+v", ready)
	}

	failing := func(reason, message, attempted string) fluxDeliveryObservation {
		return observed(in, func(a fluxdelivery.Applier) fluxObjectStatus {
			if a.Phase != release.PhasePre {
				return fluxObjectStatus{Name: a.Name, Found: true, Ready: "False", Reason: "DependencyNotReady", Current: true}
			}
			return fluxObjectStatus{Name: a.Name, Found: true, Ready: "False", Reason: reason, Message: message, Current: true, LastAttempted: attempted}
		})
	}

	err := failing("ReconciliationFailed", forbiddenMessage, d).failure("prod", "v2", d)
	var refusal *fluxDeliveryGrantRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a Forbidden for an apply identity: err = %v, want the grant refusal", err)
	}
	if exitcode.Of(err) != exitWrong || refusal.Reporter != in.Appliers[0].Name ||
		!strings.Contains(err.Error(), "forge env deploy prod --target flux-delivery") {
		t.Errorf("mapped refusal = %v (exit %d)", err, exitcode.Of(err))
	}

	err = failing("HealthCheckFailed", "timeout waiting for: [Job/acme-prod/migrate-abc status: 'Failed']", d).failure("prod", "v2", d)
	if exitcode.Of(err) != exitWrong || !strings.Contains(err.Error(), "HealthCheckFailed") || errors.As(err, &refusal) {
		t.Errorf("a health failure: %v (exit %d), want Flux's own words, exit 1", err, exitcode.Of(err))
	}

	old := "sha256:" + strings.Repeat("0", 64)
	if err := failing("ReconciliationFailed", forbiddenMessage, "latest@"+old).failure("prod", "v2", d); err != nil {
		t.Errorf("a failure of the PREVIOUS revision decided this one: %v", err)
	}

	stalled := ready
	stalled.Renderer.Stalled, stalled.Renderer.Reason = true, "BuildFailed"
	if err := stalled.failure("prod", "v2", d); err == nil || !strings.Contains(err.Error(), "renderer has not applied the promoted inputs") {
		t.Errorf("a stalled renderer: %v", err)
	}
}

// TestWaitFluxDelivery: the wait's three outcomes, through the observe seam.
// Not parallel: it swaps the seam.
func TestWaitFluxDelivery(t *testing.T) {
	rel := fluxRelease(true)
	in, _ := rel.input("2", "promo-2")
	d := in.Source.Digest
	prev := fluxDeliveryObserve
	t.Cleanup(func() { fluxDeliveryObserve = prev })
	quiet := func(string, ...any) {}

	calls := 0
	fluxDeliveryObserve = func(context.Context, FluxDeliveryEntity, fluxdelivery.BindingInput) (fluxDeliveryObservation, error) {
		calls++
		return observed(in, func(a fluxdelivery.Applier) fluxObjectStatus {
			return fluxObjectStatus{Name: a.Name, Found: true, Ready: "True", Current: true, LastApplied: d, LastAttempted: d}
		}), nil
	}
	if err := waitFluxDelivery(context.Background(), "prod", "v2", rel.fd, in, time.Minute, quiet); err != nil || calls != 1 {
		t.Fatalf("converged: err = %v after %d reads", err, calls)
	}

	fluxDeliveryObserve = func(context.Context, FluxDeliveryEntity, fluxdelivery.BindingInput) (fluxDeliveryObservation, error) {
		return observed(in, func(a fluxdelivery.Applier) fluxObjectStatus {
			return fluxObjectStatus{Name: a.Name, Found: true, Ready: "False", Reason: "ReconciliationFailed", Message: forbiddenMessage, Current: true, LastAttempted: d}
		}), nil
	}
	var refusal *fluxDeliveryGrantRefusal
	if err := waitFluxDelivery(context.Background(), "prod", "v2", rel.fd, in, time.Minute, quiet); !errors.As(err, &refusal) {
		t.Fatalf("forbidden: err = %v, want the grant refusal", err)
	}

	fluxDeliveryObserve = func(context.Context, FluxDeliveryEntity, fluxdelivery.BindingInput) (fluxDeliveryObservation, error) {
		return observed(in, func(a fluxdelivery.Applier) fluxObjectStatus {
			return fluxObjectStatus{Name: a.Name, Found: true, Ready: "Unknown", Reason: "Progressing", Current: true}
		}), nil
	}
	if err := waitFluxDelivery(context.Background(), "prod", "v2", rel.fd, in, time.Nanosecond, quiet); exitcode.Of(err) != exitTimedOut {
		t.Fatalf("still progressing at the deadline: err = %v (exit %d), want exit 8", err, exitcode.Of(err))
	}
}
