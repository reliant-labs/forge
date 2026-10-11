package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func traceJSONLogger(buf *bytes.Buffer, cfg Config) *slog.Logger {
	return slog.New(NewLogHandler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}), cfg))
}

func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func recordingSpan(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	t.Cleanup(func() { span.End() })
	return ctx, span.SpanContext()
}

func TestJSONLogsCarryTraceAndSpanIDsWhenSpanIsActive(t *testing.T) {
	clearOTelEnv(t)
	var buf bytes.Buffer
	logger := traceJSONLogger(&buf, Config{})
	ctx, sc := recordingSpan(t)

	logger.InfoContext(ctx, "in span", "k", "v")
	logger.LogAttrs(ctx, slog.LevelWarn, "attrs path")
	logger.Info("no ctx")
	logger.InfoContext(context.Background(), "no span")

	lines := decodeLines(t, &buf)
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4: %s", len(lines), buf.String())
	}
	for _, i := range []int{0, 1} {
		if lines[i]["trace_id"] != sc.TraceID().String() || lines[i]["span_id"] != sc.SpanID().String() {
			t.Errorf("line %d ids = %v/%v, want %s/%s", i, lines[i]["trace_id"], lines[i]["span_id"], sc.TraceID(), sc.SpanID())
		}
	}
	for _, i := range []int{2, 3} {
		if _, ok := lines[i]["trace_id"]; ok {
			t.Errorf("line %d has trace_id without an active span: %v", i, lines[i])
		}
		if _, ok := lines[i]["span_id"]; ok {
			t.Errorf("line %d has span_id without an active span", i)
		}
	}
}

func TestTraceHandlerKeepsAnExistingTraceID(t *testing.T) {
	var buf bytes.Buffer
	logger := traceJSONLogger(&buf, Config{})
	ctx, _ := recordingSpan(t)
	logger.InfoContext(ctx, "x", "trace_id", "explicit")
	lines := decodeLines(t, &buf)
	if lines[0]["trace_id"] != "explicit" {
		t.Errorf("explicit trace_id was overwritten: %v", lines[0]["trace_id"])
	}
}

func TestTraceHandlerSurvivesWithAttrsAndWithGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := traceJSONLogger(&buf, Config{}).With("component", "db")
	ctx, sc := recordingSpan(t)
	logger.InfoContext(ctx, "x")
	lines := decodeLines(t, &buf)
	if lines[0]["component"] != "db" || lines[0]["trace_id"] != sc.TraceID().String() {
		t.Errorf("With() lost data: %v", lines[0])
	}
}

func TestTraceHandlerHonoursLevel(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}), Config{})
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("handler enabled below the wrapped handler's level")
	}
}

// OTLP logs are off by default: with an endpoint present and nothing else, the
// log handler is the stdout wrapper alone (no multi-handler, no bridge) and no
// log export request is ever made.
func TestOTLPLogsAreOffByDefault(t *testing.T) {
	clearOTelEnv(t)
	snapshotGlobals(t)
	rcv := newOTLPReceiver(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", rcv.URL)

	var buf bytes.Buffer
	cfg := Config{ServiceName: "svc"}
	h := NewLogHandler(slog.NewJSONHandler(&buf, nil), cfg)
	if _, ok := h.(*traceHandler); !ok {
		t.Fatalf("default log handler = %T, want the plain trace handler (no OTLP bridge)", h)
	}
	shutdown, _, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(h).Info("hello")
	flush(t, shutdown)
	if got := rcv.requests("/v1/logs"); len(got) != 0 {
		t.Errorf("logs were exported without the switch: %+v", got)
	}
}

func TestOTLPLogsSwitchShipsLogsAndKeepsStdout(t *testing.T) {
	for name, enable := range map[string]func(t *testing.T) Config{
		"env":    func(t *testing.T) Config { t.Setenv("OTEL_LOGS_EXPORTER", "otlp"); return Config{ServiceName: "svc"} },
		"config": func(t *testing.T) Config { return Config{ServiceName: "svc", OTLPLogs: true} },
	} {
		t.Run(name, func(t *testing.T) {
			clearOTelEnv(t)
			snapshotGlobals(t)
			prev := otel.GetLoggerProvider()
			t.Cleanup(func() { otel.SetLoggerProvider(prev) })
			rcv := newOTLPReceiver(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", rcv.URL)
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=logs-key")
			cfg := enable(t)

			shutdown, _, err := Setup(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil), cfg))
			logger.Info("shipped line", "k", "v")
			flush(t, shutdown)

			got := rcv.requests("/v1/logs")
			if len(got) == 0 {
				t.Fatal("no OTLP log export reached /v1/logs")
			}
			if !bytes.Contains(got[0].Body, []byte("shipped line")) {
				t.Error("exported log body does not contain the message")
			}
			if got[0].Header.Get("Authorization") != "logs-key" {
				t.Errorf("log export lost OTLP headers: %v", got[0].Header)
			}
			if !strings.Contains(buf.String(), "shipped line") {
				t.Error("stdout line must still be written when OTLP logs are on")
			}
		})
	}
}

func TestOTLPLogsNeedAnEndpointAndAnEnabledSDK(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	if exportEnabled(signalLogs, Config{}) {
		t.Error("logs export enabled with no endpoint")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	if !exportEnabled(signalLogs, Config{}) {
		t.Error("logs export disabled despite switch and endpoint")
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if exportEnabled(signalLogs, Config{}) {
		t.Error("OTEL_SDK_DISABLED=true must disable logs export")
	}
}
