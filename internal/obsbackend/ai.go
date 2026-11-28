package obsbackend

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/reliant-labs/forge/pkg/observe/genai"
	"github.com/reliant-labs/forge/pkg/obsquery"
	"github.com/reliant-labs/forge/pkg/releasehealth"
)

// AIRequest reads one window's GenAI calls per service: what release
// health's AI findings compare (pkg/releasehealth ai.go).
type AIRequest struct {
	Narrow
	// Version, when set, counts only spans whose resource service.version
	// is it: a hosted promotion's after window, read as the judged release's
	// own calls.
	Version string
}

// AIResult is one window's GenAI calls, by service, with the statements run.
type AIResult struct {
	Services   []AIServiceCalls
	Statements []Statement
}

// AIServiceCalls are one service's GenAI calls over one window.
type AIServiceCalls struct {
	Service string
	Window  releasehealth.AIWindow
	// Tools are its calls by tool name.
	Tools map[string]releasehealth.AICalls
}

// The GenAI operations that are model calls (pkg/observe/genai).
var aiModelOps = []string{genai.OpChat, genai.OpTextCompletion, genai.OpGenerateContent, genai.OpEmbeddings}

// AIPanelID identifies the model and tool call counts panel: what a
// test double tells it apart by.
const AIPanelID = "ai-calls"

// aiPanel is one window's model and tool calls, by service and tool name:
// calls, failed calls (error status, a cancellation excepted: error.type
// canceled is no failure, as the AI hub counts it) and the p95 duration. A
// model call carries no tool name; each tool row is one tool's calls.
// A separate service-only panel reads model latency
// across all model operations.
func aiPanel(version string) obsquery.Panel {
	ops := append(slices.Clone(aiModelOps), genai.OpExecuteTool)
	where := []obsquery.PanelPredicate{{Field: "span.attr." + genai.AttrOperationName, Op: "in", Values: ops}}
	if version != "" {
		where = append(where, obsquery.PanelPredicate{Field: opsVersionField, Op: "eq", Value: version})
	}
	return obsquery.Panel{ID: AIPanelID, Kind: "table", Signal: "traces", Where: where,
		Series: []obsquery.PanelSeries{
			{Label: "calls", Agg: "count"},
			{Label: "errors", Agg: "count", Where: []obsquery.PanelPredicate{
				{Field: "span.status", Op: "eq", Value: "error"},
				{Field: "span.attr." + genai.AttrErrorType, Op: "neq", Value: genai.ErrorTypeCanceled},
			}},
			{Label: "p95", Agg: "quantile", Quantile: 0.95, Field: "span.duration"},
		},
		GroupBy: []string{"service.name", "span.attr." + genai.AttrOperationName, "span.attr." + genai.AttrToolName},
		Limit:   obsquery.MaxTableRows}
}

// aiModelLatencyPanel computes p95 over all model-call spans per service,
// without splitting the distribution by operation or tool attributes.
func aiModelLatencyPanel(version string) obsquery.Panel {
	p := aiPanel(version)
	p.ID = "ai-model-latency"
	p.Where[0].Values = slices.Clone(aiModelOps)
	p.GroupBy = []string{"service.name"}
	p.Series = []obsquery.PanelSeries{{Label: "p95", Agg: "quantile", Quantile: 0.95, Field: "span.duration"}}
	return p
}

