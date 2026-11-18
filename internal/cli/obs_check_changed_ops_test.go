package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

// checkP is the promotion every changed-op test judges: 11:40, so the before
// window is [11:25, 11:40) and the after window [11:40, 11:55).
var checkP = time.Date(2026, 10, 22, 11, 40, 0, 0, time.UTC)

const (
	notifySend   = "notify.Send"
	createTodo   = "todo.v1.TodoService/CreateTodo"
	commitNotify = "3f2c1ab9e0d4c5b6a7f8091a2b3c4d5e6f708192"
	procPanel    = "controlplane.v1.ObservabilityService/QueryPanel"
)

// fakeOpsReader answers each window's ops read with its figures, and records
// the requests.
type fakeOpsReader struct {
	before, after []obsbackend.OpStats
	err           error
	reqs          []obsbackend.OpsRequest
}

func (f *fakeOpsReader) Ops(_ context.Context, req obsbackend.OpsRequest) (obsbackend.OpsResult, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return obsbackend.OpsResult{}, f.err
	}
	if req.Range.From.Before(checkP) {
		return obsbackend.OpsResult{Ops: f.before}, nil
	}
	return obsbackend.OpsResult{Ops: f.after}, nil
}

// opsLocal is the local backend with its OpsReader replaced.
type opsLocal struct {
	obsbackend.Local
	ops obsbackend.OpsReader
}

func (o opsLocal) Ops(ctx context.Context, req obsbackend.OpsRequest) (obsbackend.OpsResult, error) {
	return o.ops.Ops(ctx, req)
}

// fixedOpChanges is a changed-op source that says ops changed, and records
// the subject it was asked about.
func fixedOpChanges(asked *[]releasehealth.Subject, ops ...changedOp) opChangeFunc {
	return func(_ context.Context, _ string, s releasehealth.Subject) opChanges {
		*asked = append(*asked, s)
		return opChanges{Read: true, Base: "base0123", Ops: ops}
	}
}

func notifyOp() changedOp {
	return changedOp{Name: notifySend, Kind: releasehealth.OpKindComponent,
		Commits: []releasehealth.OpCommit{{SHA: commitNotify, Subject: "fix(notify): retry (#42)", PR: 42}}}
}

func stats(svc, name string, calls, errRatio, p95 float64) obsbackend.OpStats {
	return obsbackend.OpStats{Service: svc, Name: name, Calls: calls, Errors: calls * errRatio, ErrorRatio: errRatio, P50Ms: p95 / 2, P95Ms: p95, P99Ms: p95 * 2}
}

func equalTraffic() *healthRunner {
	return &healthRunner{
		before: obsbackend.Result{Rows: [][]any{redRow("api", 1000, 10, 100)}},
		after:  obsbackend.Result{Rows: [][]any{redRow("api", 1000, 10, 100)}},
	}
}

func findingOf(doc map[string]any, code string) map[string]any {
	for _, f := range doc["findings"].([]any) {
		if m := f.(map[string]any); m["code"] == code {
			return m
		}
	}
	return nil
}

