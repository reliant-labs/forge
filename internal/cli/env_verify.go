package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/cluster"
)

// Environment verification: proving a binding ledger's claims against the
// cluster that is actually serving traffic.
//
// `forge release verify` proves a release ledger names artifacts that really
// exist in a public registry. This is its sibling one layer down: it proves
// the environment is RUNNING those artifacts. The two questions are
// independent, and the gap between them is where the last release went wrong —
// a ledger said prod ran v1.5.13 and nothing in the tooling could say whether
// prod had ever received it.
//
// WHY THE LEDGER CANNOT ANSWER THIS. `promoted_at` in an EnvBinding is stamped
// when the env is PROMOTED, not when it is deployed. Promotion writes a
// pointer; deployment moves bytes. A binding is therefore a statement of
// intent, and reading it back to confirm the intent was carried out is
// circular — it would report success for an env that was promoted and then
// never deployed at all, which is precisely the failure mode this exists to
// catch. Checking that required reading live digests out of the cluster by
// hand, one `kubectl get deploy -o jsonpath` per workload.
//
// WHY NOT CONTROL-PLANE. control-plane carries `observed_*` columns and an
// ObservedState struct that look like exactly the right source, and they are
// not. An audit found NOTHING WRITES THEM — the reconciler was never built, so
// every row returns column defaults, and a verifier reading them would report a
// confident, uniformly wrong answer. Even fully populated they would be the
// wrong source: they are documented as a MIRROR maintained so a drift badge can
// render without a cluster round-trip, and a mirror is the one thing a verifier
// must not trust. A stale mirror and a lying ledger are the same class of
// failure, and this command exists because of that class. The in-tree
// precedent is control-plane's billing/inframeter, which takes the live
// cluster as its candidate set on the money-critical path for this reason.
//
// So: the cluster is the only source, read through the same kubectl path
// `forge env deploy` writes through, targeting the same declaratively-derived
// context.

// imageState is the verdict for ONE image named in a binding.
//
// FIVE STATES, AND NONE OF THEM IS OPTIONAL. The tempting shape is a boolean —
// matches or does not — and every condition that does not fit has to be forced
// into one of those two, which is how a verifier becomes actively harmful.
// Filing "could not reach the cluster" under drift means a VPN blip pages
// someone about a release defect that does not exist, and a check that cries
// wolf is disabled within the week. Filing it under match means a green report
// over an environment nobody looked at, which is the exact state the last
// release shipped in.
type imageState int

const (
	// imageMatch: the cluster is running the digest the binding declares.
	imageMatch imageState = iota
	// imageDrift: the cluster is running a DIFFERENT digest. The headline
	// case, and the reason the command exists.
	imageDrift
	// imageMissing: declared in the binding, running nowhere in the
	// namespace. Never deployed, or deleted since.
	imageMissing
	// imageUntagged: the workload runs this image by mutable TAG, not by
	// digest, so there is no digest to compare. Distinct from drift because
	// nothing is proven wrong, and distinct from match because nothing is
	// proven right — a tag is a name that can be repointed, so the bytes
	// behind it are unknown. Reachable via a --no-digest deploy.
	imageUntagged
	// imageUnreachable: the cluster could not be read. Says NOTHING about
	// the environment.
	imageUnreachable
)

// String renders the fixed-width label used in the report, mirroring
// verifyStatus.String() so the two commands' output reads as one family.
func (s imageState) String() string {
	switch s {
	case imageMatch:
		return "MATCH"
	case imageDrift:
		return "DRIFT"
	case imageMissing:
		return "MISSING"
	case imageUntagged:
		return "UNTAGGED"
	case imageUnreachable:
		return "UNREACHABLE"
	default:
		return "UNKNOWN"
	}
}

// MarshalJSON emits the LOWERCASE string form so `forge env verify --json` is
// readable by a human and by jq, rather than emitting the iota. Mirrors
// deploytarget.Health.MarshalJSON.
//
// Derived from String() rather than written out a second time: the text column
// and the JSON value must never be able to disagree about which of the five
// states a verdict is in, and a second switch is exactly how that drift starts.
// The case difference is presentational — String() is padded into a
// fixed-width report column, JSON values are conventionally lowercase.
func (s imageState) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strings.ToLower(s.String()) + `"`), nil
}

// UnmarshalJSON reads the string form back, making the wire contract
// round-trippable so a consumer — including this package's own tests — can
// decode a report into the same types that produced it.
//
// AN UNRECOGNIZED STATE IS AN ERROR, NOT A DEFAULT. imageMatch is the zero
// value, so the usual "fall back to the zero value" spelling would decode a
// state this binary does not know about — a typo, or a newer forge that added
// a sixth state — into a clean MATCH. That is the single worst failure this
// type can have: a green verdict over an environment nobody actually checked,
// which is the exact condition the five-state model exists to prevent.
// Failing loudly makes the version skew visible instead of silently benign.
func (s *imageState) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("image state must be a string: %w", err)
	}
	switch strings.ToLower(name) {
	case "match":
		*s = imageMatch
	case "drift":
		*s = imageDrift
	case "missing":
		*s = imageMissing
	case "untagged":
		*s = imageUntagged
	case "unreachable":
		*s = imageUnreachable
	default:
		return fmt.Errorf("unknown image state %q (expected match, drift, missing, untagged or unreachable) — "+
			"refusing to decode it as a default, which would report an unchecked environment as clean", name)
	}
	return nil
}

