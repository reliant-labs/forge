package releasehealth

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

// aiSvc is a service's AI figures whose after window is the judged release's
// own (v2, the release input() names).
func aiSvc(before, after AIWindow, tools ...AITool) AIHealth {
	return AIHealth{Service: "agent", Before: before, After: after, Tools: tools, AfterVersion: "v2"}
}

func win(calls, errs int64, p95 float64) AIWindow {
	return AIWindow{Model: AICalls{Calls: calls, Errors: errs}, ModelP95Ms: p95}
}

// judged is a passing report with the AI rule applied to hs.
func judged(t *testing.T, failOn []string, hs ...AIHealth) Report {
	t.Helper()
	in := input(t, true, svc("agent", 1000, 1000, 0, 0, 100, 100))
	if failOn != nil {
		in.Thresholds.FailOn = normalizeCodes(failOn)
	}
	return WithAI(Evaluate(in), hs, nil)
}

func TestAIRule(t *testing.T) {
	const thinEntry = NotEvaluatedAI + " 1 service(s) below ai_min_calls (50 model and tool calls in each window): agent"
	for _, tc := range []struct {
		name string
		h    AIHealth
		want []string
		thin bool
	}{
		{"no change", aiSvc(win(100, 1, 800), win(100, 1, 800)), nil, false},
		{"error rate rise past the limit", aiSvc(win(100, 0, 800), win(100, 10, 800)), []string{release.HealthAIErrorRate}, false},
		{"a rise below the limit", aiSvc(win(100, 0, 800), win(100, 5, 800)), nil, false},
		{"too few errors after", aiSvc(win(1000, 0, 800), win(60, 4, 800)), nil, false},
		{"p95 past the ratio", aiSvc(win(100, 0, 800), win(100, 0, 1300)), []string{release.HealthAILatency}, false},
		{"p95 under the ratio", aiSvc(win(100, 0, 800), win(100, 0, 1100)), nil, false},
		{"no before p95", aiSvc(win(100, 0, 0), win(100, 0, 900)), nil, false},
		{"thin before window", aiSvc(win(10, 0, 800), win(100, 50, 5000)), nil, true},
		{"tool calls count toward the error rate", AIHealth{Service: "agent", AfterVersion: "v2",
			Before: AIWindow{Model: AICalls{Calls: 50}, Tool: AICalls{Calls: 50}}, After: AIWindow{Model: AICalls{Calls: 50}, Tool: AICalls{Calls: 50, Errors: 20}}},
			[]string{release.HealthAIErrorRate}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := judged(t, nil, tc.h)
			if got := codes(r.Findings); !slices.Equal(got, tc.want) {
				t.Errorf("findings %v, want %v", got, tc.want)
			}
			for _, f := range r.Findings {
				if f.Policy != PolicyWarn {
					t.Errorf("%s: policy %s, want warn by default", f.Code, f.Policy)
				}
			}
			wantVerdict := Pass
			if tc.thin {
				wantVerdict = Inconclusive
			}
			if r.Verdict != wantVerdict {
				t.Errorf("verdict %s, want %s", r.Verdict, wantVerdict)
			}
			if got := slices.Contains(r.NotEvaluated, thinEntry); got != tc.thin {
				t.Errorf("not_evaluated %v, thin %v", r.NotEvaluated, tc.thin)
			}
			if !reflect.DeepEqual(r.AI, []AIHealth{tc.h}) {
				t.Errorf("ai = %+v", r.AI)
			}
		})
	}
}

func TestWithAIThinSamplesMakeHealthyReportInconclusive(t *testing.T) {
	base := Evaluate(input(t, true, svc("agent", 1000, 1000, 0, 0, 100, 100)))
	if base.Verdict != Pass || !base.AfterComplete {
		t.Fatalf("expected a complete healthy report, got %+v", base)
	}
	for _, tc := range []struct {
		name string
		h    AIHealth
	}{
		{"service", aiSvc(win(10, 0, 800), win(100, 0, 800))},
		{"tool", aiSvc(win(100, 0, 800), win(100, 0, 800),
			AITool{Tool: "rare", Before: AICalls{Calls: 3}, After: AICalls{Calls: 3}})},
		{"unscoped version", AIHealth{Service: "agent", Before: win(100, 0, 800), After: win(100, 0, 800)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := WithAI(base, []AIHealth{tc.h}, nil)
			if r.Verdict != Inconclusive || len(r.Findings) != 0 {
				t.Fatalf("unevaluated AI must not pass or produce findings: %+v", r)
			}
			if !slices.ContainsFunc(r.NotEvaluated, func(e string) bool { return strings.HasPrefix(e, NotEvaluatedAI) }) {
				t.Fatalf("missing AI uncertainty: %v", r.NotEvaluated)
			}
			if again := WithAI(r, nil, nil); !reflect.DeepEqual(again, r) {
				t.Errorf("not idempotent: %+v", again)
			}
			r = WithAI(r, []AIHealth{aiSvc(win(100, 0, 800), win(100, 0, 800))}, nil)
			if r.Verdict != Pass {
				t.Errorf("adequate AI sample should clear uncertainty: %+v", r)
			}
		})
	}
}

