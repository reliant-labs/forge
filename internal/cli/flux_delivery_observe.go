package cli

// READING a self-hosted delivery back (Bundle.applier = "flux"): what the
// env's Flux reports for the pointer forge wrote. One reader serves the
// deploy's wait and `forge env status`, so the two cannot disagree about
// whether a release is live.
//
// Four objects answer it, read by the names the input carries:
//
//   - the pointer: when it was last written (its managedFields), the moment
//     a renderer condition must postdate to be about this input;
//   - the delivery ResourceSet: has the renderer applied the pointer's input?
//     Its evidence is the OCIRepository it renders: only a source whose
//     spec.ref.digest is the input's digest proves the renderer consumed it.
//     Stalled since the pointer was written is a failure; a Stalled older
//     than the write is about a previous input, and is progress;
//   - the OCIRepository: can Flux fetch the bundle at its digest? Judged only
//     once its spec requests the digest and its status describes that spec
//     (observedGeneration): an error from the previous digest decides nothing;
//   - one Kustomization per applier: has it applied the digest
//     (status.lastAppliedRevision) and is it Ready? A Ready=False whose
//     status is current and whose last ATTEMPT was this digest is a failure,
//     relayed verbatim — except a
//     `Forbidden` for a forge-delivery-* identity, which is mapped to the
//     grant refusal's message (flux_delivery_grants.go): the reconciler's half
//     of the same tripwire the deploy's preflight runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/fluxdelivery"
	"github.com/reliant-labs/forge/pkg/release"
)

