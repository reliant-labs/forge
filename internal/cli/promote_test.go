package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

// Tests for `forge env promote`'s compare-and-set (control-plane
// docs/design/hosted-deploy-primitives.md §3.1, task F2).
//
// The hosted cases run the real hostedStore over the real cloud.Client
// against fakeDeployService (hosted_ledger_test.go), which applies the
// SERVER's order — pinned, idempotent no-op, compare-and-set, in flight — and
// answers refusals in control-plane's wire form (reason header + a
// DeployPromoteRefusal detail). So each case asserts what forge SENT and what
// it made of the answer, through the same bytes production carries.

// hostedPromoteFixture is a hosted ledger with releases v1..v3 cut and the
// env "prod" promoted along `promoted` in order.
func hostedPromoteFixture(t *testing.T, promoted ...string) (*fakeDeployService, *hostedStore) {
	t.Helper()
	fake := newFakeDeployService(map[string]string{"prod": "env-prod-uuid"})
	store, _ := newHostedTestStore(t, fake)
	ctx := context.Background()
	for i, v := range []string{"v1", "v2", "v3"} {
		r := ociRelease(v, map[string]string{"api": sha(fmt.Sprint(i + 1))})
		r.CreatedAt = parseFixtureTime(fmt.Sprintf("2026-0%d-01T00:00:00Z", i+1))
		if _, err := store.Cut(ctx, r); err != nil {
			t.Fatalf("cut %s: %v", v, err)
		}
	}
	for _, v := range promoted {
		if _, err := store.Append(ctx, release.Promotion{Env: "prod", Release: v, Kind: release.KindPromote}, appendGuard{}); err != nil {
			t.Fatalf("seed promote %s: %v", v, err)
		}
	}
	return fake, store
}

// lastPromoteBody is the request body of the most recent Promote call.
func (f *fakeDeployService) lastPromoteBody(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.bodies) - 1; i >= 0; i-- {
		if f.bodies[i].Path == "/"+procPromote {
			return f.bodies[i].Body
		}
	}
	t.Fatal("no Promote call was made")
	return nil
}

// runHostedPromote runs the command body against the fixture's store, with
// stdout captured.
func runHostedPromote(t *testing.T, store *hostedStore, version string, opts promoteOptions) (string, error) {
	t.Helper()
	opts.ProjectDir = t.TempDir()
	opts.Bindings, opts.Releases = store, store
	opts.Git = allCommitsPresent()
	opts.Run.None = true // the test process may itself be running in CI
	var err error
	out := captureStdout(t, func() { err = runPromote(context.Background(), version, "prod", opts) })
	return out, err
}