// imageVerification is one image's verdict.
//
// Declared and Running are BOTH carried, and carried separately from Detail,
// because the drift case is only actionable with both digests in hand: "these
// differ" sends the reader back to run the kubectl query this command just
// ran. Workloads names where the running digest was found, so a reader knows
// which of the several workloads sharing an image to look at.
//
// The json tags are part of the `--json` output contract: additive extension
// only, so a consumer filtering on the states it knows keeps working.
type imageVerification struct {
	// Image is the bare image name as it appears in the binding's Resolved
	// map ("control-plane", "reliant").
	Image string `json:"image"`
	// Declared is the digest the binding froze at promote time.
	Declared string `json:"declared"`
	// Running is the digest observed in the cluster. Empty when missing or
	// unreachable; for UNTAGGED it holds the tag reference instead, which is
	// what the reader needs to see.
	Running string `json:"running,omitempty"`
	// Workloads names the workloads carrying this image, as
	// "Deployment/api-server[container]".
	Workloads []string   `json:"workloads,omitempty"`
	State     imageState `json:"state"`
	Detail    string     `json:"detail,omitempty"`
}

// clusterImageLister is the ONE cluster capability verification needs,
// declared HERE at the consumer rather than exported from the cluster package
// for callers to depend on.
//
// It is a seam so unit tests can construct the interesting states without a
// cluster. That is not a convenience: the headline case — a binding declaring
// digest A while the cluster runs digest B — cannot be produced against a real
// cluster on demand without deliberately deploying a wrong image to it, and a
// test that needs a live cluster to run is a test that does not run.
//
// The error return means the read did not COMPLETE. That is what separates
// UNREACHABLE from every other state, so the two signals stay distinct all the
// way from kubectl's stderr to the process exit code.
type clusterImageLister interface {
	ListWorkloadImages(ctx context.Context, kubeContext, namespace string) ([]cluster.WorkloadImage, error)
}

// kubectlImageLister is the production lister, over the same kubectl path the
// deploy pipeline uses.
type kubectlImageLister struct{}

func (kubectlImageLister) ListWorkloadImages(ctx context.Context, kubeContext, namespace string) ([]cluster.WorkloadImage, error) {
	return cluster.ListWorkloadImages(ctx, kubeContext, namespace)
}

// parsedImageRef is an image reference split into the parts comparison needs.
type parsedImageRef struct {
	// Name is the bare image name — the last path segment before any tag or
	// digest ("control-plane" from "ghcr.io/acme/control-plane@sha256:…").
	// This is the key a binding's Resolved map uses.
	Name string
	// Digest is the "sha256:…" portion, empty when the ref is tag-only.
	Digest string
	// Tag is the ":tag" portion, empty when absent.
	Tag string
	// Ref is the original, unmodified reference.
	Ref string
}

// parseImageRef splits a container image reference.
//
// The digest is cut FIRST, before any registry-port handling, because '@' can
// appear only once and only before the digest. The remaining host:port
// ambiguity is then unambiguous: a colon is a tag separator only if it falls
// after the last '/', which is what keeps "localhost:5000/api" from being read
// as image "localhost" at tag "5000/api".
func parseImageRef(ref string) parsedImageRef {
	out := parsedImageRef{Ref: ref}
	rest := ref

	if name, digest, found := strings.Cut(rest, "@"); found {
		out.Digest = digest
		rest = name
	}

	// A colon is a tag separator only after the final path separator;
	// before it, it is a registry port.
	if idx := strings.LastIndex(rest, ":"); idx >= 0 && idx > strings.LastIndex(rest, "/") {
		out.Tag = rest[idx+1:]
		rest = rest[:idx]
	}

	if idx := strings.LastIndex(rest, "/"); idx >= 0 {
		out.Name = rest[idx+1:]
	} else {
		out.Name = rest
	}
	return out
}

