package cli

// The two seams F4 left narrow on purpose, now that they carry what the
// hosted RPCs require.
//
// WHAT IS ACTUALLY UNDER TEST. Both widenings exist to close a failure that
// COMPILES: a seam that cannot express a safety property produces a hosted
// path silently weaker than the file one, and connect-go's JSON codec
// discards what is not sent — so the symptom is not an error but an absent
// check. These tests therefore assert on the REQUEST BODY, not only on the
// return value: "the guard reached the server" is the property, and a
// misspelled or missing field is indistinguishable from success at every
// other layer.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// TestBeginApply_SendsTheGuardToTheServer is the anti-stomp property. An
// apply asserts the same compare-and-set a promote does, and the server
// checks it under the env row lock before anything moves (doc §6.3, F-5).
// With the pre-widening `supersede bool` seam there was nowhere to put the
// expectation, so it would have reached the server asserting NOTHING — no
// error, no log line, just a deploy that overwrote whatever landed under it.
func TestBeginApply_SendsTheGuardToTheServer(t *testing.T) {
	t.Parallel()
	f := &fakeDSOTCaller{replies: map[string]any{
		procBeginApply: map[string]any{"apply": json.RawMessage(cpApplyFixture)},
	}}
	var recorder applyRecorder = hostedRecordStore{
		client:   f,
		project:  "app",
		resolver: stubEnvResolver{id: "env_prod"},
	}

	if _, err := recorder.BeginApply(context.Background(), testApplyRecord(), appendGuard{
		ExpectedCurrentID: "pr_0",
	}); err != nil {
		t.Fatalf("BeginApply: %v", err)
	}

	body := f.body(t, procBeginApply)
	// `expectedCurrentPromotionId`, NOT promote's `expectCurrent`:
	// BeginDeployApplyRequest and PromoteReleaseRequest express the identical
	// CAS and disagree on the field name. The wrong spelling is discarded
	// server-side, which is why this is pinned literally.
	wantFields(t, body, map[string]any{
		"environmentId":              "env_prod",
		"bundleId":                   "bnd_1",
		"expectedCurrentPromotionId": "pr_0",
	})
}

// TestBeginApply_SendsExpectedUnboundForAFirstApply is the same property for
// the other half of the oneof. A first apply asserts "this env has no
// promotion", and BeginApply spells that `expected_unbound` where Promote
// spells it `expect_unbound` — so the two encoders are deliberately separate
// and each needs its own proof.
func TestBeginApply_SendsExpectedUnboundForAFirstApply(t *testing.T) {
	t.Parallel()
	f := &fakeDSOTCaller{replies: map[string]any{
		procBeginApply: map[string]any{"apply": json.RawMessage(cpApplyFixture)},
	}}
	var recorder applyRecorder = hostedRecordStore{
		client: f, project: "app", resolver: stubEnvResolver{id: "env_prod"},
	}

	if _, err := recorder.BeginApply(context.Background(), testApplyRecord(), appendGuard{
		ExpectUnbound: true,
	}); err != nil {
		t.Fatalf("BeginApply: %v", err)
	}

	body := f.body(t, procBeginApply)
	wantFields(t, body, map[string]any{"expectedUnbound": true})
	if _, present := body["expectedCurrentPromotionId"]; present {
		t.Error("the CAS is a oneof: asserting unbound must not also name a promotion")
	}
}

// TestRecordBundle_HostedSeamSendsBytesNotTheDescription is the bundle half.
// The server records what it VERIFIES: it checks digest = sha256(manifest),
// checks the manifest's config descriptor names sha256(config), and takes the
// shape, provenance, config digest and release from its own strict decode of
// that blob (doc §6.3).
//
// So the assertion is as much about what is ABSENT as what is present. A
// client that could state a shape could state one that disagrees with the
// bytes it pushed, and every reader downstream would then trust the
// description over the artifact.
func TestRecordBundle_HostedSeamSendsBytesNotTheDescription(t *testing.T) {
	t.Parallel()
	f := &fakeDSOTCaller{replies: map[string]any{
		procEnsureEnv:    map[string]any{"environment": map[string]any{"id": "env_prod"}},
		procRecordBundle: map[string]any{"bundle": json.RawMessage(cpBundleFixture), "created": true},
	}}
	var recorder bundleRecorder = hostedRecordStore{
		client: f, project: "app", resolver: stubEnvResolver{id: "env_prod"},
	}

	rec, created, err := recorder.RecordBundle(context.Background(),
		release.BundleRecord{
			Env: "prod", Shape: dsotTestShape(),
			// A DELIBERATELY WRONG description. The record that comes
			// back must carry the server's values, not these — which is
			// the whole point of the server deriving them from the bytes.
			Digest:       "sha256:" + rep64('0'),
			ConfigDigest: "sha256:" + rep64('0'),
			Release:      "v0.0.0-not-this",
		},
		bundleBlobs{
			Repository: "ghcr.io/acme/app/bundle.v1/prod",
			Manifest:   []byte(`{"schemaVersion":2}`),
			Config:     []byte(`{"schema":"forge.dev/bundle/v1"}`),
		})
	if err != nil {
		t.Fatalf("RecordBundle: %v", err)
	}
	if !created || rec.ID != "bnd_1" {
		t.Errorf("created=%v id=%q, want created and bnd_1", created, rec.ID)
	}
	if rec.Release != "v1.4.0" || rec.ConfigDigest != "sha256:"+rep64('e') {
		t.Errorf("the record must come from the SERVER's verified decode, not the caller's description: %+v", rec)
	}

	body := f.body(t, procRecordBundle)
	wantFields(t, body, map[string]any{
		"environmentId": "env_prod",
		"repository":    "ghcr.io/acme/app/bundle.v1/prod",
		"manifest":      "eyJzY2hlbWFWZXJzaW9uIjoyfQ==",
		"config":        "eyJzY2hlbWEiOiJmb3JnZS5kZXYvYnVuZGxlL3YxIn0=",
	})
	for _, described := range []string{"shape", "provenance", "configDigest", "digest", "releaseVersion"} {
		if _, present := body[described]; present {
			t.Errorf("RecordBundle must not send %q: the server derives it from the verified config blob, "+
				"and a client that can state it can state one that disagrees with the bytes it pushed", described)
		}
	}
}

