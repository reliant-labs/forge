package serverkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/reliant-labs/forge/pkg/observe"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/serverkit"
)

// probeSeen is what one request's handler saw.
type probeSeen struct {
	header string // X-Reliant-Probe as the handler read it
	probe  bool   // observe.IsProbe on the handler's context
}

// probeServer is a real Connect server built the way serverkit.Run builds one:
// a NewChain chain (otelconnect Trace slot, the chain's logger and meter) under
// Edge.Wrap, so the request crosses the same layers a hosted workload's does.
type probeServer struct {
	url   string
	spans *tracetest.SpanRecorder
	logs  *bytes.Buffer
	read  *sdkmetric.ManualReader
	seen  *probeSeen
}

const probeProcedure = "/test.v1.ProbeService/Ping"

func newProbeServer(t *testing.T) probeServer {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	trace, err := otelconnect.NewInterceptor(otelconnect.WithTracerProvider(tp), otelconnect.WithTrustRemote())
	if err != nil {
		t.Fatal(err)
	}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	logs := &bytes.Buffer{}
	logger := slog.New(observe.NewLogHandler(slog.NewJSONHandler(logs, nil), observe.Config{ServiceName: "probe-test"}))

	chain, err := serverkit.NewChain(serverkit.ChainDeps{
		Auth:   serverkit.AuthFor(serverkit.AudienceUser, tokenAuth{token: "t"}),
		Trace:  trace,
		Meter:  mp.Meter("probe.test"),
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := &probeSeen{}
	mux := http.NewServeMux()
	mux.Handle(probeProcedure, connect.NewUnaryHandler(probeProcedure,
		func(ctx context.Context, req *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			seen.header = req.Header().Get(release.ProbeEdgeHeader)
			seen.probe = observe.IsProbe(ctx)
			logger.InfoContext(ctx, "handled")
			observe.Track(observe.WithLogger(ctx, logger), "pinged")
			return connect.NewResponse(&emptypb.Empty{}), nil
		}, chain.HandlerOptions()...))

	edge, err := serverkit.Edge{}.Wrap(serverkit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(edge(mux))
	t.Cleanup(srv.Close)
	return probeServer{url: srv.URL, spans: spans, logs: logs, read: reader, seen: seen}
}

func (s probeServer) call(t *testing.T, probe []string) {
	t.Helper()
	client := connect.NewClient[emptypb.Empty, emptypb.Empty](http.DefaultClient, s.url+probeProcedure)
	req := connect.NewRequest(&emptypb.Empty{})
	req.Header().Set("Authorization", "Bearer t")
	for _, v := range probe {
		req.Header().Add(release.ProbeEdgeHeader, v)
	}
	if _, err := client.CallUnary(t.Context(), req); err != nil {
		t.Fatal(err)
	}
}

// probeAttr returns the forge.probe attribute of the request's server span, and
// whether it has one.
func (s probeServer) probeAttr(t *testing.T) (attribute.Value, bool) {
	t.Helper()
	var server sdktrace.ReadOnlySpan
	for _, sp := range s.spans.Ended() {
		if sp.Name() == strings.TrimPrefix(probeProcedure, "/") || sp.Name() == probeProcedure {
			server = sp
		}
	}
	if server == nil {
		t.Fatalf("no server span among %d ended spans", len(s.spans.Ended()))
	}
	for _, kv := range server.Attributes() {
		if string(kv.Key) == release.ProbeAttr {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// logLines decodes every line written while serving the request.
func (s probeServer) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.logs.String()), "\n") {
		if line == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatal("the request wrote no log lines")
	}
	return out
}

func (s probeServer) rpcCalls(t *testing.T) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := s.read.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "rpc.server.calls" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				n += dp.Value
			}
		}
	}
	return n
}

// A hosted workload honours the edge's X-Reliant-Probe stamp only when it
// trusts its edge (FORGE_TRUSTED_EDGE=reliant), and a handler never sees the
// header either way. A request it honours is marked forge.probe=true on its
// server span and its log lines, as obsquery's real-traffic filters
// (SpanNotProbeSQL: SpanAttributes['forge.probe'] != 'true', LogNotProbeSQL
// likewise) read it, and is left out of the rpc metrics and product events.
func TestEdgeMarksReleaseProbesOnlyBehindATrustedEdge(t *testing.T) {
	cases := []struct {
		name    string
		edge    string // FORGE_TRUSTED_EDGE
		header  []string
		isProbe bool
	}{
		{name: "trusted edge stamped a probe", edge: "reliant", header: []string{"1"}, isProbe: true},
		{name: "no trusted edge: a client's header", edge: "", header: []string{"1"}},
		{name: "another edge: a client's header", edge: "none", header: []string{"1"}},
		{name: "trusted edge, real request", edge: "reliant"},
		{name: "trusted edge, value true is not the stamp", edge: "reliant", header: []string{"true"}},
		{name: "trusted edge, value 0 is not the stamp", edge: "reliant", header: []string{"0"}},
		{name: "trusted edge, a list value is not the stamp", edge: "reliant", header: []string{"1, 1"}},
		{name: "trusted edge, a repeated header is not the stamp", edge: "reliant", header: []string{"1", "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(observe.EnvTrustedEdge, tc.edge)
			s := newProbeServer(t)
			s.call(t, tc.header)

			if s.seen.header != "" {
				t.Errorf("the handler read %s: %q; the edge layer must delete it", release.ProbeEdgeHeader, s.seen.header)
			}
			if s.seen.probe != tc.isProbe {
				t.Errorf("observe.IsProbe in the handler = %v, want %v", s.seen.probe, tc.isProbe)
			}

			v, ok := s.probeAttr(t)
			switch {
			case tc.isProbe && !ok:
				t.Errorf("server span has no %s attribute", release.ProbeAttr)
			case tc.isProbe && (v.Type() != attribute.BOOL || v.Emit() != "true"):
				// The ClickHouse exporter stores Emit(); SpanNotProbeSQL compares it to 'true'.
				t.Errorf("server span %s = %s %q, want bool true", release.ProbeAttr, v.Type(), v.Emit())
			case !tc.isProbe && ok:
				t.Errorf("a real request's server span carries %s=%s", release.ProbeAttr, v.Emit())
			}

			product := false
			for _, line := range s.logLines(t) {
				got, has := line[release.ProbeAttr]
				if tc.isProbe && got != true {
					t.Errorf("probe log line %q has %s=%v (present %v), want true", line["msg"], release.ProbeAttr, got, has)
				}
				if !tc.isProbe && has {
					t.Errorf("real log line %q carries %s=%v", line["msg"], release.ProbeAttr, got)
				}
				if strings.HasPrefix(line["msg"].(string), observe.EventNamePrefix) {
					product = true
				}
			}
			if product == tc.isProbe {
				t.Errorf("product event recorded = %v for probe = %v: Track must record real requests only", product, tc.isProbe)
			}

			wantCalls := int64(1)
			if tc.isProbe {
				wantCalls = 0
			}
			if got := s.rpcCalls(t); got != wantCalls {
				t.Errorf("rpc.server.calls = %d, want %d", got, wantCalls)
			}
		})
	}
}
