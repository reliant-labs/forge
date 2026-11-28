package obstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/pkg/obsquery"
	"github.com/reliant-labs/forge/pkg/testkit"
)

var (
	t0 = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

func ptr(v float64) *float64 { return &v }

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func TestAggWire(t *testing.T) {
	for _, c := range []struct {
		agg  obsquery.Agg
		q    float64
		want string
		wq   float64
	}{
		{obsquery.AggAvg, 0, "avg", 0},
		{obsquery.AggSum, 0, "sum", 0},
		{obsquery.AggMin, 0, "min", 0},
		{obsquery.AggMax, 0, "max", 0},
		{obsquery.AggCount, 0, "count", 0},
		{obsquery.AggIncrease, 0, "increase", 0},
		{obsquery.AggRate, 0, "rate", 0},
		{obsquery.AggLast, 0, "last", 0},
		{obsquery.AggP50, 0, "quantile", 0.5},
		{obsquery.AggP95, 0, "quantile", 0.95},
		{obsquery.AggP99, 0, "quantile", 0.99},
		{obsquery.AggQuantile, 0.9, "quantile", 0.9},
	} {
		got, q := aggWire(c.agg, c.q)
		if got != c.want || q != c.wq {
			t.Errorf("aggWire(%v, %v) = (%q, %v), want (%q, %v)", c.agg, c.q, got, q, c.want, c.wq)
		}
	}
}

func TestWireSeriesMetas(t *testing.T) {
	if got := wireSeriesMetas(nil); got != nil {
		t.Errorf("no series = %v, want nil", got)
	}
	got := wireSeriesMetas([]obsquery.SeriesMeta{
		{Label: "A", Metric: "calls", Info: obsquery.MetricInfo{Type: obsquery.MetricSum, Monotonic: true, Temporality: obsquery.TemporalityCumulative}, Agg: obsquery.AggRate, ValueUnit: "{call}/s"},
		{Label: "B", Info: obsquery.MetricInfo{Type: obsquery.MetricHistogram, Temporality: obsquery.TemporalityDelta}, Agg: obsquery.AggQuantile, Quantile: 0.9},
		{Label: "C", Info: obsquery.MetricInfo{Type: obsquery.MetricGauge}, Agg: obsquery.AggAvg},
	})
	want := []map[string]any{
		{"label": "A", "metricType": "METRIC_TYPE_SUM", "metric": "calls", "monotonic": true, "temporality": "METRIC_TEMPORALITY_CUMULATIVE", "aggregation": "rate", "valueUnit": "{call}/s"},
		{"label": "B", "metricType": "METRIC_TYPE_HISTOGRAM", "temporality": "METRIC_TEMPORALITY_DELTA", "aggregation": "quantile", "quantile": 0.9},
		{"label": "C", "metricType": "METRIC_TYPE_GAUGE", "aggregation": "avg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("metas = %#v\nwant   %#v", got, want)
	}
}

func TestGroupKeyIsOrderIndependentAndDistinguishesGroups(t *testing.T) {
	a := groupKey(map[string]string{"x": "1", "y": "2"})
	b := groupKey(map[string]string{"y": "2", "x": "1"})
	if a != b {
		t.Errorf("same group keyed differently: %s vs %s", a, b)
	}
	if a == groupKey(map[string]string{"x": "1"}) || groupKey(nil) != "null" {
		t.Errorf("different groups collided: %s", a)
	}
}

func TestCPPanelResponseLine(t *testing.T) {
	res := obsquery.MetricPanelResult{
		StepSeconds: 60, Truncated: true, SeriesTotal: 7, Warnings: []string{"step_raised"},
		Meta: []obsquery.SeriesMeta{{Label: "A", Metric: "heap", Info: obsquery.MetricInfo{Type: obsquery.MetricGauge}, Agg: obsquery.AggAvg}},
		Series: []obsquery.MetricSeriesResult{
			{Label: "A", Labels: map[string]string{"rpc.method": "List"}, Reduced: ptr(4), Points: []obsquery.MetricPoint{
				{Time: t0, Value: 3},
				{Time: t0.Add(time.Minute), Value: 0, Null: true},
				{Time: t0.Add(2 * time.Minute).In(time.FixedZone("x", 3600)), Value: 5, Partial: true},
			}},
			{Label: "A", Reduced: ptr(0)},
		},
	}
	m := decode(t, CPPanelResponse("line", res))
	if m["kind"] != "line" || m["stepSeconds"] != float64(60) || m["truncated"] != true || m["seriesTotal"] != float64(7) {
		t.Errorf("header = %v", m)
	}
	if !reflect.DeepEqual(m["warnings"], []any{"step_raised"}) {
		t.Errorf("warnings = %v", m["warnings"])
	}
	if meta, _ := m["seriesMeta"].([]any); len(meta) != 1 {
		t.Errorf("seriesMeta = %v", m["seriesMeta"])
	}
	series := m["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("series = %v", series)
	}
	s0 := series[0].(map[string]any)
	if s0["label"] != "A" || s0["value"] != float64(4) || !reflect.DeepEqual(s0["group"], map[string]any{"rpc.method": "List"}) {
		t.Errorf("series 0 = %v", s0)
	}
	pts := s0["points"].([]any)
	want := []map[string]any{
		{"time": "2026-10-22T10:00:00Z", "value": float64(3)},
		{"time": "2026-10-22T10:01:00Z", "null": true},
		{"time": "2026-10-22T10:02:00Z", "value": float64(5), "partial": true},
	}
	for i, w := range want {
		if !reflect.DeepEqual(pts[i], any(w)) {
			t.Errorf("point %d = %v, want %v (zero values are omitted, times are UTC)", i, pts[i], w)
		}
	}
	if s1 := series[1].(map[string]any); len(s1) != 1 || s1["label"] != "A" {
		t.Errorf("a zero reduction and no points are omitted, got %v", s1)
	}
}

func TestCPPanelResponseOmitsEmptyHeader(t *testing.T) {
	m := decode(t, CPPanelResponse("line", obsquery.MetricPanelResult{}))
	if len(m) != 1 || m["kind"] != "line" {
		t.Errorf("an empty result is just its kind, got %v", m)
	}
}

func TestCPPanelResponseTableAndBarPivotSeriesIntoRows(t *testing.T) {
	res := obsquery.MetricPanelResult{Series: []obsquery.MetricSeriesResult{
		{Label: "A", Labels: map[string]string{"m": "List"}, Reduced: ptr(2)},
		{Label: "B", Labels: map[string]string{"m": "List"}, Reduced: ptr(0.5)},
		{Label: "A", Labels: map[string]string{"m": "Get"}, Reduced: ptr(1)},
		{Label: "B", Labels: map[string]string{"m": "Get"}},
	}}
	for _, kind := range []string{"table", "bar"} {
		m := decode(t, CPPanelResponse(kind, res))
		if !reflect.DeepEqual(m["columns"], []any{"A", "B"}) {
			t.Errorf("%s columns = %v", kind, m["columns"])
		}
		rows := m["rows"].([]any)
		want := []map[string]any{
			{"group": map[string]any{"m": "List"}, "values": []any{2.0, 0.5}},
			{"group": map[string]any{"m": "Get"}, "values": []any{1.0, 0.0}},
		}
		if len(rows) != 2 || !reflect.DeepEqual(rows[0], any(want[0])) || !reflect.DeepEqual(rows[1], any(want[1])) {
			t.Errorf("%s rows = %v, want %v (a null reduction is 0, groups keep first-seen order)", kind, rows, want)
		}
		if _, ok := m["series"]; ok {
			t.Errorf("%s carries series: %v", kind, m)
		}
	}
	m := decode(t, CPPanelResponse("table", obsquery.MetricPanelResult{Series: []obsquery.MetricSeriesResult{{Label: "A", Reduced: ptr(3)}}}))
	if row := m["rows"].([]any)[0].(map[string]any); row["group"] != nil || !reflect.DeepEqual(row["values"], []any{3.0}) {
		t.Errorf("an ungrouped row has no group: %v", row)
	}
}

func stmt(sql string, params ...string) obsquery.Stmt {
	p := map[string]string{}
	for i, v := range params {
		p[fmt.Sprintf("p%d", i)] = v
	}
	return obsquery.Stmt{SQL: sql, Params: p}
}

func TestFakeMetricsRunAnswersEachStatementKind(t *testing.T) {
	seen := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	f := &FakeMetrics{
		StoredTraces: []string{"aa", "bb"},
		Metrics: map[string]FakeMetric{
			"rpc.calls": {
				Kind: obsquery.CatalogSum, Unit: "{call}", Description: "calls", Monotonic: true, Temporality: 2,
				Services: []string{"api", "worker"}, Series: 4, IntervalSec: 30, LastSeen: seen,
				Attrs:        []FakeAttr{{Key: "method", Distinct: 3}, {Key: "host", Resource: true, Distinct: 1}},
				DeclaredKeys: []string{"method"},
				Points:       []FakePoint{{Bucket: t0, Labels: []string{"List"}, Value: 2}, {Bucket: t0, Labels: []string{"Get"}, Value: 1}, {Bucket: t0.Add(time.Minute), Labels: []string{"Get"}, Value: 3}},
				Reduced:      []FakePoint{{Bucket: t0, Labels: []string{"List"}, Value: 9}},
				Exemplars:    []FakeExemplar{{Time: seen, Value: 1.5, TraceID: "aa", SpanID: "s1", Attrs: map[string]string{"k": "v"}}},
			},
			"heap": {Kind: obsquery.CatalogGauge, Total: 11, Points: []FakePoint{{Bucket: t0, Labels: []string{"x"}, Value: 1}}},
		},
	}
	ctx := context.Background()
	run := func(st obsquery.Stmt) obsbackend.Result {
		t.Helper()
		r, err := f.Run(ctx, st, obsbackend.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("catalog lists every metric sorted, filtered by name substring", func(t *testing.T) {
		r := run(stmt("SELECT x AS last_seen"))
		if len(r.Rows) != 2 || r.Rows[0][0] != "heap" || r.Rows[1][0] != "rpc.calls" {
			t.Fatalf("catalog = %v", r.Rows)
		}
		row := r.Rows[1]
		if row[1] != json.Number("2") || row[2] != "{call}" || row[3] != "calls" || row[4] != json.Number("1") || row[5] != json.Number("2") ||
			!reflect.DeepEqual(row[6], []any{"api", "worker"}) || row[7] != json.Number("2") || row[8] != "2026-10-22 09:30:00" || row[9] != json.Number("4") {
			t.Errorf("rpc.calls row = %#v", row)
		}
		if r.Rows[0][9] != json.Number("1") {
			t.Errorf("a metric with no series count reports 1, got %v", r.Rows[0][9])
		}
		filtered := run(stmt("SELECT positionCaseInsensitive(MetricName, {p0:String}) AS last_seen", "RPC"))
		if len(filtered.Rows) != 1 || filtered.Rows[0][0] != "rpc.calls" {
			t.Errorf("--contains RPC = %v", filtered.Rows)
		}
	})

	t.Run("lookup reports the kind, unit, series count and export interval", func(t *testing.T) {
		r := run(stmt("SELECT 1 AS interval", "rpc.calls"))
		want := []any{json.Number("2"), "{call}", json.Number("1"), json.Number("2"), json.Number("0"), json.Number("4"), json.Number("100"), "1970-01-01 00:00:00", "1970-01-01 00:00:00", json.Number("30")}
		if len(r.Rows) != 1 || !reflect.DeepEqual(r.Rows[0], want) {
			t.Errorf("lookup = %#v", r.Rows)
		}
		d := run(stmt("SELECT 1 AS interval", "heap")).Rows[0]
		if d[5] != json.Number("1") || d[9] != json.Number("60") {
			t.Errorf("defaults are one series every 60s, got %v", d)
		}
	})

	t.Run("attributes, declared keys and exemplars", func(t *testing.T) {
		if got := run(stmt("SELECT distinct_values", "rpc.calls")).Rows; !reflect.DeepEqual(got, [][]any{{"method", json.Number("0"), json.Number("3")}, {"host", json.Number("1"), json.Number("1")}}) {
			t.Errorf("attributes = %#v", got)
		}
		if got := run(stmt("SELECT arrayJoin(mapKeys(Attributes))", "rpc.calls")).Rows; !reflect.DeepEqual(got, [][]any{{"method"}}) {
			t.Errorf("declared keys = %#v", got)
		}
		ex := run(stmt("SELECT `Exemplars.TimeUnix`", "rpc.calls")).Rows
		if len(ex) != 1 || ex[0][0] != "2026-10-22 09:30:00" || ex[0][1] != json.Number("1.5") || ex[0][2] != "aa" || ex[0][3] != "s1" ||
			!reflect.DeepEqual(ex[0][4], map[string]any{"k": "v"}) {
			t.Errorf("exemplars = %#v", ex)
		}
		if got := run(stmt("SELECT DISTINCT TraceId")).Rows; !reflect.DeepEqual(got, [][]any{{"aa"}, {"bb"}}) {
			t.Errorf("stored traces = %v", got)
		}
	})

	t.Run("series answer a line with its distinct-group total, and a reduction its reduced rows", func(t *testing.T) {
		line := run(stmt("SELECT toStartOfInterval(TimeUnix, x)", "rpc.calls"))
		if len(line.Rows) != 3 || line.Rows[0][0] != "2026-10-22 10:00:00" || !reflect.DeepEqual(line.Rows[0][1], []any{"List"}) ||
			line.Rows[0][2] != json.Number("2") || line.Rows[0][3] != json.Number("2") {
			t.Errorf("line rows = %#v (2 distinct groups: List, Get)", line.Rows)
		}
		red := run(stmt("SELECT sum(Value)", "rpc.calls"))
		if len(red.Rows) != 1 || red.Rows[0][2] != json.Number("9") {
			t.Errorf("reduced rows = %#v", red.Rows)
		}
		if got := run(stmt("SELECT toStartOfInterval(TimeUnix, x)", "heap")).Rows[0][3]; got != json.Number("11") {
			t.Errorf("an explicit Total wins over the distinct-group count, got %v", got)
		}
	})

	t.Run("a statement naming no known metric answers nothing", func(t *testing.T) {
		for _, sql := range []string{"SELECT 1 AS interval", "SELECT distinct_values", "SELECT arrayJoin(mapKeys(Attributes))", "SELECT `Exemplars.TimeUnix`", "SELECT toStartOfInterval(x)"} {
			if r := run(stmt(sql, "nope")); len(r.Rows) != 0 {
				t.Errorf("%s: rows = %v", sql, r.Rows)
			}
		}
	})

	t.Run("Err fails every statement but is still recorded", func(t *testing.T) {
		boom := errors.New("boom")
		bad := &FakeMetrics{Err: boom}
		if _, err := bad.Run(ctx, stmt("SELECT 1"), obsbackend.DefaultLimits()); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
		if len(bad.Ran()) != 1 {
			t.Errorf("ran = %v", bad.Ran())
		}
	})

	if got := len(f.Ran()); got < 10 {
		t.Errorf("Ran recorded %d statements", got)
	}
}

func TestFakeMetricsRanIsACopyAndSafeForConcurrentUse(t *testing.T) {
	f := &FakeMetrics{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Run(context.Background(), obsquery.Stmt{SQL: "SELECT 1"}, obsbackend.DefaultLimits())
		}()
	}
	wg.Wait()
	ran := f.Ran()
	if len(ran) != 8 {
		t.Fatalf("ran = %d, want 8", len(ran))
	}
	ran[0].SQL = "mutated"
	if f.Ran()[0].SQL != "SELECT 1" {
		t.Error("Ran returns the internal slice")
	}
}

func TestSortedNamesAndHelpers(t *testing.T) {
	got := sortedNames(map[string]FakeMetric{"b": {}, "c": {}, "a": {}})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("sortedNames = %v", got)
	}
	if b2i(true) != 1 || b2i(false) != 0 {
		t.Error("b2i")
	}
	if got := chTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 7200))); got != "2026-01-02 01:04:05" {
		t.Errorf("chTime = %q, want UTC", got)
	}
	if got := strs([]string{"a", "b"}); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Errorf("strs = %v", got)
	}
}