// TestBeginApply_MachineBackendRefusesAStaleGuard proves the machine backend
// applies the SAME rule rather than accepting the parameter and ignoring it.
//
// That is the failure the widening is guarding against on this side: a
// backend that took the guard and dropped it would satisfy the interface, the
// hosted path would be protected, and the file path — control-plane's own
// prod, after F3 — would not be.
func TestBeginApply_MachineBackendRefusesAStaleGuard(t *testing.T) {
	dir := newLedgerTestProject(t, "apply-guard-project")
	var recorder applyRecorder = testRecordStore(t, dir)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// The env IS bound, so an apply expecting it to be unbound is stale.
	testPromote(t, dir, release.Promotion{
		Env: "prod", Release: "v1.0.0", Kind: release.KindPromote,
		Resolved: map[string]string{"api": sha("a")},
	})

	_, err := recorder.BeginApply(ctx, release.Apply{
		Env: "prod", BundleID: "bundle-1",
		CreatedAt: now, DeadlineAt: now.Add(10 * time.Minute),
	}, appendGuard{ExpectUnbound: true})
	if err == nil {
		t.Fatal("an apply asserting 'unbound' against a bound env must be refused; " +
			"accepting the guard and ignoring it leaves the file path unprotected")
	}
	// The refusal must be the SAME type a refused promote produces, so the
	// exit code and the --json refusal object have one source whichever
	// write was declined.
	var refusal *promoteRefusedError
	if !errors.As(err, &refusal) {
		t.Fatalf("a refused apply must be a *promoteRefusedError, got %T: %v", err, err)
	}
	if refusal.Reason != reasonPromotionConflict {
		t.Errorf("reason = %q, want %q", refusal.Reason, reasonPromotionConflict)
	}
}

// TestBeginApply_MachineBackendAdmitsAMatchingGuard is the other side: the
// guard must not refuse a correct expectation. Without this, a backend that
// refused everything would pass the test above.
func TestBeginApply_MachineBackendAdmitsAMatchingGuard(t *testing.T) {
	dir := newLedgerTestProject(t, "apply-guard-ok-project")
	var recorder applyRecorder = testRecordStore(t, dir)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	promoted := testPromote(t, dir, release.Promotion{
		Env: "prod", Release: "v1.0.0", Kind: release.KindPromote,
		Resolved: map[string]string{"api": sha("a")},
	})

	begun, err := recorder.BeginApply(ctx, release.Apply{
		Env: "prod", BundleID: "bundle-1", PromotionID: promoted.ID,
		CreatedAt: now, DeadlineAt: now.Add(10 * time.Minute),
	}, appendGuard{ExpectedCurrentID: promoted.ID})
	if err != nil {
		t.Fatalf("an apply naming the env's current promotion must be admitted: %v", err)
	}
	if begun.ID == "" {
		t.Fatal("the backend must stamp an apply id")
	}
}

// TestBeginApply_AnAbandonedApplyDoesNotBlockTheNextOne is failure mode F-2.
//
// An apply that began and never reported is a DIFFERENT fact from one that
// failed: past its deadline nobody is ever going to report, so the row reads
// ABANDONED. The next apply must not be refused by it. The opposite — an
// in-flight refusal that outlives the applier — would wedge an env
// permanently after one crashed deploy, with no way through but a flag whose
// whole meaning is "override a live apply".
func TestBeginApply_AnAbandonedApplyDoesNotBlockTheNextOne(t *testing.T) {
	dir := newLedgerTestProject(t, "abandoned-apply-project")
	var recorder applyRecorder = testRecordStore(t, dir)
	ctx := context.Background()

	// An apply whose deadline has already passed, and which never
	// reported — the shape a crashed or network-partitioned applier leaves.
	past := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	abandoned, err := recorder.BeginApply(ctx, release.Apply{
		Env: "prod", BundleID: "bundle-1",
		CreatedAt: past, DeadlineAt: past.Add(10 * time.Minute),
	}, appendGuard{})
	if err != nil {
		t.Fatalf("BeginApply: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	next, err := recorder.BeginApply(ctx, release.Apply{
		Env: "prod", BundleID: "bundle-2",
		CreatedAt: now, DeadlineAt: now.Add(10 * time.Minute),
	}, appendGuard{})
	if err != nil {
		t.Fatalf("an apply past its deadline with no outcome is ABANDONED and must not block the next: %v", err)
	}
	if next.ID == abandoned.ID {
		t.Fatal("the second apply must be its own record")
	}
	// And it must NOT be marked as having superseded one: nothing was
	// overridden, because nothing was in flight. A false supersede mark
	// would put an override in the audit trail that never happened.
	if next.SupersededInFlight {
		t.Error("stepping past an abandoned apply is not a supersede — nothing was in flight to override")
	}
}

// stubEnvResolver answers the env-id lookup without a control plane. The
// resolution is not what these tests are about; what forge SENDS once it has
// an id is.
type stubEnvResolver struct{ id string }

func (s stubEnvResolver) ResolveEnvironmentID(context.Context, string) (string, error) {
	return s.id, nil
}
