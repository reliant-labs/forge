package observe

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/reliant-labs/forge/pkg/release"
)

// observe declares the probe contract again rather than import pkg/release
// into every workload; the two must never drift.
func TestProbeContractIsRelease(t *testing.T) {
	if HeaderProbe != release.ProbeEdgeHeader {
		t.Errorf("HeaderProbe = %q, release.ProbeEdgeHeader = %q", HeaderProbe, release.ProbeEdgeHeader)
	}
	if AttrProbe != release.ProbeAttr {
		t.Errorf("AttrProbe = %q, release.ProbeAttr = %q", AttrProbe, release.ProbeAttr)
	}
}

func TestTrustedEdgeIsOnlyReliant(t *testing.T) {
	for v, want := range map[string]bool{"reliant": true, " reliant ": true, "": false, "none": false, "Reliant": false, "1": false} {
		t.Setenv(EnvTrustedEdge, v)
		if got := TrustedEdge(); got != want {
			t.Errorf("TrustedEdge() with %s=%q = %v, want %v", EnvTrustedEdge, v, got, want)
		}
	}
}

// ProbeFromEdge reads its trust once, when it is built: flipping the variable
// afterwards changes nothing for that listener.
func TestProbeFromEdgeReadsTrustWhenBuilt(t *testing.T) {
	var marked bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { marked = IsProbe(r.Context()) })
	t.Setenv(EnvTrustedEdge, "")
	h := ProbeFromEdge(next)
	t.Setenv(EnvTrustedEdge, TrustedEdgeReliant)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderProbe, "1")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if marked {
		t.Fatal("a listener built without a trusted edge honoured the probe header")
	}
	if r.Header.Get(HeaderProbe) != "" {
		t.Fatal("the header survived ProbeFromEdge")
	}
}

// Behind a trusted edge, the span TracingInterceptor opens (Deps.Tracer) is
// marked too, not only the Trace slot's server span; and the OTLP log record
// carries the attribute as LogNotProbeSQL reads it (LogAttributes['forge.probe']
// = 'true').
func TestProbeMarksTracingSpanAndOTLPLogs(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	prev := otel.GetLoggerProvider()
	t.Cleanup(func() { otel.SetLoggerProvider(prev) })
	bridge := &captureProcessor{}
	otel.SetLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(bridge)))

	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	var stdout bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&stdout, nil), Config{ServiceName: "svc"}))

	const procedure = "/observe.test.v1.ProbeService/Ping"
	mux := http.NewServeMux()
	mux.Handle(procedure, connect.NewUnaryHandler(procedure,
		func(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			logger.InfoContext(ctx, "handled")
			return connect.NewResponse(&emptypb.Empty{}), nil
		},
		connect.WithInterceptors(Chain(Deps{Logger: logger, Tracer: tp.Tracer("t")})...),
	))
	srv := httptest.NewServer(probeFromEdge(mux, true))
	t.Cleanup(srv.Close)

	for _, probe := range []bool{true, false} {
		spans.Reset()
		bridge.mu.Lock()
		bridge.recs = nil
		bridge.mu.Unlock()
		req := connect.NewRequest(&emptypb.Empty{})
		if probe {
			req.Header().Set(HeaderProbe, "1")
		}
		if _, err := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+procedure).CallUnary(t.Context(), req); err != nil {
			t.Fatal(err)
		}

		ended := spans.Ended()
		if len(ended) != 1 {
			t.Fatalf("probe=%v: %d spans, want TracingInterceptor's one", probe, len(ended))
		}
		got := attrOf(ended[0].Attributes(), AttrProbe)
		if probe && (got.Type() != attribute.BOOL || got.Emit() != "true") {
			t.Errorf("probe span %s = %s %q, want bool true", AttrProbe, got.Type(), got.Emit())
		}
		if !probe && got.Type() != attribute.INVALID {
			t.Errorf("real span carries %s=%s", AttrProbe, got.Emit())
		}

		bridge.mu.Lock()
		recs := bridge.recs
		bridge.mu.Unlock()
		if len(recs) == 0 {
			t.Fatalf("probe=%v: nothing reached the OTLP bridge", probe)
		}
		for _, rec := range recs {
			v, has := rec[AttrProbe]
			if probe && v.Emit() != "true" {
				t.Errorf("probe OTLP record %v: %s = %q, want \"true\"", rec, AttrProbe, v.Emit())
			}
			if !probe && has {
				t.Errorf("real OTLP record carries %s=%s", AttrProbe, v.Emit())
			}
		}
	}
}

func attrOf(kvs []attribute.KeyValue, key string) attribute.Value {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}
