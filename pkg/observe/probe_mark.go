package observe

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// attrForgeProbe is the span attribute that marks a request as a probe:
// release.ProbeAttr, declared again here so a workload does not import the
// release-shape package for one string (probe_mark_test.go holds them equal).
// Readers keep marked spans out of real traffic (obsquery.SpanNotProbeSQL).
const attrForgeProbe = "forge.probe"

// MarkProbeSpans marks the request in ctx as a probe: forge.probe=true on its
// server span (the one RequestSpanInterceptor recorded) and on the innermost
// span, so release health, RED and the issue views leave it out while trace
// search still finds it.
//
// Call it only from a handler whose every caller is a probe, such as the
// health rpc: the mark takes a request out of every error rate, so a handler
// that real callers reach must never set it on their behalf.
func MarkProbeSpans(ctx context.Context) {
	mark := attribute.Bool(attrForgeProbe, true)
	server := serverSpanOf(ctx)
	server.SetAttributes(mark)
	if inner := trace.SpanFromContext(ctx); inner != server {
		inner.SetAttributes(mark)
	}
}

// healthServicePrefix is the gRPC health service's procedure prefix.
const healthServicePrefix = "/grpc.health.v1.Health/"

// IsHealthProcedure reports whether procedure is the gRPC health service's
// (Check, Watch). Its calls are probes: MetricsInterceptor records no
// rpc.server.* sample for them, so metric alerts and dashboards count only
// real traffic, and the health handler marks its spans (MarkProbeSpans).
func IsHealthProcedure(procedure string) bool {
	return strings.HasPrefix(procedure, healthServicePrefix)
}