// fluxObjectStatus is one Flux object's Ready condition and revisions.
type fluxObjectStatus struct {
	Name  string `json:"name"`
	Found bool   `json:"found"`
	// Ready is the Ready condition's status: "True", "False", "Unknown",
	// or "" when the object reports none yet.
	Ready   string `json:"ready,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Stalled is the Stalled condition's status (flux-operator's renderer).
	Stalled bool `json:"stalled,omitempty"`
	// LastApplied and LastAttempted are a Kustomization's revisions; the
	// source's artifact revision for an OCIRepository.
	LastApplied   string `json:"last_applied_revision,omitempty"`
	LastAttempted string `json:"last_attempted_revision,omitempty"`
	// Current is true when the status describes the object's current spec
	// (status.observedGeneration == metadata.generation).
	Current bool `json:"current"`
	// RequestedDigest is an OCIRepository's spec.ref.digest: the digest the
	// renderer last rendered into it.
	RequestedDigest string `json:"requested_digest,omitempty"`
	// StalledSince is when the Stalled condition last turned True (zero
	// when it is not Stalled, or did not say).
	StalledSince time.Time `json:"stalled_since,omitempty"`
}

// fluxDeliveryObservation is the delivery as Flux reports it.
type fluxDeliveryObservation struct {
	// PointerWrittenAt is when the pointer was last written, by the API
	// server's clock (zero when it could not be read).
	PointerWrittenAt time.Time          `json:"pointer_written_at,omitempty"`
	Renderer         fluxObjectStatus   `json:"renderer"`
	Source           fluxObjectStatus   `json:"source"`
	Appliers         []fluxObjectStatus `json:"appliers"`
}

// rendered: the renderer is installed, healthy and current, and the source it
// renders requests digest — the evidence it consumed the promoted input. An
// applier Ready on the digest without it is an orphan of an earlier render,
// not this delivery.
func (o fluxDeliveryObservation) rendered(digest string) bool {
	r := o.Renderer
	return r.Found && r.Current && !r.Stalled && r.Ready == "True" &&
		o.Source.Found && o.Source.RequestedDigest == digest
}

// sourceFetched: the source's status describes its current spec, which
// requests digest, and it has fetched that digest.
func (o fluxDeliveryObservation) sourceFetched(digest string) bool {
	s := o.Source
	return s.Found && s.Current && s.RequestedDigest == digest && s.Ready == "True" && revisionDigestOf(s.LastApplied) == digest
}

// converged: the renderer applied the input, the source fetched its digest,
// and every applier applied the digest and is Ready, each by a status that
// describes its current spec.
func (o fluxDeliveryObservation) converged(digest string) bool {
	if len(o.Appliers) == 0 || !o.rendered(digest) || !o.sourceFetched(digest) {
		return false
	}
	for _, a := range o.Appliers {
		if !a.Found || a.Ready != "True" || !a.Current || revisionDigestOf(a.LastApplied) != digest {
			return false
		}
	}
	return true
}

// rendererStalledOnInput: the renderer is Stalled, and turned so no earlier
// than the pointer's last write, so the stall is about this input. Both times
// are the cluster's. A stall that cannot be dated after the write is not
// attributed to it: the wait keeps going and its timeout names the stall.
func (o fluxDeliveryObservation) rendererStalledOnInput() bool {
	r := o.Renderer
	return r.Found && r.Stalled && !o.PointerWrittenAt.IsZero() && !r.StalledSince.IsZero() &&
		!r.StalledSince.Before(o.PointerWrittenAt)
}

// failure is the verdict when Flux reports this digest failing, or nil while
// it is still progressing. A Forbidden for an apply identity is the grant
// refusal; anything else is Flux's own words.
func (o fluxDeliveryObservation) failure(env, rel, digest string) error {
	if o.rendererStalledOnInput() {
		return exitCodeError{code: exitWrong, msg: fmt.Sprintf(
			"env %s: the renderer has not applied the promoted inputs: ResourceSet %s is Stalled (%s): %s",
			env, o.Renderer.Name, o.Renderer.Reason, strings.TrimSpace(o.Renderer.Message))}
	}
	if o.Source.Found && o.Source.Current && o.Source.RequestedDigest == digest &&
		o.Source.Ready == "False" && !fluxProgressing(o.Source.Reason) {
		return exitCodeError{code: exitWrong, msg: fmt.Sprintf(
			"env %s: its Flux cannot fetch bundle %s: OCIRepository %s reports %s: %s",
			env, shortDigest(digest), o.Source.Name, o.Source.Reason, strings.TrimSpace(o.Source.Message))}
	}
	var failing []fluxObjectStatus
	for _, a := range o.Appliers {
		if a.Found && a.Current && a.Ready == "False" && revisionDigestOf(a.LastAttempted) == digest && !fluxProgressing(a.Reason) {
			failing = append(failing, a)
		}
	}
	if len(failing) == 0 {
		return nil
	}
	for _, a := range failing {
		if missing := fluxDeliveryForbidden(a.Message); len(missing) > 0 {
			refusal := newFluxDeliveryGrantRefusal(env, rel, missing)
			refusal.Reporter, refusal.Detail = a.Name, a.Message
			return refusal
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "env %s failed to converge to %s: %d of its Kustomizations report an error\n", env, shortDigest(digest), len(failing))
	for _, a := range failing {
		fmt.Fprintf(&b, "  %s: %s — %s\n", a.Name, a.Reason, strings.TrimSpace(a.Message))
	}
	return exitCodeError{code: exitWrong, msg: strings.TrimRight(b.String(), "\n")}
}

// fluxProgressing are the Ready=False reasons that mean "not yet", not "no".
func fluxProgressing(reason string) bool {
	switch reason {
	case "Progressing", "DependencyNotReady", "ProgressingWithRetry", "ReconciliationInProgress":
		return true
	}
	return false
}

// summary is one progress line: how many appliers have applied the digest.
func (o fluxDeliveryObservation) summary(digest string) string {
	if !o.Renderer.Found {
		return "the delivery ResourceSet is not installed (run the bootstrap)"
	}
	var prefix string
	switch {
	case o.Renderer.Stalled:
		prefix = fmt.Sprintf("the renderer %s is Stalled (%s): %s; ", o.Renderer.Name, o.Renderer.Reason, strings.TrimSpace(o.Renderer.Message))
	case !o.rendered(digest):
		prefix = "the renderer has not rendered the promoted input yet; "
	case !o.sourceFetched(digest):
		prefix = fmt.Sprintf("OCIRepository %s has not fetched %s yet; ", o.Source.Name, shortDigest(digest))
	}
	done := 0
	var pending []string
	for _, a := range o.Appliers {
		if a.Found && a.Ready == "True" && revisionDigestOf(a.LastApplied) == digest {
			done++
			continue
		}
		state := "not rendered yet"
		if a.Found {
			state = emptyAs(a.Reason, "waiting")
		}
		pending = append(pending, a.Name+" ("+state+")")
	}
	line := prefix + fmt.Sprintf("%d/%d Kustomizations applied %s", done, len(o.Appliers), shortDigest(digest))
	if len(pending) > 0 {
		line += "; waiting on " + strings.Join(pending, ", ")
	}
	return line
}

// fluxDeliveryForbiddenRE matches the API server's RBAC refusal as
// kustomize-controller relays it, for an impersonated ServiceAccount.
var fluxDeliveryForbiddenRE = regexp.MustCompile(`User "system:serviceaccount:[^:"]+:([^"]+)" cannot ([a-z]+) resource "([^"]+)" in API group "([^"]*)"(?: in the namespace "([^"]+)"| at the cluster scope)`)

// fluxDeliveryForbidden reads every apply-identity refusal out of a Flux
// message, as the permissions it lacked.
func fluxDeliveryForbidden(message string) []fluxDeliveryPermission {
	var out []fluxDeliveryPermission
	seen := map[string]bool{}
	for _, m := range fluxDeliveryForbiddenRE.FindAllStringSubmatch(message, -1) {
		sa := m[1]
		if sa != fluxdelivery.PreServiceAccount && sa != fluxdelivery.ApplyServiceAccount && sa != fluxdelivery.NoneServiceAccount {
			continue
		}
		resource, _, _ := strings.Cut(m[3], "/") // a subresource is granted with its resource
		p := fluxDeliveryPermission{Identity: sa, Scope: fluxDeliveryScopeCluster, Group: m[4], Resource: resource, Verb: m[2]}
		if m[5] != "" {
			p.Scope = "namespace:" + m[5]
		}
		if !seen[p.line()] {
			seen[p.line()] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].line() < out[j].line() })
	return out
}