func TestObsCheckLocalChangedOpErrorsWarn(t *testing.T) {
	var asked []releasehealth.Subject
	ops := &fakeOpsReader{
		before: []obsbackend.OpStats{stats("api", notifySend, 200, 0.01, 10)},
		after:  []obsbackend.OpStats{stats("api", notifySend, 200, 0.2, 12)},
	}
	h := newObsCheckHarness()
	h.opChanges = fixedOpChanges(&asked, notifyOp())
	h.wrapLocal = func(l obsbackend.Local) obsbackend.Backend { return opsLocal{l, ops} }
	out, _, err := h.run(t, devFacts(), nil, equalTraffic(), "check", "dev", "--since", "20m", "--json")
	if err != nil {
		t.Fatalf("a changed op that regressed warns by default: %v\n%s", err, out)
	}
	if len(asked) != 1 || !asked[0].PromotedAt.Equal(checkP) || promotionSubject(asked[0]) {
		t.Errorf("the source is asked once, about the local instant: %+v", asked)
	}
	// One read per window, of the changed ops' names alone.
	if len(ops.reqs) != 2 || !slices.Equal(ops.reqs[0].Names, []string{notifySend}) ||
		!ops.reqs[0].Range.From.Equal(checkP.Add(-15*time.Minute)) || !ops.reqs[0].Range.To.Equal(checkP) ||
		!ops.reqs[1].Range.From.Equal(checkP) || !ops.reqs[1].Range.To.Equal(checkP.Add(15*time.Minute)) {
		t.Errorf("ops reads: %+v", ops.reqs)
	}
	doc := decode(t, out)
	if doc["verdict"] != "PASS" {
		t.Errorf("verdict = %v", doc["verdict"])
	}
	f := findingOf(doc, release.HealthChangedOpErrors)
	if f == nil || f["policy"] != releasehealth.PolicyWarn || f["op"] != notifySend || f["commit"] != commitNotify || f["pr"] != float64(42) || f["service"] != "api" {
		t.Fatalf("changed_op_errors finding = %v", f)
	}
	if msg := fmt.Sprint(f["message"]); !strings.HasSuffix(msg, "changed in 3f2c1ab (#42)") {
		t.Errorf("message = %s", msg)
	}
	co := doc["changed_ops"].([]any)
	if len(co) != 1 {
		t.Fatalf("changed_ops = %v", co)
	}
	c := co[0].(map[string]any)
	if c["op"] != notifySend || c["service"] != "api" || c["kind"] != "component" ||
		c["before"].(map[string]any)["requests"] != float64(200) || c["after"].(map[string]any)["error_rate"] != 0.2 ||
		c["commits"].([]any)[0].(map[string]any)["sha"] != commitNotify {
		t.Errorf("changed op = %v", c)
	}
	sug := fmt.Sprint(doc["suggested"])
	for _, want := range []string{
		"forge obs traces dev --service api --filter span.attr.forge.op=notify.Send --errors",
		"forge obs blast-radius dev --base base0123 --head " + commitNotify,
	} {
		if !strings.Contains(sug, want) {
			t.Errorf("suggested lacks %q: %s", want, sug)
		}
	}
	if ths := fmt.Sprint(doc["thresholds"]); !strings.Contains(ths, releasehealth.ThresholdChangedOpMinCalls) {
		t.Errorf("the changed-op thresholds are listed: %s", ths)
	}

	// The text output names the op and its commit, the codes aligned.
	h2 := newObsCheckHarness()
	h2.opChanges, h2.wrapLocal = h.opChanges, h.wrapLocal
	text, _, err := h2.run(t, devFacts(), nil, equalTraffic(), "check", "dev", "--since", "20m")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CHANGED OP", "notify.Send", "3f2c1ab (#42)", "warn changed_op_errors notify.Send (api): error rate"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestObsCheckLocalChangedOpFailsWhenFailOnNamesIt(t *testing.T) {
	var asked []releasehealth.Subject
	ops := &fakeOpsReader{
		before: []obsbackend.OpStats{stats("api", notifySend, 200, 0.01, 10)},
		after:  []obsbackend.OpStats{stats("api", notifySend, 200, 0.2, 12)},
	}
	h := newObsCheckHarness()
	h.opChanges = fixedOpChanges(&asked, notifyOp())
	h.wrapLocal = func(l obsbackend.Local) obsbackend.Backend { return opsLocal{l, ops} }
	out, _, err := h.run(t, devFacts(), nil, equalTraffic(), "check", "dev", "--since", "20m", "--json", "--fail-on", "error_rate,changed_op_errors")
	if exitCodeForError(err) != exitWrong {
		t.Fatalf("exit = %d (%v)", exitCodeForError(err), err)
	}
	doc := decode(t, out)
	if f := findingOf(doc, release.HealthChangedOpErrors); doc["verdict"] != "FAIL" || f == nil || f["policy"] != releasehealth.PolicyFail {
		t.Errorf("verdict %v, finding %v", doc["verdict"], f)
	}
	if sug := fmt.Sprint(doc["suggested"]); !strings.Contains(sug, "--filter span.attr.forge.op=notify.Send --errors") || !strings.Contains(sug, "forge obs endpoints dev --sort errors") {
		t.Errorf("a FAIL suggests the op's traces with the rest: %s", sug)
	}
}

// panelReply is a QueryPanel answer of one ops-server row.
func panelReply(svc, name string, calls, errs, p95 float64) string {
	return fmt.Sprintf(`{"rows":[{"group":{"service.name":%q,"span.name":%q},"values":[%g,%g,%g,%g,%g]}]}`, svc, name, calls, errs, p95/2, p95, p95*2)
}

// A control plane that predates the changed-op rule sends no ops: the CLI
// reads them itself, and --fail-on still re-judges the rest of the answer.
func TestObsCheckHostedAnswerGetsItsChangedOps(t *testing.T) {
	reply := healthReply("RELEASE_VERDICT_PASS", true, latencyFinding, "")
	rpc := changedOp{Name: createTodo, Kind: releasehealth.OpKindRPC, Commits: []releasehealth.OpCommit{{SHA: "c0ffee1234567", Subject: "perf: batch writes"}}}
	run := func(t *testing.T, args ...string) (int, map[string]any, *seqCaller, []releasehealth.Subject) {
		var asked []releasehealth.Subject
		caller := &seqCaller{replies: map[string][]string{procHealth: {reply},
			procPanel: {panelReply("api", createTodo, 400, 0, 20), panelReply("api", createTodo, 400, 0, 90)}}}
		h := newObsCheckHarness()
		h.opChanges = fixedOpChanges(&asked, rpc)
		out, _, err := h.run(t, cloudFacts(), caller, nil, append([]string{"check", "prod", "--json"}, args...)...)
		return exitCodeForError(err), decode(t, out), caller, asked
	}

	code, doc, caller, asked := run(t)
	if code != 0 || doc["verdict"] != "PASS" {
		t.Fatalf("env policy: exit %d, %v", code, doc["verdict"])
	}
	if len(asked) != 1 || asked[0].Release != "v2" || asked[0].GitCommit != "abc123" || asked[0].PreviousRelease != "v1" {
		t.Errorf("the source is asked about the promotion: %+v", asked)
	}
	f := findingOf(doc, release.HealthChangedOpLatency)
	if f == nil || f["policy"] != releasehealth.PolicyWarn || f["op"] != createTodo || f["value"] != 4.5 {
		t.Fatalf("changed_op_latency = %v", f)
	}
	var panels int
	for _, c := range caller.calls {
		if c.procedure == procPanel {
			panels++
			if !strings.Contains(c.body, createTodo) || !strings.Contains(c.body, "ops-server") {
				t.Errorf("panel request: %s", c.body)
			}
		}
	}
	if panels != 2 {
		t.Errorf("QueryPanel ran %d times, want once per window", panels)
	}
	if sug := fmt.Sprint(doc["suggested"]); !strings.Contains(sug, "forge obs traces prod --service api --route "+createTodo+" --slow") {
		t.Errorf("suggested = %s", sug)
	}

	// --fail-on re-judges the control plane's latency finding (Rejudge),
	// and the changed-op finding follows the same set.
	code, doc, _, _ = run(t, "--fail-on", "latency")
	if code != exitWrong || doc["verdict"] != "FAIL" {
		t.Fatalf("--fail-on latency: exit %d, %v", code, doc["verdict"])
	}
	if f := findingOf(doc, release.HealthLatency); f == nil || f["policy"] != releasehealth.PolicyFail {
		t.Errorf("latency = %v", f)
	}
	if f := findingOf(doc, release.HealthChangedOpLatency); f == nil || f["policy"] != releasehealth.PolicyWarn {
		t.Errorf("changed_op_latency under --fail-on latency = %v", f)
	}
	code, doc, _, _ = run(t, "--fail-on", "changed_op_latency")
	if f := findingOf(doc, release.HealthChangedOpLatency); code != exitWrong || f == nil || f["policy"] != releasehealth.PolicyFail {
		t.Errorf("--fail-on changed_op_latency: exit %d, %v", code, f)
	}
	if f := findingOf(doc, release.HealthLatency); f == nil || f["policy"] != releasehealth.PolicyWarn {
		t.Errorf("latency under --fail-on changed_op_latency = %v", f)
	}
}

// A changed op that regressed keeps --early-pass waiting for the whole
// window, as any finding does; the diff is read once however many polls.
func TestObsCheckChangedOpBlocksAnEarlyPass(t *testing.T) {
	var asked []releasehealth.Subject
	before, after := panelReply("api", createTodo, 400, 0, 20), panelReply("api", createTodo, 400, 0, 90)
	caller := &seqCaller{replies: map[string][]string{
		procHealth: {healthReply("RELEASE_VERDICT_INCONCLUSIVE", false, "", ""), healthReply("RELEASE_VERDICT_PASS", true, "", "")},
		procPanel:  {before, after, before, after},
	}}
	h := newObsCheckHarness()
	h.opChanges = fixedOpChanges(&asked, changedOp{Name: createTodo, Kind: releasehealth.OpKindRPC})
	out, errOut, err := h.run(t, cloudFacts(), caller, nil, "check", "prod", "--wait", "--early-pass", "5m", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	doc := decode(t, out)
	if h.sleeps != 1 || doc["verdict"] != "PASS" || doc["early_pass"] != nil || findingOf(doc, release.HealthChangedOpLatency) == nil {
		t.Errorf("sleeps %d, verdict %v, early_pass %v, findings %v", h.sleeps, doc["verdict"], doc["early_pass"], doc["findings"])
	}
	if len(asked) != 1 {
		t.Errorf("the diff was read %d times, want once", len(asked))
	}
}

// --explain lists the changed ops' statements with the rest, and runs none.
func TestObsCheckExplainListsTheChangedOpStatements(t *testing.T) {
	var asked []releasehealth.Subject
	hr := &healthRunner{}
	h := newObsCheckHarness()
	h.opChanges = fixedOpChanges(&asked, notifyOp())
	out, _, err := h.run(t, devFacts(), nil, hr, "check", "dev", "--since", "20m", "--explain", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if len(hr.ran) != 0 {
		t.Errorf("--explain ran %d statements", len(hr.ran))
	}
	var purposes []string
	for _, s := range decode(t, out)["statements"].([]any) {
		purposes = append(purposes, fmt.Sprint(s.(map[string]any)["purpose"]))
	}
	if n := strings.Count(strings.Join(purposes, "|"), "ops component"); n != 2 {
		t.Errorf("want the component-ops statement once per window, got %d: %v", n, purposes)
	}
}

// The changed-op thresholds are applied by the CLI, never sent: an older
// control plane refuses names it does not know.
func TestObsCheckHoldsTheChangedOpThresholdsBackFromTheControlPlane(t *testing.T) {
	var asked []releasehealth.Subject
	caller := &seqCaller{replies: map[string][]string{procHealth: {healthReply("RELEASE_VERDICT_PASS", true, "", "")},
		procPanel: {panelReply("api", createTodo, 30, 0, 20), panelReply("api", createTodo, 30, 0, 90)}}}
	h := newObsCheckHarness()
	h.opChanges = fixedOpChanges(&asked, changedOp{Name: createTodo, Kind: releasehealth.OpKindRPC})
	out, _, err := h.run(t, cloudFacts(), caller, nil, "check", "prod", "--json",
		"--threshold", "min_requests=20", "--threshold", "changed_op_min_calls=25")
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(caller.calls[0].body), &sent)
	if ov, _ := sent["thresholdOverrides"].(map[string]any); ov["min_requests"] != "20" || ov[releasehealth.ThresholdChangedOpMinCalls] != nil {
		t.Errorf("thresholdOverrides sent = %v", sent["thresholdOverrides"])
	}
	doc := decode(t, out)
	// 30 calls a window clears 25: the op is compared, and its latency found.
	if findingOf(doc, release.HealthChangedOpLatency) == nil {
		t.Errorf("the override was not applied: %v", doc["findings"])
	}
	if ths := fmt.Sprint(doc["thresholds"]); !strings.Contains(ths, "name:changed_op_min_calls source:request value:25") {
		t.Errorf("thresholds = %s", ths)
	}
}

func TestObsCheckChangedOpsNotEvaluated(t *testing.T) {
	var asked []releasehealth.Subject
	thin := &fakeOpsReader{
		before: []obsbackend.OpStats{stats("api", notifySend, 10, 0, 10)},
		after:  []obsbackend.OpStats{stats("api", notifySend, 10, 0.5, 90)},
	}
	for name, tc := range map[string]struct {
		changes opChangeFunc
		wrap    func(obsbackend.Local) obsbackend.Backend
		want    string
	}{
		"thin windows": {fixedOpChanges(&asked, notifyOp()), func(l obsbackend.Local) obsbackend.Backend { return opsLocal{l, thin} },
			"changed_op: 1 op(s) below changed_op_min_calls (50 calls in each window): notify.Send"},
		"no OpsReader": {fixedOpChanges(&asked, notifyOp()), func(l obsbackend.Local) obsbackend.Backend {
			return struct {
				obsbackend.Backend
				obsbackend.HealthReader
			}{l, l}
		}, "changed_op: the local backend cannot read per-op figures"},
		"the read fails": {fixedOpChanges(&asked, notifyOp()), func(l obsbackend.Local) obsbackend.Backend {
			return opsLocal{l, &fakeOpsReader{err: errors.New("clickhouse is down")}}
		}, "changed_op: the changed ops' figures could not be read: clickhouse is down"},
		"no checkout": {noCheckoutOpChanges, nil, "changed_op: there is no checkout here"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newObsCheckHarness()
			h.opChanges, h.wrapLocal = tc.changes, tc.wrap
			out, _, err := h.run(t, devFacts(), nil, equalTraffic(), "check", "dev", "--since", "20m", "--json")
			if err != nil {
				t.Fatalf("a changed op the check cannot judge never fails it: %v", err)
			}
			doc := decode(t, out)
			if ne := fmt.Sprint(doc["not_evaluated"]); !strings.Contains(ne, tc.want) {
				t.Errorf("not_evaluated = %s, want %q", ne, tc.want)
			}
			if doc["verdict"] != "PASS" || findingOf(doc, release.HealthChangedOpErrors) != nil {
				t.Errorf("verdict %v, findings %v", doc["verdict"], doc["findings"])
			}
		})
	}
}

// Nothing changed since P: no changed ops, and no not_evaluated noise.
func TestObsCheckNothingChangedSaysNothing(t *testing.T) {
	h := newObsCheckHarness()
	h.opChanges = func(context.Context, string, releasehealth.Subject) opChanges {
		return opChanges{Read: true, Base: "b"}
	}
	out, _, err := h.run(t, devFacts(), nil, equalTraffic(), "check", "dev", "--since", "20m", "--json")
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, out)
	if _, has := doc["changed_ops"]; has || strings.Contains(fmt.Sprint(doc["not_evaluated"]), releasehealth.NotEvaluatedChangedOps) {
		t.Errorf("changed_ops %v, not_evaluated %v", doc["changed_ops"], doc["not_evaluated"])
	}
}