// TestPromote_ApplySendsThePlannedPromotionID is the headline: with NO flag,
// a promote asserts the env is still on the promotion the plan read. That is
// what makes anti-stomp the default rather than a flag CI has to remember.
func TestPromote_ApplySendsThePlannedPromotionID(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1")
	planned, _, err := store.Current(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := runHostedPromote(t, store, "v2", promoteOptions{}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	body := fake.lastPromoteBody(t)
	if got := body["expectedCurrentPromotionId"]; got != planned.ID {
		t.Fatalf("expectedCurrentPromotionId = %v, want the planned %q (body %v)", got, planned.ID, body)
	}
	if _, set := body["expectUnbound"]; set {
		t.Errorf("expectUnbound must not be sent beside an id — the wire field is a oneof: %v", body)
	}
	if _, set := body["supersedeInFlight"]; set {
		t.Errorf("supersedeInFlight must be sent only with --supersede: %v", body)
	}
}

// An env the plan saw as never promoted is promoted with expect_unbound —
// the other half of "always CAS". Without it, two first promotes would both
// land.
func TestPromote_UnboundEnvSendsExpectUnbound(t *testing.T) {
	fake, store := hostedPromoteFixture(t)
	if _, err := runHostedPromote(t, store, "v1", promoteOptions{}); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	body := fake.lastPromoteBody(t)
	if body["expectUnbound"] != true {
		t.Fatalf("a first promote must send expectUnbound, got %v", body)
	}
	if _, set := body["expectedCurrentPromotionId"]; set {
		t.Errorf("no id may be sent beside expectUnbound: %v", body)
	}
}

// TestPromote_ExpectCurrentOverridesThePlan: the value captured when an
// approval was REQUESTED wins over the plan read after it, so a hotfix that
// landed while the approval waited is caught.
func TestPromote_ExpectCurrentOverridesThePlan(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1")
	captured, _, _ := store.Current(context.Background(), "prod")
	// The hotfix lands while the pipeline waits for approval.
	if _, err := store.Append(context.Background(), release.Promotion{Env: "prod", Release: "v2", Kind: release.KindPromote}, appendGuard{}); err != nil {
		t.Fatal(err)
	}
	hotfix, _, _ := store.Current(context.Background(), "prod")

	out, err := runHostedPromote(t, store, "v3", promoteOptions{ExpectCurrent: captured.ID})
	if body := fake.lastPromoteBody(t); body["expectedCurrentPromotionId"] != captured.ID {
		t.Fatalf("--expect-current must replace the planned id %q with %q, sent %v", hotfix.ID, captured.ID, body)
	}
	if got := exitCodeForError(err); got != exitConflict {
		t.Fatalf("a promote expecting the pre-hotfix promotion must exit %d, got %d (%v)", exitConflict, got, err)
	}
	cur, _, _ := store.Current(context.Background(), "prod")
	if cur.ID != hotfix.ID {
		t.Fatalf("the hotfix was overwritten: current is %+v", cur)
	}
	for _, want := range []string{"REFUSED", "promotion_conflict", "NOTHING WRITTEN", hotfix.ID} {
		if !strings.Contains(out, want) {
			t.Errorf("the refused report must say %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Promoted env") {
		t.Errorf("a refused promote must never read as a success:\n%s", out)
	}
}

// `--expect-current unbound` round-trips a captured "no promotion yet"
// through one CI variable.
func TestPromote_ExpectCurrentUnboundLiteral(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1")
	_, err := runHostedPromote(t, store, "v2", promoteOptions{ExpectCurrent: expectUnboundLiteral})
	body := fake.lastPromoteBody(t)
	if body["expectUnbound"] != true || body["expectedCurrentPromotionId"] != nil {
		t.Fatalf("--expect-current unbound must send expectUnbound and no id, got %v", body)
	}
	if got := exitCodeForError(err); got != exitConflict {
		t.Fatalf("expecting unbound on a bound env must exit %d, got %d (%v)", exitConflict, got, err)
	}
}

// TestPromote_RefusalExitCodes pins §3.A's mapping end to end: the server's
// reason, not its message, picks the code.
func TestPromote_RefusalExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*fakeDeployService)
		opts   promoteOptions
		reason string
		want   int
	}{
		{"conflict", nil, promoteOptions{ExpectCurrent: "promo-stale"}, reasonPromotionConflict, exitConflict},
		{"pinned", func(f *fakeDeployService) { f.pinned = true }, promoteOptions{}, reasonEnvironmentPinned, exitRefused},
		{"in flight", func(f *fakeDeployService) { f.inFlightPhase = wireRolloutPhaseProgressing }, promoteOptions{}, reasonRolloutInFlight, exitRefused},
		{"in flight, header only", func(f *fakeDeployService) {
			f.inFlightPhase = wireRolloutPhaseProgressing
			f.refusalWithoutDetail = true
		}, promoteOptions{}, reasonRolloutInFlight, exitRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, store := hostedPromoteFixture(t, "v1")
			if tc.setup != nil {
				tc.setup(fake)
			}
			_, err := runHostedPromote(t, store, "v2", tc.opts)
			var refused *promoteRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("want a *promoteRefusedError, got %T: %v", err, err)
			}
			if refused.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", refused.Reason, tc.reason)
			}
			if got := exitCodeForError(err); got != tc.want {
				t.Errorf("exit code = %d, want %d", got, tc.want)
			}
			if cur, _, _ := store.Current(context.Background(), "prod"); cur.Release != "v1" {
				t.Errorf("a refused promote wrote: prod is on %s", cur.Release)
			}
		})
	}
}

// --supersede is sent, admits the in-flight promote, and the server records
// it on the new entry.
func TestPromote_SupersedeIsSentAndRecorded(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1")
	fake.inFlightPhase = wireRolloutPhaseProgressing
	if _, err := runHostedPromote(t, store, "v2", promoteOptions{Supersede: true}); err != nil {
		t.Fatalf("--supersede must admit an in-flight promote: %v", err)
	}
	if body := fake.lastPromoteBody(t); body["supersedeInFlight"] != true {
		t.Fatalf("supersedeInFlight not sent: %v", body)
	}
	cur, _, _ := store.Current(context.Background(), "prod")
	if cur.Release != "v2" || !cur.SupersededInFlight {
		t.Fatalf("the superseding promote must land and be recorded, got %+v", cur)
	}
}

