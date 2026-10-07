package generator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TestGenerateGrafanaDashboards_ValidJSONAndRealSeries writes the dashboards
// for a sample project and asserts (a) every dashboard is valid JSON after
// the project-name substitution, and (b) every overview metric query names a
// series the runtime actually emits.
//
// (b) has failed twice. The RED row first queried
// http_server_request_duration_seconds, which nothing emits; then otelconnect
// moved from `rpc.server.duration` (milliseconds) to the current semantic
// conventions' `rpc.server.call.duration` (seconds) and the panels, still on
// rpc_server_duration_milliseconds, went blank again with the app exporting
// the whole time. So the RPC names asserted here come from otelconnect
// ITSELF (otelconnectServerSeries), not from a list typed into this file.
func TestGenerateGrafanaDashboards_ValidJSONAndRealSeries(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateGrafanaDashboards("sampleproj", dir); err != nil {
		t.Fatalf("GenerateGrafanaDashboards: %v", err)
	}

	dashDir := filepath.Join(dir, "deploy", "observability", "grafana", "dashboards")
	for _, name := range []string{"overview-dashboard.json", "logs-dashboard.json", "traces-dashboard.json"} {
		raw, err := os.ReadFile(filepath.Join(dashDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		if strings.Contains(string(raw), "{{PROJECT_NAME}}") {
			t.Fatalf("%s still carries the unsubstituted project-name placeholder", name)
		}
	}

	overview, err := os.ReadFile(filepath.Join(dashDir, "overview-dashboard.json"))
	if err != nil {
		t.Fatalf("read overview: %v", err)
	}
	var doc struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(overview, &doc); err != nil {
		t.Fatal(err)
	}

	emitted, labels := otelconnectServerSeries(t)
	// What the rest of the runtime exports over OTLP: the instrumented sql
	// driver's pool and query timing. Read off a running scaffold's
	// Prometheus; there is no in-process emitter to ask.
	for _, s := range []string{
		"go_sql_query_timing_milliseconds_count", "go_sql_query_timing_milliseconds_bucket",
		"go_sql_connections_in_use", "go_sql_connections_idle",
	} {
		emitted[s] = true
	}

	metricName := regexp.MustCompile(`([a-z_][a-z0-9_]*)\{`)
	queried := 0
	for _, p := range doc.Panels {
		for _, target := range p.Targets {
			for _, m := range metricName.FindAllStringSubmatch(target.Expr, -1) {
				queried++
				if !emitted[m[1]] {
					t.Errorf("panel %q queries %s, which the runtime does not emit — the panel is permanently empty:\n%s",
						p.Title, m[1], target.Expr)
				}
			}
			// Every RPC label a panel groups or filters by must be one the
			// interceptor sets.
			for _, l := range regexp.MustCompile(`\b(rpc_[a-z_]+)\b`).FindAllString(target.Expr, -1) {
				if strings.HasPrefix(l, "rpc_server_") {
					continue // a metric name, not a label
				}
				if !labels[l] {
					t.Errorf("panel %q uses label %s, which otelconnect does not set (it sets %v):\n%s", p.Title, l, labels, target.Expr)
				}
			}
		}
	}
	if queried == 0 {
		t.Fatal("found no metric selectors in the overview dashboard — this guard has gone blind")
	}
	if !strings.Contains(string(overview), `job=\"sampleproj\"`) {
		t.Errorf("overview queries are not scoped to the project's job (the OTLP service.name)")
	}
}

// otelconnectServerSeries runs one RPC through the otelconnect server
// interceptor the scaffold wires, collects what it records, and returns the
// Prometheus series names and labels the lgtm collector writes for it.
//
// The translation is the OTLP→Prometheus one: dots become underscores, the
// unit is appended ("s" → "_seconds", "ms" → "_milliseconds"), and a
// histogram becomes _bucket/_sum/_count.
func otelconnectServerSeries(t *testing.T) (series, labels map[string]bool) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	interceptor, err := otelconnect.NewInterceptor(otelconnect.WithMeterProvider(mp))
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/demo.v1.DemoService/Ping", connect.NewUnaryHandler("/demo.v1.DemoService/Ping",
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			return connect.NewResponse(&emptypb.Empty{}), nil
		}, connect.WithInterceptors(interceptor)))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+"/demo.v1.DemoService/Ping")
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatalf("ping: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	units := map[string]string{"s": "_seconds", "ms": "_milliseconds", "By": "_bytes", "1": ""}
	series, labels = map[string]bool{}, map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "rpc.server.") {
				continue
			}
			base := strings.ReplaceAll(m.Name, ".", "_") + units[m.Unit]
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				if h64, ok := m.Data.(metricdata.Histogram[int64]); ok {
					for _, dp := range h64.DataPoints {
						for _, kv := range dp.Attributes.ToSlice() {
							labels[strings.ReplaceAll(string(kv.Key), ".", "_")] = true
						}
					}
				}
				series[base] = true
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					series[base+suffix] = true
				}
				continue
			}
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				series[base+suffix] = true
			}
			for _, dp := range hist.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					labels[strings.ReplaceAll(string(kv.Key), ".", "_")] = true
				}
			}
		}
	}
	if len(series) == 0 {
		t.Fatal("otelconnect recorded no rpc.server.* instruments — this guard has gone blind")
	}
	return series, labels
}
