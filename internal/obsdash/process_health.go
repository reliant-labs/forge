package obsdash

import "sort"

// An empty process inventory matches nothing, not every tenant service.
// Eq on an impossible service name is valid spec even with no deployments.
func identityScope(field string, values []string) []Predicate {
	if len(values) == 0 {
		return []Predicate{{Field: field, Op: OpEq, Value: "__forge_no_deployed_process__"}}
	}
	return []Predicate{{Field: field, Op: OpIn, Values: values}}
}

func sortedIdentities(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, v := range values {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

// rpcHealthPanels reads counters/histograms, not sampled server spans. Error
// classification is exactly the observe interceptor's error_class attribute.
func rpcHealthPanels() []Panel {
	calls := func(agg Agg) Series {
		return Series{Label: "requests", Agg: agg, Field: "rpc.server.calls", MetricType: "sum"}
	}
	errors := func(agg Agg) Series {
		return Series{Label: "server faults", Agg: agg, Field: "rpc.server.errors", MetricType: "sum", Where: []Predicate{{Field: "metric.attr.error_class", Op: OpEq, Value: "server"}}}
	}
	duration := func(q float64) Series {
		return Series{Label: "p95", Agg: AggQuantile, Quantile: q, Field: "rpc.server.duration", MetricType: "histogram"}
	}
	return []Panel{
		{ID: "requests", Title: "Requests", Kind: KindNumber, Signal: Metrics, Series: []Series{calls(AggIncrease)}, Unit: UnitCount, Width: 8, Height: 4},
		{ID: "errors", Title: "Server faults", Kind: KindNumber, Signal: Metrics, Series: []Series{errors(AggIncrease)}, Unit: UnitCount, Width: 8, Height: 4},
		{ID: "p95", Title: "p95 RPC duration", Kind: KindNumber, Signal: Metrics, Series: []Series{duration(.95)}, Unit: UnitSeconds, Width: 8, Height: 4},
		{ID: "request-rate", Title: "Request rate by process", Kind: KindLine, Signal: Metrics, Series: []Series{calls(AggRate)}, Unit: UnitCount, Width: 8, Height: 7},
		{ID: "error-rate", Title: "Server fault rate by process", Kind: KindLine, Signal: Metrics, Series: []Series{errors(AggRate), calls(AggRate)}, Ratio: true, Unit: UnitPercent, Width: 8, Height: 7},
		{ID: "latency", Title: "p95 duration by process", Kind: KindLine, Signal: Metrics, Series: []Series{duration(.95)}, Unit: UnitSeconds, Width: 8, Height: 7},
	}
}
