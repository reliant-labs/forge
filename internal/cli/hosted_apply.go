package cli

// The HOSTED apply half (doc §6.3): BeginApply, FinishApply, ListApplies.
//
// AN APPLY IS TWO RECORDS, NOT ONE. BeginApply is written BEFORE anything
// moves — "who tried to make it so, from where" — and FinishApply is a
// separate, write-once report of how it ended. Both halves stay append-only,
// and "the apply never reported back" shows up as the ABSENCE of an outcome
// rather than as a status somebody had to remember to set. That absence is
// read against the deadline (release.DeriveApplyState): before it the apply is
// running, after it nobody is ever going to report and the row is abandoned.
//
// BeginApply carries the same compare-and-set a promote does, plus the plan
// digest and stop-class acknowledgements (O-13). Every one of those checks is
// made by the SERVER under the env row lock, and nothing here pre-checks with
// a read: a read-then-write from a client is precisely the race the lock
// exists to close.
//
// FinishApply is one of exactly two requests in this service that carry
// OBSERVED state (the other is ReportLocalSession). For a self-managed
// cluster the platform cannot reach, forge is the only observer there is. It
// is therefore recorded as a REPORT — `reportedBy` is set by the server, never
// sent — and never feeds billing or policy (F-9).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

const (
	procBeginApply  = "controlplane.v1.DeployService/BeginApply"
	procFinishApply = "controlplane.v1.DeployService/FinishApply"
	procListApplies = "controlplane.v1.DeployService/ListApplies"
)

// hostedApplyListLimit bounds an apply listing, as hostedReleaseListLimit
// does for releases. The server caps a page anyway.
const hostedApplyListLimit = 100

// ─── Wire shapes ─────────────────────────────────────────────────────────────

// wireApplyOutcome is controlplane.v1.DeployApplyOutcome.
//
// Workloads is a google.protobuf.Struct and is kept raw for the reason
// shapeFromWire keeps a shape raw: forge's apply reporting is its own shape,
// and a camelCase mirror of it here could only ever disagree with
// release.ApplyWorkload. The Struct's content is the canonical JSON of
// forge's own type, so release's snake_case tags decode it directly.
type wireApplyOutcome struct {
	Status     string          `json:"status"`
	Summary    string          `json:"summary,omitempty"`
	Workloads  json.RawMessage `json:"workloads,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	// ReportedBy is SET BY THE SERVER from the credential that reported.
	// The label that keeps the report honest: a reader sees whose claim
	// this is. forge reads it and never sends it.
	ReportedBy string `json:"reportedBy,omitempty"`
}

// wireApply is controlplane.v1.DeployApply.
//
// `admittedFindings` (tag 7) is reserved for #516 and absent here.
type wireApply struct {
	ID            string   `json:"id"`
	EnvironmentID string   `json:"environmentId,omitempty"`
	BundleID      string   `json:"bundleId,omitempty"`
	PromotionID   string   `json:"promotionId,omitempty"`
	AppliedBy     string   `json:"appliedBy,omitempty"`
	Run           *wireRun `json:"run,omitempty"`
	// SupersededInFlight: this apply was started over one that had not
	// finished. Recorded because an override that leaves no trace is
	// indistinguishable from the refusal never having fired.
	SupersededInFlight bool              `json:"supersededInFlight,omitempty"`
	CreatedAt          time.Time         `json:"createdAt"`
	DeadlineAt         *time.Time        `json:"deadlineAt,omitempty"`
	Outcome            *wireApplyOutcome `json:"outcome,omitempty"`
	// State is DERIVED on read: running | succeeded | failed | timed_out |
	// abandoned. Read THIS, not the presence of Outcome, to decide what
	// happened — an absent outcome means two different things and the
	// deadline is what separates them.
	State string `json:"state,omitempty"`
	// PlanDigest and ApprovedBy are the plan this apply was approved
	// against, and who approved it. The approver comes from the credential,
	// never from the client (O-11).
	PlanDigest string `json:"planDigest,omitempty"`
	ApprovedBy string `json:"approvedBy,omitempty"`
}

// wireObjectHash is controlplane.v1.DeployObjectHash: one live object as
// forge observed it at the apply's end, for drift detection (F-17).
type wireObjectHash struct {
	Cluster    string `json:"cluster,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	Hash       string `json:"hash"`
}