// aiCalls runs the panels through run: Local and Cloud differ only in run.
func aiCalls(ctx context.Context, req AIRequest, run panelRun) (AIResult, error) {
	var res AIResult
	by := map[string]*AIServiceCalls{}
	count := func(v float64) int64 { return int64(finite(v) + 0.5) }
	rows, st, err := run(ctx, "ai calls", aiPanel(req.Version))
	res.Statements = append(res.Statements, st...)
	if err != nil {
		return res, err
	}
	latencies, st, err := run(ctx, "ai model latency", aiModelLatencyPanel(req.Version))
	res.Statements = append(res.Statements, st...)
	if err != nil || req.Explain {
		return res, err
	}
	// A full table may have been truncated by either backend. Without a
	// completeness signal, even exactly the cap cannot be safely judged.
	if len(rows) >= obsquery.MaxTableRows {
		return res, Undetermined("ai calls reached the table row limit (%d); incomplete AI counts cannot be evaluated", obsquery.MaxTableRows)
	}
	if len(latencies) >= obsquery.MaxTableRows {
		return res, Undetermined("ai model latency reached the table row limit (%d); incomplete AI latency cannot be evaluated", obsquery.MaxTableRows)
	}
	for _, r := range rows {
		if len(r.group) < 3 || len(r.values) < 3 {
			continue
		}
		name, op, tool := r.group[0], r.group[1], strings.TrimSpace(r.group[2])
		s := by[name]
		if s == nil {
			s = &AIServiceCalls{Service: name, Tools: map[string]releasehealth.AICalls{}}
			by[name] = s
		}
		c := releasehealth.AICalls{Calls: count(r.values[0]), Errors: count(r.values[1])}
		if op == genai.OpExecuteTool {
			s.Window.Tool.Calls += c.Calls
			s.Window.Tool.Errors += c.Errors
			if tool != "" {
				t := s.Tools[tool]
				t.Calls, t.Errors = t.Calls+c.Calls, t.Errors+c.Errors
				s.Tools[tool] = t
			}
			continue
		}
		s.Window.Model.Calls += c.Calls
		s.Window.Model.Errors += c.Errors
	}
	for _, r := range latencies {
		if len(r.group) < 1 || len(r.values) < 1 {
			continue
		}
		if s := by[r.group[0]]; s != nil {
			s.Window.ModelP95Ms = finite(r.values[0])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(by)) {
		res.Services = append(res.Services, *by[name])
	}
	return res, nil
}

// AI implements AIReader over the local ClickStack.
func (l Local) AI(ctx context.Context, req AIRequest) (AIResult, error) {
	return aiCalls(ctx, req, l.panelRunner(req.Narrow))
}

// AI implements AIReader: the AI panel, run by QueryPanel (structured
// panels every control plane with QueryPanel compiles).
func (c Cloud) AI(ctx context.Context, req AIRequest) (AIResult, error) {
	return aiCalls(ctx, req, c.panelRunner(req.Narrow))
}

// JoinAIHealth pairs each service's calls across the two windows, ordered
// by service, with its tools ordered by name. afterVersion is the version
// the after window was read at ("" for every version).
func JoinAIHealth(before, after []AIServiceCalls, afterVersion string) []releasehealth.AIHealth {
	type pair struct{ b, a *AIServiceCalls }
	by := map[string]*pair{}
	for i := range before {
		s := &before[i]
		if by[s.Service] == nil {
			by[s.Service] = &pair{}
		}
		by[s.Service].b = s
	}
	for i := range after {
		s := &after[i]
		if by[s.Service] == nil {
			by[s.Service] = &pair{}
		}
		by[s.Service].a = s
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]releasehealth.AIHealth, 0, len(names))
	for _, n := range names {
		p := by[n]
		h := releasehealth.AIHealth{Service: n, AfterVersion: afterVersion}
		tools := map[string]*releasehealth.AITool{}
		tool := func(name string) *releasehealth.AITool {
			if tools[name] == nil {
				tools[name] = &releasehealth.AITool{Tool: name}
			}
			return tools[name]
		}
		if p.b != nil {
			h.Before = p.b.Window
			for name, c := range p.b.Tools {
				tool(name).Before = c
			}
		}
		if p.a != nil {
			h.After = p.a.Window
			for name, c := range p.a.Tools {
				tool(name).After = c
			}
		}
		tn := make([]string, 0, len(tools))
		for t := range tools {
			tn = append(tn, t)
		}
		slices.Sort(tn)
		for _, t := range tn {
			h.Tools = append(h.Tools, *tools[t])
		}
		out = append(out, h)
	}
	return out
}
