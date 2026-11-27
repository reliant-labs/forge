package release

import (
	"reflect"
	"slices"
	"testing"
)

// TestAICodesAreFindings: the AI codes are finding codes a policy may fail
// on, appended at the end (the order findings sort in), and the default
// policy fails on none of them (llm-agent-observability decision 9: new
// findings warn first).
func TestAICodesAreFindings(t *testing.T) {
	n := len(HealthFindings)
	if !reflect.DeepEqual(HealthFindings[n-3:], []string{HealthAIErrorRate, HealthAILatency, HealthAIToolErrors}) || HealthFindings[n-4] != HealthA11yRegressed {
		t.Errorf("HealthFindings = %v: the AI codes must be appended at the end", HealthFindings)
	}
	if !reflect.DeepEqual(AIFindings, HealthFindings[n-3:]) {
		t.Errorf("AIFindings = %v", AIFindings)
	}
	if HealthAIErrorRate != "ai_error_rate" || HealthAILatency != "ai_latency" || HealthAIToolErrors != "ai_tool_errors" {
		t.Error("the codes are a wire contract: ai_error_rate, ai_latency, ai_tool_errors")
	}
	def := DefaultReleaseHealth()
	for _, c := range AIFindings {
		if def.Fails(c) {
			t.Errorf("the default policy fails on %s; it warns", c)
		}
	}
	h := DefaultReleaseHealth()
	h.FailOn = append(h.FailOn, AIFindings...)
	if err := h.validate(); err != nil {
		t.Errorf("a policy failing on the AI codes: %v", err)
	}
}

// A control plane that predates the feature refuses a fail_on naming a code
// it does not know: it is sent the policy without the AI codes. A current
// reader gets the policy whole.
func TestLimitShapeWithholdsTheAICodesFromAnOldReader(t *testing.T) {
	old := &BundleReader{Features: slices.DeleteFunc(BundleDocFeatures(), func(f string) bool { return f == BundleFeatureReleaseHealthAI })}
	full := Shape{Kind: EnvSelfManaged, ReleaseHealth: func() *ShapeReleaseHealth {
		h := DefaultReleaseHealth()
		h.MinRequests = 20
		h.FailOn = append(slices.Clone(h.FailOn), HealthAIErrorRate, HealthLatency)
		return &h
	}()}
	got := old.LimitShape(full)
	if got.ReleaseHealth == nil || got.ReleaseHealth.MinRequests != 20 {
		t.Fatalf("the rest of the policy was lost: %+v", got.ReleaseHealth)
	}
	if slices.Contains(got.ReleaseHealth.FailOn, HealthAIErrorRate) || !slices.Contains(got.ReleaseHealth.FailOn, HealthLatency) {
		t.Errorf("fail_on = %v: the AI code is withheld, the rest kept", got.ReleaseHealth.FailOn)
	}
	if err := got.ReleaseHealth.validate(); err != nil {
		t.Errorf("what an old reader is sent: %v", err)
	}
	cur := &BundleReader{Features: BundleDocFeatures()}
	if g := cur.LimitShape(full); !slices.Contains(g.ReleaseHealth.FailOn, HealthAIErrorRate) {
		t.Errorf("a current reader lost the AI code: %v", g.ReleaseHealth.FailOn)
	}
}
