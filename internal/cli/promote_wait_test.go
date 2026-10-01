package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Tests for `promote --wait` / `--deploy` (control-plane
// docs/design/hosted-deploy-primitives.md §3.2, task F3).
//
// These run the REAL promote through the real hostedStore against
// fakeDeployService, so the promotion the wait is handed is the one the
// server actually wrote — not a value the test chose.

// capturedWait records the options followPromote handed the wait, and answers
// with a stated outcome. Installed over runPromoteWait, which exists for
// exactly this: the fact under test is WHICH promotion the wait was scoped
// to, and that is only observable here.
type capturedWait struct {
	calls []envWaitOptions
	env   []string
	err   error
}

func (c *capturedWait) install(t *testing.T) {
	t.Helper()
	prev := runPromoteWait
	runPromoteWait = func(_ context.Context, env string, opts envWaitOptions) error {
		c.env = append(c.env, env)
		c.calls = append(c.calls, opts)
		return c.err
	}
	t.Cleanup(func() { runPromoteWait = prev })
}

// TestPromoteWait_WaitsOnThePromotionTheWriteReturned is the headline, and
// the whole reason --wait is not just "promote; env wait".
//
// If the wait re-read the env's CURRENT promotion, a hotfix landing in the
// seconds after this promote would silently become the thing being waited on
// — and the pipeline would report its own release healthy on the strength of
// somebody else's. So the id comes from plan.Recorded, the entry the ledger
// returned.
func TestPromoteWait_WaitsOnThePromotionTheWriteReturned(t *testing.T) {
	var wait capturedWait
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1")
	if _, err := runHostedPromote(t, store, "v2", promoteOptions{
		Follow: promoteFollowOptions{Wait: true},
	}); err != nil {
		t.Fatalf("promote --wait: %v", err)
	}
	written, _, _ := store.Current(context.Background(), "prod")
	if written.Release != "v2" {
		t.Fatalf("the promote did not land: prod is on %s", written.Release)
	}
	if len(wait.calls) != 1 {
		t.Fatalf("the wait ran %d time(s), want 1", len(wait.calls))
	}
	if got := wait.calls[0].PromotionID; got != written.ID {
		t.Fatalf("the wait was scoped to %q, want the promotion the write RETURNED (%q)", got, written.ID)
	}
	if wait.env[0] != "prod" {
		t.Errorf("the wait ran against env %q", wait.env[0])
	}
}

// A promote the ledger treated as an idempotent NO-OP still waits — on the
// EXISTING promotion. A CI retry whose first attempt landed and whose wait
// then timed out must be able to re-run the whole step and still gate on its
// own release.
func TestPromoteWait_NoOpRetryWaitsOnTheExistingPromotion(t *testing.T) {
	var wait capturedWait
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1", "v2")
	existing, _, _ := store.Current(context.Background(), "prod")

	if _, err := runHostedPromote(t, store, "v2", promoteOptions{
		Follow: promoteFollowOptions{Wait: true},
	}); err != nil {
		t.Fatalf("re-promoting the release prod already runs must be a no-op that still waits: %v", err)
	}
	if len(wait.calls) != 1 {
		t.Fatalf("a no-op promote must still wait, ran %d time(s)", len(wait.calls))
	}
	if got := wait.calls[0].PromotionID; got != existing.ID {
		t.Fatalf("the wait was scoped to %q, want the existing promotion %q", got, existing.ID)
	}
}

// The wait's exit code IS the promote's: a promote whose release rolled out
// degraded must not report success because the pointer moved.
func TestPromoteWait_TheWaitsExitCodeIsThePromotes(t *testing.T) {
	wait := capturedWait{err: &exitCodeError{code: exitTimedOut, msg: "still progressing"}}
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1")
	_, err := runHostedPromote(t, store, "v2", promoteOptions{Follow: promoteFollowOptions{Wait: true}})
	if got := exitCodeForError(err); got != exitTimedOut {
		t.Fatalf("exit code = %d, want the wait's %d (%v)", got, exitTimedOut, err)
	}
	// And the promote still LANDED. The gate reporting red does not undo
	// the pointer — recovery is roll forward, and a promote silently
	// reverted by its own gate would be the worst of both.
	if cur, _, _ := store.Current(context.Background(), "prod"); cur.Release != "v2" {
		t.Fatalf("a failed wait must not revert the promote: prod is on %s", cur.Release)
	}
}

// TestPromoteWait_FailedWaitStillEmitsTheJSONDocument is the F2/F3 seam, and
// it is the case that matters MOST for a pipeline.
//
// A wait that degrades, times out or is superseded arrives in runPromote as a
// post-plan error — the same shape a FAILED WRITE has. F2's guard was written
// when a failed write was the only such error, and a failed write has no
// document worth reading because nothing was recorded. A failed WAIT is the
// opposite: the promote was applied, `recorded.id` exists, and that id is
// precisely what the next step needs.
//
// F6's forge-promote action does `promote --json > promote.json` and then
// `jq -r .recorded.id` to record gates against the promotion. A failed
// rollout is exactly when that evidence must be recorded, so dropping the
// document leaves `gate record` with no promotion id — the gate evidence for
// the bad release is the evidence that goes missing.
//
// So: the document is emitted whenever the plan was APPLIED or refused, and
// its exit_code is the wait's.
func TestPromoteWait_FailedWaitStillEmitsTheJSONDocument(t *testing.T) {
	wait := capturedWait{err: &exitCodeError{code: exitTimedOut, msg: "still progressing"}}
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1")
	out, err := runHostedPromote(t, store, "v2", promoteOptions{
		JSON: true, Follow: promoteFollowOptions{Wait: true},
	})
	if got := exitCodeForError(err); got != exitTimedOut {
		t.Fatalf("exit code = %d, want the wait's %d (%v)", got, exitTimedOut, err)
	}

	var doc map[string]any
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("a failed wait after an APPLIED promote must still emit the document: %v\nstdout=%q", jerr, out)
	}
	// applied:true is the fact that distinguishes this from a refusal.
	// The pointer DID move; only the gate went red.
	if doc["applied"] != true {
		t.Errorf("applied = %v, want true — the promote landed", doc["applied"])
	}
	// The envelope carries the WAIT's code, so the document and the
	// process status still agree.
	if doc["ok"] != false || doc["exit_code"] != float64(exitTimedOut) {
		t.Errorf("ok/exit_code = %v/%v, want false/%d", doc["ok"], doc["exit_code"], exitTimedOut)
	}
	// recorded.id is what F6's action pipes into `gate record`.
	recorded, ok := doc["recorded"].(map[string]any)
	if !ok {
		t.Fatalf("`recorded` must be present — F6 reads recorded.id to record gates against this promotion:\n%s", out)
	}
	written, _, _ := store.Current(context.Background(), "prod")
	if recorded["id"] != written.ID {
		t.Errorf("recorded.id = %v, want the promotion that landed (%q)", recorded["id"], written.ID)
	}
	if _, present := doc["refusal"]; present {
		t.Errorf("a failed WAIT is not a refusal — nothing declined the write:\n%s", out)
	}
}

