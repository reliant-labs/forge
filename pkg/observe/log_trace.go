package observe

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/trace"
)

// Log attribute keys carrying the active span, matching what the audit
// middleware already writes.
const (
	LogKeyTraceID = "trace_id"
	LogKeySpanID  = "span_id"
)

// NewTraceHandler wraps next so every record logged with a context holding a
// valid span carries trace_id and span_id. Records without a span, and records
// that already carry a trace_id attribute, pass through unchanged. The ids are
// only available to the *Context logging methods (InfoContext, LogAttrs, ...):
// slog.Info and friends have no context to read.
//
// Under logger.WithGroup the ids are nested in the group, as with any
// attribute added after the group opens.
func NewTraceHandler(next slog.Handler) slog.Handler { return &traceHandler{next: next} }

// NewLogHandler is the handler serverkit builds for the process logger: next
// (the stdout JSON or text handler) wrapped by NewTraceHandler. When
// the OTLP logs switch is on (Config.OTLPLogs or OTEL_LOGS_EXPORTER=otlp) it is also teed into the OpenTelemetry logs bridge,
// which Setup connects to an OTLP exporter.
//
// OTLP logs export is OFF by default and is the SECOND shipping path, not an
// addition to the first: the stdout stream is still written, so a workload that
// enables the OTLP exporter must not also be collected from its stdout/log files
// (the local collector's file reader, a cluster node agent), or every line
// arrives twice. Exactly one shipping path per environment.
func NewLogHandler(next slog.Handler, cfg Config) slog.Handler {
	h := NewTraceHandler(next)
	if !exportEnabled(signalLogs, cfg) {
		return h
	}
	return slog.NewMultiHandler(h, otelslog.NewHandler(logScopeName(cfg)))
}

func logScopeName(cfg Config) string {
	if cfg.ServiceName != "" {
		return cfg.ServiceName
	}
	return "unknown"
}

type traceHandler struct{ next slog.Handler }

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() && !hasAttr(r, LogKeyTraceID) {
		r.AddAttrs(
			slog.String(LogKeyTraceID, sc.TraceID().String()),
			slog.String(LogKeySpanID, sc.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{next: h.next.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{next: h.next.WithGroup(name)}
}

func hasAttr(r slog.Record, key string) bool {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		found = a.Key == key
		return !found
	})
	return found
}
