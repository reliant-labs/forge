package obsbackend

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/releasehealth"
)

func TestEmptyAIWindowsDoNotProveHealthyRelease(t *testing.T) {
	thresholds, err := releasehealth.Resolve(releasehealth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	base := releasehealth.Evaluate(releasehealth.Input{
		Windows: releasehealth.Windows{AfterComplete: true},
		Services: []releasehealth.ServiceHealth{{
			Service: "agent",
			Before:  releasehealth.WindowStats{Requests: 1000, P95Ms: 100},
			After:   releasehealth.WindowStats{Requests: 1000, P95Ms: 100},
		}},
		Thresholds: thresholds,
	})
	if base.Verdict != releasehealth.Pass {
		t.Fatalf("expected healthy base report: %+v", base)
	}
	before, after := AIResult{}, AIResult{}
	r := releasehealth.WithAI(base, JoinAIHealth(before.Services, after.Services, ""), nil)
	if r.Verdict != releasehealth.Inconclusive || len(r.Findings) != 0 {
		t.Fatalf("empty AI windows must not prove a release good: %+v", r)
	}
	for _, entry := range r.NotEvaluated {
		if strings.HasPrefix(entry, releasehealth.NotEvaluatedAI) && strings.Contains(entry, "agent") {
			return
		}
	}
	t.Fatalf("missing named AI uncertainty: %v", r.NotEvaluated)
}
