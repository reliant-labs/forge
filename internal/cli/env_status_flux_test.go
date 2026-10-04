package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/flux"
	"github.com/reliant-labs/forge/pkg/release"
)

const statusDigest = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func statusObservation(statuses ...flux.Status) flux.Observation {
	return flux.Observation{Statuses: statuses, At: time.Unix(1700000000, 0).UTC()}
}

// TestFluxConvergenceState_FourStatesFourActions pins the state vocabulary.
//
// Each state is a DIFFERENT THING TO DO NEXT, which is the only justification
// for having four rather than two. The pair that must not be collapsed is
// `pending` and `progressing`: a pointer that was never written needs a
// DEPLOY, and one still reconciling needs a WAIT. Reporting the first as the
// second leaves an operator watching a cluster that will never converge
// because nothing ever asked it to.
func TestFluxConvergenceState_FourStatesFourActions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		obs  flux.Observation
		want string
	}{
		{
			name: "converged",
			obs: statusObservation(flux.Status{
				Name: "dev-k8s", Cluster: "k3d-a", Found: true, Ready: true,
				Revision: statusDigest, Reason: "ReconciliationSucceeded",
			}),
			want: "converged",
		},
		{
			name: "failed — read Detail, it is Flux's own reason",
			obs: statusObservation(flux.Status{
				Name: "dev-k8s", Cluster: "k3d-a", Found: true, Revision: statusDigest,
				Reason: "HealthCheckFailed", Message: "deployment/api not ready",
			}),
			want: "failed",
		},
		{
			name: "progressing — wait",
			obs: statusObservation(flux.Status{
				Name: "dev-k8s", Cluster: "k3d-a", Found: true, Reason: "Progressing",
			}),
			want: "progressing",
		},
		{
			// Healthy, but on the PREVIOUS release. Still
			// progressing toward this one — not converged, and not a
			// failure.
			name: "ready on the previous revision is progressing, not converged",
			obs: statusObservation(flux.Status{
				Name: "dev-k8s", Cluster: "k3d-a", Found: true, Ready: true,
				Revision: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Reason:   "ReconciliationSucceeded",
			}),
			want: "progressing",
		},
		{
			// No pointer in the cluster at all: nobody has deployed,
			// or somebody deleted it. The fix is a deploy.
			name: "pending — nothing was ever written",
			obs:  statusObservation(flux.Status{Name: "dev-k8s", Cluster: "k3d-a"}),
			want: "pending",
		},
		{
			// One cluster converged, one absent. Not converged, and
			// `pending` would understate it — something IS there.
			name: "multi-cluster, one absent",
			obs: statusObservation(
				flux.Status{Name: "a", Cluster: "k3d-a", Found: true, Ready: true, Revision: statusDigest},
				flux.Status{Name: "b", Cluster: "k3d-b"},
			),
			want: "progressing",
		},
	} {
		if got := fluxConvergenceState(tc.obs, statusDigest); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFluxConvergenceOf_LabelsTheOBSERVER.
//
// A convergence record is a SECONDARY OBSERVATION, and a reader deciding how
// much to trust it needs to know whose claim it is. Forge did not apply this —
// Flux did — so the block must not read as forge's own verdict. It also must
// not read as the control plane's, which is what the renderer said
// unconditionally before this path existed.
func TestFluxConvergenceOf_LabelsTheOBSERVER(t *testing.T) {
	t.Parallel()
	got := fluxConvergenceOf(
		statusObservation(flux.Status{
			Name: "dev-k8s", Cluster: "k3d-a", Found: true, Ready: true,
			Revision: statusDigest, Reason: "ReconciliationSucceeded",
		}),
		statusDigest,
		release.BundleDoc{Release: "v1.4.0"},
	)
	if got == nil {
		t.Fatal("fluxConvergenceOf returned nil")
	}
	if !strings.Contains(got.ObservedBy, "in-cluster") {
		t.Errorf("ObservedBy = %q; it must say the in-cluster reconciler saw this, not forge and not a control plane",
			got.ObservedBy)
	}
	if convergenceObserver(*got) != got.ObservedBy {
		t.Errorf("convergenceObserver returned %q, want the record's own ObservedBy", convergenceObserver(*got))
	}
	// The DIGEST is the field that says which config is running; the
	// bundle IS its content.
	if got.BundleDigest != statusDigest {
		t.Errorf("BundleDigest = %q, want %q", got.BundleDigest, statusDigest)
	}
	if got.BundleID != "v1.4.0" {
		t.Errorf("BundleID = %q, want the bundle's release label", got.BundleID)
	}
	if got.ObservedAt == "" {
		t.Error("ObservedAt is empty; an observation with no time cannot be judged stale")
	}
}

// TestConvergenceObserver_DefaultsToTheControlPlane pins that every record
// written before this path existed still renders as it did. A hosted env's
// convergence carries no ObservedBy, and it must not silently become "the
// in-cluster reconciler".
func TestConvergenceObserver_DefaultsToTheControlPlane(t *testing.T) {
	t.Parallel()
	if got := convergenceObserver(envStatusConvergence{State: "succeeded"}); got != "the control plane" {
		t.Errorf("observer = %q, want the control plane for a record that names none", got)
	}
}

// TestFluxConvergenceOf_DetailCarriesTheSummary pins that the human-facing
// detail says what is being waited on rather than restating the state. The
// case that matters is Flux healthy on the wrong revision, where "not ready"
// would be false and "ready" would be the trap.
func TestFluxConvergenceOf_DetailCarriesTheSummary(t *testing.T) {
	t.Parallel()
	got := fluxConvergenceOf(
		statusObservation(flux.Status{
			Name: "dev-k8s", Cluster: "k3d-a", Found: true, Ready: true,
			Revision: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			Reason:   "ReconciliationSucceeded",
		}),
		statusDigest, release.BundleDoc{},
	)
	if !strings.Contains(got.Detail, "not yet") {
		t.Errorf("Detail = %q; it must name the revision gap rather than restate the state", got.Detail)
	}
}

// TestFluxTargetClusters_TheINTERSECTION pins that the pointer is written only
// where the env DECLARES a cluster AND the bundle CARRIES a path for it.
//
// Either side alone is wrong in a different direction. The bundle's side alone
// would keep deploying to a cluster the env removed — writing a departed
// cluster's objects somewhere. The declaration's side alone would build
// Kustomizations over paths that do not exist, which reconcile Ready having
// applied nothing.
func TestFluxTargetClusters_TheINTERSECTION(t *testing.T) {
	t.Parallel()
	entities := &KCLEntities{
		Clusters:      []ClusterEntity{fluxTestCluster("a"), fluxTestCluster("declared-empty")},
		ClusterTarget: &ClusterTargetEntity{Cluster: "k3d-a"},
	}
	trees := []release.BundleClusterTree{
		{Cluster: "k3d-a", Path: release.BundleClusterPath("k3d-a"), Documents: 5},
		// Declared, routed nothing: no path to apply.
		{Cluster: "k3d-declared-empty", Path: release.BundleClusterPath("k3d-declared-empty"), Documents: 0},
		// Carried, no longer declared: stale.
		{Cluster: "k3d-departed", Path: release.BundleClusterPath("k3d-departed"), Documents: 3},
		// The unclustered tree: nothing applies it.
		{Cluster: "", Path: release.BundleClusterPath(""), Documents: 2},
	}
	got := fluxTargetClusters(entities, trees)
	if len(got) != 1 || got[0] != "k3d-a" {
		t.Errorf("targets = %v, want only k3d-a", got)
	}
}

// TestDeclaredFluxClusters_ReadsEveryPlaceADeployWouldWrite pins that the
// pointer goes into exactly the contexts a direct apply would have applied to.
// A context the deploy routes objects to but this misses would be a cluster
// running the previous release with nothing reporting it.
func TestDeclaredFluxClusters_ReadsEveryPlaceADeployWouldWrite(t *testing.T) {
	t.Parallel()
	entities := &KCLEntities{
		ClusterTarget: &ClusterTargetEntity{Cluster: "k3d-primary"},
		Workloads: []WorkloadEntity{{
			Name:    "api",
			Runtime: RuntimeEntity{Type: RuntimeCluster, Cluster: &ClusterRuntime{Cluster: "k3d-workload"}},
		}},
		Clusters:         []ClusterEntity{fluxTestCluster("declared")},
		ManifestClusters: []ManifestClusterEntity{{Cluster: "k3d-manifests"}},
	}
	got := declaredFluxClusters(entities)
	for _, want := range []string{"k3d-primary", "k3d-workload", "k3d-declared", "k3d-manifests"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("declaredFluxClusters = %v, missing %s — a context the deploy writes to must get a pointer", got, want)
		}
	}
	// De-duplicated: one cluster named twice is one pointer.
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for c, n := range seen {
		if n > 1 {
			t.Errorf("%s appears %d times; a cluster named twice must yield one pointer", c, n)
		}
	}
}

// TestDescribeFluxFailures_RelaysFluxsOwnWords pins that the exit-1 message
// carries Flux's reason verbatim plus the command that shows more.
//
// Remapping the reason would eventually disagree with what
// `flux get kustomization` shows the operator for the same object, leaving
// them to debug the difference rather than the deploy.
func TestDescribeFluxFailures_RelaysFluxsOwnWords(t *testing.T) {
	t.Parallel()
	msg := describeFluxFailures("dev-k8s", []flux.Status{{
		Name: "dev-k8s", Cluster: "k3d-a", Found: true,
		Reason: "HealthCheckFailed", Message: "deployment/api not ready after 5m0s",
	}})
	for _, want := range []string{"HealthCheckFailed", "deployment/api not ready", "kubectl", flux.Namespace, "k3d-a"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message does not contain %q:\n%s", want, msg)
		}
	}
}