// fluxDeliveryObserve reads the delivery. A var so the wait and status are
// testable without a cluster.
var fluxDeliveryObserve = func(ctx context.Context, fd FluxDeliveryEntity, in fluxdelivery.BindingInput) (fluxDeliveryObservation, error) {
	obs := fluxDeliveryObservation{
		Renderer: fluxObjectStatus{Name: fd.ResourceSet},
		Source:   fluxObjectStatus{Name: in.Source.Name},
	}
	get := func(resource string) (map[string]map[string]any, error) {
		out, err := exec.CommandContext(ctx, "kubectl", "--context", fd.Cluster, "-n", fd.Namespace,
			"get", resource, "-o", "json").Output()
		if err != nil {
			return nil, fmt.Errorf("read %s in %s on %s: %w", resource, fd.Namespace, fd.Cluster, kubectlErr(err))
		}
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(out, &list); err != nil {
			return nil, fmt.Errorf("decode %s: %w", resource, err)
		}
		byName := map[string]map[string]any{}
		for _, item := range list.Items {
			md, _ := item["metadata"].(map[string]any)
			name, _ := md["name"].(string)
			byName[name] = item
		}
		return byName, nil
	}
	pointers, err := get("resourcesetinputproviders.fluxcd.controlplane.io")
	if err != nil {
		return obs, err
	}
	obs.PointerWrittenAt = lastWrittenAt(pointers[fluxdelivery.InputProviderName(fd.Env, "")])
	sets, err := get("resourcesets.fluxcd.controlplane.io")
	if err != nil {
		return obs, err
	}
	sources, err := get("ocirepositories.source.toolkit.fluxcd.io")
	if err != nil {
		return obs, err
	}
	ks, err := get("kustomizations.kustomize.toolkit.fluxcd.io")
	if err != nil {
		return obs, err
	}
	obs.Renderer = fluxStatusOf(fd.ResourceSet, sets[fd.ResourceSet])
	obs.Source = fluxStatusOf(in.Source.Name, sources[in.Source.Name])
	if obs.Source.Found {
		obs.Source.LastApplied, _ = nested(sources[in.Source.Name], "status", "artifact", "revision").(string)
		obs.Source.RequestedDigest, _ = nested(sources[in.Source.Name], "spec", "ref", "digest").(string)
	}
	for _, a := range in.Appliers {
		obs.Appliers = append(obs.Appliers, fluxStatusOf(a.Name, ks[a.Name]))
	}
	return obs, nil
}

// fluxStatusOf reads one object's Ready/Stalled conditions and revisions.
func fluxStatusOf(name string, obj map[string]any) fluxObjectStatus {
	s := fluxObjectStatus{Name: name}
	if obj == nil {
		return s
	}
	s.Found = true
	gen, _ := nested(obj, "metadata", "generation").(float64)
	observed, _ := nested(obj, "status", "observedGeneration").(float64)
	s.Current = gen == 0 || observed == gen
	s.LastApplied, _ = nested(obj, "status", "lastAppliedRevision").(string)
	s.LastAttempted, _ = nested(obj, "status", "lastAttemptedRevision").(string)
	conds, _ := nested(obj, "status", "conditions").([]any)
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		typ, _ := cm["type"].(string)
		status, _ := cm["status"].(string)
		switch typ {
		case "Ready":
			s.Ready = status
			s.Reason, _ = cm["reason"].(string)
			s.Message, _ = cm["message"].(string)
		case "Stalled":
			s.Stalled = status == "True"
			if s.Stalled {
				s.StalledSince = parseKubeTime(cm["lastTransitionTime"])
			}
		}
	}
	return s
}

