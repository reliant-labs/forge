package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
)

// Tests for `forge env wait` (control-plane
// docs/design/hosted-deploy-primitives.md §3.2, task F3).
//
// Every case SCRIPTS A PHASE SEQUENCE and asserts the exit code, because the
// exit codes are the contract a pipeline branches on and each one means
// something a pipeline reacts to differently. A phase sequence is also the
// only way to pin the behaviours that are about TIME rather than about one
// observation: that a transient DEGRADED is tolerated by default, that
// --fail-fast is not, and that a wait which never finishes is 5 and not 1.
//
// The fake answers GetRollout in control-plane's wire form (proto3 JSON,
// enum value names) so a typo in forge's wire structs fails the test rather
// than moving with it.

// fakeRolloutService scripts GetRollout: the nth call gets phases[n], and
// the last entry repeats forever so a "never finishes" case can run to its
// deadline without the script running out.
type fakeRolloutService struct {
	mu     sync.Mutex
	phases []string
	calls  int
	bodies []map[string]any

	// promotion is the promotion every answer carries.
	promotionID string
	release     string
	// converges mirrors DeployEnvironment.converges_promotions.
	converges bool
	// workloads, when set, replaces the default one-workload set. The
	// phase of each is the scripted env phase unless stated here.
	workloads []wireWorkloadRollout
	unpinned  []wireWorkloadRollout
	// err, when set, is returned instead of an answer.
	err error
}

func newFakeRollout(phases ...string) *fakeRolloutService {
	return &fakeRolloutService{phases: phases, promotionID: "promo-1", release: "v2", converges: true}
}

func (f *fakeRolloutService) Call(_ context.Context, procedure string, req, out any) error {
	raw, _ := json.Marshal(req)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, body)
	if procedure != procGetRollout {
		return fmt.Errorf("unexpected procedure %s", procedure)
	}
	phase := f.phases[len(f.phases)-1]
	if f.calls < len(f.phases) {
		phase = f.phases[f.calls]
	}
	// Counted BEFORE the error branch: a test asserting "this failure was
	// not retried" is asserting on the call count, and skipping the
	// increment on the error path would make that assertion vacuously
	// true however many times the loop actually called.
	f.calls++
	if f.err != nil {
		return f.err
	}

	workloads := f.workloads
	if workloads == nil {
		workloads = []wireWorkloadRollout{{
			DeploymentID: "dep-api", Name: "api", Artifact: "ghcr.io/acme/api",
			PinnedDigest: sha("2"), ObservedDigest: sha("2"), Phase: phase,
			ObservedState: "DEPLOY_OBSERVED_STATE_READY", Verdict: "DEPLOY_VERDICT_CONVERGING",
			UpdatedReplicas: 3, DesiredReplicas: 3,
		}}
		if phase == wireRolloutPhaseDegraded {
			workloads[0].ObservedState = "DEPLOY_OBSERVED_STATE_DEGRADED"
			workloads[0].LastError = "CrashLoopBackOff: exit 1"
			workloads[0].UpdatedReplicas = 0
		}
	}
	rollout := wireRollout{
		Promotion: wirePromotion{
			ID: f.promotionID, EnvironmentID: "env-prod-uuid", ReleaseVersion: f.release,
			Kind: wireKindPromote, CreatedAt: parseFixtureTime("2026-09-23T01:00:00Z"),
		},
		Phase: phase, Workloads: workloads, Unpinned: f.unpinned,
		StabilityWindowMS: 120000, ConvergesPromotions: f.converges,
		Reason: "api: " + rolloutPhaseName(phase),
	}
	payload, err := json.Marshal(map[string]any{"rollout": rollout})
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, out)
}

func (f *fakeRolloutService) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRolloutService) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

// waitOpts is the common option set: the fake as the client, a fast poll, and
// a budget short enough that the "never finishes" cases are instant.
func waitOpts(fake *fakeRolloutService) envWaitOptions {
	return envWaitOptions{
		Target: func(context.Context, string) (waitTarget, error) {
			return waitTarget{Client: fake, EnvironmentID: "env-prod-uuid", Endpoint: "https://cp.example"}, nil
		},
		Interval: time.Millisecond,
		Timeout:  40 * time.Millisecond,
	}
}