// The evaluator's own reads, answered by FakeMetrics, produce a panel result
// that CPPanelResponse renders into the control plane's wire shape.
func TestCPPanelResponseOfARealEvaluation(t *testing.T) {
	f := &FakeMetrics{Metrics: map[string]FakeMetric{
		"heap": {Kind: obsquery.CatalogGauge, Unit: "By", Series: 1, Services: []string{"api"}, LastSeen: t1,
			Points: []FakePoint{{Bucket: t0, Value: 10}, {Bucket: t0.Add(time.Minute), Value: 30}}},
	}}
	run := func(ctx context.Context, st obsquery.Stmt) (obsquery.Rows, error) {
		r, err := f.Run(ctx, st, obsbackend.DefaultLimits())
		if err != nil {
			return nil, err
		}
		return obsbackend.RowsOf(r), nil
	}
	p := obsquery.Panel{ID: "p", Kind: "line", Signal: "metrics", Series: []obsquery.PanelSeries{{Label: "A", Field: "heap", Agg: "avg", MetricType: "gauge"}}}
	scope := obsquery.Scope{Database: obsquery.LocalDatabase, Local: true, Start: t0, End: t1}
	res, err := obsquery.EvalMetricPanel(context.Background(), run, scope, p, obsquery.EvalOptions{Now: t1.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, CPPanelResponse("line", res))
	series, _ := m["series"].([]any)
	if len(series) != 1 {
		t.Fatalf("response = %v", m)
	}
	pts := series[0].(map[string]any)["points"].([]any)
	var vals []string
	for _, p := range pts {
		vals = append(vals, fmt.Sprint(p.(map[string]any)["value"]))
	}
	if strings.Join(vals, ",") != "10,30" {
		t.Errorf("point values = %v, want the canned 10,30", vals)
	}
	meta := m["seriesMeta"].([]any)[0].(map[string]any)
	if meta["metricType"] != "METRIC_TYPE_GAUGE" || meta["aggregation"] != "avg" || meta["metric"] != "heap" {
		t.Errorf("meta = %v", meta)
	}
}

func TestCPPanelResponseOmitsEmptyCollections(t *testing.T) {
	for _, kind := range []string{"line", "table", "bar"} {
		m := decode(t, CPPanelResponse(kind, obsquery.MetricPanelResult{}))
		for _, k := range []string{"series", "columns", "rows"} {
			if v, ok := m[k]; ok {
				t.Errorf("%s: %q = %v present on an empty result; protojson omits an empty repeated field", kind, k, v)
			}
		}
	}
}

func TestStartBootsClickHouseAndSeedWritesTheFixture(t *testing.T) {
	testkit.Slow(t, "boots a ClickHouse container")
	runner, write := Start(t)
	now := time.Now().UTC().Truncate(time.Second)
	Seed(t, write, now)

	count := func(sql string) string {
		t.Helper()
		r, err := runner.Run(context.Background(), obsquery.Stmt{SQL: sql}, obsbackend.DefaultLimits())
		if err != nil || len(r.Rows) != 1 {
			t.Fatalf("%s: %v %v", sql, r.Rows, err)
		}
		return fmt.Sprint(r.Rows[0][0])
	}
	for sql, want := range map[string]string{
		"SELECT count() FROM otel_traces WHERE TraceId = '" + TraceID + "'":                                "14",
		"SELECT count() FROM otel_traces WHERE ResourceAttributes['deployment.environment.name'] = 'prod'": "1",
		"SELECT count() FROM otel_logs":                                              "4",
		"SELECT count() FROM otel_metrics_sum WHERE MetricName = 'rpc.server.calls'": "12",
		"SELECT count() FROM otel_metrics_gauge WHERE MetricName = 'heap'":           "5",
	} {
		if got := count(sql); got != want {
			t.Errorf("%s = %s, want %s", sql, got, want)
		}
	}
}
