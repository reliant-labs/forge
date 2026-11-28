package obsbackend

import (
	"slices"
	"testing"

	"github.com/reliant-labs/forge/pkg/observe/genai"
)

func TestAIModelLatencyCombinesOperations(t *testing.T) {
	p := aiModelLatencyPanel("release-1")
	if !slices.Equal(p.GroupBy, []string{"service.name"}) {
		t.Fatalf("model latency must aggregate all operations per service, got groups %v", p.GroupBy)
	}
	if len(p.Series) != 1 || p.Series[0].Agg != "quantile" || p.Series[0].Quantile != 0.95 || p.Series[0].Field != "span.duration" {
		t.Fatalf("want model span duration p95, got %+v", p.Series)
	}
	if len(p.Where) != 2 || p.Where[0].Field != "span.attr."+genai.AttrOperationName || p.Where[0].Op != "in" || !slices.Equal(p.Where[0].Values, aiModelOps) {
		t.Fatalf("want every model operation and no tool calls, got %+v", p.Where)
	}
	if p.Where[1].Field != opsVersionField || p.Where[1].Value != "release-1" {
		t.Fatalf("release version filter lost: %+v", p.Where)
	}
	if len(aiModelLatencyPanel("").Where) != 1 {
		t.Fatal("unversioned model latency must not filter by release")
	}
}