// verifyEnvImages compares a binding's declared digests against what the
// cluster runs, returning one verdict per DECLARED image.
//
// Iterating the declared set (not the running set) is deliberate and is what
// makes MISSING detectable: an image the binding names and the cluster does
// not run produces no cluster row to iterate, so a loop over running workloads
// would silently skip it — reporting a clean environment for one that never
// received half its release.
//
// The converse (running images the binding does not declare) is NOT reported
// as a failure. A namespace legitimately contains workloads outside the
// release — a database, a sidecar proxy, another team's chart — and a verifier
// that failed on those would be red permanently for correct environments.
func verifyEnvImages(running []cluster.WorkloadImage, declared map[string]string) []imageVerification {
	// Index the cluster's images by bare name. Several workloads commonly
	// share one image (an api-server and a worker off the same build), so
	// this is one-to-many.
	type runningRef struct {
		parsed   parsedImageRef
		workload string
	}
	byName := map[string][]runningRef{}
	for _, w := range running {
		p := parseImageRef(w.Image)
		if p.Name == "" {
			continue
		}
		// A config-borne reference is labelled with the env var it came
		// from, so a reader can tell "this is what the operator will
		// launch" from "this is a container that is running now".
		where := fmt.Sprintf("%s/%s[%s]", w.Kind, w.Name, w.Container)
		if w.EnvVar != "" {
			where = fmt.Sprintf("%s/%s[%s $%s]", w.Kind, w.Name, w.Container, w.EnvVar)
		}
		byName[p.Name] = append(byName[p.Name], runningRef{
			parsed:   p,
			workload: where,
		})
	}

	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)

	results := make([]imageVerification, 0, len(names))
	for _, name := range names {
		want := declared[name]
		res := imageVerification{Image: name, Declared: want}

		refs := byName[name]
		if len(refs) == 0 {
			res.State = imageMissing
			res.Detail = "declared by the binding but no workload in this namespace runs it — never deployed, or deleted since"
			results = append(results, res)
			continue
		}

		for _, r := range refs {
			res.Workloads = append(res.Workloads, r.workload)
		}

		// Collect the distinct digests actually running. More than one is
		// its own kind of drift: a namespace mid-rollout, or two workloads
		// that were supposed to share a build and no longer do.
		seen := map[string]bool{}
		var digests []string
		var tagOnly []string
		for _, r := range refs {
			switch {
			case r.parsed.Digest != "":
				if !seen[r.parsed.Digest] {
					seen[r.parsed.Digest] = true
					digests = append(digests, r.parsed.Digest)
				}
			default:
				tagOnly = append(tagOnly, r.parsed.Ref)
			}
		}

		switch {
		case len(digests) == 0:
			// Every workload runs this image by tag. Nothing is proven
			// either way — a tag can be repointed at any bytes.
			res.State = imageUntagged
			res.Running = strings.Join(dedupe(tagOnly), ", ")
			res.Detail = fmt.Sprintf("running by mutable tag, not by digest — cannot prove it is %s. "+
				"A --no-digest deploy leaves the tag unpinned; re-deploy without it to make this checkable", want)
		case len(digests) == 1 && digests[0] == want && len(tagOnly) == 0:
			res.State = imageMatch
			res.Running = digests[0]
		case len(digests) == 1 && digests[0] == want:
			// Digest matches where pinned, but some workload is on a tag.
			// Not drift — but not a clean match either, and quietly
			// upgrading it to MATCH would hide the unpinned workload.
			res.State = imageUntagged
			res.Running = digests[0]
			res.Detail = fmt.Sprintf("digest matches where pinned, but %d workload(s) run this image by tag (%s) and cannot be checked",
				len(tagOnly), strings.Join(dedupe(tagOnly), ", "))
		case len(digests) == 1:
			res.State = imageDrift
			res.Running = digests[0]
			res.Detail = fmt.Sprintf("declared %s but running %s", want, digests[0])
		default:
			sort.Strings(digests)
			res.State = imageDrift
			res.Running = strings.Join(digests, ", ")
			res.Detail = fmt.Sprintf("declared %s but workloads run %d DIFFERENT digests (%s) — a partial rollout, or workloads that no longer share a build",
				want, len(digests), strings.Join(digests, ", "))
		}
		results = append(results, res)
	}
	return results
}

// unreachableVerifications turns a failed cluster read into one UNREACHABLE
// verdict per declared image.
//
// Reporting per-image rather than as a single command-level error keeps the
// report shape identical whether the cluster answered or not, so a caller
// (human or CI) parses one format. The reason is repeated on each line rather
// than stated once because these lines are commonly grepped individually.
func unreachableVerifications(declared map[string]string, cause error) []imageVerification {
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]imageVerification, 0, len(names))
	for _, name := range names {
		out = append(out, imageVerification{
			Image:    name,
			Declared: declared[name],
			State:    imageUnreachable,
			Detail:   fmt.Sprintf("could not read the cluster: %v", cause),
		})
	}
	return out
}

// envVerifyTally counts verdicts by state, for the summary line and the exit
// code decision.
//
// All five buckets are carried into JSON separately, and Unreachable in
// particular is never folded into another count: the whole reason exit 2
// exists is that a cluster nobody could read is not evidence a release is
// wrong, and a report that sums it into drift re-creates the confusion the
// exit codes were split to prevent.
type envVerifyTally struct {
	Match       int `json:"match"`
	Drift       int `json:"drift"`
	Missing     int `json:"missing"`
	Untagged    int `json:"untagged"`
	Unreachable int `json:"unreachable"`
}

func tallyEnvVerifications(results []imageVerification) envVerifyTally {
	var t envVerifyTally
	for _, r := range results {
		switch r.State {
		case imageMatch:
			t.Match++
		case imageDrift:
			t.Drift++
		case imageMissing:
			t.Missing++
		case imageUntagged:
			t.Untagged++
		case imageUnreachable:
			t.Unreachable++
		}
	}
	return t
}

// dedupe removes duplicates while preserving order.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
