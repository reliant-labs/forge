package serverkit

import (
	"context"
	"io"
	"log/slog"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The phase durations are exported as the declared built-in histogram, one
// data point per phase, so a dashboard can plot where each rollout's
// shutdown spent its budget.
func TestDrainPhases_RecordTheDeclaredHistogram(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")
	p := newDrainPhases(slog.New(slog.NewTextHandler(io.Discard, nil)), meter)
	p.done("pre_stop", "drain: pre-stop")
	p.done("listener", "drain: listener")
	p.complete(0)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	phases := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != drainMetric {
				continue
			}
			if m.Unit != "s" {
				t.Errorf("unit = %q, want s (the declaration's)", m.Unit)
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is a %T, want a float64 histogram", drainMetric, m.Data)
			}
			for _, dp := range hist.DataPoints {
				v, _ := dp.Attributes.Value("phase")
				phases[v.AsString()] = true
			}
		}
	}
	for _, want := range []string{"pre_stop", "listener", "total"} {
		if !phases[want] {
			t.Errorf("no %s data point for phase %q; got %v", drainMetric, want, phases)
		}
	}
}