// TestEnvWait_ExitCodes is the headline: §3.A's table, end to end, for every
// terminal phase. The three non-obvious rows are the point — a rollout still
// progressing at the deadline is 5 and NOT 1 (retry the wait, the release was
// never judged bad), superseded is its own 6, and unknown is 2 because "we
// cannot see it" is not permission.
func TestEnvWait_ExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		phases []string
		want   int
	}{
		{"succeeded", []string{wireRolloutPhaseSucceeded}, exitOK},
		{"progressing then succeeded", []string{
			wireRolloutPhasePending, wireRolloutPhaseProgressing,
			wireRolloutPhaseStabilizing, wireRolloutPhaseSucceeded}, exitOK},
		{"degraded at the deadline", []string{wireRolloutPhaseDegraded}, exitWrong},
		{"superseded", []string{wireRolloutPhaseSuperseded}, exitSuperseded},
		{"unknown at the deadline", []string{wireRolloutPhaseUnknown}, exitUndetermined},
		{"still pending at the deadline", []string{wireRolloutPhasePending}, exitTimedOut},
		{"still progressing at the deadline", []string{wireRolloutPhaseProgressing}, exitTimedOut},
		{"still stabilizing at the deadline", []string{wireRolloutPhaseStabilizing}, exitTimedOut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeRollout(tc.phases...)
			var err error
			captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", waitOpts(fake)) })
			if got := exitCodeForError(err); got != tc.want {
				t.Fatalf("exit code = %d, want %d (%v)", got, tc.want, err)
			}
		})
	}
}

// A STABILIZING rollout is not a failure, and a wait that gave up on it must
// say "retry the wait", not "the release is bad". The timeout message is what
// a human reads off a red job, so it carries the distinction the exit code
// carries.
func TestEnvWait_TimeoutSaysRetryTheWaitNotRePromote(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseStabilizing)
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", waitOpts(fake)) })
	if got := exitCodeForError(err); got != exitTimedOut {
		t.Fatalf("exit code = %d, want %d", got, exitTimedOut)
	}
	for _, want := range []string{"still stabilizing", "Retry the wait", "do not re-promote", "promo-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the timeout message must say %q, got:\n%s", want, err)
		}
	}
}

// TestEnvWait_TransientDegradedIsTolerated is the Q4 default, and the reason
// it is the default: a pod that crash-loops once on a cold start and then
// settles is ordinary, and failing on the first degraded observation would
// make this gate flaky in exactly the way that gets a gate switched off.
//
// Mutation check: making DEGRADED terminal (the --fail-fast behaviour) turns
// this case red while the next one still passes, so the two pin opposite
// halves of the ruling.
func TestEnvWait_TransientDegradedIsTolerated(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseDegraded, wireRolloutPhaseProgressing, wireRolloutPhaseSucceeded)
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", waitOpts(fake)) })
	if err != nil {
		t.Fatalf("a degraded observation that then settles must still succeed by default, got %v", err)
	}
}

// --fail-fast is the opposite trade: exit on the FIRST degraded observation,
// without waiting out the budget, for a pipeline that prefers speed to
// tolerance.
func TestEnvWait_FailFastExitsOnTheFirstDegraded(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseDegraded, wireRolloutPhaseSucceeded)
	opts := waitOpts(fake)
	opts.FailFast = true
	opts.Timeout = time.Minute // generous: fail-fast must not need the deadline
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if got := exitCodeForError(err); got != exitWrong {
		t.Fatalf("--fail-fast on a degraded rollout must exit %d, got %d (%v)", exitWrong, got, err)
	}
	if n := fake.callCount(); n != 1 {
		t.Errorf("--fail-fast polled %d times; it must stop at the first degraded observation", n)
	}
	// The failure NAMES the workload: "prod is degraded" sends someone to
	// a dashboard, the replica counts and the container's own error are a
	// next step.
	for _, want := range []string{"api", "CrashLoopBackOff", "0/3 updated replicas"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the degraded report must say %q, got:\n%s", want, err)
		}
	}
}

// TestEnvWait_NonConvergingEnvRefusesFast: when nothing on the control plane
// will apply the promotion, waiting can only ever time out — and a timeout
// would blame the release for a missing converger. So it refuses immediately,
// with the command that would actually ship it.
func TestEnvWait_NonConvergingEnvRefusesFast(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhasePending)
	fake.converges = false
	opts := waitOpts(fake)
	opts.Timeout = time.Minute
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if got := exitCodeForError(err); got != exitUndetermined {
		t.Fatalf("a non-converging env must exit %d immediately, got %d (%v)", exitUndetermined, got, err)
	}
	if n := fake.callCount(); n != 1 {
		t.Errorf("the refusal must be immediate, polled %d times", n)
	}
	for _, want := range []string{"does not converge promotions", "forge env deploy prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say %q, got:\n%s", want, err)
		}
	}
}