func TestWithAIMissingServicesAreInconclusive(t *testing.T) {
	base := Evaluate(input(t, true, healthy()))
	for _, hs := range [][]AIHealth{nil, {}, {aiSvc(win(100, 0, 800), win(100, 0, 800))}} {
		r := WithAI(base, hs, nil)
		want := NotEvaluatedAI + " 1 service(s) have no AI calls in either window: api"
		if r.Verdict != Inconclusive || len(r.Findings) != 0 || !slices.Contains(r.NotEvaluated, want) {
			t.Fatalf("missing service AI must be explicit uncertainty: %+v", r)
		}
		if again := WithAI(r, nil, nil); !reflect.DeepEqual(again, r) {
			t.Errorf("not idempotent: %+v", again)
		}
		complete := aiSvc(win(100, 0, 800), win(100, 0, 800))
		complete.Service = "api"
		if got := WithAI(r, []AIHealth{complete}, nil); got.Verdict != Pass {
			t.Errorf("complete service evidence should clear uncertainty: %+v", got)
		}
	}
}

func TestAIToolErrorsNameTheTool(t *testing.T) {
	h := aiSvc(win(100, 0, 800), win(100, 0, 800),
		AITool{Tool: "search_docs", Before: AICalls{Calls: 100, Errors: 1}, After: AICalls{Calls: 100, Errors: 30}},
		AITool{Tool: "fine", Before: AICalls{Calls: 100}, After: AICalls{Calls: 100, Errors: 2}},
		AITool{Tool: "rare", Before: AICalls{Calls: 3}, After: AICalls{Calls: 3, Errors: 3}})
	r := judged(t, nil, h)
	if got := codes(r.Findings); !slices.Equal(got, []string{release.HealthAIToolErrors}) {
		t.Fatalf("findings %v", got)
	}
	f := r.Findings[0]
	if f.Ref != "search_docs" || f.Service != "agent" || !strings.Contains(f.Message, "tool search_docs (agent): error rate 1.0% -> 30.0% (+29.0%, limit +10.0%; 30 errors in 100 calls)") {
		t.Errorf("finding %+v", f)
	}
	if !slices.Contains(r.NotEvaluated, NotEvaluatedAI+" 1 tool(s) below ai_min_calls (50 calls in each window): rare") {
		t.Errorf("not_evaluated %v", r.NotEvaluated)
	}
}

func TestAIFindingMessages(t *testing.T) {
	r := judged(t, nil, aiSvc(win(200, 2, 800), win(200, 40, 2000)))
	var e, l Finding
	for _, f := range r.Findings {
		switch f.Code {
		case release.HealthAIErrorRate:
			e = f
		case release.HealthAILatency:
			l = f
		}
	}
	if !strings.Contains(e.Message, "agent: AI call error rate 1.0% -> 20.0% (+19.0%, limit +5.0%; 40 of 200 model and tool calls failed)") || e.Threshold != DefaultAIMaxErrorRateIncrease {
		t.Errorf("error finding %+v", e)
	}
	if !strings.Contains(l.Message, "agent: model call p95 800ms -> 2.00s (2.50x, limit 1.5x)") || l.Threshold != DefaultAIMaxP95Ratio {
		t.Errorf("latency finding %+v", l)
	}
}

// A project that lists an AI code in fail_on fails the release on it.
func TestAIFindingFailsWhenFailOnNamesIt(t *testing.T) {
	failOn := append(slices.Clone(release.DefaultReleaseHealth().FailOn), release.HealthAIErrorRate)
	r := judged(t, failOn, aiSvc(win(100, 0, 800), win(100, 20, 2000)))
	if r.Verdict != Fail {
		t.Errorf("verdict %s, want FAIL", r.Verdict)
	}
	for _, f := range r.Findings {
		want := map[string]string{release.HealthAIErrorRate: PolicyFail, release.HealthAILatency: PolicyWarn}[f.Code]
		if f.Policy != want {
			t.Errorf("%s: policy %s, want %s", f.Code, f.Policy, want)
		}
	}
}

