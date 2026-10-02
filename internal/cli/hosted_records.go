package cli

// The HOSTED half of F3's record seams (binding_store.go): the twin of
// machineRecordStore.
//
// ONE OF THE THREE IS IMPLEMENTED HERE, AND THAT IS NOT AN OVERSIGHT.
// `sessionReporter` is satisfied in full below. `bundleRecorder` and
// `applyRecorder` are NOT, because their signatures cannot express what the
// hosted RPCs require, and satisfying them anyway would mean a hosted path
// that is silently weaker than the file one. The two gaps, and what to do
// about them, are written out at the bottom of this file for F6a — the task
// that owns the callers and can change the seams.
//
// The clients themselves are complete and consumer-shaped
// (hostedBundleClient, hostedApplyClient, hostedSessionClient). Nothing is
// missing from the wire layer; what is missing is a seam wide enough to drive
// it through.

import (
	"context"
	"fmt"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// hostedRecordStore is the records half of one project's hosted ledger, the
// twin of machineRecordStore.
//
// A separate type from hostedStore for the reason F3 split machineRecordStore
// from machineBindingStore: the two have disjoint consumers, and a store
// handed to `forge env up` to report a session has no business also exposing
// Append.
type hostedRecordStore struct {
	client cloudCaller
	// project scopes every write. Env identity on a control plane is
	// (org, project, name), so a records store without it would report a
	// session against whichever project the server guessed.
	project string
	// resolver turns an env NAME into the control plane's id, for the RPCs
	// that are keyed on one.
	resolver hostedEnvResolver
}

// hostedRecordStoreFor builds the records half for a project on one control
// plane. The mirror of recordStoreFor, which does the same for the machine
// ledger.
func hostedRecordStoreFor(client cloudCaller, project string) hostedRecordStore {
	return hostedRecordStore{
		client:   client,
		project:  project,
		resolver: cloudEnvResolver{client: client, project: project},
	}
}

// ReportSession satisfies sessionReporter.
//
// The seam has no `stopped` parameter, and it does not need one: a stopped
// session is one whose StoppedAt is set, which is how the file store reads it
// too. So the two backends derive the same fact from the same field rather
// than from a flag one of them would have to be told.
//
// It NEVER BLOCKS the caller's stack in spirit, but it does return its error,
// because the seam is `error`-returning and swallowing it here would hide a
// misconfiguration forever. §7.4's "never blocks `forge env up`" is the
// CALLER's rule, and ReportSessionBestEffort is the spelling that states it at
// the call site.
func (s hostedRecordStore) ReportSession(ctx context.Context, sess release.LocalSession) error {
	_, err := hostedSessionClient{client: s.client}.ReportSession(ctx, s.project, sess, sess.StoppedAt != nil)
	return err
}

// Bundle satisfies the read half of bundleRecorder: a bundle by id, or (nil,
// nil) when the control plane holds none.
//
// The env name is not available on this call, and a BundleRecord's Env is
// what a reader renders. So it is filled from the record the server returns —
// which carries the environment ID — rather than guessed. See the note below
// about why the WRITE half is not here.
func (s hostedRecordStore) Bundle(ctx context.Context, id string) (*release.BundleRecord, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: reading a bundle needs an id", release.ErrInvalid)
	}
	// "" as the env: the read is addressed by id alone, and a caller that
	// holds a bundle id holds it from a record that already named its env.
	// Inventing a name here would put a wrong one on the record.
	return hostedBundleClient{client: s.client}.GetBundleByID(ctx, "", id)
}

// envID resolves an env name to the control plane's id.
func (s hostedRecordStore) envID(ctx context.Context, env string) (string, error) {
	return s.resolver.ResolveEnvironmentID(ctx, env)
}

// Compile-time proof of the one seam this backend satisfies in full. The
// assertion is what keeps a rename from silently dropping the hosted store
// out of a capability its consumers type-assert for.
var _ sessionReporter = hostedRecordStore{}