// --deploy's wait has just applied the pins from this machine, so the
// non-converging refusal must NOT fire: the thing it protects against cannot
// happen when the client did the converging itself.
func TestEnvWait_AllowNonConvergingAdmitsTheDeployBridge(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseProgressing, wireRolloutPhaseSucceeded)
	fake.converges = false
	opts := waitOpts(fake)
	opts.AllowNonConverging = true
	opts.Timeout = time.Minute
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if err != nil {
		t.Fatalf("with AllowNonConverging the wait must proceed, got %v", err)
	}
}

// TestEnvWait_ReleaseMismatchIsSupersededNotATimeout: `--release v6` against
// an env that has moved on to v7 must say so AT ONCE. Without this check it
// would wait out the whole budget and then report 5, which reads as "v6 is
// still rolling out" for a release that was never going to roll out again.
func TestEnvWait_ReleaseMismatchIsSupersededNotATimeout(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseSucceeded)
	fake.release = "v7"
	opts := waitOpts(fake)
	opts.Release = "v6"
	opts.Timeout = time.Minute
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if got := exitCodeForError(err); got != exitSuperseded {
		t.Fatalf("a --release that the current promotion does not bind must exit %d, got %d (%v)", exitSuperseded, got, err)
	}
	if !strings.Contains(err.Error(), "overtaken") {
		t.Errorf("the message must say it was overtaken, got: %v", err)
	}
	// And the matching release proceeds, so the check is not just always
	// refusing.
	ok := newFakeRollout(wireRolloutPhaseSucceeded)
	opts2 := waitOpts(ok)
	opts2.Release = "v2"
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts2) })
	if err != nil {
		t.Fatalf("--release naming what the promotion binds must proceed, got %v", err)
	}
}

// TestEnvWait_WaitsOnTheRequestedPromotion: --promotion rides to the wire, so
// a CI retry of a timed-out wait continues against the SAME release rather
// than silently adopting whatever is current now.
func TestEnvWait_WaitsOnTheRequestedPromotion(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseSucceeded)
	opts := waitOpts(fake)
	opts.PromotionID = "promo-captured"
	captureStdout(t, func() { _ = runEnvWait(context.Background(), "prod", opts) })
	body := fake.lastBody()
	if body["promotionId"] != "promo-captured" {
		t.Fatalf("promotionId = %v, want the requested promo-captured (body %v)", body["promotionId"], body)
	}

	// With no --promotion the field is OMITTED, not sent empty: absent
	// means "the env's current promotion" server-side, and an empty
	// string would be a claim about a promotion with no id.
	plain := newFakeRollout(wireRolloutPhaseSucceeded)
	captureStdout(t, func() { _ = runEnvWait(context.Background(), "prod", waitOpts(plain)) })
	if _, set := plain.lastBody()["promotionId"]; set {
		t.Errorf("promotionId must be omitted when none was asked for, got %v", plain.lastBody())
	}
}

// TestEnvWait_UnpinnedWorkloadsDoNotGate is owner ruling Q4's "databases are
// non-blocking": a managed database cannot carry a release artifact, so
// letting a degraded one fail the gate would make every release hostage to
// something the release did not change — and the red would be
// indistinguishable from the release being bad. It is REPORTED either way.
func TestEnvWait_UnpinnedWorkloadsDoNotGate(t *testing.T) {
	degradedDB := []wireWorkloadRollout{{
		DeploymentID: "dep-orders", Name: "orders",
		ObservedState: "DEPLOY_OBSERVED_STATE_DEGRADED", Phase: wireRolloutPhaseDegraded,
		LastError: "disk pressure",
	}}

	fake := newFakeRollout(wireRolloutPhaseSucceeded)
	fake.unpinned = degradedDB
	var out string
	var err error
	out = captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", waitOpts(fake)) })
	if err != nil {
		t.Fatalf("a degraded UNPINNED workload must not fail the gate, got %v", err)
	}
	// Reported, and under its own heading: "the release is healthy and
	// the database is not" has to be something the output can say.
	for _, want := range []string{"orders", "does not gate"} {
		if !strings.Contains(out, want) {
			t.Errorf("the unpinned workload must still be reported (%q), got:\n%s", want, out)
		}
	}

	strict := newFakeRollout(wireRolloutPhaseSucceeded)
	strict.unpinned = degradedDB
	opts := waitOpts(strict)
	opts.IncludeUnpinned = true
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if got := exitCodeForError(err); got != exitWrong {
		t.Fatalf("--include-unpinned must let a degraded database fail the gate (exit %d), got %d (%v)", exitWrong, got, err)
	}
}