// TestPromote_JSONCarriesTheRefusal: --json is one document on a refusal
// too, applied:false, with what was expected and what is actually there,
// and its ok/exit_code agree with the process status.
func TestPromote_JSONCarriesTheRefusal(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1", "v2")
	fake.inFlightPhase = wireRolloutPhaseStabilizing
	planned, _, _ := store.Current(context.Background(), "prod")

	out, err := runHostedPromote(t, store, "v3", promoteOptions{JSON: true})
	if got := exitCodeForError(err); got != exitRefused {
		t.Fatalf("exit code = %d, want %d (%v)", got, exitRefused, err)
	}
	var doc map[string]any
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("stdout must be exactly one JSON document: %v\n%s", jerr, out)
	}
	if doc["applied"] != false || doc["ok"] != false || doc["exit_code"] != float64(exitRefused) {
		t.Errorf("applied/ok/exit_code = %v/%v/%v, want false/false/%d", doc["applied"], doc["ok"], doc["exit_code"], exitRefused)
	}
	if doc["expected"] != planned.ID {
		t.Errorf("expected = %v, want the planned %q", doc["expected"], planned.ID)
	}
	refusal, ok := doc["refusal"].(map[string]any)
	if !ok {
		t.Fatalf("a refused --json document must carry `refusal`:\n%s", out)
	}
	if refusal["reason"] != reasonRolloutInFlight || refusal["actual_phase"] != "stabilizing" {
		t.Errorf("refusal reason/phase = %v/%v", refusal["reason"], refusal["actual_phase"])
	}
	if refusal["expected_current_promotion_id"] != planned.ID {
		t.Errorf("the refusal must echo the expectation, got %v", refusal["expected_current_promotion_id"])
	}
	actual, _ := refusal["actual_current"].(map[string]any)
	if actual["id"] != planned.ID || actual["release"] != "v2" {
		t.Errorf("actual_current must name what is there (v2, %s), got %v", planned.ID, refusal["actual_current"])
	}
	if _, present := doc["recorded"]; present {
		t.Errorf("a refused promote recorded nothing, so `recorded` must be absent:\n%s", out)
	}
	current, _ := doc["current"].(map[string]any)
	if current["promotion_id"] != planned.ID {
		t.Errorf("current.promotion_id = %v, want %q — it is what CI captures for --expect-current", current["promotion_id"], planned.ID)
	}
}

// --plan --json shows the id a later --expect-current should carry, and
// sends nothing.
func TestPromote_PlanShowsThePromotionIDAndWritesNothing(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1")
	planned, _, _ := store.Current(context.Background(), "prod")
	before := fake.callCount(procPromote)

	out, err := runHostedPromote(t, store, "v2", promoteOptions{DryRun: true, JSON: true})
	if err != nil {
		t.Fatalf("--plan: %v", err)
	}
	if n := fake.callCount(procPromote); n != before {
		t.Fatalf("--plan called Promote %d time(s)", n-before)
	}
	var doc promotePlan
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("decode: %v\n%s", jerr, out)
	}
	if doc.Current.PromotionID != planned.ID || doc.Expected != planned.ID {
		t.Fatalf("current.promotion_id/expected = %q/%q, want %q", doc.Current.PromotionID, doc.Expected, planned.ID)
	}
	if !doc.OK || doc.ExitCode != exitOK || doc.Applied {
		t.Errorf("a plan is ok, exit 0, not applied; got ok=%v exit=%d applied=%v", doc.OK, doc.ExitCode, doc.Applied)
	}
}

// TestPromote_RetryOfALandedPromoteIsANoOp pins the server's step order on
// the forge side: a CI retry of a promote that already landed carries the
// PREVIOUS id as its expectation, and must succeed, not conflict. The file
// ledger applies the same order itself.
func TestPromote_RetryOfALandedPromoteIsANoOp(t *testing.T) {
	store := newMemBindingStore(map[string]release.Promotion{"prod": {ID: "p-1", Release: "v1"}})
	releases := newMemReleaseLedger(
		rel("v1", "2026-01-01T00:00:00Z", "", false, map[string]string{"api": sha("1")}),
		rel("v2", "2026-02-01T00:00:00Z", "", false, map[string]string{"api": sha("2")}),
	)
	opts := promoteOptions{ProjectDir: t.TempDir(), Bindings: store, Releases: releases, Git: allCommitsPresent(), ExpectCurrent: "p-1"}
	opts.Run.None = true
	for attempt := 1; attempt <= 2; attempt++ {
		var err error
		captureStdout(t, func() { err = runPromote(context.Background(), "v2", "prod", opts) })
		if err != nil {
			t.Fatalf("attempt %d: a retry of a landed promote must be a no-op, got %v", attempt, err)
		}
	}
	if n := len(store.history["prod"]); n != 2 {
		t.Fatalf("history has %d entries, want 2 (v1, v2 once)", n)
	}
}

