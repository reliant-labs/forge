package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

func pendingApply() *deploytarget.ApplyPendingError {
	return &deploytarget.ApplyPendingError{
		Env:     "prod",
		Bundle:  "sha256:badfa995129f7cd83b1db0a44b80733f5bd29a130705cafb6e9962f8eda40721",
		Current: "sha256:4750cbaa564172e82a0b51ef319a08ddd6a48ad09eab602d1e0ce3f55c95e3f6",
		Waited:  10 * time.Minute,
	}
}

// A publish the platform had not reported applied is exit 8 (still in
// progress), not exit 1, and the wrap keeps the provider's error findable.
func TestDeployApplying_IsExitEightAndKeepsTheChain(t *testing.T) {
	inner := fmt.Errorf("deploy hosted: %w", pendingApply())
	err := asApplying(inner)
	if got := exitCodeForError(err); got != exitTimedOut {
		t.Fatalf("exit code = %d, want %d (still in progress)", got, exitTimedOut)
	}
	if applyingOf(err) == nil {
		t.Fatal("the wrapped error lost the *ApplyPendingError")
	}
	if again := asApplying(err); again != err {
		t.Error("asApplying wrapped an already-wrapped error a second time")
	}
	if msg := err.Error(); strings.Contains(msg, "TIMED OUT") || !strings.Contains(msg, "STILL APPLYING, not failed") {
		t.Errorf("message = %q, want still applying and no TIMED OUT", msg)
	}
	other := errors.New("boom")
	if asApplying(other) != other {
		t.Error("asApplying changed an unrelated error")
	}
}

// The deploy's headline says the release is published and still applying,
// names the command that keeps watching, and says neither FAILED nor re-plan.
func TestRenderPromotePlan_StillApplyingIsNotAFailure(t *testing.T) {
	err := asApplying(pendingApply())
	plan := promotePlan{Env: "prod", Applied: true, FollowError: err.Error(),
		Target:   promotePlanTarget{Release: "v2"},
		Shipped:  []string{"[hosted] published to the control plane: api, web"},
		Applying: applyingDocument(pendingApply())}
	plan.followed = true
	var out bytes.Buffer
	renderPromotePlanText(&out, plan)
	got := out.String()
	for _, want := range []string{"PUBLISHED, STILL APPLYING", "Nothing has failed", "forge env status prod --wait",
		"shipped   [hosted] published", "sha256:badfa995129f", "sha256:4750cbaa5641"} {
		if !strings.Contains(got, want) {
			t.Errorf("the plan does not say %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"FAILED", "TIMED OUT", "PARTLY", "--plan-only"} {
		if strings.Contains(got, bad) {
			t.Errorf("a still-applying deploy rendered %q:\n%s", bad, got)
		}
	}
}

// A still-applying publish is recorded as shipped, never as "failed part-way".
func TestShipLog_StillApplyingIsShipped(t *testing.T) {
	var log shipLog
	_ = log.run([]string{"[hosted] published"}, func() error { return fmt.Errorf("x: %w", pendingApply()) })
	if len(log.Shipped()) != 1 || len(log.Partial()) != 0 {
		t.Fatalf("shipped %v partial %v, want the publish shipped", log.Shipped(), log.Partial())
	}
}

// A hosted stage still applying with a later machine-owned stage left (a
// local apply, a bucket/Firebase frontend) is an INCOMPLETE deploy: exit 1,
// naming what did not run, never "still applying" (review of #1499).
func TestStageStopError_PendingHostedWithLaterStagesIsIncomplete(t *testing.T) {
	pending := fmt.Errorf("deploy hosted: %w", pendingApply())
	work := map[deployStage][]string{
		stageLocal:     {"[k8s-cluster] cluster=prod ns=app"},
		stageFrontends: {"frontend web"},
	}
	err := stageStopError("prod", pending, []deployStage{stageFrontends}, work)
	if applyingOf(err) != nil || applyingOf(asApplying(err)) != nil {
		t.Fatalf("an incomplete deploy was classified as still applying: %v", err)
	}
	if got := exitCodeForError(asApplying(err)); got != exitWrong {
		t.Errorf("exit code = %d, want %d", got, exitWrong)
	}
	if msg := err.Error(); !strings.Contains(msg, "INCOMPLETE") || !strings.Contains(msg, "frontend web") ||
		!strings.Contains(msg, "deploy again") {
		t.Errorf("message = %q, want the stages that did not run and how to ship them", msg)
	}
	var log shipLog
	_ = log.run([]string{"[hosted] published"}, func() error { return pending })
	if len(log.Shipped()) != 1 {
		t.Errorf("the hosted publish itself still shipped: %v", log.Shipped())
	}

	// Nothing left for this machine: still applying.
	if err := stageStopError("prod", pending, []deployStage{stageFrontends}, map[deployStage][]string{}); applyingOf(err) == nil {
		t.Errorf("a pending hosted stage with nothing after it must stay still-applying: %v", err)
	}
	if err := stageStopError("prod", pending, nil, work); applyingOf(err) == nil {
		t.Errorf("a pending hosted stage that ran last must stay still-applying: %v", err)
	}
	// Any other error is untouched.
	other := errors.New("boom")
	if stageStopError("prod", other, []deployStage{stageFrontends}, work) != other {
		t.Error("a non-pending error was rewritten")
	}
}