// TestEnvWait_JSONShape: one document, the envelope agreeing with the process
// status, and every field §3.2 names.
func TestEnvWait_JSONShape(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseProgressing, wireRolloutPhaseDegraded)
	opts := waitOpts(fake)
	opts.JSON = true
	opts.FailFast = true
	var err error
	out := captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })

	var doc map[string]any
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("stdout must be exactly one JSON document: %v\n%s", jerr, out)
	}
	if doc["ok"] != false || doc["exit_code"] != float64(exitWrong) {
		t.Errorf("ok/exit_code = %v/%v, want false/%d", doc["ok"], doc["exit_code"], exitWrong)
	}
	if got := exitCodeForError(err); got != exitWrong {
		t.Errorf("the document's exit_code must be the process's: process %d", got)
	}
	if doc["env"] != "prod" || doc["phase"] != "degraded" {
		t.Errorf("env/phase = %v/%v", doc["env"], doc["phase"])
	}
	if doc["converges_promotions"] != true || doc["stability_window_ms"] != float64(120000) {
		t.Errorf("converges_promotions/stability_window_ms = %v/%v", doc["converges_promotions"], doc["stability_window_ms"])
	}
	promotion, _ := doc["promotion"].(map[string]any)
	if promotion["id"] != "promo-1" || promotion["release"] != "v2" {
		t.Errorf("promotion = %v, want promo-1/v2", doc["promotion"])
	}
	workloads, _ := doc["workloads"].([]any)
	if len(workloads) != 1 {
		t.Fatalf("workloads = %v", doc["workloads"])
	}
	w, _ := workloads[0].(map[string]any)
	if w["name"] != "api" || w["phase"] != "degraded" || w["last_error"] != "CrashLoopBackOff: exit 1" {
		t.Errorf("workload = %v", w)
	}
	// The replica pair is the H1 evidence, and it must survive as NUMBERS
	// even when updated is 0 — the whole point is distinguishing "0 of 3
	// updated" from "not reported".
	if w["updated_replicas"] != float64(0) || w["desired_replicas"] != float64(3) {
		t.Errorf("updated/desired replicas = %v/%v, want 0/3", w["updated_replicas"], w["desired_replicas"])
	}
	// The TRANSITION LIST: progressing → degraded. A rollout that flapped
	// tells a different story from one that was degraded from the start,
	// and only the sequence can distinguish them.
	transitions, _ := doc["transitions"].([]any)
	if len(transitions) != 2 {
		t.Fatalf("transitions = %v, want progressing then degraded", doc["transitions"])
	}
	first, _ := transitions[0].(map[string]any)
	second, _ := transitions[1].(map[string]any)
	if first["phase"] != "progressing" || second["phase"] != "degraded" {
		t.Errorf("transitions = %v/%v", first["phase"], second["phase"])
	}
}

// A control plane that does not serve GetRollout is exit 2 with the hint —
// not a fifteen-minute wait to discover it never will.
func TestEnvWait_UnimplementedIsAFastExitTwo(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseSucceeded)
	fake.err = wireCodeError(cloud.CodeUnimplemented)
	opts := waitOpts(fake)
	opts.Timeout = time.Minute
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if got := exitCodeForError(err); got != exitUndetermined {
		t.Fatalf("an unimplemented GetRollout must exit %d, got %d (%v)", exitUndetermined, got, err)
	}
	if n := fake.callCount(); n != 1 {
		t.Errorf("polled %d times; unimplemented will not become implemented within the budget", n)
	}
	if !strings.Contains(err.Error(), "forge env verify") {
		t.Errorf("the message must name the fallback verb, got: %v", err)
	}
}

// A promotion id this control plane does not hold is a CONFLICT (3), not a
// wait that hangs: the caller named something that is not there.
func TestEnvWait_UnknownPromotionIsAConflict(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseSucceeded)
	fake.err = wireCodeError(cloud.CodeNotFound)
	opts := waitOpts(fake)
	opts.PromotionID = "promo-nope"
	opts.Timeout = time.Minute
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if got := exitCodeForError(err); got != exitConflict {
		t.Fatalf("an unknown promotion must exit %d, got %d (%v)", exitConflict, got, err)
	}
}

