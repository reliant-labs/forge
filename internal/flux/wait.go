package flux

// Reading convergence back out of the cluster.
//
// WHAT "CONVERGED" MEANS HERE, EXACTLY, and why it is two conditions and not
// one. A Kustomization is converged when:
//
//	lastAppliedRevision == the digest forge pinned     AND     Ready=True
//
// Neither alone is the answer, and the two failure modes they exclude are
// different:
//
//   - Ready=True ALONE is the previous release reporting success. A
//     Kustomization that converged to v5 and has not yet fetched v6 is
//     Ready=True the whole time; a wait on readiness would pass instantly for
//     a release that had not been applied at all. This is the same trap
//     env_wait.go's header describes for the hosted rollout verdict, arriving
//     by a different road.
//   - THE REVISION ALONE is an apply that was admitted and then failed its
//     health check. `wait: true` makes Flux health-check what it applied, so
//     Ready goes False with a reason while the revision has already advanced.
//
// So the revision says WHICH release is being reported on, and Ready says
// whether it is healthy. Both, or it is not converged.
//
// THE REVISION IS COMPARED STRING-EQUAL to the OCI manifest digest. See the
// package doc for why that holds (`layerSelector` selects a layer; it does not
// re-key the revision) and why an e2e — not a unit test — is what pins it.
//
// forge DOES NOT READ THE ENV'S OWN OBJECTS on this path. It would be easy to
// also poll the Deployments and report pod-level detail, and it would be
// wrong: Flux already health-checks them under `wait: true`, so a second
// opinion could only ever disagree with the one that decides. When Flux says
// not-Ready it also says WHY, and relaying that reason is strictly better than
// re-deriving a worse one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/cluster"
)

// Status is one Kustomization's convergence, as the cluster reports it.
type Status struct {
	// Name and Cluster identify which Kustomization this is.
	Name    string
	Cluster string
	// Revision is `status.lastAppliedRevision`: the bundle digest of the
	// last SUCCESSFUL apply. Empty before the first one.
	Revision string
	// Ready is the Ready condition's status.
	Ready bool
	// Reason and Message are Flux's OWN words for the current condition,
	// relayed rather than remapped. A translation layer here would
	// eventually disagree with what `flux get kustomization` shows an
	// operator for the same object, and they would be debugging the
	// difference.
	Reason  string
	Message string
	// Found is false when the object is not in the cluster at all —
	// distinct from "found and not ready". A pointer forge wrote and that
	// has since been deleted is a different diagnosis from one that is
	// failing to apply, and folding them loses the one that says somebody
	// removed it.
	Found bool
}

// Converged reports whether this Kustomization has applied `digest` and is
// healthy. Both halves — see the file header.
func (s Status) Converged(digest string) bool {
	return s.Found && s.Ready && s.Revision == digest
}

// Failing reports a Kustomization that is present and reporting Ready=False
// for a reason that is not simply "still working".
//
// PROGRESSING IS NOT FAILING, and conflating them is what makes a gate flaky
// enough to be switched off. Flux reports `Progressing` (and
// `ReconciliationSucceeded` only at the end) while it fetches, builds and
// health-checks — a state every healthy deploy passes through for as long as
// its rollout takes. A wait that exited non-zero on the first Ready=False
// would fail every deploy that took longer than one poll.
func (s Status) Failing() bool {
	if !s.Found || s.Ready {
		return false
	}
	switch s.Reason {
	case reasonProgressing, reasonDependencyNotReady, "":
		return false
	}
	return true
}

// Flux's condition reasons that mean "not finished", as opposed to "wrong".
// Matched by name because they are Flux's own vocabulary and it is what an
// operator sees; an unrecognised reason is treated as a FAILURE, which is the
// safe direction — a new Flux reason forge does not know about will surface as
// a loud failure naming Flux's own message, not as a wait that hangs.
const (
	reasonProgressing        = "Progressing"
	reasonDependencyNotReady = "DependencyNotReady"
)

// Observation is the whole pointer's convergence at one instant.
type Observation struct {
	// Statuses is one per Kustomization polled, in the order given.
	Statuses []Status
	// At is when it was read.
	At time.Time
}

// Converged reports whether EVERY Kustomization has converged to `digest`.
//
// All of them, because a multi-cluster env's release is live when every
// cluster is running it. Reporting success on the first would report a release
// as shipped while one cluster still ran the previous one — which is exactly
// the gap the mixed-env case in deploy_promote_follow.go exists to close,
// arriving here per cluster instead of per runtime.
func (o Observation) Converged(digest string) bool {
	if len(o.Statuses) == 0 {
		return false
	}
	for _, s := range o.Statuses {
		if !s.Converged(digest) {
			return false
		}
	}
	return true
}

// Failures is every Kustomization reporting a real failure.
func (o Observation) Failures() []Status {
	var out []Status
	for _, s := range o.Statuses {
		if s.Failing() {
			out = append(out, s)
		}
	}
	return out
}

// Summary is the one-line progress report: how many have converged, and what
// the laggard is doing.
func (o Observation) Summary(digest string) string {
	done := 0
	for _, s := range o.Statuses {
		if s.Converged(digest) {
			done++
		}
	}
	if done == len(o.Statuses) && len(o.Statuses) > 0 {
		return fmt.Sprintf("%d/%d converged", done, len(o.Statuses))
	}
	for _, s := range o.Statuses {
		if s.Converged(digest) {
			continue
		}
		return fmt.Sprintf("%d/%d converged; %s: %s", done, len(o.Statuses), s.Name, s.describe(digest))
	}
	return fmt.Sprintf("%d/%d converged", done, len(o.Statuses))
}