// The FILE ledger (self-managed envs) applies the same compare-and-set,
// against the history it reads at write time.
func TestFileLedger_CompareAndSet(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)
	ctx := context.Background()
	p := func(v string) release.Promotion {
		return release.Promotion{Env: "prod", Release: v, Kind: release.KindPromote, Resolved: map[string]string{"api": sha("a")}}
	}

	first, err := store.Append(ctx, p("v1"), appendGuard{ExpectUnbound: true})
	if err != nil {
		t.Fatalf("expect-unbound on an empty log must write: %v", err)
	}
	if _, err := store.Append(ctx, p("v2"), appendGuard{ExpectUnbound: true}); exitCodeForError(err) != exitConflict {
		t.Fatalf("expect-unbound on a bound log must conflict (exit 3), got %v", err)
	}
	var refused *promoteRefusedError
	_, err = store.Append(ctx, p("v2"), appendGuard{ExpectedCurrentID: "someone-elses"})
	if !errors.As(err, &refused) || refused.ActualCurrent == nil || refused.ActualCurrent.ID != first.ID {
		t.Fatalf("a stale expectation must be refused naming the actual current %q, got %v", first.ID, err)
	}
	if _, err := store.Append(ctx, p("v2"), appendGuard{ExpectedCurrentID: first.ID}); err != nil {
		t.Fatalf("a matching expectation must write: %v", err)
	}
	// The no-op precedes the CAS: re-promoting v2 with the now-stale id of
	// v1 is a retry of a success, not a conflict.
	if _, err := store.Append(ctx, p("v2"), appendGuard{ExpectedCurrentID: first.ID}); err != nil {
		t.Fatalf("a retry of the landed promote must be a no-op: %v", err)
	}
	history, _ := store.History("prod")
	if len(history) != 2 {
		t.Fatalf("log has %d lines, want 2", len(history))
	}
	data, _ := os.ReadFile(promotionLogPath(dir, "prod"))
	if strings.Count(string(data), "\n") != 2 {
		t.Fatalf("a refused append wrote a line:\n%s", data)
	}
}

// TestPromote_UnwiredFlagsRefuseBeforeAnyWrite: every flag F3/F4/F5 own is
// declared now (F2 owns promote.go), and until its owner wires it, it
// refuses the whole promote — never moving the pointer and then failing on
// the part the caller asked for.
func TestPromote_UnwiredFlagsRefuseBeforeAnyWrite(t *testing.T) {
	cases := map[string]promoteOptions{
		"--wait":      {Follow: promoteFollowOptions{Wait: true}},
		"--deploy":    {Follow: promoteFollowOptions{Deploy: true}},
		"--timeout":   {Follow: promoteFollowOptions{Timeout: 1}},
		"--fail-fast": {Follow: promoteFollowOptions{FailFast: true}},
		"--gate":      {Gates: []string{"name=lint,status=passed"}},
		// --from / --from-promotion are wired (F5); their behaviour is
		// pinned in promote_from_test.go.
	}
	for flag, opts := range cases {
		t.Run(flag, func(t *testing.T) {
			fake, store := hostedPromoteFixture(t, "v1")
			before := fake.callCount(procPromote)
			_, err := runHostedPromote(t, store, "v2", opts)
			if err == nil || !strings.Contains(err.Error(), "not supported by this forge build") {
				t.Fatalf("%s must refuse with the not-supported message, got %v", flag, err)
			}
			if n := fake.callCount(procPromote); n != before {
				t.Fatalf("%s refused AFTER writing: Promote called %d time(s)", flag, n-before)
			}
		})
	}
}

// Every promote flag the plan names is declared on the command, so F3/F4/F5/
// F8 never edit promote.go.
func TestPromoteCmd_DeclaresEveryPlannedFlag(t *testing.T) {
	cmd := newPromoteCmd()
	for _, name := range []string{
		"expect-current", "expect-unbound", "supersede",
		"wait", "deploy", "timeout", "fail-fast",
		"gate", "from", "from-promotion",
		"run-id", "run-url", "no-run",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s is not declared on `forge env promote`", name)
		}
	}
	cmd.SetArgs([]string{"v1", "--to", "prod", "--expect-current", "x", "--expect-unbound"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "expect-current") {
		t.Fatalf("--expect-current and --expect-unbound together must be refused, got %v", err)
	}
}

// The --run-id flag reaches the wire: §3.A's run identity on promote.
func TestPromote_RunIdentityIsSent(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1")
	opts := promoteOptions{}
	_, err := runHostedPromote(t, store, "v2", opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := fake.lastPromoteBody(t)["run"]; set {
		t.Fatal("--no-run must send no run")
	}

	opts = promoteOptions{ProjectDir: t.TempDir(), Bindings: store, Releases: store, Git: allCommitsPresent(),
		Run: runOptions{ID: "manual-42", URL: "https://ci.example/42"}}
	captureStdout(t, func() { err = runPromote(context.Background(), "v3", "prod", opts) })
	if err != nil {
		t.Fatal(err)
	}
	run, _ := fake.lastPromoteBody(t)["run"].(map[string]any)
	if run["id"] != "manual-42" || run["url"] != "https://ci.example/42" {
		t.Fatalf("run = %v, want id manual-42 with its url", run)
	}
}