// The same seam in TEXT mode: the change set still prints, so a human sees
// what moved before they read why the gate went red.
func TestPromoteWait_FailedWaitStillRendersTheChangeSet(t *testing.T) {
	wait := capturedWait{err: &exitCodeError{code: exitWrong, msg: "api: CrashLoopBackOff"}}
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1")
	out, err := runHostedPromote(t, store, "v2", promoteOptions{Follow: promoteFollowOptions{Wait: true}})
	if got := exitCodeForError(err); got != exitWrong {
		t.Fatalf("exit code = %d, want %d (%v)", got, exitWrong, err)
	}
	if !strings.Contains(out, "v2") {
		t.Errorf("the change set must still render after a failed wait, got:\n%s", out)
	}
}

// --timeout and --fail-fast reach the wait rather than being accepted and
// ignored.
func TestPromoteWait_TuningFlagsReachTheWait(t *testing.T) {
	var wait capturedWait
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1")
	if _, err := runHostedPromote(t, store, "v2", promoteOptions{
		Follow: promoteFollowOptions{Wait: true, Timeout: 90 * time.Second, FailFast: true},
	}); err != nil {
		t.Fatal(err)
	}
	got := wait.calls[0]
	if got.Timeout != 90*time.Second || !got.FailFast {
		t.Fatalf("timeout/fail-fast = %s/%v, want 90s/true", got.Timeout, got.FailFast)
	}
	// Without --deploy the non-converging fast refusal stays ARMED: there
	// is no client-side apply, so an env that converges nothing can only
	// time out, and the refusal is what says so immediately.
	if got.AllowNonConverging {
		t.Error("a plain --wait must keep the non-converging refusal armed")
	}
}

