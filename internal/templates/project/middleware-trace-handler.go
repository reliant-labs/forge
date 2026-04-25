//go:build ignore

package middleware

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// TraceHandler wraps an slog.Handler to automatically inject trace_id and span_id
// from the OpenTelemetry span context into every log record.
type TraceHandler struct {
	inner slog.Handler
}

// NewTraceHandler returns a new TraceHandler that wraps the given handler.
func NewTraceHandler(inner slog.Handler) *TraceHandler {
	return &TraceHandler{inner: inner}
}

// Enabled reports whether the inner handler handles records at the given level.
func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds trace_id and span_id attributes from the OpenTelemetry span context, then delegates to the inner handler.
func (h *TraceHandler) Handle(ctx context.Context, record slog.Record) error {
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.HasTraceID() {
		record.AddAttrs(slog.String("trace_id", spanCtx.TraceID().String()))
		if spanCtx.HasSpanID() {
			record.AddAttrs(slog.String("span_id", spanCtx.SpanID().String()))
		}
	}
	return h.inner.Handle(ctx, record)
}

// WithAttrs returns a new TraceHandler whose inner handler has the given attributes.
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup returns a new TraceHandler whose inner handler has the given group name.
func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{inner: h.inner.WithGroup(name)}
}