package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/pkg/observe/genai"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

// aiRow is one row of the AI panel (obsbackend.AIPanelID): a service's calls
// of one operation, and of one tool for execute_tool.
func aiRow(svc, op, tool string, calls, errs, p95 float64) string {
	return fmt.Sprintf(`{"group":{"service.name":%q,"span.attr.gen_ai.operation.name":%q,"span.attr.gen_ai.tool.name":%q},"values":[%g,%g,%g]}`,
		svc, op, tool, calls, errs, p95)
}

// A hosted check reads each window's AI calls itself (the after window at
// the judged release's version), judges them by the AI rule, and warns: a
// release whose agent got slower and whose search tool broke still passes
// under the default policy, and the JSON names both.
func TestObsCheckHostedJudgesTheAICalls(t *testing.T) {
	reply := healthReply("RELEASE_VERDICT_PASS", true, "", "")
	before := rowsReply(aiRow("agent", genai.OpChat, "", 400, 4, 900), aiRow("agent", genai.OpExecuteTool, "search", 200, 2, 50))
	after := rowsReply(aiRow("agent", genai.OpChat, "", 400, 4, 2700), aiRow("agent", genai.OpExecuteTool, "search", 200, 80, 50))
	run := func(t *testing.T, args ...string) (int, map[string]any, *seqCaller) {
		caller := &seqCaller{replies: map[string][]string{procHealth: {reply}, procAIPanel: {before, after}}}
		caller.replies[procPanel+"#ai-model-latency"] = []string{
			rowsReply(`{"group":{"service.name":"agent"},"values":[900]}`),
			rowsReply(`{"group":{"service.name":"agent"},"values":[2700]}`),
		}
		out, _, err := newObsCheckHarness().run(t, cloudFacts(), caller, nil, append([]string{"check", "prod", "--json"}, args...)...)
		return exitCodeForError(err), decode(t, out), caller
	}

	code, doc, caller := run(t)
	if code != 0 || doc["verdict"] != "PASS" {
		t.Fatalf("default policy: exit %d, verdict %v: AI findings warn", code, doc["verdict"])
	}
	lat := findingOf(doc, release.HealthAILatency)
	if lat == nil || lat["policy"] != releasehealth.PolicyWarn || lat["value"] != 3.0 || lat["service"] != "agent" {
		t.Errorf("ai_latency = %v", lat)
	}
	tool := findingOf(doc, release.HealthAIToolErrors)
	if tool == nil || tool["ref"] != "search" || !strings.Contains(fmt.Sprint(tool["message"]), "tool search (agent)") {
		t.Errorf("ai_tool_errors = %v", tool)
	}
	if e := findingOf(doc, release.HealthAIErrorRate); e == nil {
		t.Errorf("ai_error_rate: 84 of 600 calls failed after, 6 before: %v", doc["findings"])
	}
	if ai, _ := doc["ai"].([]any); len(ai) != 1 {
		t.Errorf("ai = %v", doc["ai"])
	}
	if len(caller.aiCalls) != 2 {
		t.Fatalf("the AI panel ran %d times, want once per window", len(caller.aiCalls))
	}
	if b := caller.aiCalls[0].body; strings.Contains(b, "service.version") {
		t.Errorf("the before window is read whole: %s", b)
	}
	if a := caller.aiCalls[1].body; !strings.Contains(a, `"field":"resource.attr.service.version","op":"eq","value":"v2"`) ||
		!strings.Contains(a, `"op":"neq","value":"canceled"`) || !strings.Contains(a, genai.OpExecuteTool) {
		t.Errorf("the after window is the release's own calls, cancellations no failure: %s", a)
	}
	// The AI thresholds never reach the control plane, which may predate them.
	if strings.Contains(caller.calls[0].body, "ai_min_calls") {
		t.Errorf("an AI threshold was sent: %s", caller.calls[0].body)
	}

	code, doc, caller = run(t, "--fail-on", "ai_tool_errors", "--threshold", "ai_min_calls=10")
	if code != 1 || doc["verdict"] != "FAIL" {
		t.Errorf("--fail-on ai_tool_errors: exit %d, verdict %v", code, doc["verdict"])
	}
	if f := findingOf(doc, release.HealthAIToolErrors); f == nil || f["policy"] != releasehealth.PolicyFail {
		t.Errorf("ai_tool_errors = %v", f)
	}
	if strings.Contains(caller.calls[0].body, "ai_min_calls") {
		t.Errorf("an AI threshold override was sent: %s", caller.calls[0].body)
	}
}

// A service with no GenAI calls has nothing to judge and says nothing; a
// read that fails is named in not_evaluated, never a finding.
func TestObsCheckAIReadsNothingOrFails(t *testing.T) {
	reply := healthReply("RELEASE_VERDICT_PASS", true, "", "")
	caller := &seqCaller{replies: map[string][]string{procHealth: {reply}}}
	out, _, err := newObsCheckHarness().run(t, cloudFacts(), caller, nil, "check", "prod", "--json")
	doc := decode(t, out)
	if exitCodeForError(err) != 0 || doc["ai"] != nil {
		t.Errorf("no AI calls: exit %d, ai %v", exitCodeForError(err), doc["ai"])
	}
	ne, _ := doc["not_evaluated"].([]any)
	for _, e := range ne {
		if strings.HasPrefix(fmt.Sprint(e), releasehealth.NotEvaluatedAI) {
			t.Errorf("no AI calls is not a not_evaluated entry: %v", e)
		}
	}

	caller = &seqCaller{replies: map[string][]string{procHealth: {reply}, procAIPanel: {`not json`}}}
	out, _, err = newObsCheckHarness().run(t, cloudFacts(), caller, nil, "check", "prod", "--json")
	doc = decode(t, out)
	if exitCodeForError(err) != 2 || doc["verdict"] != "INCONCLUSIVE" || !strings.Contains(fmt.Sprint(doc["not_evaluated"]), releasehealth.NotEvaluatedAI+" the AI calls could not be read") {
		t.Errorf("a failed read: exit %d, verdict %v, not_evaluated %v", exitCodeForError(err), doc["verdict"], doc["not_evaluated"])
	}
}

// The local backend runs the same panel over the ClickStack.
func TestAIPanelIsOneTableReadPerWindow(t *testing.T) {
	if obsbackend.AIPanelID != "ai-calls" {
		t.Errorf("AIPanelID = %q", obsbackend.AIPanelID)
	}
	j := obsbackend.JoinAIHealth(
		[]obsbackend.AIServiceCalls{{Service: "b", Window: releasehealth.AIWindow{Model: releasehealth.AICalls{Calls: 1}}, Tools: map[string]releasehealth.AICalls{"t": {Calls: 2}}}},
		[]obsbackend.AIServiceCalls{{Service: "a"}, {Service: "b", Tools: map[string]releasehealth.AICalls{"u": {Calls: 3}}}}, "v2")
	if len(j) != 2 || j[0].Service != "a" || j[1].Service != "b" || len(j[1].Tools) != 2 || j[1].Tools[0].Tool != "t" || j[1].Tools[1].After.Calls != 3 || j[1].AfterVersion != "v2" {
		t.Errorf("JoinAIHealth = %+v", j)
	}
}
