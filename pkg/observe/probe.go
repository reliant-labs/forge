// File: probe.go — a platform release probe, marked so it is never counted as real traffic.
//
// The control plane probes a hosted release on a steady schedule to judge it
// when real traffic is thin. The platform's edge verifies each probe's
// signature (workloads never hold the key), deletes every client copy of
// X-Reliant-Probe, and stamps X-Reliant-Probe: 1 on a verified probe before it
// reaches the workload. ProbeFromEdge reads that stamp into the request's
// context, and the request is then:
//
//   - marked forge.probe=true on its server span (the Trace slot's span, and
//     TracingInterceptor's) and on every log record NewLogHandler writes with
//     its context, so the real-traffic figures (RED, error logs, issues) leave
//     it out;
//   - not recorded in the rpc.server.* metrics (MetricsInterceptor), so the
//     metric alerts never see it;
//   - not a product event: Track is a no-op.
//
// # Threat model
//
// A request marked as a probe disappears from its service's metrics, error
// rates, issues and alerts. A client that could mark its own requests could
// therefore hide an attack from the tenant. So the header is honoured only
// when the process trusts its edge (TrustedEdge, FORGE_TRUSTED_EDGE=reliant):
// behind Reliant's proxy, which strips any copy a client sent before stamping
// its own. Anywhere else the header is a client's claim and is ignored.
//
// Either way the header is deleted before anything behind ProbeFromEdge sees
// it: a handler never reads the stamp, and never mistakes a client's copy for
// the platform's.
//
// The value must be exactly "1", sent once. Anything else is not the proxy's
// stamp, and the request is real traffic.
//
// Telemetry a workload emits while serving a probe is still billed: the
// forge.probe attribute is written by the tenant's own process, so nothing
// that bills may trust it.

package observe

import (
	"context"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The probe contract, as the platform edge and obsquery read it. The values are
// release.ProbeEdgeHeader and release.ProbeAttr (forge/pkg/release), declared
// again here so every workload does not import the release-shape package for
// two strings; a test holds them equal.
const (
	// HeaderProbe is what the platform edge stamps ("1") on a request it
	// verified as a release probe.
	HeaderProbe = "X-Reliant-Probe"
	// AttrProbe is the span and log attribute, value true, a probe carries.
	AttrProbe = "forge.probe"
)

// headerProbeValue is the edge's stamp.
const headerProbeValue = "1"

type probeKey struct{}

// WithProbe returns ctx marked as serving a platform release probe.
func WithProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeKey{}, true)
}

// IsProbe reports whether ctx serves a platform release probe (see the file
// comment).
func IsProbe(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	probe, _ := ctx.Value(probeKey{}).(bool)
	return probe
}

// ProbeFromEdge marks a request as a platform release probe when the process
// trusts the Reliant edge (TrustedEdge) and the edge stamped
// X-Reliant-Probe: 1. It always deletes the header before next runs. The
// trust is read once, here. serverkit installs it outermost on every listener.
func ProbeFromEdge(next http.Handler) http.Handler {
	return probeFromEdge(next, TrustedEdge())
}

func probeFromEdge(next http.Handler, trusted bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(HeaderProbe)
		if len(values) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del(HeaderProbe)
		if trusted && len(values) == 1 && values[0] == headerProbeValue {
			r = r.WithContext(WithProbe(r.Context()))
		}
		next.ServeHTTP(w, r)
	})
}

// markProbeSpan sets forge.probe=true on span when ctx serves a probe.
func markProbeSpan(ctx context.Context, span trace.Span) {
	if IsProbe(ctx) {
		span.SetAttributes(attribute.Bool(AttrProbe, true))
	}
}

// probeLogHandler adds forge.probe=true to every record logged with a probe's
// context. It wraps the whole log handler, as errorInputsHandler does, so the
// stdout line and the OTLP record both carry it. Under logger.WithGroup the
// attribute nests in the group, as trace_id does.
type probeLogHandler struct{ next slog.Handler }

func (h probeLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h probeLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if IsProbe(ctx) && !hasAttr(r, AttrProbe) {
		r = r.Clone()
		r.AddAttrs(slog.Bool(AttrProbe, true))
	}
	return h.next.Handle(ctx, r)
}

func (h probeLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return probeLogHandler{next: h.next.WithAttrs(attrs)}
}

func (h probeLogHandler) WithGroup(name string) slog.Handler {
	return probeLogHandler{next: h.next.WithGroup(name)}
}
