package obsbackend

import (
	"context"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/observe/genai"
	"github.com/reliant-labs/forge/pkg/obsquery"
)

func TestAICallsRejectsCappedPanels(t *testing.T) {
	for _, panelID := range []string{AIPanelID, "ai-model-latency"} {
		t.Run(panelID, func(t *testing.T) {
			for _, size := range []int{obsquery.MaxTableRows - 1, obsquery.MaxTableRows, obsquery.MaxTableRows + 1} {
				run := func(_ context.Context, _ string, p obsquery.Panel) ([]panelRow, []Statement, error) {
					n := 1
					if p.ID == panelID {
						n = size
					}
					rows := make([]panelRow, n)
					for i := range rows {
						if p.ID == AIPanelID {
							rows[i] = panelRow{group: []string{"svc", genai.OpChat, ""}, values: []float64{1, 0, 10}}
						} else {
							rows[i] = panelRow{group: []string{"svc"}, values: []float64{10}}
						}
					}
					return rows, nil, nil
				}
				res, err := aiCalls(context.Background(), AIRequest{}, run)
				if size >= obsquery.MaxTableRows {
					if err == nil || !strings.Contains(err.Error(), "table row limit") {
						t.Fatalf("%d rows: want row-limit error, got %v", size, err)
					}
					if len(res.Services) != 0 {
						t.Fatalf("%d rows: returned partial services: %v", size, res.Services)
					}
				} else if err != nil || len(res.Services) != 1 {
					t.Fatalf("%d rows: want complete service, got %v, %v", size, res.Services, err)
				}
			}
		})
	}
}
