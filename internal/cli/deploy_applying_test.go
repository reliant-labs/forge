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