func TestObsCheckChangedOpFields(t *testing.T) {
	var asked []releasehealth.Subject
	ops := &fakeOpsReader{
		before: []obsbackend.OpStats{stats("api", notifySend, 200, 0.01, 10), stats("worker", notifySend, 100, 0, 10)},
		after:  []obsbackend.OpStats{stats("api", notifySend, 200, 0.2, 12), stats("worker", notifySend, 100, 0, 10)},
	}
	h := newObsCheckHarness()
	h.opChanges = fixedOpChanges(&asked, notifyOp())
	h.wrapLocal = func(l obsbackend.Local) obsbackend.Backend { return opsLocal{l, ops} }
	out, _, err := h.run(t, devFacts(), nil, equalTraffic(), "check", "dev", "--since", "20m", "--json",
		"--fields", "changed_ops.service,changed_ops.after.requests,findings.op,findings.pr")
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, out)
	// One OpHealth per service the op ran in.
	if got := fmt.Sprint(doc["changed_ops"]); got != "[map[after:map[requests:200] service:api] map[after:map[requests:100] service:worker]]" {
		t.Errorf("changed_ops = %s", got)
	}
	if got := fmt.Sprint(doc["findings"]); got != "[map[op:notify.Send pr:42]]" {
		t.Errorf("findings = %s", got)
	}
}