// A transient read failure is RETRIED while there is budget — a control plane
// restarting mid-rollout must not fail a release — and only becomes exit 2
// when the budget runs out.
func TestEnvWait_TransientReadFailureIsRetriedThenExitsTwo(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseSucceeded)
	fake.err = errors.New("connection reset by peer")
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", waitOpts(fake)) })
	if got := exitCodeForError(err); got != exitUndetermined {
		t.Fatalf("an unreadable control plane must exit %d (could not look), got %d (%v)", exitUndetermined, got, err)
	}
	if n := fake.callCount(); n < 2 {
		t.Errorf("a transient failure must be retried, polled only %d time(s)", n)
	}
}

// --stable-for holds AFTER the server's own window, and an UNBROKEN run is
// what it measures: a rollout that leaves succeeded restarts the clock,
// because a release that flapped out of healthy was not stable.
func TestEnvWait_StableForRequiresAnUnbrokenRun(t *testing.T) {
	fake := newFakeRollout(wireRolloutPhaseSucceeded)
	opts := waitOpts(fake)
	opts.StableFor = 25 * time.Millisecond
	opts.Timeout = time.Minute
	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", opts) })
	if err != nil {
		t.Fatalf("a rollout that stays succeeded must pass --stable-for, got %v", err)
	}
	if n := fake.callCount(); n < 2 {
		t.Errorf("--stable-for must keep polling past the first succeeded read, polled %d time(s)", n)
	}
}

// TestEnvWait_SelfManagedEnvCannotBeWaitedOn exercises the PRODUCTION
// resolution (no Target seam): an env whose KCL declares no control plane has
// no server-computed rollout to read, because its ledger is this project's
// files and nothing observes it.
//
// It is exit 2, not 1: there is nothing wrong with the env, there is just
// nothing here that can answer the question — and the message names the verb
// that can.
func TestEnvWait_SelfManagedEnvCannotBeWaitedOn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	declareEnvDir(t, dir, "prod")
	t.Chdir(dir)
	// A render that declares workloads and NO control_plane: the file
	// ledger's shape.
	t.Setenv("FORGE_KCL_RENDER_FIXTURE", writeKCLFixture(t,
		`{"output":{"workloads":[{"name":"api","kind":"service","image":"api","runtime":{"type":"cluster","cluster":"c","namespace":"n"},"spec":{"kind":"service"}}]}}`))

	var err error
	captureStdout(t, func() { err = runEnvWait(context.Background(), "prod", envWaitOptions{Timeout: time.Second}) })
	if got := exitCodeForError(err); got != exitUndetermined {
		t.Fatalf("a self-managed env must exit %d, got %d (%v)", exitUndetermined, got, err)
	}
	for _, want := range []string{"declares no hosted control plane", "forge env verify prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message must say %q, got:\n%v", want, err)
		}
	}
}

// Every flag §3.2 names is declared, so a pipeline written against the design
// doc runs.
func TestEnvWaitCmd_DeclaresEveryPlannedFlag(t *testing.T) {
	cmd := newEnvWaitCmd()
	for _, name := range []string{"promotion", "release", "timeout", "stable-for", "fail-fast", "include-unpinned", "interval", "json", "watch-json"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s is not declared on `forge env wait`", name)
		}
	}
	// The Q4 defaults, read off the command rather than asserted in prose.
	if got := cmd.Flags().Lookup("timeout").DefValue; got != envWaitDefaultTimeout.String() {
		t.Errorf("--timeout default = %s, want %s", got, envWaitDefaultTimeout)
	}
	if got := cmd.Flags().Lookup("fail-fast").DefValue; got != "false" {
		t.Errorf("--fail-fast default = %s, want false (a transient crash loop must not fail the gate)", got)
	}
	// --promotion and --release name the promotion two different ways.
	cmd.SetArgs([]string{"prod", "--promotion", "p-1", "--release", "v1"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "promotion") {
		t.Fatalf("--promotion with --release must be refused, got %v", err)
	}
}

// wireCodeError is a control-plane failure carrying one Connect code: the
// REAL *cloud.Error, so the classification under test is the one production
// performs. A hand-rolled stand-in would pass even if the code branch stopped
// reading the real type.
func wireCodeError(code string) error {
	return &cloud.Error{Code: code, Procedure: procGetRollout, Endpoint: "https://cp.example", Message: "no"}
}