// TestPromoteWait_UnwiredCombinationsRefuseBeforeAnyWrite: a tuning flag
// without --wait does NOTHING, and silently doing nothing is the failure
// worth refusing — a pipeline that passed --fail-fast and got no gate would
// believe it had one. It refuses BEFORE the write, so the pointer does not
// move for a command that was going to fail anyway.
func TestPromoteWait_UnwiredCombinationsRefuseBeforeAnyWrite(t *testing.T) {
	cases := map[string]promoteFollowOptions{
		"--fail-fast without --wait": {FailFast: true},
		"--timeout without --wait":   {Timeout: time.Minute},
	}
	for name, follow := range cases {
		t.Run(name, func(t *testing.T) {
			fake, store := hostedPromoteFixture(t, "v1")
			before := fake.callCount(procPromote)
			_, err := runHostedPromote(t, store, "v2", promoteOptions{Follow: follow})
			if err == nil || !strings.Contains(err.Error(), "--wait") {
				t.Fatalf("%s must be refused naming --wait, got %v", name, err)
			}
			if n := fake.callCount(procPromote); n != before {
				t.Fatalf("%s refused AFTER writing: Promote called %d time(s)", name, n-before)
			}
		})
	}
}

// A plain promote — no follow-through flag — runs no wait at all. The
// primitive is unchanged: promote moves a pointer and ships nothing.
func TestPromoteWait_NoFollowThroughRunsNoWait(t *testing.T) {
	var wait capturedWait
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1")
	if _, err := runHostedPromote(t, store, "v2", promoteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(wait.calls) != 0 {
		t.Fatalf("a promote with no --wait ran the wait %d time(s)", len(wait.calls))
	}
}

// A REFUSED promote must not wait. There is nothing to wait on — the pointer
// did not move — and waiting would gate on whatever promotion is current,
// which belongs to whoever won the race.
func TestPromoteWait_RefusedPromoteDoesNotWait(t *testing.T) {
	var wait capturedWait
	wait.install(t)

	fake, store := hostedPromoteFixture(t, "v1")
	fake.pinned = true
	_, err := runHostedPromote(t, store, "v2", promoteOptions{Follow: promoteFollowOptions{Wait: true}})
	if got := exitCodeForError(err); got != exitRefused {
		t.Fatalf("exit code = %d, want %d (%v)", got, exitRefused, err)
	}
	if len(wait.calls) != 0 {
		t.Fatalf("a refused promote waited %d time(s)", len(wait.calls))
	}
}

// --plan writes nothing, so it waits for nothing either. A preview that
// blocked for fifteen minutes would not be a preview.
func TestPromoteWait_PlanDoesNotWait(t *testing.T) {
	var wait capturedWait
	wait.install(t)

	_, store := hostedPromoteFixture(t, "v1")
	if _, err := runHostedPromote(t, store, "v2", promoteOptions{
		DryRun: true, Follow: promoteFollowOptions{Wait: true},
	}); err != nil {
		t.Fatal(err)
	}
	if len(wait.calls) != 0 {
		t.Fatalf("--plan ran the wait %d time(s)", len(wait.calls))
	}
}

// validatePromoteFollow runs BEFORE the plan, which is what makes an invalid
// combination refuse the whole command rather than moving the pointer first.
func TestValidatePromoteFollow(t *testing.T) {
	cases := []struct {
		name    string
		in      promoteFollowOptions
		wantErr bool
	}{
		{"nothing", promoteFollowOptions{}, false},
		{"wait alone", promoteFollowOptions{Wait: true}, false},
		{"deploy alone", promoteFollowOptions{Deploy: true}, false},
		{"deploy and wait", promoteFollowOptions{Deploy: true, Wait: true}, false},
		{"wait with tuning", promoteFollowOptions{Wait: true, Timeout: time.Minute, FailFast: true}, false},
		{"fail-fast alone", promoteFollowOptions{FailFast: true}, true},
		{"timeout alone", promoteFollowOptions{Timeout: time.Minute}, true},
		{"negative timeout", promoteFollowOptions{Wait: true, Timeout: -time.Second}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePromoteFollow(tc.in); (err != nil) != tc.wantErr {
				t.Fatalf("validatePromoteFollow(%+v) = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}