// applyFromWire converts one apply. env is the NAME the caller asked about.
// It returns the apply and its outcome SEPARATELY, mirroring the two records
// they are: a caller that folds them has to do so knowingly, and
// release.DeriveApplyState is the folding every reader shares.
func applyFromWire(env string, w wireApply) (release.Apply, *release.ApplyOutcome, error) {
	a := release.Apply{
		ID: w.ID, Env: env, BundleID: w.BundleID, PromotionID: w.PromotionID,
		AppliedBy: w.AppliedBy, Run: runFromWire(w.Run),
		PlanDigest: w.PlanDigest, SupersededInFlight: w.SupersededInFlight,
		CreatedAt: w.CreatedAt,
	}
	if w.DeadlineAt != nil {
		a.DeadlineAt = *w.DeadlineAt
	}
	outcome, err := applyOutcomeFromWire(w.ID, w.Outcome)
	if err != nil {
		return release.Apply{}, nil, fmt.Errorf("control plane returned apply %s: %w", w.ID, err)
	}
	return a, outcome, nil
}

func applyOutcomeFromWire(applyID string, w *wireApplyOutcome) (*release.ApplyOutcome, error) {
	if w == nil {
		return nil, nil
	}
	o := release.ApplyOutcome{
		ApplyID: applyID, Status: release.ApplyStatus(w.Status),
		Summary: w.Summary, ReportedBy: w.ReportedBy,
	}
	if w.FinishedAt != nil {
		o.FinishedAt = *w.FinishedAt
	}
	if len(w.Workloads) > 0 && string(w.Workloads) != "null" {
		// A google.protobuf.Struct is an OBJECT, never a bare list, so the
		// per-resource results ride under a `workloads` member rather than
		// as the Struct itself. applyWorkloadsStruct writes exactly that
		// shape, and control-plane's own protojson confirms it.
		var envelope struct {
			Workloads []release.ApplyWorkload `json:"workloads"`
		}
		if err := json.Unmarshal(w.Workloads, &envelope); err != nil {
			// The per-resource detail is DISPLAY data hanging off a
			// terminal outcome. One forge cannot decode must not make the
			// outcome itself unreadable, so the status survives and the
			// detail is reported as the error it is.
			return nil, fmt.Errorf("apply outcome workloads: %w", err)
		}
		o.Workloads = envelope.Workloads
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return &o, nil
}

// ─── The client ──────────────────────────────────────────────────────────────

// applyGuardWireFields renders the guard as BeginDeployApplyRequest's tags
// 4–6, which are NOT spelled the same as PromoteReleaseRequest's.
//
// The two requests express the identical compare-and-set and disagree on one
// field name: Promote's oneof member is `expect_unbound` (tag 8), while
// BeginApply's plain bool is `expected_unbound` (tag 5). Reusing
// guardWireFields here sent `expectUnbound`, which connect-go discards as an
// unknown field — so a first apply that asserted "this env has no promotion"
// would have reached the server asserting NOTHING, and the CAS that exists to
// stop a stomp would have silently not run.
//
// So the duplication is deliberate: two small encoders at their two consumers
// beat one shared encoder that has to be right about both spellings. The
// difference is a proto fact, not a forge choice, and it is pinned by a test
// against control-plane's own protojson for each request.
func applyGuardWireFields(guard appendGuard, req map[string]any) {
	switch {
	case guard.ExpectUnbound:
		req["expectedUnbound"] = true
	case guard.ExpectedCurrentID != "":
		req["expectedCurrentPromotionId"] = guard.ExpectedCurrentID
	}
	if guard.SupersedeInFlight {
		req["supersedeInFlight"] = true
	}
}

// hostedApplyClient records and reads applies on one control plane.
type hostedApplyClient struct {
	client cloudCaller
}

// BeginApply records the attempt and returns it.
//
// IT TAKES THE DOMAIN RECORD, not a parallel request struct. release.Apply
// already carries every field BeginDeployApplyRequest needs except the
// compare-and-set, which is appendGuard — the promote path's own type — so an
// apply and a promote cannot drift in what they compare or how they spell it.
// environmentID is separate because the wire is keyed on the control plane's
// id while a release.Apply names its env, and deriving one from the other
// here would mean a resolve inside a write.
//
// The deadline travels as a.DeadlineAt rather than a seconds count: the apply
// record is what gets stored, so the wait budget the caller recorded and the
// one the server enforces are the same value by construction.
//
// A refusal — the CAS failed, a stale plan, an unacknowledged stop finding,
// an apply already in flight — comes back as a *promoteRefusedError, the same
// type a refused promote produces, so the exit code and the --json refusal
// object have one source whichever write was declined.
func (c hostedApplyClient) BeginApply(ctx context.Context, environmentID string, a release.Apply, guard appendGuard) (release.Apply, error) {
	if err := a.Validate(); err != nil {
		return release.Apply{}, err
	}
	req := map[string]any{
		"environmentId": environmentID,
		"bundleId":      a.BundleID,
	}
	if a.PromotionID != "" {
		req["promotionId"] = a.PromotionID
	}
	applyGuardWireFields(guard, req)
	if run := runWireFields(a.Run); run != nil {
		req["run"] = run
	}
	if a.PlanDigest != "" {
		req["planDigest"] = a.PlanDigest
	}
	if len(a.AcknowledgedFindings) > 0 {
		req["acknowledgedFindings"] = a.AcknowledgedFindings
	}
	// The budget as the server wants it: whole seconds from now to the
	// deadline the caller recorded. Rounded UP, so a sub-second remainder
	// never sends 0 — which the server reads as "no budget given" and
	// would replace with its own default, silently widening a deadline the
	// caller chose.
	if secs := int(a.DeadlineAt.Sub(a.CreatedAt).Round(time.Second) / time.Second); secs > 0 {
		req["deadlineSeconds"] = secs
	}
	var resp struct {
		Apply wireApply `json:"apply"`
	}
	if err := c.client.Call(ctx, procBeginApply, req, &resp); err != nil {
		if refused := applyRefusalFromWire(err); refused != nil {
			return release.Apply{}, refused
		}
		return release.Apply{}, bundlesUnsupported(err)
	}
	recorded, _, err := applyFromWire(a.Env, resp.Apply)
	return recorded, err
}

// FinishApply reports the outcome and says whether this call recorded it.
// Inserted exactly once: the same outcome again is created=false (a retry),
// and a DIFFERENT outcome for one apply is AlreadyExists — a conflict, because
// an apply ended one way and a second claim about it cannot both be true.
func (c hostedApplyClient) FinishApply(ctx context.Context, env string, o release.ApplyOutcome) (release.Apply, bool, error) {
	if err := o.Validate(); err != nil {
		return release.Apply{}, false, err
	}
	req := map[string]any{
		"applyId": o.ApplyID,
		"status":  string(o.Status),
	}
	if o.Summary != "" {
		req["summary"] = o.Summary
	}
	if len(o.Workloads) > 0 {
		// A google.protobuf.Struct carries the canonical JSON of forge's
		// own ApplyWorkload list verbatim, snake_case keys and all —
		// the same rule the declared shape follows.
		workloads, err := applyWorkloadsStruct(o.Workloads)
		if err != nil {
			return release.Apply{}, false, err
		}
		req["workloads"] = workloads
	}
	req["finishedAt"] = o.FinishedAt.UTC().Format(time.RFC3339Nano)
	// ReportedBy is NOT sent. The server sets it from the credential, and a
	// client-supplied attribution would be the reporting party vouching for
	// itself — the one part of a report that must not come from it.
	var resp struct {
		Apply   wireApply `json:"apply"`
		Created bool      `json:"created"`
	}
	if err := c.client.Call(ctx, procFinishApply, req, &resp); err != nil {
		return release.Apply{}, false, bundlesUnsupported(err)
	}
	a, _, err := applyFromWire(env, resp.Apply)
	if err != nil {
		return release.Apply{}, false, err
	}
	return a, resp.Created, nil
}

// FinishApplyWithObserved is FinishApply plus the live objects forge read back
// at the apply's end, for drift (F-17). Separate from FinishApply rather than
// an extra parameter on it, because observing is optional: forge reads objects
// back on a self-managed env, where nothing else can reach the cluster, and
// not on a hosted one, where the platform's own observer supplies them.
func (c hostedApplyClient) FinishApplyWithObserved(ctx context.Context, env string, o release.ApplyOutcome, observed []release.ShapeObject) (release.Apply, bool, error) {
	if len(observed) == 0 {
		return c.FinishApply(ctx, env, o)
	}
	if err := o.Validate(); err != nil {
		return release.Apply{}, false, err
	}
	req := map[string]any{
		"applyId":    o.ApplyID,
		"status":     string(o.Status),
		"finishedAt": o.FinishedAt.UTC().Format(time.RFC3339Nano),
	}
	if o.Summary != "" {
		req["summary"] = o.Summary
	}
	if len(o.Workloads) > 0 {
		workloads, err := applyWorkloadsStruct(o.Workloads)
		if err != nil {
			return release.Apply{}, false, err
		}
		req["workloads"] = workloads
	}
	rows := make([]map[string]any, 0, len(observed))
	for _, obj := range observed {
		row := map[string]any{"kind": obj.Kind, "name": obj.Name, "hash": obj.Hash}
		if obj.Cluster != "" {
			row["cluster"] = obj.Cluster
		}
		if obj.APIVersion != "" {
			row["apiVersion"] = obj.APIVersion
		}
		if obj.Namespace != "" {
			row["namespace"] = obj.Namespace
		}
		rows = append(rows, row)
	}
	req["observedObjects"] = rows
	var resp struct {
		Apply   wireApply `json:"apply"`
		Created bool      `json:"created"`
	}
	if err := c.client.Call(ctx, procFinishApply, req, &resp); err != nil {
		return release.Apply{}, false, bundlesUnsupported(err)
	}
	a, _, err := applyFromWire(env, resp.Apply)
	if err != nil {
		return release.Apply{}, false, err
	}
	return a, resp.Created, nil
}

// applyWorkloadsStruct renders the per-resource results as the generic object
// a google.protobuf.Struct holds. It goes through release's own JSON so there
// is exactly one spelling of ApplyWorkload on the wire.
func applyWorkloadsStruct(in []release.ApplyWorkload) (map[string]any, error) {
	raw, err := json.Marshal(map[string]any{"workloads": in})
	if err != nil {
		return nil, fmt.Errorf("apply outcome workloads: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("apply outcome workloads: %w", err)
	}
	return out, nil
}

// ApplyRecord is one apply as a READER sees it: the attempt, its outcome if
// one arrived, and the state derived from both plus the clock.
type ApplyRecord struct {
	Apply   release.Apply
	Outcome *release.ApplyOutcome
	// State is release.DeriveApplyState's answer. Carried rather than
	// recomputed by each caller so "abandoned" is drawn at one place.
	State release.ApplyState
}

// ListApplies reads an env's applies, newest first, keyset-paged like
// ListPromotions. beforeApplyID is the page cursor, "" for the first page.
func (c hostedApplyClient) ListApplies(ctx context.Context, env, environmentID, beforeApplyID string, limit int, now time.Time) ([]ApplyRecord, error) {
	if limit <= 0 || limit > hostedApplyListLimit {
		limit = hostedApplyListLimit
	}
	req := map[string]any{"environmentId": environmentID, "limit": limit}
	if beforeApplyID != "" {
		req["beforeApplyId"] = beforeApplyID
	}
	var resp struct {
		Applies []wireApply `json:"applies"`
	}
	if err := c.client.Call(ctx, procListApplies, req, &resp); err != nil {
		return nil, bundlesUnsupported(err)
	}
	out := make([]ApplyRecord, 0, len(resp.Applies))
	for _, w := range resp.Applies {
		a, outcome, err := applyFromWire(env, w)
		if err != nil {
			return nil, err
		}
		out = append(out, ApplyRecord{
			Apply: a, Outcome: outcome,
			State: release.DeriveApplyState(a, outcome, now),
		})
	}
	return out, nil
}