func TestPromotionSubject(t *testing.T) {
	if promotionSubject(releasehealth.Subject{PromotedAt: checkP}) || !promotionSubject(releasehealth.Subject{Release: "v2"}) {
		t.Error("a local subject is only its instant; a promotion names a release")
	}
}

// localOpBase and mapOpChanges over a real repository: the base is the
// newest commit before P, and a range this checkout does not hold is
// not_evaluated, never an error.
func TestLocalOpBaseOverGit(t *testing.T) {
	if testing.Short() {
		t.Skip("runs git in a temporary repository; runs in task test")
	}
	dir := t.TempDir()
	git := func(date string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q", "-b", "main")
	commit := func(date, msg string) string {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git(date, "add", ".")
		git(date, "commit", "-q", "-m", msg)
		return git(date, "rev-parse", "HEAD")
	}
	first := commit("2026-10-22T11:00:00Z", "first")
	commit("2026-10-22T11:50:00Z", "second")

	ctx := context.Background()
	if base, err := localOpBase(ctx, dir, checkP); err != nil || base != first {
		t.Errorf("base = %q, %v; want the commit before P %s", base, err, first)
	}
	if base, err := localOpBase(ctx, dir, checkP.Add(-2*time.Hour)); err != nil || base != "" {
		t.Errorf("no commit predates P: base = %q, %v", base, err)
	}
	ch := mapOpChanges(ctx, dir, "0123456789abcdef0123456789abcdef01234567", first)
	if ch.Read || len(ch.NotEvaluated) != 1 || !strings.HasPrefix(ch.NotEvaluated[0], "changed_op: the release's commits are not in this checkout (0123456..") {
		t.Errorf("a range outside the checkout: %+v", ch)
	}
}