// describe says what this Kustomization is waiting on, in the terms that
// distinguish the cases a reader acts on differently.
func (s Status) describe(digest string) string {
	switch {
	case !s.Found:
		return "not found in the cluster"
	case s.Revision == "":
		return "no successful apply yet" + reasonSuffix(s)
	case s.Revision != digest:
		// The one state worth naming precisely: Flux is healthy on the
		// PREVIOUS release and has not yet applied this one. Reporting
		// it as "not ready" would be false, and reporting it as ready
		// would be the trap this whole comparison exists to close.
		return fmt.Sprintf("applied %s, not yet %s", shortDigest(s.Revision), shortDigest(digest)) + reasonSuffix(s)
	case !s.Ready:
		return "applied, not yet healthy" + reasonSuffix(s)
	}
	return "converged"
}

func reasonSuffix(s Status) string {
	switch {
	case s.Message != "":
		return " (" + s.Reason + ": " + firstLine(s.Message) + ")"
	case s.Reason != "":
		return " (" + s.Reason + ")"
	}
	return ""
}

func shortDigest(d string) string {
	hex := strings.TrimPrefix(d, "sha256:")
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}

// Observe reads the current convergence of the pointer's Kustomizations.
//
// ONE kubectl PER CLUSTER, not per object: a pointer with four Kustomizations
// in one cluster is one `kubectl get` with four names, so the four statuses
// come from one apiserver read at one instant. Four reads could each see a
// different moment, which is how an observation ends up reporting a
// Kustomization as converged to a revision another one says was superseded.
func Observe(ctx context.Context, p Pointer, now time.Time) (Observation, error) {
	out := Observation{At: now}
	byCluster := map[string][]string{}
	var order []string
	for _, k := range p.Kustomizations {
		if _, seen := byCluster[k.Cluster]; !seen {
			order = append(order, k.Cluster)
		}
		byCluster[k.Cluster] = append(byCluster[k.Cluster], k.Name)
	}
	for _, kctx := range order {
		got, err := observeCluster(ctx, kctx, byCluster[kctx])
		if err != nil {
			return Observation{}, err
		}
		out.Statuses = append(out.Statuses, got...)
	}
	return out, nil
}

// observeCluster reads one cluster's Kustomizations.
//
// NotFound is not an error. A pointer object that is absent is a real,
// readable state — the write has not landed, or somebody deleted it — and the
// wait reports it as such with `Found: false`. Turning it into an error would
// make "the pointer is gone" indistinguishable from "the cluster is
// unreachable", and only one of those is worth retrying.
var observeCluster = func(ctx context.Context, kctx string, names []string) ([]Status, error) {
	args := append([]string{"get", kindKustomization + "." + strings.TrimSuffix(apiVersionKustomize, "/v1")},
		names...)
	args = append(args, "-n", Namespace, "-o", "json", "--ignore-not-found")
	cmd := exec.CommandContext(ctx, "kubectl", cluster.KubectlArgs(kctx, args...)...)
	raw, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			// kubectl exits non-zero when ANY named object is
			// missing even with --ignore-not-found. That is the
			// absent-pointer state, not a read failure.
			if strings.Contains(stderr, "NotFound") || strings.Contains(stderr, "not found") {
				return missingStatuses(kctx, names), nil
			}
			if stderr != "" {
				return nil, fmt.Errorf("flux: read kustomizations in %s: %w: %s", kctx, err, firstLine(stderr))
			}
		}
		return nil, fmt.Errorf("flux: read kustomizations in %s: %w", kctx, err)
	}
	return parseStatuses(kctx, names, raw)
}

func missingStatuses(kctx string, names []string) []Status {
	out := make([]Status, 0, len(names))
	for _, n := range names {
		out = append(out, Status{Name: n, Cluster: kctx})
	}
	return out
}

// parseStatuses decodes `kubectl get -o json`, which is a single object for
// one name and a List for several.
//
// Statuses come back IN THE ORDER ASKED FOR, not in the order the apiserver
// returned, so a caller's report lines are stable across polls. An apiserver's
// list order is not a contract, and a progress display that reordered itself
// between polls would read as objects appearing and disappearing.
func parseStatuses(kctx string, names []string, raw []byte) ([]Status, error) {
	var doc struct {
		Kind  string            `json:"kind"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("flux: parse kustomization status from %s: %w", kctx, err)
	}
	items := doc.Items
	if doc.Kind != "" && !strings.HasSuffix(doc.Kind, "List") {
		items = []json.RawMessage{raw}
	}
	found := map[string]Status{}
	for _, item := range items {
		s, err := parseStatus(kctx, item)
		if err != nil {
			return nil, err
		}
		found[s.Name] = s
	}
	out := make([]Status, 0, len(names))
	for _, n := range names {
		if s, ok := found[n]; ok {
			out = append(out, s)
			continue
		}
		out = append(out, Status{Name: n, Cluster: kctx})
	}
	return out, nil
}

func parseStatus(kctx string, raw json.RawMessage) (Status, error) {
	var obj struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			LastAppliedRevision string `json:"lastAppliedRevision"`
			Conditions          []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Status{}, fmt.Errorf("flux: parse kustomization from %s: %w", kctx, err)
	}
	s := Status{
		Name: obj.Metadata.Name, Cluster: kctx, Found: true,
		Revision: obj.Status.LastAppliedRevision,
	}
	for _, c := range obj.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		s.Ready = c.Status == "True"
		s.Reason, s.Message = c.Reason, c.Message
		break
	}
	return s, nil
}
