package cli

// The HOSTED half of F3's record seams (binding_store.go): the twin of
// machineRecordStore.
//
// ALL THREE ARE IMPLEMENTED HERE. F4 left two of them unsatisfied on purpose,
// because the seams as declared could not express what the hosted RPCs
// require and satisfying them anyway would have produced a hosted path
// silently weaker than the file one. Both seams are now wide enough:
// bundleRecorder carries the bundle's own BYTES (the server records what it
// verifies), and applyRecorder.BeginApply carries the appendGuard (the
// compare-and-set the server checks under the env row lock). See
// binding_store.go for why each is shaped that way.

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

// RecordBundle satisfies the write half of bundleRecorder: it sends the
// BYTES and lets the server derive the record from them.
//
// The BundleRecord parameter supplies only what the wire is keyed on and what
// the returned record is labelled with — the env NAME. Everything a reader
// downstream trusts (shape, provenance, config digest, release) comes back
// from the server's own strict decode of the verified config blob, never from
// the description this client could have written. That is the point of the
// seam carrying both: the caller states where it pushed and hands over the
// bytes, and has no way to state a shape at all.
func (s hostedRecordStore) RecordBundle(ctx context.Context, b release.BundleRecord, blobs bundleBlobs) (release.BundleRecord, bool, error) {
	envID, err := s.EnsureEnvironmentID(ctx, b.Env, hostedKindForRecord(b.Shape.Kind))
	if err != nil {
		return release.BundleRecord{}, false, err
	}
	return hostedBundleClient{client: s.client}.RecordBundle(
		ctx, b.Env, envID, blobs.Repository, blobs.Manifest, blobs.Config, b.Run)
}

// BeginApply satisfies applyRecorder, forwarding the guard so the server
// checks the compare-and-set under the env row lock.
//
// Nothing is pre-checked with a read here. A client-side read of the env's
// current promotion, followed by a write asserting what it saw, is exactly
// the race the server's lock exists to close.
func (s hostedRecordStore) BeginApply(ctx context.Context, a release.Apply, guard appendGuard) (release.Apply, error) {
	envID, err := s.envID(ctx, a.Env)
	if err != nil {
		return release.Apply{}, err
	}
	return hostedApplyClient{client: s.client}.BeginApply(ctx, envID, a, guard)
}

// FinishApply reports the outcome. The `created` flag the client returns is
// dropped: to a CALLER, "this call recorded the outcome" and "the same
// outcome was already recorded" are the same success — an apply ended once,
// and a retry after a lost response must not read as a failure (F-2). A
// DIFFERENT outcome for one apply is still an error, from the server.
func (s hostedRecordStore) FinishApply(ctx context.Context, env string, o release.ApplyOutcome) error {
	_, _, err := hostedApplyClient{client: s.client}.FinishApply(ctx, env, o)
	return err
}

// hostedKindForRecord maps a shape's env kind to the hosted kind an ensure
// carries — the inverse of hostedControlPlaneKindName.
//
// A bundle is recorded for an env the project's KCL declares, so whichever
// verb reaches the control plane first creates it. The kind is IMMUTABLE
// server-side, so a kind that disagrees with the existing row is refused
// there, by the party that holds the row, rather than reconciled here.
//
// An unrecognised kind maps to the empty kind rather than to a plausible
// default. A guess would be a client quietly choosing an env's kind, and the
// one it would most likely guess — persistent — is the kind whose secrets are
// write-only and whose promotions the converger applies.
func hostedKindForRecord(kind release.EnvKind) deploytarget.HostedEnvKind {
	switch kind {
	case release.EnvLocal:
		return deploytarget.HostedEnvLocal
	case release.EnvPersistent:
		return deploytarget.HostedEnvPersistent
	case release.EnvSelfManaged:
		return deploytarget.HostedEnvSelfManaged
	default:
		return ""
	}
}

// envID resolves an env name to the control plane's id.
func (s hostedRecordStore) envID(ctx context.Context, env string) (string, error) {
	return s.resolver.ResolveEnvironmentID(ctx, env)
}

// Compile-time proof that the hosted backend satisfies every seam it claims,
// exactly as binding_store.go asserts for the machine backend. The assertions
// are what keep a method rename from silently dropping a store out of a
// capability its consumers type-assert for.
var (
	_ sessionReporter = hostedRecordStore{}
	_ bundleRecorder  = hostedRecordStore{}
	_ applyRecorder   = hostedRecordStore{}
)

// Applies is the configured apply client, for the paths that need more of it
// than the applyRecorder seam exposes — ListApplies for a reader, and
// FinishApplyWithObserved for the drift objects a self-managed deploy reads
// back (F-17), which is optional and so deliberately not on the seam.
//
// ONE place to get a configured client, rather than assembling a
// hostedApplyClient and an env resolver at each call site and risking two
// different opinions about which env.
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
