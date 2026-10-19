package serverkit

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/reliant-labs/forge/pkg/observe/metrics"
)

// drain_phases.go — what a graceful shutdown did, one line per phase.
//
// A rollout that misbehaves is diagnosed from the shutdown's own log, and the
// sequence used to say "server stopping" and then nothing until an error, so
// "where did the 30 seconds go" had no answer: was it the pre-stop pause, a
// stream nobody ended, a worker ignoring its context, a teardown blocked on a
// pool? Each phase now writes one INFO line, every one with `phase` and its
// `elapsed`, and the listener's carries what the drain did to its streams. The
// same durations go to forge.server.drain.duration, exported by the telemetry
// flush that ends the shutdown.

// drainMetric is the built-in histogram of shutdown phase durations.
const drainMetric = "forge.server.drain.duration"

// drainPhases logs and measures the phases of one shutdown.
type drainPhases struct {
	logger *slog.Logger
	hist   metric.Float64Histogram // nil: no meter
	start  time.Time
	last   time.Time
}

// newDrainPhases starts timing a shutdown now. A nil meter measures nothing.
func newDrainPhases(logger *slog.Logger, meter metric.Meter) *drainPhases {
	now := time.Now()
	p := &drainPhases{logger: logger, start: now, last: now}
	if meter != nil {
		s := metrics.BuiltinSpec(drainMetric)
		if h, err := meter.Float64Histogram(s.Name, metric.WithUnit(s.Unit), metric.WithDescription(s.Description)); err == nil {
			p.hist = h
		}
	}
	return p
}

// done records that phase has finished: one INFO line, msg plus attrs, with
// the phase's name and how long it took.
func (p *drainPhases) done(phase, msg string, attrs ...any) {
	now := time.Now()
	elapsed := now.Sub(p.last)
	p.last = now
	p.record(phase, elapsed)
	p.logger.Info(msg, append([]any{"phase", phase, "elapsed", elapsed.Round(time.Millisecond)}, attrs...)...)
}

// complete records the whole shutdown against its budget.
func (p *drainPhases) complete(budget time.Duration) {
	total := time.Since(p.start)
	p.record("total", total)
	p.logger.Info("drain: complete", "phase", "total", "elapsed", total.Round(time.Millisecond), "shutdown_timeout", budget)
}

func (p *drainPhases) record(phase string, d time.Duration) {
	if p.hist != nil {
		p.hist.Record(context.Background(), d.Seconds(), metric.WithAttributes(attribute.String("phase", phase)))
	}
}
