package cli

// Whether a promotion was APPLIED, as the ledger can say it.
//
// A promotion is a pointer, and the apply that realizes it is a second event
// that can fail. On 2026-10-07 prod's binding moved to a release whose apply
// then failed, and every reader — `env status`, `--history`, the next plan —
// presented that release as what prod ran. Nothing in the ledger said
// otherwise, because nothing had been written to say it.
//
// Both ledgers now record the apply's verdict on the promotion it realized:
//
//   - a HOSTED ledger as a recorded gate named "apply" (the same post-promote
//     evidence `forge gate record` writes), so every reader of the control
//     plane's ledger sees it, not just this machine;
//   - a FILE ledger as the machine ledger's apply record (beginApplyRecord),
//     which already existed and is read here.
//
// The readers below turn either into one fact: this promotion's latest apply
// FAILED, with what it said.

import (
	"context"
	"fmt"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// applyGateName is the recorded gate a client-side apply's verdict is filed
// under on a hosted ledger.
const applyGateName = "apply"

// recordApplyOutcome files the client-side apply's verdict on the promotion
// it realized, for a HOSTED ledger. Best-effort: the deploy's outcome is the
// apply's, and a ledger that cannot take the evidence must not change it — so
// a failure to record is printed, never returned.
//
// A deploy held on a person (queued) recorded nothing about the cluster half
// worth judging, so it records nothing here.
func recordApplyOutcome(ctx context.Context, env string, plan promotePlan, ledger envLedger, started time.Time, applyErr error, o promoteFollowOptions) {
	if !ledger.Hosted || plan.Recorded == nil || o.clientDeploy.dryRun || queuedOf(applyErr) != nil {
		return
	}
	finished := time.Now().UTC()
	startedUTC := started.UTC()
	gate := release.Gate{
		Name: applyGateName, Status: release.GateStatusPassed,
		Summary:   "applied from this machine",
		StartedAt: &startedUTC, FinishedAt: &finished,
	}
	if applyErr != nil {
		gate.Status = release.GateStatusFailed
		gate.Summary = oneLine(applyErr.Error(), 300)
	}
	backend, err := resolveGateBackend(ctx, env)
	if err == nil && backend.store == nil {
		err = fmt.Errorf("no gate store for %s", env)
	}
	if err == nil {
		_, _, err = backend.store.recordGate(ctx, plan.Recorded.ID, gate)
	}
	if err != nil {
		o.notice("[deploy] Note: the apply's %s verdict could not be recorded on promotion %s: %v\n",
			gate.Status, plan.Recorded.ID, err)
	}
}

// promotionApply is what the ledger says about whether a promotion was
// applied. The zero value is "nothing recorded", which is not a failure:
// promotions written before this evidence existed, and every env the control
// plane converges itself, carry none.
type promotionApply struct {
	// Failed is true when the LATEST recorded apply of the promotion did not
	// succeed (failed, or timed out).
	Failed bool
	// Summary is what the failed apply said.
	Summary string
}

// applyOf reads a promotion's apply evidence: the latest "apply" gate on a
// hosted promotion, else the latest machine-ledger apply record for it.
func applyOf(projectDir, env string, p release.Promotion) promotionApply {
	if g, ok := latestApplyGate(p); ok {
		return promotionApply{Failed: g.Status == release.GateStatusFailed, Summary: g.Summary}
	}
	store, err := openMachineLedger(projectDir)
	if err != nil {
		return promotionApply{}
	}
	records, err := store.Applies(env)
	if err != nil {
		return promotionApply{}
	}
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r.Apply.PromotionID != p.ID || r.Outcome == nil {
			continue
		}
		return promotionApply{Failed: r.Outcome.Status != release.ApplySucceeded, Summary: r.Outcome.Summary}
	}
	return promotionApply{}
}

// latestApplyGate is the newest "apply" gate recorded on p, in append order.
func latestApplyGate(p release.Promotion) (release.Gate, bool) {
	for i := len(p.RecordedGates) - 1; i >= 0; i-- {
		if p.RecordedGates[i].Name == applyGateName {
			return p.RecordedGates[i], true
		}
	}
	return release.Gate{}, false
}

// lastAppliedPromotion is the newest promotion of env before the current one
// whose apply did not fail — the ledger's best answer to "what is actually
// running" when the current binding's apply failed. Reads at most one page of
// history; false when there is none, or the ledger cannot list history.
func lastAppliedPromotion(ctx context.Context, bindings bindingStore, projectDir, env, currentID string) (release.Promotion, bool) {
	reader, ok := bindings.(bindingHistoryReader)
	if !ok {
		return release.Promotion{}, false
	}
	page, err := reader.HistoryPage(ctx, env, historyQuery{Limit: 50})
	if err != nil {
		return release.Promotion{}, false
	}
	for _, p := range page.Promotions {
		if p.ID == currentID {
			continue
		}
		if !applyOf(projectDir, env, p).Failed {
			return p, true
		}
	}
	return release.Promotion{}, false
}