// Version scoping: a service whose after figures are not the judged
// release's is not judged, and says so.
func TestAIIsJudgedOnTheReleasesOwnCalls(t *testing.T) {
	h := aiSvc(win(100, 0, 800), win(100, 50, 5000))
	h.AfterVersion = ""
	r := judged(t, nil, h)
	if len(r.Findings) != 0 {
		t.Errorf("findings %v", codes(r.Findings))
	}
	if !slices.ContainsFunc(r.NotEvaluated, func(e string) bool {
		return strings.HasPrefix(e, NotEvaluatedAI+" 1 service(s) have no after-window AI calls of v2")
	}) {
		t.Errorf("not_evaluated %v", r.NotEvaluated)
	}
}

func TestWithAIIsIdempotentAndListsItsThresholds(t *testing.T) {
	hs := []AIHealth{aiSvc(win(100, 0, 800), win(100, 20, 2000))}
	got := judged(t, nil, hs...)
	if again := WithAI(got, hs, nil); !reflect.DeepEqual(again, got) {
		t.Errorf("not idempotent:\n got %+v\nwant %+v", again, got)
	}
	if own := WithAI(got, nil, nil); !reflect.DeepEqual(own, got) {
		t.Errorf("nil re-judges the report's own:\n got %+v\nwant %+v", own, got)
	}
	l := fromList(got.Thresholds)
	for name, want := range map[string]string{ThresholdAIMinCalls: "50", ThresholdAIMaxErrorRateIncrease: "0.05",
		ThresholdAIMinErrors: "5", ThresholdAIMaxP95Ratio: "1.5", ThresholdAIToolMaxErrorRateIncrease: "0.1"} {
		if l[name] != (Threshold{name, want, SourceDefault}) {
			t.Errorf("%s = %+v", name, l[name])
		}
	}
	names := make([]string, len(got.Thresholds))
	for i, th := range got.Thresholds {
		names[i] = th.Name
	}
	if i, j := slices.Index(names, ThresholdAIToolMaxErrorRateIncrease), slices.Index(names, ThresholdFailOn); i < 0 || i+1 != j {
		t.Errorf("the AI rows come just before fail_on: %v", names)
	}
}

func TestAIOverrides(t *testing.T) {
	hs := []AIHealth{aiSvc(win(30, 0, 800), win(30, 10, 800))}
	r := judged(t, nil, hs...)
	if len(r.Findings) != 0 {
		t.Fatalf("30 calls is thin under the default: %v", codes(r.Findings))
	}
	r = WithAI(r, hs, map[string]string{ThresholdAIMinCalls: "20", ThresholdAIMaxP95Ratio: "fast"})
	if got := codes(r.Findings); !slices.Equal(got, []string{release.HealthAIErrorRate}) {
		t.Errorf("with ai_min_calls=20: %v", got)
	}
	l := fromList(r.Thresholds)
	if l[ThresholdAIMinCalls] != (Threshold{ThresholdAIMinCalls, "20", SourceRequest}) || l[ThresholdAIMaxP95Ratio].Value != "1.5" {
		t.Errorf("thresholds %+v", r.Thresholds)
	}

	// Resolve accepts the AI names (WithAI applies them) and refuses a bad value.
	if _, err := ParseOverrides([]string{"ai_min_calls=20"}); err != nil {
		t.Errorf("ParseOverrides: %v", err)
	}
	if _, err := Resolve(Options{Overrides: map[string]string{ThresholdAIMinCalls: "20"}}); err != nil {
		t.Errorf("Resolve: %v", err)
	}
	for _, bad := range []map[string]string{{ThresholdAIMinCalls: "0"}, {ThresholdAIMaxErrorRateIncrease: "2"}, {ThresholdAIMaxP95Ratio: "1"}} {
		if _, err := Resolve(Options{Overrides: bad}); err == nil {
			t.Errorf("Resolve accepted %v", bad)
		}
	}
}

func TestAIHealthJSON(t *testing.T) {
	raw, err := json.Marshal(aiSvc(win(1, 0, 2), win(3, 1, 4), AITool{Tool: "t", After: AICalls{Calls: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"service":"agent","before":{"model":{"calls":1,"errors":0},"tool":{"calls":0,"errors":0},"model_p95_ms":2},` +
		`"after":{"model":{"calls":3,"errors":1},"tool":{"calls":0,"errors":0},"model_p95_ms":4},` +
		`"tools":[{"tool":"t","before":{"calls":0,"errors":0},"after":{"calls":1,"errors":0}}],"after_version":"v2"}`
	if string(raw) != want {
		t.Errorf("json\n got %s\nwant %s", raw, want)
	}
}