// ─── THE TWO SEAMS THIS BACKEND DOES NOT SATISFY, AND WHY ────────────────────
//
// Both gaps are the same shape: the seam's parameter list carries less than
// the hosted RPC requires, and the missing part is a SAFETY property rather
// than a convenience. Implementing the method anyway would compile, and would
// produce a hosted path quietly weaker than the file path it is supposed to
// mirror. Neither is a wire-layer problem — hostedBundleClient and
// hostedApplyClient are complete — so the fix belongs with whoever owns the
// callers and the seams, which is F6a.
//
// 1. bundleRecorder.RecordBundle(ctx, b release.BundleRecord)
//
//    RecordBundle on a control plane takes the bundle's OWN BYTES — the OCI
//    manifest and the config blob — plus the repository they were pushed to.
//    The server checks digest = sha256(manifest), checks that the manifest's
//    config descriptor names sha256(config), decodes the config strictly as a
//    release.BundleDoc, and takes the shape, provenance, config digest and
//    release FROM THE VERIFIED DOCUMENT (doc §6.3).
//
//    A release.BundleRecord carries the DESCRIPTION of all of that and none
//    of the bytes. So the only way to satisfy this signature would be to send
//    the description — which is exactly what §6.3 forbids, because a client
//    that can state a shape can record one that disagrees with the bytes it
//    pushed, and every reader downstream then trusts the description over the
//    artifact. The file ledger has the same asymmetry and gets away with it:
//    its blobs are in a local OCI layout it also owns, so there is no second
//    party to lie to.
//
//    RECOMMENDATION for F6a: widen the seam to carry the bytes, e.g.
//
//        RecordBundle(ctx context.Context, b release.BundleRecord, blobs release.BundleBlobs) (…)
//
//    where BundleBlobs is {Repository string; Manifest, Config []byte}. The
//    machine backend ignores the blobs it already holds; the hosted backend
//    sends them and ignores the description. That keeps ONE seam, and keeps
//    "the bytes are the contract" true on the side where it matters.
//    hostedBundleClient.RecordBundle already takes exactly that: (env,
//    environmentID, bundleBlobs, run).
//
// 2. applyRecorder.BeginApply(ctx, a release.Apply, supersede bool)
//
//    BeginApply on a control plane asserts a COMPARE-AND-SET — the env's
//    current promotion must be the one the plan was read against, or the env
//    must be unbound — and the server checks it under the env row lock before
//    anything moves (doc §6.3, F-5, F-19). release.Apply has no field for
//    that expectation, and `supersede` is the in-flight override, not the CAS.
//
//    Satisfying the signature would mean sending no expectation at all, and
//    connect-go discards what is not sent: the apply would reach the server
//    asserting NOTHING. The anti-stomp guard would not fire, and the failure
//    is invisible — no error, no log line, just a deploy that overwrites
//    whatever landed under it. That is the precise stomp promote_cas.go
//    exists to close, reopened on the apply path.
//
//    A client-side read of Current(env) to fill the expectation is NOT a fix:
//    read-then-write from a client is the race the server's row lock exists
//    to close, and hosted_ledger.go's header says so.
//
//    RECOMMENDATION for F6a: carry the guard, which both backends already
//    speak — appendGuard is the promote path's own type:
//
//        BeginApply(ctx context.Context, a release.Apply, guard appendGuard) (release.Apply, error)
//
//    appendGuard already holds SupersedeInFlight, so the bool folds into it
//    and the parameter count does not grow. The machine backend applies the
//    same rule locally (admitPromotion does this for promotions); the hosted
//    backend forwards it. hostedApplyClient.BeginApply already takes exactly
//    that: (environmentID, release.Apply, appendGuard).
//
// Until those land, a caller drives the hosted side through
// hostedBundleClient and hostedApplyClient directly. Both are exported within
// the package and fully tested against control-plane's own protojson.

// hostedApplyRecorder is the apply client bound to a project, for the caller
// that drives it directly until the seam above is widened.
//
// It exists so F6a has ONE place to get a configured apply client — the same
// service hostedRecordStoreFor provides for sessions — rather than
// assembling a hostedApplyClient and an env resolver at each call site and
// risking two different opinions about which env.
func (s hostedRecordStore) Applies() hostedApplyClient {
	return hostedApplyClient{client: s.client}
}

// Bundles is the same, for bundles.
func (s hostedRecordStore) Bundles() hostedBundleClient {
	return hostedBundleClient{client: s.client}
}

// Plans is the same, for PlanDeploy.
func (s hostedRecordStore) Plans() hostedPlanClient {
	return hostedPlanClient{client: s.client}
}

// Live is the same, for the Live view and drift.
func (s hostedRecordStore) Live() hostedLiveClient {
	return hostedLiveClient{client: s.client}
}

// EnsureEnvironmentID resolves (creating if needed) the control-plane id for
// an env a WRITE is about to target, which is the rule every mutating hosted
// command follows: the env is declared in the project's KCL, so whichever
// verb reaches it first creates it.
func (s hostedRecordStore) EnsureEnvironmentID(ctx context.Context, env string, kind deploytarget.HostedEnvKind) (string, error) {
	return ensureHostedEnv(ctx, s.client, deploytarget.HostedEnvRef{Project: s.project, Name: env, Kind: kind})
}