// lastWrittenAt is the latest write any field manager made to obj, by the API
// server's clock (metadata.managedFields[].time). Zero for a missing object.
func lastWrittenAt(obj map[string]any) time.Time {
	var latest time.Time
	fields, _ := nested(obj, "metadata", "managedFields").([]any)
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		if t := parseKubeTime(fm["time"]); t.After(latest) {
			latest = t
		}
	}
	return latest
}

// parseKubeTime reads a Kubernetes metav1.Time (RFC 3339); zero when absent
// or malformed.
func parseKubeTime(v any) time.Time {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// nested walks a decoded JSON object by keys.
func nested(obj map[string]any, keys ...string) any {
	var cur any = obj
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// fluxDeliveryStatusOf is `forge env status`'s reconciler line for a flux env:
// what its Flux reports for the live pointer, judged against the bound
// promotion. ("", nil) for every other env, and for one the render cannot
// resolve: the rest of the status still reports.
//
// Only a converged pointer that projects the bound promotion is a success.
// Workload digests can match while Flux is unreachable, was never pointed, or
// is pointed at another promotion — or while a config-only release is still
// applying — so each of those is a non-success verdict here: a failure Flux
// reports is exit 1, anything this cannot prove is exit 2.
func fluxDeliveryStatusOf(ctx context.Context, projectDir, env string, bound release.Promotion) (string, error) {
	fd, err := fluxDeliveryOfEnv(ctx, projectDir, env)
	if err != nil || fd == nil {
		return "", nil
	}
	undetermined := func(line, format string, a ...any) (string, error) {
		return "  reconciler  in-cluster Flux: " + line, exitCodeError{code: exitUndetermined, msg: fmt.Sprintf(format, a...)}
	}
	live, err := fluxDeliveryPointerIO.read(ctx, *fd)
	if err != nil {
		return undetermined("UNREACHABLE ("+err.Error()+")", "env %s: its in-cluster Flux could not be read, so its delivery is unverified: %v", env, err)
	}
	if live == nil {
		return undetermined("never pointed at a release (deploy one: forge env deploy "+env+")",
			"env %s: its in-cluster Flux has never been pointed at a release, so release %s is not delivered", env, bound.Release)
	}
	in, err := fluxdelivery.LiveInput(live)
	if err != nil {
		return undetermined("the pointer cannot be read ("+err.Error()+")", "env %s: its delivery pointer cannot be read: %v", env, err)
	}
	if bound.ID != "" && in.Promotion != bound.ID {
		return undetermined(fmt.Sprintf("pointed at promotion %s, not the bound %s", emptyAs(in.Promotion, "(none)"), bound.ID),
			"env %s: its in-cluster Flux is pointed at promotion %s, not the bound promotion %s (release %s); re-run: forge env deploy %s",
			env, emptyAs(in.Promotion, "(none)"), bound.ID, bound.Release, env)
	}
	obs, err := fluxDeliveryObserve(ctx, *fd, in)
	if err != nil {
		return undetermined("UNREACHABLE ("+err.Error()+")", "env %s: its in-cluster Flux could not be read, so its delivery is unverified: %v", env, err)
	}
	digest := in.Source.Digest
	switch {
	case obs.converged(digest):
		return fmt.Sprintf("  reconciler  in-cluster Flux applied %s (promotion %s): %s", shortDigest(digest), in.Promotion, obs.summary(digest)), nil
	default:
		if ferr := obs.failure(env, bound.Release, digest); ferr != nil {
			return fmt.Sprintf("  reconciler  in-cluster Flux FAILING on %s (promotion %s)", shortDigest(digest), in.Promotion), ferr
		}
		return fmt.Sprintf("  reconciler  in-cluster Flux PROGRESSING on %s (promotion %s): %s", shortDigest(digest), in.Promotion, obs.summary(digest)),
			exitCodeError{code: exitUndetermined, msg: fmt.Sprintf(
				"env %s: its in-cluster Flux has not finished applying %s (%s); gate on it with: forge env status %s --wait",
				env, shortDigest(digest), obs.summary(digest), env)}
	}
}
