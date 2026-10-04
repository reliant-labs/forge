package cli

// `forge env status <env>` for an env converged by a Flux in its own cluster.
//
// WHAT CHANGES, AND WHY IT IS A CHANGE WORTH MAKING. A machine-ledger env used
// to report `convergence  — no reconciler for this env`, and that sentence was
// TRUE at the time: nothing watched a jsonl file, so forge's own apply was the
// whole story and there was no reconciler to report on. For an env that
// declares no lifecycle and targets a cluster, it is now false — there IS a
// reconciler, it is in the cluster, and its verdict is readable. Leaving the
// old sentence in place would tell an operator to stop looking at exactly the
// moment the answer became available.
//
// FORGE IS STILL NOT THE WITNESS, which is why this fills in the SAME
// `convergence` block a hosted env's control plane fills in, rather than
// inventing a forge-authored verdict beside it. Flux applied the env; forge
// read what Flux reports. The difference from the hosted case is only how
// direct the reading is — first-hand from the apiserver instead of relayed
// through a control plane's observer — and `ObservedBy` carries exactly that
// distinction rather than hiding it.
//
// THE STATE VOCABULARY IS FLUX'S, passed through. `convergenceOf` does the
// same for a control plane's phase names, and for the same reason: a
// translation layer here would eventually disagree with what
// `flux get kustomization` shows an operator for the same object, and they
// would be debugging the difference rather than the env.
//
// IT NEVER FAILS THE STATUS. A status command's job is to report what it can
// see; an unreachable cluster is a fact to state, not a reason to produce no
// document. Every failure path here lands in `ConvergenceDetail` — which is
// the field that already exists to say WHY there is no convergence to show —
// so `forge env status` on a laptop with no cluster running still prints the
// provenance and the sessions it did read.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/internal/flux"
	"github.com/reliant-labs/forge/pkg/release"
)

// observerInCluster is [envStatusConvergence.ObservedBy] for this path.
const observerInCluster = "the in-cluster reconciler (Flux)"

// convergenceObserver names who saw a convergence, defaulting to the control
// plane — which is what every record written before this path existed is.
func convergenceObserver(c envStatusConvergence) string {
	if strings.TrimSpace(c.ObservedBy) != "" {
		return c.ObservedBy
	}
	return "the control plane"
}

// fluxConvergenceFor reads an env's convergence out of the Kustomizations
// forge's pointer created.
//
// It returns the convergence, or a DETAIL saying why there is none — never an
// error, per the file header. The two are mutually exclusive and a caller
// assigns both fields from them.
func fluxConvergenceFor(ctx context.Context, env, projectDir string, entities *KCLEntities, now time.Time) (*envStatusConvergence, string) {
	digest := newestRecordedBundleDigest(projectDir, env)
	if digest == "" {
		// No bundle has ever been recorded, so no pointer has ever been
		// written. Distinct from "the reconciler has not converged":
		// there is nothing for it to converge TO, and the fix is a
		// build rather than a wait.
		return nil, "no bundle recorded yet for this env — run `forge env build " + env + "`"
	}
	doc, err := fluxBundleDocFromLayout(ctx, projectDir, digest)
	if err != nil {
		return nil, fmt.Sprintf("this env's bundle %s could not be read from this machine's ledger (%v)",
			shortDigest(digest), err)
	}
	clusters := fluxTargetClusters(entities, doc.ClusterPaths)
	if len(clusters) == 0 {
		return nil, "this env's bundle routes no documents to any cluster it declares"
	}

	var pointers []flux.Pointer
	for _, kctx := range clusters {
		// The pointer is REBUILT rather than read back, because what we
		// need from it is only the NAMES to poll — and those are a pure
		// function of (env, cluster, paths), which is why
		// flux.KustomizationName exists. Reading the live objects to
		// discover their own names would be circular, and listing by
		// label would return pointers for other envs that happen to
		// share the cluster.
		//
		// The repository and requestedAt are irrelevant here (nothing
		// is applied), but BuildPointer refuses an empty repository, so
		// the published address is reconstructed from the declaration —
		// the same string a deploy would write.
		p, perr := fluxPointerFor(fluxPointerInput{
			env:          env,
			repository:   clusterReachableRegistry(bundleRepositoryFor(entities, env)),
			digest:       digest,
			cluster:      kctx,
			clusterPaths: doc.ClusterPaths,
			requestedAt:  now,
		})
		if perr != nil {
			return nil, fmt.Sprintf("this env's pointer could not be described (%v)", perr)
		}
		if p.Empty() {
			continue
		}
		pointers = append(pointers, p)
	}
	if len(pointers) == 0 {
		return nil, "this env's bundle routes no documents to any cluster it declares"
	}

	obs, err := fluxObserve(ctx, pointers, now)
	if err != nil {
		// An unreachable cluster. Stated, not folded into "not
		// converged": "I could not look" and "it has not converged" are
		// different diagnoses, and reporting the first as the second
		// would send an operator to debug a deploy when the problem is
		// a stopped cluster.
		return nil, fmt.Sprintf("this env's in-cluster reconciler could not be read (%v)", err)
	}
	return fluxConvergenceOf(obs, digest, doc), ""
}

// fluxConvergenceOf projects an observation onto the status block.
func fluxConvergenceOf(obs flux.Observation, digest string, doc release.BundleDoc) *envStatusConvergence {
	out := &envStatusConvergence{
		BundleDigest: digest,
		ObservedBy:   observerInCluster,
		ObservedAt:   obs.At.UTC().Format(time.RFC3339),
		Detail:       obs.Summary(digest),
	}
	if doc.Release != "" {
		// The bundle's own release, as its id. A machine ledger's
		// bundle has no server-assigned id, and the digest is already
		// the identity — so this is the human-facing label rather than
		// a second key.
		out.BundleID = doc.Release
	}
	out.State = fluxConvergenceState(obs, digest)
	return out
}

// fluxConvergenceState is the one word the status line leads with.
//
// FOUR STATES, and each is a different thing to do next:
//
//	converged     every Kustomization applied this digest and is healthy.
//	failed        at least one reports a real error. Read Detail: it is
//	              Flux's own reason.
//	progressing   applied-and-health-checking, or still fetching. Wait.
//	pending       no pointer in the cluster at all — either none was ever
//	              written (nobody has deployed) or somebody deleted it.
//	              The fix is a deploy, not a wait, which is why it is not
//	              folded into progressing.
func fluxConvergenceState(obs flux.Observation, digest string) string {
	switch {
	case obs.Converged(digest):
		return "converged"
	case len(obs.Failures()) > 0:
		return "failed"
	}
	for _, s := range obs.Statuses {
		if s.Found {
			return "progressing"
		}
	}
	return "pending"
}

// bundleRepositoryFor is where this env's bundles are published, host-side.
func bundleRepositoryFor(entities *KCLEntities, env string) string {
	base := fluxRegistryBase(entities)
	if base == "" {
		// BuildPointer refuses an empty repository, and a status read
		// needs a pointer only for its NAMES. A placeholder keeps the
		// read working for an env whose registry declaration is
		// missing — the deploy is where that is refused, with a message
		// naming the declaration to add.
		return "unknown-registry/" + env
	}
	return bundle.Repository(base, env)
}
