package flux

import (
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// TestStatusConverged_NeedsTheREVISIONAndReady is the single most important
// assertion on this path, because each half alone passes for a release that
// was never applied.
func TestStatusConverged_NeedsTheREVISIONAndReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    Status
		want bool
	}{
		{
			// THE TRAP. Flux converged the PREVIOUS release and has not
			// yet fetched this one, so it is Ready=True the whole time.
			// A wait on readiness alone passes instantly for a release
			// that has not been applied at all.
			name: "ready on the previous revision",
			s:    Status{Found: true, Ready: true, Revision: otherDigest, Reason: "ReconciliationSucceeded"},
		},
		{
			// The other half: the apply was admitted and the revision
			// advanced, then the health check failed.
			name: "right revision, not healthy",
			s:    Status{Found: true, Ready: false, Revision: testDigest, Reason: "HealthCheckFailed"},
		},
		{
			name: "not in the cluster at all",
			s:    Status{Found: false, Ready: true, Revision: testDigest},
		},
		{
			name: "no apply yet",
			s:    Status{Found: true, Ready: false, Revision: ""},
		},
		{
			name: "converged",
			s:    Status{Found: true, Ready: true, Revision: testDigest, Reason: "ReconciliationSucceeded"},
			want: true,
		},
	} {
		if got := tc.s.Converged(testDigest); got != tc.want {
			t.Errorf("%s: Converged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStatusFailing_ProgressingIsNotFailing pins the distinction that decides
// whether this gate is trustworthy. Every healthy deploy passes through
// Ready=False/Progressing for as long as its rollout takes, so a wait that
// exited on the first Ready=False would fail every deploy slower than one
// poll — which is how a gate gets switched off.
func TestStatusFailing_ProgressingIsNotFailing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    Status
		want bool
	}{
		{"progressing", Status{Found: true, Reason: "Progressing"}, false},
		{"waiting on a dependency", Status{Found: true, Reason: "DependencyNotReady"}, false},
		{"no reason yet", Status{Found: true, Reason: ""}, false},
		{"ready", Status{Found: true, Ready: true, Reason: "ReconciliationSucceeded"}, false},
		{"absent is not failing", Status{Found: false, Reason: "BuildFailed"}, false},

		// Terminal for this revision: Flux has already applied and
		// health-checked, and nothing about the revision will change.
		{"build failed", Status{Found: true, Reason: "BuildFailed"}, true},
		{"health check failed", Status{Found: true, Reason: "HealthCheckFailed"}, true},
		{"artifact missing", Status{Found: true, Reason: "ArtifactFailed"}, true},
		// An UNRECOGNISED reason is a failure, which is the safe
		// direction: a new Flux reason forge does not know about
		// surfaces loudly, naming Flux's own message, rather than
		// hanging the wait.
		{"a reason forge has never seen", Status{Found: true, Reason: "SomeFutureFluxReason"}, true},
	} {
		if got := tc.s.Failing(); got != tc.want {
			t.Errorf("%s: Failing = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestObservationConverged_EveryClusterOrNone pins that a multi-cluster env is
// converged only when ALL of its clusters are. Reporting success on the first
// would report a release as shipped while one cluster still ran the previous
// one.
func TestObservationConverged_EveryClusterOrNone(t *testing.T) {
	t.Parallel()
	done := Status{Name: "a", Cluster: "k3d-a", Found: true, Ready: true, Revision: testDigest}
	behind := Status{Name: "b", Cluster: "k3d-b", Found: true, Ready: true, Revision: otherDigest}

	if (Observation{Statuses: []Status{done, behind}}).Converged(testDigest) {
		t.Error("converged with one cluster still on the previous revision")
	}
	if !(Observation{Statuses: []Status{done}}).Converged(testDigest) {
		t.Error("not converged with the only cluster converged")
	}
	// NO Kustomizations is not converged. An empty observation would
	// otherwise satisfy a universal quantifier and report success for a
	// deploy that polled nothing.
	if (Observation{}).Converged(testDigest) {
		t.Error("an empty observation reported converged; it must not satisfy the gate vacuously")
	}
}

// TestObservationSummary_NamesTheRevisionGap pins the progress line's most
// useful case: Flux is healthy, on the WRONG release. "not ready" would be
// false and "ready" would be the trap, so the line has to say which revision
// is applied.
func TestObservationSummary_NamesTheRevisionGap(t *testing.T) {
	t.Parallel()
	obs := Observation{Statuses: []Status{
		{Name: "dev-k8s", Cluster: "k3d-a", Found: true, Ready: true, Revision: otherDigest,
			Reason: "ReconciliationSucceeded"},
	}}
	got := obs.Summary(testDigest)
	if !strings.Contains(got, "not yet") || !strings.Contains(got, shortDigest(testDigest)) {
		t.Errorf("Summary = %q; it must name the revision actually applied and the one wanted", got)
	}
	if !strings.Contains(got, "0/1") {
		t.Errorf("Summary = %q; it must count how many have converged", got)
	}
}

// TestObservationSummary_AbsentPointerSaysSo pins that a deleted pointer reads
// as absent rather than as not-yet-ready: the fix is a deploy, not a wait.
func TestObservationSummary_AbsentPointerSaysSo(t *testing.T) {
	t.Parallel()
	obs := Observation{Statuses: []Status{{Name: "dev-k8s", Cluster: "k3d-a"}}}
	if got := obs.Summary(testDigest); !strings.Contains(got, "not found") {
		t.Errorf("Summary = %q; an absent pointer must be distinguishable from an unhealthy one", got)
	}
}

// TestParseStatuses_OrderAndAbsence pins two decode properties.
//
// ORDER: statuses come back in the order ASKED FOR, not the apiserver's list
// order, so a progress display does not reorder itself between polls (which
// reads as objects appearing and disappearing).
//
// ABSENCE: a name the list did not carry is Found=false, not an error — "the
// pointer is gone" and "the cluster is unreachable" are different diagnoses
// and only one is worth retrying.
func TestParseStatuses_OrderAndAbsence(t *testing.T) {
	t.Parallel()
	// The apiserver returns b before a; we asked for a then b.
	raw := []byte(`{
	  "kind": "KustomizationList",
	  "items": [
	    {"metadata":{"name":"b"},"status":{"lastAppliedRevision":"` + otherDigest + `",
	      "conditions":[{"type":"Ready","status":"False","reason":"HealthCheckFailed","message":"deployment/api not ready"}]}},
	    {"metadata":{"name":"a"},"status":{"lastAppliedRevision":"` + testDigest + `",
	      "conditions":[{"type":"Ready","status":"True","reason":"ReconciliationSucceeded"}]}}
	  ]}`)
	got, err := parseStatuses("k3d-a", []string{"a", "b", "missing"}, raw)
	if err != nil {
		t.Fatalf("parseStatuses: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d statuses, want one per requested name", len(got))
	}
	if got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "missing" {
		t.Errorf("order = %s,%s,%s; must follow the order asked for, not the apiserver's",
			got[0].Name, got[1].Name, got[2].Name)
	}
	if !got[0].Converged(testDigest) {
		t.Errorf("a: Converged = false; it reports the digest and Ready=True (%+v)", got[0])
	}
	if !got[1].Failing() || got[1].Message == "" {
		t.Errorf("b: want failing with Flux's own message, got %+v", got[1])
	}
	if got[2].Found {
		t.Error("missing: Found = true; a name the cluster does not hold must read as absent")
	}
}

// TestParseStatuses_SingleObjectNotAList pins the other shape `kubectl get -o
// json` returns: one name yields the object itself, not a List.
func TestParseStatuses_SingleObjectNotAList(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"kind":"Kustomization","metadata":{"name":"only"},
	  "status":{"lastAppliedRevision":"` + testDigest + `",
	  "conditions":[{"type":"Ready","status":"True","reason":"ReconciliationSucceeded"}]}}`)
	got, err := parseStatuses("k3d-a", []string{"only"}, raw)
	if err != nil {
		t.Fatalf("parseStatuses: %v", err)
	}
	if len(got) != 1 || !got[0].Converged(testDigest) {
		t.Fatalf("got %+v; want one converged status", got)
	}
}

// TestParseStatuses_IgnoresNonReadyConditions pins that only the Ready
// condition decides readiness. Flux sets others (Reconciling, Stalled), and
// reading the last condition instead of the named one would make the verdict
// depend on the apiserver's condition ordering.
func TestParseStatuses_IgnoresNonReadyConditions(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"kind":"Kustomization","metadata":{"name":"only"},
	  "status":{"lastAppliedRevision":"` + testDigest + `","conditions":[
	    {"type":"Ready","status":"True","reason":"ReconciliationSucceeded"},
	    {"type":"Reconciling","status":"True","reason":"ProgressingWithRetry"}]}}`)
	got, err := parseStatuses("k3d-a", []string{"only"}, raw)
	if err != nil {
		t.Fatalf("parseStatuses: %v", err)
	}
	if !got[0].Converged(testDigest) {
		t.Errorf("Converged = false; a trailing Reconciling condition must not override Ready (%+v)", got[0])
	}
}

// TestEncode_IsAMultiDocumentStreamKubectlAccepts pins that the pointer
// serializes to what `kubectl apply -f -` reads: several documents, separated,
// source first. `--dry-run` prints this exact string, so a reader checking what
// forge is about to write is reading the bytes rather than a description.
func TestEncode_IsAMultiDocumentStreamKubectlAccepts(t *testing.T) {
	t.Parallel()
	p, err := BuildPointer(PointerInput{
		Env: "dev-k8s", Repository: "k3d-reg:5000/p/bundle.v1/dev-k8s", Digest: testDigest,
		Cluster: "k3d-a", RequestedAt: time.Unix(0, 0).UTC(),
		ClusterPaths: []release.BundleClusterTree{
			{Cluster: "k3d-a", Path: release.BundleClusterPath("k3d-a"), Documents: 2},
		},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	stream, err := Encode(p.All())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Count(stream, "\n---\n") != 1 {
		t.Errorf("stream has %d separators, want 1 between the two documents:\n%s",
			strings.Count(stream, "\n---\n"), stream)
	}
	if i, j := strings.Index(stream, kindOCIRepository), strings.Index(stream, kindKustomization); i < 0 || j < 0 || i > j {
		t.Errorf("source must be encoded before the kustomization; got offsets %d and %d", i, j)
	}
	// The digest is what makes the pointer a pointer; if it is not in the
	// bytes, nothing downstream can be comparing against it.
	if !strings.Contains(stream, testDigest) {
		t.Errorf("stream does not carry the pinned digest:\n%s", stream)
	}
}
